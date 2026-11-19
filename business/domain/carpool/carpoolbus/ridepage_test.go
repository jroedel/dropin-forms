package carpoolbus_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/carpool/carpoolbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/business/types/phone"
)

func rideKey(t *testing.T) carpoolbus.RideKey {
	t.Helper()

	k, err := submissionbus.ParseAnswerKey("a-test-secret-that-is-long-enough-for-a-key")
	if err != nil {
		t.Fatal(err)
	}

	return carpoolbus.NewRideKey(k)
}

// A ride link is short enough for a text, leads to its own submission, and
// is refused when it was not made here or was changed on the way.
func TestARideLinkIsShortAndOnlyOurs(t *testing.T) {
	k := rideKey(t)
	id := types.NewID()

	token, err := carpoolbus.MintRideLink(k, id)
	if err != nil {
		t.Fatalf("MintRideLink: %v", err)
	}

	if len(token) > 40 {
		t.Errorf("the token is %d characters, want at most 40 so the link fits in a text", len(token))
	}

	if got, err := carpoolbus.ReadRideLink(k, token); err != nil || got != id {
		t.Errorf("ReadRideLink = %v, %v; want %v", got, err, id)
	}

	tampered := []byte(token)
	tampered[3] ^= 1

	if _, err := carpoolbus.ReadRideLink(k, string(tampered)); !errors.Is(err, carpoolbus.ErrBadRideLink) {
		t.Errorf("a changed token: %v, want ErrBadRideLink", err)
	}

	other, _ := submissionbus.ParseAnswerKey("another-secret-that-is-long-enough-for-a-key")
	if _, err := carpoolbus.ReadRideLink(carpoolbus.NewRideKey(other), token); !errors.Is(err, carpoolbus.ErrBadRideLink) {
		t.Errorf("another installation's token: %v, want ErrBadRideLink", err)
	}
}

func (h harness) mine(t *testing.T, id types.ID) carpoolbus.Mine {
	t.Helper()

	m, err := h.b.Mine(t.Context(), h.f, id)
	if err != nil {
		t.Fatalf("Mine: %v", err)
	}

	return m
}

// A rider sees their car and the others with room, and picks another: the
// driver they joined and the one they left are texted, the rider is not.
func TestARiderChoosesAnotherCarWithRoom(t *testing.T) {
	h := newHarness(t)

	damien := h.signups.add(t, h.f, "Damien", "5125550101", "drive", 1)
	maria := h.signups.add(t, h.f, "Maria", "5125550102", "drive", 2)
	ana := h.signups.add(t, h.f, "Ana", "5125550103", "need", 0)
	h.arrange(t)

	m := h.mine(t, ana)
	if m.Car == nil || m.Car.ID != damien || len(m.Room) != 1 || m.Room[0].ID != maria {
		t.Fatalf("Ana's page: car %v, room %v; want Damien's, and Maria's to choose", m.Car, m.Room)
	}

	anaTexts := len(h.texts.to("5125550103"))
	h.later(time.Minute)

	if err := h.b.Choose(t.Context(), h.f, ana, maria); err != nil {
		t.Fatalf("Choose: %v", err)
	}

	if m := h.mine(t, ana); m.Car == nil || m.Car.ID != maria {
		t.Errorf("Ana is in %v, want Maria's car", m.Car)
	}

	// Nobody is texted at once. Both drivers hear in their next summary.
	if got := len(h.texts.to("5125550102")) + len(h.texts.to("5125550101")); got != 2 {
		t.Errorf("the drivers were texted %d times, want only their two welcomes", got)
	}

	h.later(10 * time.Minute)
	h.arrange(t)

	if got := h.texts.to("5125550102"); len(got) != 2 || !strings.Contains(got[1], "your car now: Ana, (512) 555-0103. 1 seat left.") {
		t.Errorf("Maria was texted %q", got)
	}

	if got := h.texts.to("5125550101"); len(got) != 2 || !strings.Contains(got[1], "nobody is riding with you right now") {
		t.Errorf("Damien was texted %q", got)
	}

	if got := len(h.texts.to("5125550103")); got != anaTexts {
		t.Errorf("Ana was texted %d more times about a car she chose herself", got-anaTexts)
	}

	// Choosing the car she is in does nothing; a full one is refused.
	if err := h.b.Choose(t.Context(), h.f, ana, maria); err != nil {
		t.Errorf("choosing the same car again: %v", err)
	}

	luis := h.signups.add(t, h.f, "Luis", "5125550104", "need", 0)
	h.arrange(t) // Luis takes Damien's free seat

	if err := h.b.Choose(t.Context(), h.f, ana, damien); !errors.Is(err, carpoolbus.ErrCarFull) {
		t.Errorf("choosing a full car: %v, want ErrCarFull", err)
	}

	// Somebody who has their own ride cannot choose a car.
	rosa := h.signups.add(t, h.f, "Rosa", "5125550105", "have", 0)
	if err := h.b.Choose(t.Context(), h.f, rosa, maria); !errors.Is(err, carpoolbus.ErrNotFound) {
		t.Errorf("a person with their own ride choosing a car: %v, want ErrNotFound", err)
	}

	_ = luis
}

// "I'm not coming" takes a rider out of their car, tells their driver and the
// office, and leaves the sign-up alone.
func TestARiderWhoIsNotComingLeavesTheCarAndTheOfficeIsTold(t *testing.T) {
	h := newHarness(t)

	h.signups.add(t, h.f, "Damien", "5125550101", "drive", 1)
	ana := h.signups.add(t, h.f, "Ana", "5125550103", "need", 0)
	h.signups.add(t, h.f, "Luis", "5125550104", "need", 0)
	h.arrange(t)
	h.later(time.Minute)

	if err := h.b.Say(t.Context(), h.f, ana, carpoolbus.Out, 0); err != nil {
		t.Fatalf("Say: %v", err)
	}

	plan := h.arrange(t)

	if got := ridersOf(plan.Cars[0]); strings.Join(got, ",") != "Luis" {
		t.Errorf("Damien's car = %v, want Luis in Ana's seat", got)
	}

	if !plan.NotComing[ana] || plan.Coming != 2 {
		t.Errorf("not coming %v, coming %d; want Ana out and two coming", plan.NotComing, plan.Coming)
	}

	notices := strings.Join(h.office.notices, "\n---\n")
	if !strings.Contains(notices, "Ana is not coming") || !strings.Contains(notices, "riding with Damien") ||
		!strings.Contains(notices, "still in the results") {
		t.Errorf("the office was told:\n%s", notices)
	}

	h.later(10 * time.Minute)
	h.arrange(t)

	if got := h.texts.to("5125550101"); !strings.Contains(strings.Join(got, "|"), "your car now: Luis") {
		t.Errorf("Damien was texted %q", got)
	}

	// Said twice, it is news once.
	before := len(h.office.notices)
	if err := h.b.Say(t.Context(), h.f, ana, carpoolbus.Out, 0); err != nil || len(h.office.notices) != before {
		t.Errorf("saying it again: %v, and %d more notices", err, len(h.office.notices)-before)
	}

	// And they can change their mind.
	if err := h.b.Say(t.Context(), h.f, ana, formbus.CarpoolNeed, 0); err != nil {
		t.Fatalf("Say need: %v", err)
	}

	if m := h.mine(t, ana); !m.Waiting || m.Me.Choice != formbus.CarpoolNeed {
		t.Errorf("after changing her mind: %+v", m)
	}
}

// A driver who can no longer drive sends their riders to other cars, and one
// who changes their seats keeps the riders who fit.
func TestADriverStopsDrivingOrChangesTheirSeats(t *testing.T) {
	h := newHarness(t)

	damien := h.signups.add(t, h.f, "Damien", "5125550101", "drive", 2)
	maria := h.signups.add(t, h.f, "Maria", "5125550102", "drive", 2)
	h.signups.add(t, h.f, "Ana", "5125550103", "need", 0)
	h.signups.add(t, h.f, "Luis", "5125550104", "need", 0)
	h.arrange(t)

	if err := h.b.Say(t.Context(), h.f, damien, formbus.CarpoolDrive, 99); !errors.Is(err, carpoolbus.ErrBadChoice) {
		t.Errorf("99 seats: %v, want ErrBadChoice", err)
	}

	if err := h.b.Say(t.Context(), h.f, damien, formbus.CarpoolDrive, 1); err != nil {
		t.Fatalf("one seat: %v", err)
	}

	plan := h.arrange(t)
	if got := ridersOf(plan.Cars[0]); strings.Join(got, ",") != "Ana" || !plan.Cars[0].Said {
		t.Errorf("Damien's car = %v (said %v), want Ana only, from his page", got, plan.Cars[0].Said)
	}

	if got := ridersOf(plan.Cars[1]); strings.Join(got, ",") != "Luis" {
		t.Errorf("Maria's car = %v, want Luis, who no longer fit in Damien's", got)
	}

	if err := h.b.Say(t.Context(), h.f, damien, formbus.CarpoolHave, 0); err != nil {
		t.Fatalf("no longer driving: %v", err)
	}

	plan = h.arrange(t)
	if len(plan.Cars) != 1 || plan.Cars[0].ID != maria || len(plan.Cars[0].Riders) != 2 {
		t.Errorf("after Damien stopped driving: %+v", plan.Cars)
	}
}

// The latest word counts: answers changed after something was said on the
// ride page win over it.
func TestALaterChangeToTheAnswersBeatsAnEarlierChoice(t *testing.T) {
	h := newHarness(t)

	ana := h.signups.add(t, h.f, "Ana", "5125550103", "need", 0)

	if err := h.b.Say(t.Context(), h.f, ana, formbus.CarpoolHave, 0); err != nil {
		t.Fatal(err)
	}

	if m := h.mine(t, ana); m.Me.Choice != formbus.CarpoolHave {
		t.Fatalf("after saying so: %q", m.Me.Choice)
	}

	h.signups.answer(ana, formbus.CarpoolNeed, start.Add(time.Hour))

	if m := h.mine(t, ana); m.Me.Choice != formbus.CarpoolNeed {
		t.Errorf("after changing the answers later: %q, want need", m.Me.Choice)
	}
}

// Somebody who lost their link asks for it by number: every sign-up with that
// number is texted its link, nobody else is, and asking again at once sends
// nothing.
func TestTheLinksAreTextedAgainToTheNumberThatSignedUp(t *testing.T) {
	h := newHarness(t)

	ana := h.signups.add(t, h.f, "Ana", "5125550103", "need", 0)
	h.signups.add(t, h.f, "Luis", "5125550104", "have", 0)

	h.b.SendLinks(t.Context(), h.f, phone.MustParse("(512) 555-0103"))

	got := h.texts.to("5125550103")
	if len(got) != 1 || !strings.Contains(got[0], "https://f.schoenstatt.test"+h.b.Link(ana)) {
		t.Fatalf("Ana was texted %q, want her link", got)
	}

	if len(h.texts.to("5125550104")) != 0 {
		t.Error("somebody with another number was texted")
	}

	h.b.SendLinks(t.Context(), h.f, phone.MustParse("(512) 555-0103"))

	if len(h.texts.to("5125550103")) != 1 {
		t.Error("asking again at once texted the number again")
	}

	// A number nobody gave sends nothing, and says so to nobody.
	before := len(h.texts.sent)
	h.b.SendLinks(t.Context(), h.f, phone.MustParse("(512) 555-0199"))

	if len(h.texts.sent) != before {
		t.Error("a number nobody gave was texted")
	}
}

// A link resolves to its form and submission while the submission counts,
// and to nothing once it is gone.
func TestALinkResolvesWhileTheSignUpCounts(t *testing.T) {
	h := newHarness(t)

	ana := h.signups.add(t, h.f, "Ana", "5125550103", "need", 0)
	token := strings.TrimPrefix(h.b.Link(ana), "/r/")

	f, id, err := h.b.Resolve(t.Context(), token)
	if err != nil || id != ana || f.ID != h.f.ID {
		t.Fatalf("Resolve = %v, %v, %v", f.ID, id, err)
	}

	h.signups.drop(ana)

	if _, _, err := h.b.Resolve(t.Context(), token); !errors.Is(err, carpoolbus.ErrNotFound) {
		t.Errorf("after the sign-up went: %v, want ErrNotFound", err)
	}
}

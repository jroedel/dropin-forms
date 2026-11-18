package carpoolbus_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/carpool/carpoolbus"
	"github.com/jroedel/dropin-forms/business/domain/carpool/stores/carpooldb"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/business/types/phone"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

var start = time.Date(2026, 11, 17, 18, 0, 0, 0, time.UTC)

// event is a youth retreat with a carpool.
func event(t *testing.T) formbus.Form {
	t.Helper()

	slug, err := types.ParseSlug("youth-retreat")
	if err != nil {
		t.Fatal(err)
	}

	f := formbus.Form{
		ID: slug, Title: "Youth retreat", Currency: "usd",
		Fields: []formbus.Field{
			{Name: "name", Label: "Name", Kind: formbus.KindText, Required: true},
			{Name: "mobile", Label: "Mobile", Kind: formbus.KindTel},
			{Name: "ride", Label: "Getting there", Kind: formbus.KindCarpool, Required: true, Phone: "mobile", Who: "name",
				Options: []formbus.Option{
					{Value: formbus.CarpoolHave, Label: "Own ride"},
					{Value: formbus.CarpoolDrive, Label: "Driving"},
					{Value: formbus.CarpoolNeed, Label: "Need a ride"},
				}},
		},
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	return f
}

// signups is the form's submissions, which a test adds to and changes.
type signups struct {
	mu   sync.Mutex
	subs []submissionbus.Submission
}

func (s *signups) ByForm(context.Context, types.Slug) ([]submissionbus.Submission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.subs), nil
}

// add signs somebody up, a minute after the one before.
func (s *signups) add(t *testing.T, f formbus.Form, name, number, choice string, seats int) types.ID {
	t.Helper()

	in := formbus.Values{"name": {name}, "mobile": {number}, "ride": {choice}}
	if seats > 0 {
		in["ride.seats"] = []string{strconv.Itoa(seats)}
	}

	ans, err := f.Validate(start, in)
	if err != nil {
		t.Fatalf("Validate %s: %v", name, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sub := submissionbus.Submission{
		ID: types.NewID(), Form: f.ID, Status: submissionbus.StatusReceived, Answers: ans,
		CreatedAt: start.Add(time.Duration(len(s.subs)) * time.Minute),
	}
	s.subs = append(s.subs, sub)

	return sub.ID
}

func (s *signups) drop(id types.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.subs = slices.DeleteFunc(s.subs, func(x submissionbus.Submission) bool { return x.ID == id })
}

func (s *signups) seats(t *testing.T, f formbus.Form, id types.ID, n int) {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.subs {
		if s.subs[i].ID != id {
			continue
		}

		for j := range s.subs[i].Answers.Fields {
			if s.subs[i].Answers.Fields[j].Name == "ride" {
				s.subs[i].Answers.Fields[j].Values = []string{formbus.CarpoolDrive, strconv.Itoa(n)}
			}
		}
	}
}

type forms struct{ f formbus.Form }

func (x forms) All() []formbus.Form { return []formbus.Form{x.f} }

// texter records what would have been sent. A number in refuse is answered
// as Twilio answers somebody who replied STOP.
type texter struct {
	mu     sync.Mutex
	sent   []sent
	refuse map[string]bool
}

type sent struct{ to, body string }

type optedOut struct{}

func (optedOut) Error() string    { return "Twilio refused the message (21610)" }
func (optedOut) IsOptedOut() bool { return true }

func (x *texter) Send(_ context.Context, to, body string) (string, error) {
	x.mu.Lock()
	defer x.mu.Unlock()

	if x.refuse[to] {
		return "", optedOut{}
	}

	x.sent = append(x.sent, sent{to, body})

	return "SM1", nil
}

func (x *texter) to(number string) []string {
	x.mu.Lock()
	defer x.mu.Unlock()

	var out []string

	for _, m := range x.sent {
		if m.to == phone.MustParse(number).String() {
			out = append(out, m.body)
		}
	}

	return out
}

// staff is one organiser with a mobile number, and one without.
type staff struct{ organiser, other userbus.User }

func (s staff) ForForm(context.Context, types.Slug) ([]accessbus.Grant, error) {
	return []accessbus.Grant{
		{UserID: s.organiser.ID, Role: accessbus.RoleAdmin},
		{UserID: s.other.ID, Role: accessbus.RoleResults},
	}, nil
}

func (s staff) ByID(_ context.Context, id types.ID) (userbus.User, error) {
	if id == s.organiser.ID {
		return s.organiser, nil
	}

	return s.other, nil
}

type harness struct {
	b       *carpoolbus.Business
	f       formbus.Form
	signups *signups
	texts   *texter
	store   *carpooldb.Store
}

const organiserNumber = "5125550199"

func newHarness(t *testing.T) harness {
	t.Helper()

	db, err := sqldb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if err := carpooldb.Init(t.Context(), db); err != nil {
		t.Fatal(err)
	}

	h := harness{f: event(t), signups: &signups{}, texts: &texter{refuse: map[string]bool{}}, store: carpooldb.NewStore(db)}

	people := staff{
		organiser: userbus.User{ID: types.NewID(), Enabled: true, Mobile: phone.MustParse(organiserNumber)},
		other:     userbus.User{ID: types.NewID(), Enabled: true, Mobile: phone.MustParse("5125550198")},
	}

	h.b, err = carpoolbus.NewBusiness(carpoolbus.Config{
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:       h.store,
		Submissions: h.signups,
		Forms:       forms{h.f},
		Texter:      h.texts,
		Grants:      people,
		Accounts:    people,
		Now:         func() time.Time { return start },
	})
	if err != nil {
		t.Fatal(err)
	}

	return h
}

func (h harness) arrange(t *testing.T) carpoolbus.Plan {
	t.Helper()

	if err := h.b.Arrange(t.Context(), h.f); err != nil {
		t.Fatalf("Arrange: %v", err)
	}

	plan, err := h.b.PlanOf(t.Context(), h.f)
	if err != nil {
		t.Fatalf("PlanOf: %v", err)
	}

	return plan
}

func ridersOf(c carpoolbus.Car) []string {
	var out []string
	for _, s := range c.Riders {
		out = append(out, s.Rider.Name)
	}

	return out
}

func TestRidersFillTheEarliestCarFirst(t *testing.T) {
	h := newHarness(t)

	h.signups.add(t, h.f, "Damien", "5125550101", "drive", 1)
	h.signups.add(t, h.f, "Maria", "5125550102", "drive", 3)
	h.signups.add(t, h.f, "Ana", "5125550103", "need", 0)
	h.signups.add(t, h.f, "Luis", "5125550104", "need", 0)
	h.signups.add(t, h.f, "Rosa", "5125550105", "have", 0)

	plan := h.arrange(t)

	if got := ridersOf(plan.Cars[0]); strings.Join(got, ",") != "Ana" {
		t.Errorf("Damien's car = %v, want [Ana]", got)
	}
	if got := ridersOf(plan.Cars[1]); strings.Join(got, ",") != "Luis" {
		t.Errorf("Maria's car = %v, want [Luis]", got)
	}
	if plan.OwnWay != 1 || len(plan.Waiting) != 0 {
		t.Errorf("own way %d, waiting %d", plan.OwnWay, len(plan.Waiting))
	}

	if got := h.texts.to("5125550101"); len(got) != 1 || !strings.Contains(got[0], "Ana ((512) 555-0103) is riding with you") {
		t.Errorf("Damien was texted %q", got)
	}
	if got := h.texts.to("5125550103"); len(got) != 1 || !strings.Contains(got[0], "riding with Damien ((512) 555-0101)") {
		t.Errorf("Ana was texted %q", got)
	}
	if got := h.texts.to(organiserNumber); len(got) != 2 {
		t.Errorf("the organiser was texted %q, want one per placement", got)
	}
	if got := h.texts.to("5125550198"); len(got) != 0 {
		t.Errorf("somebody who only reads results was texted %q", got)
	}

	// Again: nothing has changed, so nothing is sent.
	before := len(h.texts.sent)
	h.arrange(t)

	if len(h.texts.sent) != before {
		t.Errorf("a second pass sent %d more texts", len(h.texts.sent)-before)
	}
}

func TestARiderWaitsAndIsPlacedWhenADriverSignsUp(t *testing.T) {
	h := newHarness(t)

	h.signups.add(t, h.f, "Ana", "5125550103", "need", 0)

	plan := h.arrange(t)
	h.arrange(t)

	if len(plan.Waiting) != 1 {
		t.Fatalf("waiting = %d, want Ana", len(plan.Waiting))
	}
	if got := h.texts.to(organiserNumber); len(got) != 1 || !strings.Contains(got[0], "every car is full") {
		t.Errorf("the organiser was texted %q, want once that Ana is waiting", got)
	}

	h.signups.add(t, h.f, "Damien", "5125550101", "drive", 2)

	if got := ridersOf(h.arrange(t).Cars[0]); strings.Join(got, ",") != "Ana" {
		t.Errorf("Damien's car = %v, want [Ana]", got)
	}
}

func TestAFewerSeatsGivesUpTheLatestRider(t *testing.T) {
	h := newHarness(t)

	damien := h.signups.add(t, h.f, "Damien", "5125550101", "drive", 2)
	h.signups.add(t, h.f, "Ana", "5125550103", "need", 0)
	h.signups.add(t, h.f, "Luis", "5125550104", "need", 0)
	h.arrange(t)

	h.signups.seats(t, h.f, damien, 1)
	plan := h.arrange(t)

	if got := ridersOf(plan.Cars[0]); strings.Join(got, ",") != "Ana" {
		t.Errorf("Damien's car = %v, want [Ana]", got)
	}
	if len(plan.Waiting) != 1 || plan.Waiting[0].Rider.Name != "Luis" {
		t.Errorf("waiting = %+v, want Luis", plan.Waiting)
	}
	if got := h.texts.to("5125550104"); len(got) != 2 || !strings.Contains(got[1], "Damien can no longer take you") {
		t.Errorf("Luis was texted %q", got)
	}
}

func TestARiderWhoLeavesFreesTheSeatAndTheDriverIsTold(t *testing.T) {
	h := newHarness(t)

	h.signups.add(t, h.f, "Damien", "5125550101", "drive", 1)
	ana := h.signups.add(t, h.f, "Ana", "5125550103", "need", 0)
	h.signups.add(t, h.f, "Luis", "5125550104", "need", 0)
	h.arrange(t)

	h.signups.drop(ana)
	plan := h.arrange(t)

	if got := ridersOf(plan.Cars[0]); strings.Join(got, ",") != "Luis" {
		t.Errorf("Damien's car = %v, want Luis in Ana's seat", got)
	}
	if got := h.texts.to("5125550101"); len(got) != 3 || !strings.Contains(got[1], "Ana is no longer riding with you") {
		t.Errorf("Damien was texted %q", got)
	}
}

func TestADriverWhoLeavesSendsTheirRidersBackToWaiting(t *testing.T) {
	h := newHarness(t)

	damien := h.signups.add(t, h.f, "Damien", "5125550101", "drive", 1)
	h.signups.add(t, h.f, "Ana", "5125550103", "need", 0)
	h.arrange(t)

	h.signups.drop(damien)
	plan := h.arrange(t)

	if len(plan.Cars) != 0 || len(plan.Waiting) != 1 {
		t.Errorf("cars %d, waiting %d; want Ana waiting", len(plan.Cars), len(plan.Waiting))
	}
	if got := h.texts.to("5125550103"); len(got) != 2 || !strings.Contains(got[1], "can no longer take you") {
		t.Errorf("Ana was texted %q", got)
	}
}

func TestAnOptedOutRiderIsRecordedOnTheRide(t *testing.T) {
	h := newHarness(t)
	h.texts.refuse[phone.MustParse("5125550103").String()] = true

	h.signups.add(t, h.f, "Damien", "5125550101", "drive", 1)
	h.signups.add(t, h.f, "Ana", "5125550103", "need", 0)

	plan := h.arrange(t)

	if p := plan.Cars[0].Riders[0].Ride.Problem; !strings.Contains(p, "replied STOP") {
		t.Errorf("problem = %q", p)
	}
	if plan.Cars[0].Riders[0].Ride.DriverTexted.IsZero() {
		t.Error("the driver's text was not recorded although it went")
	}
}

func TestAnOrganiserMovesARider(t *testing.T) {
	h := newHarness(t)

	h.signups.add(t, h.f, "Damien", "5125550101", "drive", 1)
	maria := h.signups.add(t, h.f, "Maria", "5125550102", "drive", 1)
	ana := h.signups.add(t, h.f, "Ana", "5125550103", "need", 0)
	h.arrange(t)

	if err := h.b.Move(t.Context(), h.f, ana, maria, types.NewID()); err != nil {
		t.Fatalf("Move: %v", err)
	}

	plan := h.arrange(t)
	if len(plan.Cars[0].Riders) != 0 || strings.Join(ridersOf(plan.Cars[1]), ",") != "Ana" {
		t.Errorf("after the move: %v / %v", ridersOf(plan.Cars[0]), ridersOf(plan.Cars[1]))
	}

	luis := h.signups.add(t, h.f, "Luis", "5125550104", "need", 0)
	h.arrange(t)

	if err := h.b.Move(t.Context(), h.f, luis, maria, types.NewID()); err == nil {
		t.Error("moving a rider into a full car was allowed")
	}
}

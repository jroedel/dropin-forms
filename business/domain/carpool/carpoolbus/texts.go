package carpoolbus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/business/types/phone"
)

// sendBudget bounds one text. Twilio's own client gives up at ten seconds;
// this is the same number, so that a pass with a dozen placements cannot be
// held for longer than a dozen slow texts.
const sendBudget = 10 * time.Second

// texts is the messages one pass sends, for one form.
//
// Each is short and says no more than its reader needs: whose car, a number
// to call, and the link to their own ride page. Twilio and the carrier keep
// every body, so the rest of a submission stays out of them.
//
// # A rider at once, a driver in a summary
//
// A rider's car is the one fact they are waiting for, so they are texted the
// moment it changes. A driver's car changes every time somebody joins,
// switches or drops out, and in a carpool of young people that is a stream:
// fifteen texts, each about one rider, and no way to tell which is current.
// So a driver is texted once when they first offer to drive, and after that
// one summary of who is in the car now, once it has been quiet for
// [summaryQuiet]. The ride page shows the same thing at any moment.
type texts struct {
	b   *Business
	ctx context.Context
	f   formbus.Form

	// stuck is the riders this pass left with no car, for the one email to
	// the organisers at its end.
	stuck []person

	// cars is the drivers whose riders changed, for their next summary.
	cars map[types.ID]bool
}

func (b *Business) texts(ctx context.Context, f formbus.Form) *texts {
	// Detached from the caller, for notifybus's reason: an organiser who
	// closes the tab after moving somebody must not cancel the texts telling
	// the driver.
	return &texts{b: b, ctx: context.WithoutCancel(ctx), f: f}
}

// send texts one number and says when, or why it did not go.
func (t *texts) send(to phone.Number, body string) (time.Time, string) {
	if t.b.cfg.Texter == nil {
		return time.Time{}, ""
	}

	if to.IsZero() {
		return time.Time{}, "no mobile number to text"
	}

	ctx, cancel := context.WithTimeout(t.ctx, sendBudget)
	defer cancel()

	if _, err := t.b.cfg.Texter.Send(ctx, to.String(), body); err != nil {
		t.b.cfg.Log.Warn("a carpool text did not go",
			"form", t.f.ID.String(), "to", to.Redacted(), "error", err)

		return time.Time{}, problemOf(err)
	}

	return t.b.cfg.Now(), ""
}

// record keeps what happened to a rider's and driver's texts on the ride.
func (t *texts) record(rider person, riderAt, driverAt time.Time, problems ...string) {
	if t.b.cfg.Texter == nil {
		return
	}

	problem := ""
	for _, p := range problems {
		if p != "" {
			problem = p

			break
		}
	}

	if err := t.b.cfg.Store.Texted(t.ctx, rider.sub.ID, riderAt, driverAt, problem); err != nil {
		t.b.cfg.Log.Error("what was texted about a ride could not be recorded",
			"form", t.f.ID.String(), "error", err)
	}
}

// placed is a rider put in a car. The rider is texted at once; the driver
// hears in their next summary.
func (t *texts) placed(rider, driver person) {
	t.riderText(rider, driver)
	t.changed(driver.sub.ID)
}

// riderText tells a rider whose car they are in, and records how it went.
func (t *texts) riderText(rider, driver person) {
	at, problem := t.send(rider.Phone, fmt.Sprintf(
		"%s: you're riding with %s, %s.%s Reply STOP to stop these texts.",
		t.f.Title, named(driver), driver.Phone.National(), t.link(" See or change your ride: ", rider)))

	t.record(rider, at, time.Time{}, problem)
}

// changed marks a driver's car as having changed, for their next summary.
// The zero ID is nobody's car, and is ignored.
func (t *texts) changed(driver types.ID) {
	if driver.Zero() {
		return
	}

	if t.cars == nil {
		t.cars = map[types.ID]bool{}
	}

	t.cars[driver] = true
}

// finish records the cars this pass changed, and tells the office about
// anybody it left with no car.
func (t *texts) finish(now time.Time) {
	for driver := range t.cars {
		if err := t.b.cfg.Store.Changed(t.ctx, driver, now); err != nil {
			t.b.cfg.Log.Error("that a car changed could not be recorded",
				"form", t.f.ID.String(), "error", err)
		}
	}

	t.cars = nil

	t.tellOffice()
	t.stuck = nil
}

// left is a rider who no longer needs the seat they had: their driver hears
// in their next summary.
func (t *texts) left(driver person) {
	t.changed(driver.sub.ID)
}

// dropped is a rider whose car has gone, or has fewer seats than before.
func (t *texts) dropped(rider, driver person) {
	who := "Your driver"
	if driver.Name != "" {
		who = driver.Name
	}

	t.send(rider.Phone, fmt.Sprintf(
		"%s: %s can no longer take you. We'll text you when you have another car.%s",
		t.f.Title, who, t.link(" Your ride: ", rider)))

	t.changed(driver.sub.ID)
}

// waitingText tells a rider every car is full, the first time they are.
func (t *texts) waitingText(rider person) {
	t.send(rider.Phone, fmt.Sprintf(
		"%s: every car is full right now. We'll text you when there's a seat for you.%s Reply STOP to stop these texts.",
		t.f.Title, t.link(" Your ride: ", rider)))
}

// summary tells a driver who is in their car now: the first time, as a
// welcome. It records that they were told, or why that did not go.
func (t *texts) summary(driver person, riders []person, welcome bool, now time.Time) {
	var who strings.Builder

	for i, r := range riders {
		if i > 0 {
			who.WriteString("; ")
		}

		fmt.Fprintf(&who, "%s, %s", named(r), r.Phone.National())
	}

	free := seatsLeft(driver.Seats - len(riders))

	var body string

	switch {
	case welcome && len(riders) == 0:
		body = fmt.Sprintf("%s: thanks for driving! We'll text you when riders join.%s Reply STOP to stop these texts.",
			t.f.Title, t.link(" Your car: ", driver))
	case welcome:
		body = fmt.Sprintf("%s: thanks for driving! Riding with you: %s. %s.%s Reply STOP to stop these texts.",
			t.f.Title, who.String(), free, t.link(" Your car: ", driver))
	case len(riders) == 0:
		body = fmt.Sprintf("%s: nobody is riding with you right now.%s", t.f.Title, t.link(" Your car: ", driver))
	default:
		body = fmt.Sprintf("%s: your car now: %s. %s.%s", t.f.Title, who.String(), free, t.link(" Your car: ", driver))
	}

	_, problem := t.send(driver.Phone, body)

	if err := t.b.cfg.Store.Summarised(t.ctx, t.f.ID, driver.sub.ID, now, problem); err != nil {
		t.b.cfg.Log.Error("a driver's text could not be recorded", "form", t.f.ID.String(), "error", err)
	}
}

// link is a person's ride link with words before it, or nothing when there
// are no links.
func (t *texts) link(lead string, p person) string {
	link := t.b.fullLink(p.sub.ID)
	if link == "" {
		return ""
	}

	return lead + link
}

// waiting is a rider for whom every car is full: new to the carpool, or just
// taken out of a car. Kept for [texts.tellOffice].
func (t *texts) waiting(rider person) {
	t.stuck = append(t.stuck, rider)
}

// tellOffice emails the organisers once about everybody this pass left with
// no car. One message per pass rather than per rider, because a driver who
// drops out strands their whole car at once, and three emails saying the same
// thing about three people is how the fourth stops being read.
func (t *texts) tellOffice() {
	if len(t.stuck) == 0 || t.b.cfg.Office == nil {
		return
	}

	var body strings.Builder

	who := "these people are"
	if len(t.stuck) == 1 {
		who = "this person is"
	}

	fmt.Fprintf(&body, "Every car in the carpool for %s is full, so %s waiting for a ride:\n\n", t.f.Title, who)

	for _, p := range t.stuck {
		fmt.Fprintf(&body, "  %s, %s\n", named(p), p.Phone.National())
	}

	body.WriteString("\nEach of them is put in a car by themselves as soon as a seat comes free, " +
		"and texted. To find them one sooner, ask somebody else to drive, or move them " +
		"into a car on the carpool page.")

	subject := named(t.stuck[0]) + " needs a ride"
	if len(t.stuck) > 1 {
		subject = fmt.Sprintf("%d people need a ride", len(t.stuck))
	}

	t.b.cfg.Office.Carpool(t.ctx, t.f, subject, body.String())
}

// named is a person's name for a text, or a word when they gave none.
func named(p person) string {
	if p.Name != "" {
		return p.Name
	}

	if p.Driving() {
		return "a driver"
	}

	return "a rider"
}

func seatsLeft(n int) string {
	switch {
	case n <= 0:
		return "The car is full"
	case n == 1:
		return "1 seat left"
	default:
		return fmt.Sprintf("%d seats left", n)
	}
}

// optedOut is what Twilio's refusal says when somebody has replied STOP,
// read through an interface so this package does not import foundation's.
type optedOut interface {
	error
	IsOptedOut() bool
}

// problemOf is a failed text in words for the office page.
func problemOf(err error) string {
	if o, ok := errors.AsType[optedOut](err); ok && o.IsOptedOut() {
		return "they replied STOP to our texts"
	}

	return "the text did not go: " + err.Error()
}

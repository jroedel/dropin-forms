package carpoolbus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types/phone"
)

// sendBudget bounds one text. Twilio's own client gives up at ten seconds;
// this is the same number, so that a pass with a dozen placements cannot be
// held for longer than a dozen slow texts.
const sendBudget = 10 * time.Second

// texts is the messages one pass sends, for one form.
//
// Each is short and says no more than its reader needs: whose car, and a
// number to call. Twilio and the carrier keep every body, so the rest of a
// submission stays out of them.
type texts struct {
	b   *Business
	ctx context.Context
	f   formbus.Form

	// stuck is the riders this pass left with no car, for the one email to
	// the organisers at its end.
	stuck []person
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

// placed is a rider put in a car: the two of them.
func (t *texts) placed(rider, driver person, free int) {
	t.pair(rider, driver, free)
}

// pair tells a rider and a driver about each other.
func (t *texts) pair(rider, driver person, free int) {
	driverAt, driverProblem := t.send(driver.Phone, fmt.Sprintf(
		"%s: %s, %s, is riding with you. %s. Reply STOP to stop these texts.",
		t.f.Title, named(rider), rider.Phone.National(), seatsLeft(free)))

	riderAt, riderProblem := t.send(rider.Phone, fmt.Sprintf(
		"%s: you are riding with %s, %s. Reply STOP to stop these texts.",
		t.f.Title, named(driver), driver.Phone.National()))

	t.record(rider, riderAt, driverAt, riderProblem, driverProblem)
}

// left is a rider who no longer needs the seat they had.
func (t *texts) left(driver person, rider string) {
	t.send(driver.Phone, fmt.Sprintf("%s: %s is no longer riding with you.", t.f.Title, rider))
}

// dropped is a rider whose car has gone, or has fewer seats than before.
func (t *texts) dropped(rider, driver person) {
	who := "Your driver"
	if driver.Name != "" {
		who = driver.Name
	}

	t.send(rider.Phone, fmt.Sprintf(
		"%s: %s can no longer take you. We will text you when you have another car.", t.f.Title, who))
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

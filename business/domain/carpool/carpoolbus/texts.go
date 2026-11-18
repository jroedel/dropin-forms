package carpoolbus

import (
	"context"
	"errors"
	"fmt"
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

	// admins is looked up on the first text that needs it, so a pass that
	// changes nothing reads no grants.
	admins []phone.Number
	looked bool
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

// organisers texts each organiser who asked to be.
func (t *texts) organisers(body string) {
	if t.b.cfg.Texter == nil {
		return
	}

	if !t.looked {
		t.admins, t.looked = t.b.organisers(t.ctx, t.f.ID), true
	}

	for _, n := range t.admins {
		t.send(n, body)
	}
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

// placed is a rider put in a car: the two of them, and the organisers.
func (t *texts) placed(rider, driver person, free int) {
	t.pair(rider, driver, free)

	t.organisers(fmt.Sprintf("%s carpool: %s is riding with %s. %s.",
		t.f.Title, named(rider), named(driver), seatsLeft(free)))
}

// pair tells a rider and a driver about each other.
func (t *texts) pair(rider, driver person, free int) {
	driverAt, driverProblem := t.send(driver.Phone, fmt.Sprintf(
		"%s: %s (%s) is riding with you. %s. Reply STOP to stop these texts.",
		t.f.Title, named(rider), rider.Phone.National(), seatsLeft(free)))

	riderAt, riderProblem := t.send(rider.Phone, fmt.Sprintf(
		"%s: you are riding with %s (%s). Reply STOP to stop these texts.",
		t.f.Title, named(driver), driver.Phone.National()))

	t.record(rider, riderAt, driverAt, riderProblem, driverProblem)
}

// left is a rider who no longer needs the seat they had.
func (t *texts) left(driver person, rider string) {
	t.send(driver.Phone, fmt.Sprintf("%s: %s is no longer riding with you.", t.f.Title, rider))
	t.organisers(fmt.Sprintf("%s carpool: %s is no longer riding with %s.", t.f.Title, rider, named(driver)))
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

// waiting is a rider for whom every car is full, said to the organisers once.
func (t *texts) waiting(rider person) {
	t.organisers(fmt.Sprintf("%s carpool: %s needs a ride and every car is full.", t.f.Title, named(rider)))
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

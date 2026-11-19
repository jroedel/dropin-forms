package carpoolbus

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/business/types/phone"
)

// A person's own ride page: what they see, and the few things they can do
// there without writing to the office -- choose a car with room, stop needing
// a ride, offer one, change their seats, or say they are not coming.
//
// # Why riders choose
//
// Placement is automatic, and stays so for whoever does nothing. But the
// people this is for keep their options open, and a rider who wants a
// particular car will otherwise get it the only way they can: unregister and
// sign up again until the timer happens to put them there, with a round of
// texts to somebody every time. Letting them pick from the cars with a seat
// free costs nothing, and texts only the drivers it concerns.

// ErrBadChoice is a choice the page does not offer, or seats out of range.
var ErrBadChoice = errors.New("that is not a choice on this page")

// Mine is one person's ride page.
type Mine struct {
	Form formbus.Form
	ID   types.ID

	// Me is their part as it stands, Choice included: one of the form's
	// three, [Out], or empty for somebody who did not answer the question.
	Me formbus.Carpooler

	// Car is the car they are in as a rider, or the car they drive, and nil
	// when they are in neither. Waiting is a rider with no car yet.
	Car     *Car
	Waiting bool

	// Room is the other cars with a seat free, for a rider to choose from.
	Room []Car

	// MaxSeats is the most a driver may offer.
	MaxSeats int
}

// Link is the address of a person's ride page relative to the public
// surface, or empty when there are no ride links.
func (b *Business) Link(id types.ID) string {
	if b.cfg.RideKey.Zero() {
		return ""
	}

	token, err := MintRideLink(b.cfg.RideKey, id)
	if err != nil {
		b.cfg.Log.Error("a ride link could not be made", "submission_id", id.String(), "error", err)

		return ""
	}

	return "/r/" + token
}

// TextsLinks reports whether ride links can be texted: there is somebody to
// send texts, a key to sign the links, and an address to put in front of them.
func (b *Business) TextsLinks() bool {
	return b.cfg.Texter != nil && !b.cfg.RideKey.Zero() && b.cfg.BaseURL != ""
}

// fullLink is Link with the surface's address, for a text, or empty.
func (b *Business) fullLink(id types.ID) string {
	link := b.Link(id)
	if link == "" || b.cfg.BaseURL == "" {
		return ""
	}

	return strings.TrimRight(b.cfg.BaseURL, "/") + link
}

// Resolve reads a ride link: the form and the submission it is for. A link
// that is not ours is [ErrBadRideLink]; one whose submission no longer counts
// -- hidden, declined, not paid for, or on a form with no carpool -- is
// [ErrNotFound].
func (b *Business) Resolve(ctx context.Context, token string) (formbus.Form, types.ID, error) {
	id, err := ReadRideLink(b.cfg.RideKey, token)
	if err != nil {
		return formbus.Form{}, types.ID{}, err
	}

	sub, err := b.cfg.Submissions.ByID(ctx, id)

	switch {
	case errors.Is(err, submissionbus.ErrNotFound):
		return formbus.Form{}, types.ID{}, ErrNotFound
	case err != nil:
		return formbus.Form{}, types.ID{}, fmt.Errorf("reading the submission: %w", err)
	}

	f, err := b.cfg.Forms.ByID(sub.Form)
	if err != nil {
		return formbus.Form{}, types.ID{}, ErrNotFound
	}

	if _, ok := f.CarpoolOf(); !ok {
		return formbus.Form{}, types.ID{}, ErrNotFound
	}

	return f, id, nil
}

// Mine reads one person's ride page.
func (b *Business) Mine(ctx context.Context, f formbus.Form, id types.ID) (Mine, error) {
	fld, ok := f.CarpoolOf()
	if !ok {
		return Mine{}, ErrNotFound
	}

	everyone, _, _, err := b.people(ctx, f)
	if err != nil {
		return Mine{}, err
	}

	me, ok := everyone[id]
	if !ok {
		return Mine{}, ErrNotFound
	}

	plan, err := b.PlanOf(ctx, f)
	if err != nil {
		return Mine{}, err
	}

	m := Mine{Form: f, ID: id, Me: me.Carpooler, MaxSeats: fld.MaxSeats()}

	for i, c := range plan.Cars {
		switch {
		case c.ID == id:
			m.Car = &plan.Cars[i]
		case slices.ContainsFunc(c.Riders, func(s Seat) bool { return s.ID == id }):
			m.Car = &plan.Cars[i]
		}
	}

	for _, c := range plan.Cars {
		if c.ID != id && c.Free() > 0 && (m.Car == nil || c.ID != m.Car.ID) {
			m.Room = append(m.Room, c)
		}
	}

	m.Waiting = me.Riding() && m.Car == nil

	return m, nil
}

// Say records what somebody said on their ride page -- a ride of their own,
// offering one with seats, needing one, or [Out] -- and arranges the carpool
// at once, so the page they come back to shows the result.
//
// Saying they are not coming tells the office. Their sign-up is not touched:
// whether to hide it, or refund it, is the office's decision, and on a paid
// form a tap on a phone must not be able to make it.
func (b *Business) Say(ctx context.Context, f formbus.Form, id types.ID, choice string, seats int) error {
	fld, ok := f.CarpoolOf()
	if !ok {
		return ErrNotFound
	}

	switch choice {
	case formbus.CarpoolHave, formbus.CarpoolNeed, Out:
		seats = 0
	case formbus.CarpoolDrive:
		if seats < fld.MinSeats() || seats > fld.MaxSeats() {
			return ErrBadChoice
		}
	default:
		return ErrBadChoice
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	everyone, _, _, err := b.people(ctx, f)
	if err != nil {
		return err
	}

	before, ok := everyone[id]
	if !ok {
		return ErrNotFound
	}

	if before.Choice == choice && before.Seats == seats {
		return nil
	}

	var car string

	if choice == Out {
		// What they were doing, for the office: read before the car is gone.
		car, err = b.whatTheyWere(ctx, f, everyone, before)
		if err != nil {
			return err
		}
	}

	now := b.cfg.Now()

	// A choice made in the same millisecond as the sign-up, which only a test
	// manages, must still count over the answers.
	if !now.After(before.sub.UpdatedAt) {
		now = before.sub.UpdatedAt.Add(time.Millisecond)
	}

	if err := b.cfg.Store.SetChoice(ctx, Choice{Sub: id, Form: f.ID, Choice: choice, Seats: seats, At: now}); err != nil {
		return err
	}

	if choice == Out && b.cfg.Office != nil {
		b.cfg.Office.Carpool(ctx, f, named(before)+" is not coming", fmt.Sprintf(
			"%s, %s, said on their ride page that they are not coming. %s\n\n"+
				"They are out of the carpool. Their sign-up is still in the results: "+
				"hide it, or refund it, there if that is what is wanted.",
			named(before), before.Phone.National(), car))
	}

	return b.arrange(ctx, f)
}

// whatTheyWere is a sentence for the office about somebody's part in the
// carpool before they left it.
func (b *Business) whatTheyWere(ctx context.Context, f formbus.Form, everyone map[types.ID]person, p person) (string, error) {
	rides, err := b.cfg.Store.ByForm(ctx, f.ID)
	if err != nil {
		return "", fmt.Errorf("reading the rides: %w", err)
	}

	switch {
	case p.Driving():
		var riders []string

		for _, r := range rides {
			if r.Driver == p.sub.ID {
				riders = append(riders, nameOf(r, everyone[r.Rider], true))
			}
		}

		if len(riders) == 0 {
			return "They were driving, with nobody in their car yet.", nil
		}

		return "They were driving " + strings.Join(riders, ", ") +
			", who go back to waiting and are put in other cars where there is room.", nil

	case p.Riding():
		for _, r := range rides {
			if r.Rider == p.sub.ID && !r.Waiting() {
				if d, ok := everyone[r.Driver]; ok {
					return "They were riding with " + named(d) + ", who is told.", nil
				}
			}
		}

		return "They were waiting for a car.", nil

	case p.Choice == formbus.CarpoolHave:
		return "They were getting there their own way.", nil
	}

	return "", nil
}

// Choose puts a rider in the car they picked on their page. A full car is
// [ErrCarFull]; a rider who does not need a ride, or a driver who is not
// driving, is [ErrNotFound].
//
// The rider is not texted -- they are looking at the page that did it. The
// driver they joined is, and the one they left, and the seat they left is
// offered to whoever is waiting.
func (b *Business) Choose(ctx context.Context, f formbus.Form, rider, driver types.ID) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	everyone, _, _, err := b.people(ctx, f)
	if err != nil {
		return err
	}

	r, ok := everyone[rider]
	if !ok || !r.Riding() {
		return ErrNotFound
	}

	d, ok := everyone[driver]
	if !ok || !d.Driving() || d.Seats < 1 {
		return ErrNotFound
	}

	rides, err := b.cfg.Store.ByForm(ctx, f.ID)
	if err != nil {
		return fmt.Errorf("reading the rides: %w", err)
	}

	var was types.ID

	for _, x := range rides {
		if x.Rider == rider {
			was = x.Driver
		}
	}

	if was == driver {
		return nil
	}

	if err := b.cfg.Store.Place(ctx, f.ID, rider, driver, r.Name, d.Seats, b.cfg.Now(), rider); err != nil {
		return err
	}

	in := 1
	for _, x := range rides {
		if x.Driver == driver && x.Rider != rider {
			in++
		}
	}

	t := b.texts(ctx, f)
	t.joined(r, d, d.Seats-in)

	if old, ok := everyone[was]; ok && !was.Zero() {
		t.left(old, named(r))
	}

	return b.arrange(ctx, f)
}

// linkAgainEvery is how often one number may be sent its links again. The
// page that sends them is open to anybody, and without this it would be a way
// to send texts at somebody.
const linkAgainEvery = 10 * time.Minute

// SendLinks texts the ride link to everybody on the form who gave this number,
// for somebody who has lost theirs. It says nothing about whether anybody
// did: the page that asks must answer the same either way.
func (b *Business) SendLinks(ctx context.Context, f formbus.Form, number phone.Number) {
	if !b.TextsLinks() || number.IsZero() {
		return
	}

	key := f.ID.String() + "|" + number.String()
	now := b.cfg.Now()

	b.sentMu.Lock()
	if last, ok := b.sent[key]; ok && now.Sub(last) < linkAgainEvery {
		b.sentMu.Unlock()

		return
	}

	b.sent[key] = now

	// Forget what can no longer refuse anything, so the map does not grow
	// for as long as the process runs.
	for k, at := range b.sent {
		if now.Sub(at) >= linkAgainEvery {
			delete(b.sent, k)
		}
	}
	b.sentMu.Unlock()

	everyone, _, _, err := b.people(ctx, f)
	if err != nil {
		b.cfg.Log.Error("ride links could not be sent again", "form", f.ID.String(), "error", err)

		return
	}

	t := b.texts(ctx, f)

	for id, p := range everyone {
		if p.Phone != number {
			continue
		}

		t.send(p.Phone, fmt.Sprintf("%s: your ride page, to see or change your ride or tell us you can't come: %s",
			f.Title, b.fullLink(id)))
	}
}

// Package carpoolbus puts the people who need a ride into the cars of the
// people who can drive, on a form with a carpool, and texts them about it.
//
// # Worked out from what is stored, not told about each change
//
// Whether somebody is in a car depends on a great deal: their submission was
// made, or paid for, or approved, or offered a place from the waiting list;
// it was changed, hidden, refunded or declined; the driver changed their
// seats, or stopped driving. Hooking every one of those paths is how one gets
// missed. So this package has one operation, [Business.Arrange], which reads
// the form's submissions as they stand and its rides as they stand and makes
// the second agree with the first: rides whose rider or driver no longer
// qualifies are undone, cars over their seats give up their latest riders, and
// everybody still waiting is offered the earliest car with a seat free, in the
// order they signed up.
//
// It runs on a timer for every form with a carpool, and at once when an
// organiser moves somebody. Running it twice changes nothing the first run did
// not, which is what makes a timer safe.
//
// # Who counts
//
// A submission that is settled -- received, or paid -- and not declined, not
// awaiting approval and not hidden; what [submissionbus.Business.ByForm]
// returns, less the ones still awaiting a decision. Somebody whose payment is
// pending is not given a seat yet: an abandoned checkout would otherwise hold
// one, and a driver would be texted about a rider who never comes.
//
// # The texts
//
// Each placement texts the driver, the rider and the form's organisers who
// have given a mobile number on their account. Best effort, as email is in
// notifybus: a text that does not go is recorded on the ride, shown on the
// office page, and not retried by itself -- a retried text that did in fact go
// is a second copy on somebody's phone. The office page offers to send it
// again.
package carpoolbus

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/business/types/phone"
)

// The errors this package returns.
var (
	// ErrCarFull is a car with no seat for one more rider.
	ErrCarFull = errors.New("that car has no seat free")

	// ErrNotFound is a rider or driver that is not in this form's carpool.
	ErrNotFound = errors.New("not in this carpool")
)

// Ride is one person who needs a ride, and the car they are in.
//
// A rider waiting for a car has a row too, with the zero Driver. That is what
// lets the organisers be told once that somebody is waiting, rather than on
// every pass that finds them still waiting.
type Ride struct {
	Form   types.Slug
	Rider  types.ID
	Driver types.ID // zero while waiting

	// Name is the rider's name when they were last placed or began waiting,
	// kept so that a driver can be told who has left after the submission
	// that said it has stopped counting.
	Name string

	// Since is when the rider was first seen needing a ride.
	Since time.Time

	// PlacedAt and PlacedBy are when they were put in this car and by whom:
	// the zero ID for this package, an account for an organiser.
	PlacedAt time.Time
	PlacedBy types.ID

	// RiderTexted and DriverTexted are when each was told about this
	// placement, zero until they have been. Problem is why a text did not
	// go, in words, empty when nothing went wrong.
	RiderTexted  time.Time
	DriverTexted time.Time
	Problem      string
}

// Waiting reports whether the rider has no car yet.
func (r Ride) Waiting() bool { return r.Driver.Zero() }

// Storer is what this package needs from storage.
type Storer interface {
	// ByForm lists a form's rides, waiting ones included, oldest first.
	ByForm(ctx context.Context, form types.Slug) ([]Ride, error)

	// Wait records a rider as waiting, keeping when they started if they
	// already were, and reports whether the row is new.
	Wait(ctx context.Context, form types.Slug, rider types.ID, name string, now time.Time) (bool, error)

	// Place puts a rider in a car, in one transaction with the count of the
	// riders already in it: [ErrCarFull] when seats are taken. The texts
	// recorded against the rider's previous car are cleared.
	Place(ctx context.Context, form types.Slug, rider, driver types.ID, name string, seats int, now time.Time, by types.ID) error

	// Release takes a rider out of their car and back to waiting.
	Release(ctx context.Context, rider types.ID) error

	// Remove forgets a rider entirely: they no longer need a ride.
	Remove(ctx context.Context, rider types.ID) error

	// Texted records who was told and what went wrong, if anything.
	Texted(ctx context.Context, rider types.ID, riderAt, driverAt time.Time, problem string) error
}

// Submissions is what this package reads about the people in a carpool.
type Submissions interface {
	ByForm(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error)
}

// Forms is every form being served, for the timer to find the carpools in.
type Forms interface {
	All() []formbus.Form
}

// Texter sends one text, and is foundation/twilio's Sender. Nil means texting
// is off.
type Texter interface {
	Send(ctx context.Context, to, body string) (string, error)
}

// Grants, Accounts and Mutes find a form's organisers' numbers: who
// administers it, which of them gave a mobile number, and who turned its
// messages off. The same three notifybus reads for email, for its reasons.
type Grants interface {
	ForForm(ctx context.Context, form types.Slug) ([]accessbus.Grant, error)
}

// Accounts turns a grant's account into a number.
type Accounts interface {
	ByID(ctx context.Context, id types.ID) (userbus.User, error)
}

// Mutes is who has turned a form's messages off.
type Mutes interface {
	MutedForForm(ctx context.Context, form types.Slug) ([]types.ID, error)
}

// Config is what this package needs. Texter, Grants, Accounts and Mutes are
// optional; without the first nothing is texted, and without the second or
// third no organiser is.
type Config struct {
	Log         *slog.Logger
	Store       Storer
	Submissions Submissions
	Forms       Forms

	Texter   Texter
	Grants   Grants
	Accounts Accounts
	Mutes    Mutes

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Business arranges the carpools.
type Business struct {
	cfg Config

	// mu makes Arrange one at a time. The timer and an organiser's move can
	// otherwise interleave: both read the same waiting rider and both place
	// them, and the store's seat check stops the second only if the cars
	// differ.
	mu sync.Mutex
}

// NewBusiness constructs one.
func NewBusiness(cfg Config) (*Business, error) {
	switch {
	case cfg.Log == nil:
		return nil, errors.New("the carpool needs a logger")
	case cfg.Store == nil || cfg.Submissions == nil || cfg.Forms == nil:
		return nil, errors.New("the carpool needs its store, the submissions and the forms")
	}

	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	return &Business{cfg: cfg}, nil
}

// Texting reports whether texts are sent at all.
func (b *Business) Texting() bool { return b.cfg.Texter != nil }

// person is one submission's part in the carpool.
type person struct {
	sub submissionbus.Submission
	formbus.Carpooler
}

// people reads the form's carpool from its submissions: everybody who counts,
// by submission, and the drivers and riders among them in sign-up order.
func (b *Business) people(ctx context.Context, f formbus.Form) (map[types.ID]person, []person, []person, error) {
	subs, err := b.cfg.Submissions.ByForm(ctx, f.ID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reading the submissions: %w", err)
	}

	everyone := make(map[types.ID]person, len(subs))
	var drivers, riders []person

	for _, s := range subs {
		if s.Review == submissionbus.ReviewAwaiting {
			continue
		}

		c, ok := f.Carpooler(s.Answers)
		if !ok {
			continue
		}

		p := person{sub: s, Carpooler: c}
		everyone[s.ID] = p

		switch {
		// A driver nobody can text is still a driver -- the organisers can
		// see the car and ring them -- but a car with no seats is not one.
		case p.Driving() && p.Seats > 0:
			drivers = append(drivers, p)
		case p.Riding():
			riders = append(riders, p)
		}
	}

	bySignUp := func(x, y person) int {
		return cmp.Or(x.sub.CreatedAt.Compare(y.sub.CreatedAt), cmp.Compare(x.sub.ID.String(), y.sub.ID.String()))
	}

	slices.SortFunc(drivers, bySignUp)
	slices.SortFunc(riders, bySignUp)

	return everyone, drivers, riders, nil
}

// Car is one driver and who is riding with them, for the office page.
type Car struct {
	Driver formbus.Carpooler
	ID     types.ID
	Riders []Seat
}

// Free is how many seats are left.
func (c Car) Free() int { return max(c.Driver.Seats-len(c.Riders), 0) }

// Seat is one rider in a car, or waiting for one.
type Seat struct {
	Rider formbus.Carpooler
	ID    types.ID
	Ride  Ride
}

// Plan is a form's carpool as it stands.
type Plan struct {
	Cars    []Car
	Waiting []Seat

	// OwnWay is how many have their own ride.
	OwnWay int
}

// PlanOf reads a form's carpool as it stands, for the office page. It
// arranges nothing.
func (b *Business) PlanOf(ctx context.Context, f formbus.Form) (Plan, error) {
	everyone, drivers, _, err := b.people(ctx, f)
	if err != nil {
		return Plan{}, err
	}

	rides, err := b.cfg.Store.ByForm(ctx, f.ID)
	if err != nil {
		return Plan{}, fmt.Errorf("reading the rides: %w", err)
	}

	var plan Plan

	at := make(map[types.ID]int, len(drivers))
	for i, d := range drivers {
		at[d.sub.ID] = i
		plan.Cars = append(plan.Cars, Car{Driver: d.Carpooler, ID: d.sub.ID})
	}

	for _, r := range rides {
		p, ok := everyone[r.Rider]
		if !ok || !p.Riding() {
			continue
		}

		seat := Seat{Rider: p.Carpooler, ID: r.Rider, Ride: r}

		if i, ok := at[r.Driver]; ok && !r.Waiting() {
			plan.Cars[i].Riders = append(plan.Cars[i].Riders, seat)
		} else {
			plan.Waiting = append(plan.Waiting, seat)
		}
	}

	for _, p := range everyone {
		if p.Choice == formbus.CarpoolHave {
			plan.OwnWay++
		}
	}

	return plan, nil
}

// Arrange makes a form's rides agree with its submissions, and texts about
// whatever changed. See the package comment.
func (b *Business) Arrange(ctx context.Context, f formbus.Form) error {
	if _, ok := f.CarpoolOf(); !ok {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	return b.arrange(ctx, f)
}

func (b *Business) arrange(ctx context.Context, f formbus.Form) error {
	everyone, drivers, riders, err := b.people(ctx, f)
	if err != nil {
		return err
	}

	rides, err := b.cfg.Store.ByForm(ctx, f.ID)
	if err != nil {
		return fmt.Errorf("reading the rides: %w", err)
	}

	now := b.cfg.Now()
	t := b.texts(ctx, f)

	seats := make(map[types.ID]int, len(drivers))
	for _, d := range drivers {
		seats[d.sub.ID] = d.Seats
	}

	ride := make(map[types.ID]Ride, len(rides))
	inCar := make(map[types.ID][]Ride)

	// Undo what no longer holds. A rider who has gone, or no longer needs a
	// ride, is forgotten and their driver told; a rider whose car has gone
	// back to waiting, and is told.
	for _, r := range rides {
		rider, here := everyone[r.Rider]

		switch {
		case !here || !rider.Riding():
			if err := b.cfg.Store.Remove(ctx, r.Rider); err != nil {
				return fmt.Errorf("forgetting a rider: %w", err)
			}

			if driver, ok := everyone[r.Driver]; ok && !r.Waiting() {
				t.left(driver, nameOf(r, rider, here))
			}

			continue

		case !r.Waiting():
			if _, driving := seats[r.Driver]; !driving {
				if err := b.cfg.Store.Release(ctx, r.Rider); err != nil {
					return fmt.Errorf("taking a rider out of a car that has gone: %w", err)
				}

				t.dropped(rider, everyone[r.Driver])

				r.Driver = types.ID{}
			}
		}

		ride[r.Rider] = r

		if !r.Waiting() {
			inCar[r.Driver] = append(inCar[r.Driver], r)
		}
	}

	// A car with fewer seats than riders gives up its latest.
	for driver, in := range inCar {
		if len(in) <= seats[driver] {
			continue
		}

		// Latest placed last, and among those placed together the latest
		// to sign up.
		slices.SortFunc(in, func(x, y Ride) int {
			return cmp.Or(x.PlacedAt.Compare(y.PlacedAt),
				everyone[x.Rider].sub.CreatedAt.Compare(everyone[y.Rider].sub.CreatedAt))
		})

		for _, r := range in[seats[driver]:] {
			if err := b.cfg.Store.Release(ctx, r.Rider); err != nil {
				return fmt.Errorf("taking a rider out of a smaller car: %w", err)
			}

			t.dropped(everyone[r.Rider], everyone[driver])

			r.Driver = types.ID{}
			ride[r.Rider] = r
		}

		inCar[driver] = in[:seats[driver]]
	}

	// Everybody waiting, in the order they signed up, into the earliest car
	// with a seat free.
	for _, rider := range riders {
		r, known := ride[rider.sub.ID]
		if known && !r.Waiting() {
			continue
		}

		placed := false

		for _, d := range drivers {
			if len(inCar[d.sub.ID]) >= d.Seats {
				continue
			}

			err := b.cfg.Store.Place(ctx, f.ID, rider.sub.ID, d.sub.ID, rider.Name, d.Seats, now, types.ID{})

			switch {
			case errors.Is(err, ErrCarFull):
				continue
			case err != nil:
				return fmt.Errorf("placing a rider: %w", err)
			}

			inCar[d.sub.ID] = append(inCar[d.sub.ID], Ride{Rider: rider.sub.ID, Driver: d.sub.ID})
			t.placed(rider, d, d.Seats-len(inCar[d.sub.ID]))
			placed = true

			break
		}

		if placed {
			continue
		}

		fresh, err := b.cfg.Store.Wait(ctx, f.ID, rider.sub.ID, rider.Name, now)
		if err != nil {
			return fmt.Errorf("recording a rider as waiting: %w", err)
		}

		if fresh {
			t.waiting(rider)
		}
	}

	return nil
}

// Move puts a rider in a particular car, for an organiser. It refuses a full
// car with [ErrCarFull], and a rider or driver who is not in this carpool with
// [ErrNotFound].
func (b *Business) Move(ctx context.Context, f formbus.Form, rider, driver, by types.ID) error {
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

	// Already in that car: nothing to do, and nobody to text twice.
	if slices.ContainsFunc(rides, func(x Ride) bool { return x.Rider == rider && x.Driver == driver }) {
		return nil
	}

	if err := b.cfg.Store.Place(ctx, f.ID, rider, driver, r.Name, d.Seats, b.cfg.Now(), by); err != nil {
		return err
	}

	rides, err = b.cfg.Store.ByForm(ctx, f.ID)
	if err != nil {
		return fmt.Errorf("reading the rides: %w", err)
	}

	in := 0
	for _, x := range rides {
		if x.Driver == driver {
			in++
		}
	}

	t := b.texts(ctx, f)
	t.placed(r, d, d.Seats-in)

	return nil
}

// Retext sends a rider's and their driver's texts about the car they are in
// again, for an organiser after one did not go.
func (b *Business) Retext(ctx context.Context, f formbus.Form, rider types.ID) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	everyone, _, _, err := b.people(ctx, f)
	if err != nil {
		return err
	}

	rides, err := b.cfg.Store.ByForm(ctx, f.ID)
	if err != nil {
		return fmt.Errorf("reading the rides: %w", err)
	}

	i := slices.IndexFunc(rides, func(r Ride) bool { return r.Rider == rider })
	if i < 0 || rides[i].Waiting() {
		return ErrNotFound
	}

	r, rok := everyone[rider]
	d, dok := everyone[rides[i].Driver]

	if !rok || !dok {
		return ErrNotFound
	}

	in := 0
	for _, x := range rides {
		if x.Driver == rides[i].Driver {
			in++
		}
	}

	b.texts(ctx, f).pair(r, d, d.Seats-in)

	return nil
}

// nameOf is a rider's name for a text after they have gone: from their
// answers if they are still here, from the ride if not, and "A rider" when
// neither says.
func nameOf(r Ride, p person, here bool) string {
	switch {
	case here && p.Name != "":
		return p.Name
	case r.Name != "":
		return r.Name
	}

	return "A rider"
}

// organisers is the numbers of the people who administer this form and asked
// to be texted: a mobile number on their account, enabled, not muted.
func (b *Business) organisers(ctx context.Context, form types.Slug) []phone.Number {
	if b.cfg.Grants == nil || b.cfg.Accounts == nil {
		return nil
	}

	grants, err := b.cfg.Grants.ForForm(ctx, form)
	if err != nil {
		b.cfg.Log.Error("the organisers of a carpool could not be read, so they were not texted",
			"form", form.String(), "error", err)

		return nil
	}

	quiet := map[types.ID]bool{}

	if b.cfg.Mutes != nil {
		muted, err := b.cfg.Mutes.MutedForForm(ctx, form)
		if err != nil {
			// Not texting somebody who asked for silence matters more than
			// texting everybody else, so a failure here texts nobody.
			b.cfg.Log.Error("who has turned a form's messages off could not be read, so no organiser was texted",
				"form", form.String(), "error", err)

			return nil
		}

		for _, id := range muted {
			quiet[id] = true
		}
	}

	var out []phone.Number

	for _, g := range grants {
		if !g.Role.Includes(accessbus.RoleAdmin) || quiet[g.UserID] {
			continue
		}

		u, err := b.cfg.Accounts.ByID(ctx, g.UserID)
		if err != nil || !u.Enabled || u.Mobile.IsZero() || slices.Contains(out, u.Mobile) {
			continue
		}

		out = append(out, u.Mobile)
	}

	return out
}

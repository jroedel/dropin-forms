package embedapp

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jroedel/dropin-forms/business/domain/carpool/carpoolbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/business/types/phone"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// The ride page: one driver's or rider's own page on a form with a carpool,
// at /r/{token}, reached from the link in their texts or on the page that
// thanked them. carpoolbus says what is on it and why riders may choose.
//
// # No grant, and why that is safe here
//
// Every other write on this surface carries a submission grant, because the
// form it posts is public and the grant is what makes a post one this site
// handed out. Here the token in the address is already that: a page another
// site makes cannot know it, and the POST is behind the same-origin and
// form-encoded checks as every write. A grant would add a second secret to a
// page that is already behind one.
//
// # A page of its own, not the form
//
// Opened in a tab of its own, never inside somebody's website, so it carries
// no parent origin and frame-ancestors is whatever the surface's default is
// for a path with no form in it.

// Rides is the carpool, as the ride page and the thank-you page use it.
type Rides interface {
	Link(id types.ID) string
	Resolve(ctx context.Context, token string) (formbus.Form, types.ID, error)
	Mine(ctx context.Context, f formbus.Form, id types.ID) (carpoolbus.Mine, error)
	Say(ctx context.Context, f formbus.Form, id types.ID, choice string, seats int) error
	Choose(ctx context.Context, f formbus.Form, rider, driver types.ID) error
	SendLinks(ctx context.Context, f formbus.Form, number phone.Number)
	TextsLinks() bool
}

// rideView is the ride page.
type rideView struct {
	Form  formbus.Form
	Token string

	// What they are doing now, one of these true.
	Driving  bool
	Riding   bool
	OwnWay   bool
	NotSaid  bool
	NotGoing bool

	Name     string
	Waiting  bool
	Car      *rideCar
	Room     []rideCar
	Seats    int
	MaxSeats int

	// Done is what just happened, Problem what did not.
	Done    string
	Problem string
}

type rideCar struct {
	ID     string
	Driver string // the whole name for their own driver, the first name for another car
	Phone  string
	Tel    string // E.164, for tel: and sms: links
	Seats  int
	Free   int
	Riders []rideRider
}

type rideRider struct {
	Name  string
	Phone string
	Tel   string
}

// firstName is the part of a name another rider is shown: enough to know who
// is in the car, and no more of somebody else's sign-up than that.
func firstName(name string) string {
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}

	return "Somebody"
}

func (a app) ride(w http.ResponseWriter, r *http.Request) {
	f, id, ok := a.rideOf(w, r)
	if !ok {
		return
	}

	a.renderRide(w, r, http.StatusOK, f, id, "", "")
}

// rideOf reads the token in the path, answering the page itself when it does
// not lead to a ride.
func (a app) rideOf(w http.ResponseWriter, r *http.Request) (formbus.Form, types.ID, bool) {
	if a.cfg.Rides == nil {
		a.notFound(w, r)

		return formbus.Form{}, types.ID{}, false
	}

	f, id, err := a.cfg.Rides.Resolve(r.Context(), r.PathValue("token"))

	switch {
	case errors.Is(err, carpoolbus.ErrBadRideLink), errors.Is(err, carpoolbus.ErrNotFound):
		a.cfg.Log.Info("a ride link was refused",
			"request_id", web.RequestIDFrom(r.Context()), "reason", err)
		a.cfg.Render.Render(w, r, http.StatusNotFound, "ride-gone", nil)

		return formbus.Form{}, types.ID{}, false

	case err != nil:
		a.cfg.Log.Error("a ride link could not be read",
			"request_id", web.RequestIDFrom(r.Context()), "error", err)
		a.oops(w, r)

		return formbus.Form{}, types.ID{}, false
	}

	return f, id, true
}

func (a app) renderRide(w http.ResponseWriter, r *http.Request, status int, f formbus.Form, id types.ID, done, problem string) {
	m, err := a.cfg.Rides.Mine(r.Context(), f, id)

	switch {
	case errors.Is(err, carpoolbus.ErrNotFound):
		a.cfg.Render.Render(w, r, http.StatusNotFound, "ride-gone", nil)

		return
	case err != nil:
		a.cfg.Log.Error("a ride page could not be read",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
		a.oops(w, r)

		return
	}

	v := rideView{
		Form:     f,
		Token:    r.PathValue("token"),
		Name:     m.Me.Name,
		Waiting:  m.Waiting,
		Seats:    m.Me.Seats,
		MaxSeats: m.MaxSeats,
		Done:     done,
		Problem:  problem,
	}

	switch m.Me.Choice {
	case formbus.CarpoolDrive:
		v.Driving = true
	case formbus.CarpoolNeed:
		v.Riding = true
	case formbus.CarpoolHave:
		v.OwnWay = true
	case carpoolbus.Out:
		v.NotGoing = true
	default:
		v.NotSaid = true
	}

	if v.Seats == 0 {
		v.Seats = 1
	}

	if m.Car != nil {
		c := carOf(*m.Car, v.Driving)
		v.Car = &c
	}

	// Other cars by their driver's first name: enough to choose by, and no
	// more of a stranger's sign-up than that. Their own driver is named in
	// full, as the texts name them.
	for _, c := range m.Room {
		rc := carOf(c, false)
		rc.Driver, rc.Phone, rc.Tel = firstName(c.Driver.Name), "", ""
		v.Room = append(v.Room, rc)
	}

	a.cfg.Render.Render(w, r, status, "ride", v)
}

// carOf is a car for the page. A driver sees their riders' names and numbers
// in full -- they are the ones collecting them -- and a rider sees the others
// in their car by first name.
func carOf(c carpoolbus.Car, mine bool) rideCar {
	out := rideCar{
		ID:     c.ID.String(),
		Driver: c.Driver.Name,
		Phone:  c.Driver.Phone.National(),
		Tel:    c.Driver.Phone.String(),
		Seats:  c.Driver.Seats,
		Free:   c.Free(),
	}

	for _, s := range c.Riders {
		rr := rideRider{Name: firstName(s.Rider.Name)}

		if mine {
			rr.Name, rr.Phone, rr.Tel = s.Rider.Name, s.Rider.Phone.National(), s.Rider.Phone.String()
		}

		out.Riders = append(out.Riders, rr)
	}

	return out
}

// changeRide is every change made on the ride page: a car chosen, or what
// somebody is doing.
func (a app) changeRide(w http.ResponseWriter, r *http.Request) {
	f, id, ok := a.rideOf(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	if err := r.ParseForm(); err != nil {
		a.renderRide(w, r, http.StatusBadRequest, f, id, "", "We could not read that. Please try again.")

		return
	}

	if driver := r.PostFormValue("driver"); driver != "" {
		a.chooseCar(w, r, f, id, driver)

		return
	}

	choice := r.PostFormValue("choice")
	seats, _ := strconv.Atoi(r.PostFormValue("seats"))

	err := a.cfg.Rides.Say(r.Context(), f, id, choice, seats)

	switch {
	case errors.Is(err, carpoolbus.ErrBadChoice):
		a.renderRide(w, r, http.StatusUnprocessableEntity, f, id, "", "Choose how many people you can take, from 1 up.")
	case errors.Is(err, carpoolbus.ErrNotFound):
		a.cfg.Render.Render(w, r, http.StatusNotFound, "ride-gone", nil)
	case err != nil:
		a.cfg.Log.Error("a ride could not be changed",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
		a.oops(w, r)
	default:
		a.renderRide(w, r, http.StatusOK, f, id, saidWords(choice), "")
	}
}

func saidWords(choice string) string {
	switch choice {
	case carpoolbus.Out:
		return "Thanks for letting us know. You are out of the carpool, and the organisers have been told."
	case formbus.CarpoolHave:
		return "Done: you have your own way there."
	case formbus.CarpoolDrive:
		return "Done: you are down as driving."
	case formbus.CarpoolNeed:
		return "Done: you need a ride. We will text you when you are in a car."
	}

	return "Done."
}

func (a app) chooseCar(w http.ResponseWriter, r *http.Request, f formbus.Form, id types.ID, raw string) {
	driver, err := types.ParseID(raw)
	if err != nil {
		a.renderRide(w, r, http.StatusBadRequest, f, id, "", "Choose a car from the list.")

		return
	}

	err = a.cfg.Rides.Choose(r.Context(), f, id, driver)

	switch {
	case errors.Is(err, carpoolbus.ErrCarFull):
		a.renderRide(w, r, http.StatusConflict, f, id, "", "That car just filled up. These are the cars with room now.")
	case errors.Is(err, carpoolbus.ErrNotFound):
		a.renderRide(w, r, http.StatusConflict, f, id, "", "That car is no longer going. These are the cars with room now.")
	case err != nil:
		a.cfg.Log.Error("a rider could not choose a car",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
		a.oops(w, r)
	default:
		a.renderRide(w, r, http.StatusOK, f, id, "Done: you are in this car now, and the driver has been texted.", "")
	}
}

// rideURL is the link to somebody's ride page from the page that thanked
// them, or empty: no carpool, nothing to link to, or a sign-up that does not
// count yet -- one waiting for approval, or on the waiting list.
func (a app) rideURL(f formbus.Form, sub submissionbus.Submission) string {
	if a.cfg.Rides == nil || sub.Status != submissionbus.StatusReceived || sub.Review == submissionbus.ReviewAwaiting {
		return ""
	}

	if _, ok := f.CarpoolOf(); !ok {
		return ""
	}

	return a.cfg.Rides.Link(sub.ID)
}

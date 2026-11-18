// Package carpoolapp is the organisers' page for a form's carpool: every car,
// who is in it, who is still waiting, and the one change an organiser makes by
// hand -- moving somebody to another car.
//
// Reading it is behind results, like the submissions it is drawn from: the
// person counting heads is the person who wants to see the cars. Moving a
// rider and sending a text again are behind admin, because both text people
// on the form's behalf, and the reading role changes nothing (accessbus says
// why that line is kept).
//
// No script, as on every admin page: a move is a small form per rider with a
// dropdown of the cars that have room, and the page comes back redrawn.
package carpoolapp

import (
	"bytes"
	"context"
	"embed"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/carpool/carpoolbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

//go:embed templates/*.html
var templates embed.FS

// Templates is this app's pages, for the renderer.
var Templates = templates

// Forms resolves the form in the path.
type Forms interface {
	ByID(slug types.Slug) (formbus.Form, error)
}

// Carpool is the carpool domain, as this page uses it.
type Carpool interface {
	PlanOf(ctx context.Context, f formbus.Form) (carpoolbus.Plan, error)
	Move(ctx context.Context, f formbus.Form, rider, driver, by types.ID) error
	Retext(ctx context.Context, f formbus.Form, rider types.ID) error
	Texting() bool
}

// Grants says whether the reader may move people, for drawing the buttons.
// The gate in front of the route asks again.
type Grants interface {
	Allowed(ctx context.Context, userID types.ID, form types.Slug, want accessbus.Role) (bool, error)
}

// Config is what this app needs.
type Config struct {
	Log     *slog.Logger
	Forms   Forms
	Carpool Carpool
	Grants  Grants
	Render  *page.Renderer
}

type app struct {
	cfg Config
}

// Routes mounts the page behind results and its two changes behind admin.
func Routes(mux *http.ServeMux, cfg Config, guard, results, admins func(http.Handler) http.Handler) {
	a := app{cfg: cfg}

	slug := "/forms/{" + mid.FormSlugParam + "}/carpool"

	mux.Handle("GET "+slug, guard(results(http.HandlerFunc(a.show))))
	mux.Handle("GET "+slug+".csv", guard(results(http.HandlerFunc(a.export))))
	mux.Handle("POST "+slug+"/move", guard(admins(http.HandlerFunc(a.move))))
	mux.Handle("POST "+slug+"/retext", guard(admins(http.HandlerFunc(a.retext))))
}

// view is the page.
type view struct {
	FormID  string
	Title   string
	Texting bool
	CanMove bool

	// NoMobile is an organiser who would be texted about this carpool but has
	// given no number to text. Nobody else can give it for them -- saving it
	// is their agreement to be texted -- so the page that shows the carpool
	// is the place to say so, to the one person who can do something about it.
	NoMobile bool

	Cars    []carView
	Waiting []riderView
	OwnWay  int

	Done    string
	Problem string
}

type carView struct {
	ID     string
	Driver string
	Phone  string
	Seats  int
	Free   int
	Riders []riderView
}

type riderView struct {
	FormID  string
	ID      string
	Name    string
	Phone   string
	Problem string
	Texted  bool

	// MoveTo is the other cars with a seat free.
	MoveTo []carView
}

func (a app) show(w http.ResponseWriter, r *http.Request) {
	a.page(w, r, http.StatusOK, "", "")
}

func (a app) page(w http.ResponseWriter, r *http.Request, status int, done, problem string) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	plan, err := a.cfg.Carpool.PlanOf(r.Context(), f)
	if err != nil {
		a.oops(w, r, "a carpool could not be read", err)

		return
	}

	v := view{
		FormID:  f.ID.String(),
		Title:   f.Title,
		Texting: a.cfg.Carpool.Texting(),
		CanMove: a.admin(r, f),
		OwnWay:  plan.OwnWay,
		Done:    done,
		Problem: problem,
	}

	if me, ok := mid.UserFrom(r.Context()); ok {
		v.NoMobile = v.Texting && v.CanMove && me.Mobile.IsZero()
	}

	for _, c := range plan.Cars {
		v.Cars = append(v.Cars, carView{
			ID:     c.ID.String(),
			Driver: c.Driver.Name,
			Phone:  c.Driver.Phone.National(),
			Seats:  c.Driver.Seats,
			Free:   c.Free(),
		})
	}

	// Each rider's move list is the other cars with room, built after every
	// car is known.
	riders := func(seats []carpoolbus.Seat, in string) []riderView {
		var out []riderView

		for _, s := range seats {
			rv := riderView{
				FormID:  v.FormID,
				ID:      s.ID.String(),
				Name:    s.Rider.Name,
				Phone:   s.Rider.Phone.National(),
				Problem: s.Ride.Problem,
				Texted:  !s.Ride.RiderTexted.IsZero(),
			}

			for _, c := range v.Cars {
				if c.ID != in && c.Free > 0 {
					rv.MoveTo = append(rv.MoveTo, c)
				}
			}

			out = append(out, rv)
		}

		return out
	}

	for i, c := range plan.Cars {
		v.Cars[i].Riders = riders(c.Riders, v.Cars[i].ID)
	}

	v.Waiting = riders(plan.Waiting, "")

	a.cfg.Render.Render(w, r, status, "carpool", v)
}

func (a app) move(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	me, _ := mid.UserFrom(r.Context())

	rider, err1 := types.ParseID(r.PostFormValue("rider"))
	driver, err2 := types.ParseID(r.PostFormValue("driver"))

	if err1 != nil || err2 != nil {
		a.page(w, r, http.StatusBadRequest, "", "Choose a car to move them to.")

		return
	}

	err := a.cfg.Carpool.Move(r.Context(), f, rider, driver, me.ID)

	switch {
	case errors.Is(err, carpoolbus.ErrCarFull):
		a.page(w, r, http.StatusConflict, "", "That car filled up in the meantime. Choose another.")
	case errors.Is(err, carpoolbus.ErrNotFound):
		a.page(w, r, http.StatusConflict, "", "That person or that car is no longer in the carpool. The page below is as it stands now.")
	case err != nil:
		a.oops(w, r, "a rider could not be moved", err)
	default:
		a.page(w, r, http.StatusOK, "Moved, and both of them texted.", "")
	}
}

func (a app) retext(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	rider, err := types.ParseID(r.PostFormValue("rider"))
	if err != nil {
		a.page(w, r, http.StatusBadRequest, "", "We could not tell who to text. Please try again.")

		return
	}

	err = a.cfg.Carpool.Retext(r.Context(), f, rider)

	switch {
	case errors.Is(err, carpoolbus.ErrNotFound):
		a.page(w, r, http.StatusConflict, "", "They are no longer in a car. The page below is as it stands now.")
	case err != nil:
		a.oops(w, r, "a ride's texts could not be sent again", err)
	default:
		a.page(w, r, http.StatusOK, "Sent again. If it did not go this time either, the reason is beside them.", "")
	}
}

// export is the cars as a spreadsheet: one row per person in a car, and the
// people waiting with no driver.
func (a app) export(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	plan, err := a.cfg.Carpool.PlanOf(r.Context(), f)
	if err != nil {
		a.oops(w, r, "a carpool could not be read", err)

		return
	}

	var body bytes.Buffer
	out := csv.NewWriter(&body)

	rows := [][]string{{"driver", "driver_phone", "seats", "rider", "rider_phone"}}

	for _, c := range plan.Cars {
		if len(c.Riders) == 0 {
			rows = append(rows, []string{c.Driver.Name, c.Driver.Phone.National(), strconv.Itoa(c.Driver.Seats), "", ""})
		}

		for _, s := range c.Riders {
			rows = append(rows, []string{c.Driver.Name, c.Driver.Phone.National(), strconv.Itoa(c.Driver.Seats), s.Rider.Name, s.Rider.Phone.National()})
		}
	}

	for _, s := range plan.Waiting {
		rows = append(rows, []string{"", "", "", s.Rider.Name, s.Rider.Phone.National()})
	}

	if err := out.WriteAll(rows); err != nil {
		a.oops(w, r, "the carpool export could not be written", err)

		return
	}

	name := fmt.Sprintf("%s-carpool-%s.csv", f.ID, time.Now().Format(time.DateOnly))

	h := w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Content-Length", strconv.Itoa(body.Len()))

	if _, err := io.Copy(w, &body); err != nil {
		a.cfg.Log.Warn("a carpool export was cut off while being sent",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
	}
}

// admin reports whether the reader may move people. A failure to read the
// grant costs the buttons, not the page.
func (a app) admin(r *http.Request, f formbus.Form) bool {
	me, ok := mid.UserFrom(r.Context())
	if !ok || a.cfg.Grants == nil {
		return false
	}

	allowed, err := a.cfg.Grants.Allowed(r.Context(), me.ID, f.ID, accessbus.RoleAdmin)
	if err != nil {
		a.cfg.Log.Error("whether the reader may move riders could not be read",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
	}

	return allowed
}

// form resolves the form in the path, and refuses one with no carpool as not
// found: there is no page to show.
func (a app) form(w http.ResponseWriter, r *http.Request) (formbus.Form, bool) {
	slug, err := types.ParseSlug(r.PathValue(mid.FormSlugParam))
	if err != nil {
		http.NotFound(w, r)

		return formbus.Form{}, false
	}

	f, err := a.cfg.Forms.ByID(slug)

	switch {
	case errors.Is(err, formbus.ErrNotFound):
		http.NotFound(w, r)

		return formbus.Form{}, false
	case err != nil:
		a.oops(w, r, "a form could not be read", err)

		return formbus.Form{}, false
	}

	if _, ok := f.CarpoolOf(); !ok {
		http.NotFound(w, r)

		return formbus.Form{}, false
	}

	return f, true
}

func (a app) oops(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.cfg.Log.Error(what, "request_id", web.RequestIDFrom(r.Context()), "error", err)
	http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)
}

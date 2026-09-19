// Package willcallapp is the table at the door on the morning of the feast.
//
// Two volunteers stand at the Shrine from eight until Mass, holding phones.
// People arrive with the receipt email from a form that sold them something
// and are handed one token per ticket. This is the list those two check off.
//
// # Why it is not a page of submissionapp
//
// It reads the same rows, and everything else about it is different. It is
// behind a different role, it changes something, it refreshes itself, and it
// shows one thing per order rather than one column per field. Folding it into
// submissionapp would have cost that app the property its own comment states --
// "Read-only: nothing here changes a submission" -- and a sentence like that
// stops being true silently.
//
// # What guards each route
//
//	GET  /forms/{slug}/will-call             Require, then RequireFormRole(door)
//	POST /forms/{slug}/will-call/{id}/collect  the same
//	POST /forms/{slug}/will-call/{id}/undo     the same
//
// door rather than results, which cannot write, and rather than admin, which
// since the builder shipped means setting the ticket price. accessbus.RoleDoor
// says more about why the middle of the three had to exist.
//
// # It refreshes itself, and it does that without a script
//
// People in the queue buy on their phones while queuing, so a list rendered
// when the table opened is wrong by the time the queue forms. The admin
// surface has no script-src at all -- app/sdk/page.AdminPolicy says adding one
// should feel like a decision -- so the refresh is <meta http-equiv="refresh">
// rather than a poll. No CSP directive governs it, it costs nothing, and it
// works on a phone with a bad signal, which the car park has.
//
// Two things follow from choosing that. Every write redirects rather than
// rendering, so a refresh landing on a POST cannot repeat it. And the refresh
// carries the current search in its URL, because a filter silently cleared
// every ten seconds is worse than no filter -- with a "pause" link for
// somebody who is typing, since a meta refresh will interrupt that and nothing
// can stop it mid-keystroke.
package willcallapp

import (
	"context"
	"embed"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

//go:embed templates
var templates embed.FS

// Templates is this app's own template directory, handed to page.NewRenderer
// by whatever builds the renderer.
var Templates = templates

// Forms is where definitions come from. By id alone: this page is always about
// one named form.
type Forms interface {
	ByID(slug types.Slug) (formbus.Form, error)
}

// Orders is the slice of the submission domain this app needs.
//
// Read the form's submissions, and the three operations of the table. Settle
// is absent and so is Accept: this app cannot take a payment or accept a
// submission, and the interface is where that is said.
type Orders interface {
	ByForm(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error)
	Collect(ctx context.Context, now time.Time, id, by types.ID) (submissionbus.Collection, bool, error)
	Uncollect(ctx context.Context, id types.ID) error
	Collected(ctx context.Context, form types.Slug) (map[types.ID]submissionbus.Collection, error)
}

// Accounts turns the id on a collection into a name, so the page can say who
// handed something over rather than showing an opaque identifier to somebody
// standing at a table.
type Accounts interface {
	ByID(ctx context.Context, id types.ID) (userbus.User, error)
}

// Config is what this app needs.
type Config struct {
	Log      *slog.Logger
	Forms    Forms
	Orders   Orders
	Accounts Accounts
	Render   *page.Renderer

	// Refresh is how often the page reloads itself. Zero takes
	// DefaultRefresh; a test sets it to something it can assert.
	Refresh time.Duration
}

// DefaultRefresh is ten seconds.
//
// Chosen against the thing being waited for, which is a queue moving at
// walking pace and somebody finishing a payment on a phone. Faster buys
// nothing anybody could act on and interrupts more typing; slower and an order
// paid for in the queue is not on the list by the time its buyer reaches the
// front, which is the whole reason the page refreshes at all.
const DefaultRefresh = 10 * time.Second

type app struct {
	cfg     Config
	refresh int
}

// Routes mounts this app.
//
// guard is Require and door is RequireFormRole(door), both handed in by the
// muxer: a route's position in the chain is written down in one place.
func Routes(mux *http.ServeMux, cfg Config, guard, door func(http.Handler) http.Handler) {
	every := cfg.Refresh
	if every <= 0 {
		every = DefaultRefresh
	}

	a := app{cfg: cfg, refresh: int(every.Seconds())}

	behind := func(h http.HandlerFunc) http.Handler {
		return guard(door(h))
	}

	base := "/forms/{" + mid.FormSlugParam + "}/will-call"

	mux.Handle("GET "+base, behind(a.list))
	mux.Handle("POST "+base+"/{id}/collect", behind(a.collect))
	mux.Handle("POST "+base+"/{id}/undo", behind(a.undo))
}

// listView is the table's whole page.
type listView struct {
	FormID string
	Title  string

	Orders []orderView

	// Waiting, Collected and Tokens are the three numbers somebody at the
	// table actually asks: how many are still to come, how many have been
	// seen, and how many tokens that leaves to hand out.
	Waiting   int
	Collected int
	Tokens    int

	// Query is what is in the search box, and Filtered says whether it is
	// hiding anything -- so a volunteer who cannot find a name is told the
	// list is filtered rather than concluding the order is not there.
	Query    string
	Filtered bool
	Showing  int

	// Live and Refresh drive the meta refresh. Live is off when somebody has
	// paused it, which is the only way to type into the search box without
	// being interrupted.
	Live    bool
	Refresh int

	// RefreshURL is this page with its current search kept, so a refresh does
	// not silently clear the filter.
	RefreshURL string
	ToggleURL  string

	Done    string
	Problem string
}

func (v listView) Any() bool { return len(v.Orders) > 0 }

// orderView is one order in the queue.
type orderView struct {
	ID    string
	Who   string
	Email string

	// Bought is what they are owed, in the words of the definition: "2 ×
	// Adult, 1 × Child". The list is one line per order rather than one column
	// per field, because the question at the table is how many tokens to count
	// into somebody's hand.
	Bought string
	Tokens int
	Total  string

	When string

	// Collected, By and At are the check-off. By is a name rather than an id:
	// the reader is standing next to the other volunteer.
	Collected bool
	By        string
	At        string
}

// list renders the queue.
func (a app) list(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	a.show(w, r, http.StatusOK, f, listView{})
}

// collect hands over an order's tokens.
//
// Redirect on every outcome, including the refusals, and the sentence travels
// in the query. That is forced by the meta refresh: a page rendered in answer
// to a POST would be re-submitted by the reload a few seconds later, and the
// one thing this page must never do is repeat a write nobody asked for twice.
func (a app) collect(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	me, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "the will-call page was reached with no account", nil)

		return
	}

	sub, ok := a.order(w, r, f)
	if !ok {
		return
	}

	c, wrote, err := a.cfg.Orders.Collect(r.Context(), time.Now(), sub.ID, me.ID)

	switch {
	case errors.Is(err, submissionbus.ErrNotCollectable):
		// Not a fault in the request: the volunteer tapped a real button on a
		// real order, and the answer is that this one owes money.
		a.back(w, r, f, "", "That order has not been paid for, so there are no tokens for it. They can pay at the table.")

		return
	case errors.Is(err, submissionbus.ErrNotFound):
		a.back(w, r, f, "", "That order is no longer here. The list has been refreshed.")

		return
	case err != nil:
		a.oops(w, r, "an order could not be collected", err)

		return
	}

	if !wrote {
		// Somebody else got there first, which on a table worked by two people
		// is the ordinary case and not an error. Saying who is the useful part.
		a.back(w, r, f, "", "Already handed over"+a.byWhom(r.Context(), c)+". Nothing was changed.")

		return
	}

	a.back(w, r, f, "Handed over to "+who(sub)+". "+tokenPhrase(tokensOf(sub))+" to count out.", "")
}

// undo takes the mark off again, for the mis-tap that is going to happen at a
// table at eight in the morning.
func (a app) undo(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	sub, ok := a.order(w, r, f)
	if !ok {
		return
	}

	if err := a.cfg.Orders.Uncollect(r.Context(), sub.ID); err != nil {
		a.oops(w, r, "a collection could not be undone", err)

		return
	}

	a.cfg.Log.Info("collection undone at the table",
		"request_id", web.RequestIDFrom(r.Context()), "submission_id", sub.ID.String(), "form", f.ID.String())

	a.back(w, r, f, "Put back on the list.", "")
}

// back redirects to the list, keeping the search and carrying one sentence.
//
// Post/redirect/get, because this page reloads itself: rendering in answer to
// a POST would leave a reload repeating the write.
func (a app) back(w http.ResponseWriter, r *http.Request, f formbus.Form, done, problem string) {
	q := url.Values{}

	if search := strings.TrimSpace(r.FormValue("q")); search != "" {
		q.Set("q", search)
	}

	if r.FormValue("live") == "off" {
		q.Set("live", "off")
	}

	switch {
	case done != "":
		q.Set("done", done)
	case problem != "":
		q.Set("problem", problem)
	}

	to := "/forms/" + f.ID.String() + "/will-call"
	if len(q) > 0 {
		to += "?" + q.Encode()
	}

	http.Redirect(w, r, to, http.StatusSeeOther)
}

// Package submissionapp is how somebody reads what was submitted.
//
// It exists because there is no CLI. Every operational thing about this
// service happens in a browser, so the list, the detail view and the CSV
// export are not a convenience on top of a command -- they are the only way to
// find out how many lunches were sold. That is why the design document puts
// this before the payment work rather than after it: money arriving with no
// way to read the orders is worse than orders with no money.
//
// # What guards each route
//
// Everything here is behind a session, and every per-form route is behind a
// grant on that form:
//
//	GET /forms                          Require. The list is filtered to the
//	                                    forms this account holds a grant on,
//	                                    so it leaks no form it may not read.
//	GET /forms/{slug}/submissions       Require, then RequireFormRole(results)
//	GET /forms/{slug}/submissions.csv   the same
//	GET /forms/{slug}/submissions/{id}  the same
//
// The role is results rather than admin throughout. Reading the numbers is not
// editing the price, and the person counting lunches should not have to be
// able to change what a ticket costs.
//
// # Every page here is somebody else's personal data
//
// A submission holds a name, an email address, free text somebody typed, and
// what they paid. So the admin policy's Cache-Control: no-store matters here
// more than anywhere else in the service, and the CSV is served as an
// attachment with the same header rather than something a browser keeps in a
// temporary directory.
package submissionapp

import (
	"context"
	"embed"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formtoml"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

//go:embed templates
var templates embed.FS

// Templates is this app's own template directory, handed to page.NewRenderer
// by whatever builds the renderer.
var Templates = templates

// Forms is where definitions come from. An interface because this app reads
// them and never writes one.
type Forms interface {
	ByID(slug types.Slug) (formbus.Form, error)
	All() []formbus.Form
}

// Submissions is the slice of the submission domain this app needs. Read-only:
// nothing here changes a submission, which is why Settle is absent.
type Submissions interface {
	ByForm(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error)
	ByID(ctx context.Context, id types.ID) (submissionbus.Submission, error)
}

// Grants answers which forms an account may reach, for the index page. The
// per-route check is the middleware's job; this is only for deciding what to
// list.
type Grants interface {
	ForUser(ctx context.Context, userID types.ID) ([]accessbus.Grant, error)
}

// Config is what this app needs.
type Config struct {
	Log         *slog.Logger
	Forms       Forms
	Submissions Submissions
	Grants      Grants
	Render      *page.Renderer
}

type app struct {
	cfg Config
}

// Routes mounts this app.
//
// guard is Require and results is RequireFormRole(results), both handed in by
// the muxer rather than built here -- the chain is written down in one place
// and a route's position in it is not a decision an app package gets to make.
func Routes(mux *http.ServeMux, cfg Config, guard, results func(http.Handler) http.Handler) {
	a := app{cfg: cfg}

	behind := func(role func(http.Handler) http.Handler, h http.HandlerFunc) http.Handler {
		return guard(role(h))
	}

	mux.Handle("GET /forms", guard(http.HandlerFunc(a.index)))

	// The CSV route is registered before the list route only for readability;
	// net/http matches the most specific pattern regardless of order, and
	// "submissions.csv" is a literal segment rather than a wildcard, so the
	// two cannot shadow each other.
	mux.Handle("GET /forms/{"+mid.FormSlugParam+"}/submissions.csv", behind(results, a.export))
	mux.Handle("GET /forms/{"+mid.FormSlugParam+"}/submissions", behind(results, a.list))
	mux.Handle("GET /forms/{"+mid.FormSlugParam+"}/submissions/{id}", behind(results, a.one))
}

// indexView is the landing page: which forms this account may read.
type indexView struct {
	Forms []indexRow
}

type indexRow struct {
	ID    string
	Title string
	Role  string

	// Count is how many submissions there are, and Open says whether the form
	// is still taking them. Both are what somebody wants at a glance before
	// clicking anything.
	Count int
	Open  bool
}

func (v indexView) Any() bool { return len(v.Forms) > 0 }

// index lists the forms this account holds a grant on.
//
// Filtered by the grants rather than showing every form with some locked, so
// the page cannot tell somebody that a form exists which they may not read.
// A parish's list of what it is collecting money for is not sensitive in
// itself, but it is not this page's business to publish either.
func (a app) index(w http.ResponseWriter, r *http.Request) {
	u, ok := mid.UserFrom(r.Context())
	if !ok {
		// Unreachable behind Require, and handled rather than assumed so a
		// route mounted wrongly fails loudly instead of showing an empty list
		// that looks like "you have no access".
		a.oops(w, r, "a submission page was reached with no account", nil)

		return
	}

	grants, err := a.cfg.Grants.ForUser(r.Context(), u.ID)
	if err != nil {
		a.oops(w, r, "the grants could not be listed", err)

		return
	}

	// A site-wide grant means every form, so the listing is built from the
	// form store in that case rather than from the grant rows.
	//
	// Assigned rather than compared against what is already there. A grant is
	// keyed by (account, form) and site-wide is the zero form, so there is at
	// most one of these rows per account -- and comparing with Includes here
	// was a bug rather than caution: Includes refuses an unrecognised role on
	// either side, deliberately, so against the zero value it answered false
	// and a site-wide admin saw an empty list.
	var site accessbus.Role

	named := map[types.Slug]accessbus.Role{}

	for _, g := range grants {
		if g.SiteWide() {
			site = g.Role

			continue
		}

		if held, seen := named[g.Form]; !seen || g.Role.Includes(held) {
			named[g.Form] = g.Role
		}
	}

	now := time.Now()

	var view indexView

	for _, f := range a.cfg.Forms.All() {
		role := named[f.ID]
		if site.Includes(accessbus.RoleResults) && !role.Includes(site) {
			role = site
		}

		if !role.Includes(accessbus.RoleResults) {
			continue
		}

		subs, err := a.cfg.Submissions.ByForm(r.Context(), f.ID)
		if err != nil {
			a.oops(w, r, "the submissions could not be counted", err)

			return
		}

		view.Forms = append(view.Forms, indexRow{
			ID:    f.ID.String(),
			Title: f.Title,
			Role:  role.String(),
			Count: len(subs),
			Open:  f.Open(now),
		})
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "forms", view)
}

// listView is one form's submissions.
type listView struct {
	Form    formbus.Form
	FormID  string
	Columns []string
	Rows    []rowView

	// Summary is the answer to the question somebody actually came with: how
	// many, how much, and how much of it is still owed.
	Summary summaryView
}

type rowView struct {
	ID     string
	When   string
	Status string
	Total  string

	// Cells line up with Columns, one per field of the definition, so the
	// table has the same shape as the form somebody filled in.
	Cells []string
}

// summaryView is the headline. Counts by status rather than one total,
// because "sold" and "started but never paid" are different numbers and
// conflating them is how somebody orders the wrong amount of food.
type summaryView struct {
	Count    int
	Paid     int
	Pending  int
	Received int
	Failed   int

	Items     []itemTally
	PaidTotal string
	OwedTotal string
}

type itemTally struct {
	Label string

	// Confirmed counts only what is settled -- paid, or received on a form
	// that charges nothing. Pending is what somebody started and has not paid
	// for, kept separate because you cater for the first number and hope for
	// the second.
	Confirmed int
	Pending   int
}

func (a app) list(w http.ResponseWriter, r *http.Request) {
	f, subs, ok := a.formAndSubmissions(w, r)
	if !ok {
		return
	}

	view := listView{
		Form:    f,
		FormID:  f.ID.String(),
		Columns: columnsOf(f),
		Summary: summarise(f, subs),
	}

	for _, s := range subs {
		view.Rows = append(view.Rows, rowView{
			ID:     s.ID.String(),
			When:   s.CreatedAt.Local().Format("2 Jan 2006, 15:04"),
			Status: s.Status.String(),
			Total:  formbus.Show(s.Answers.Total, f.Currency),
			Cells:  cellsOf(f, s),
		})
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "submissions", view)
}

// oneView is a single submission in full.
type oneView struct {
	Form   formbus.Form
	FormID string

	ID     string
	When   string
	Status string
	Email  string
	Total  string
	Ref    string

	Answers []answerView
	Lines   []lineView
}

type answerView struct {
	Label string
	Value string

	// Answered is false for a field the person was not asked -- one hidden by
	// its condition -- which reads differently from a field they left blank
	// and is worth showing as such rather than as an empty cell.
	Answered bool
}

type lineView struct {
	Label  string
	Qty    int
	Price  string
	Amount string
}

func (a app) one(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	id, err := types.ParseID(r.PathValue("id"))
	if err != nil {
		a.notFound(w, r)

		return
	}

	s, err := a.cfg.Submissions.ByID(r.Context(), id)

	switch {
	case errors.Is(err, submissionbus.ErrNotFound):
		a.notFound(w, r)

		return
	case err != nil:
		a.oops(w, r, "the submission could not be read", err)

		return
	}

	// The submission has to belong to the form in the path. Without this, a
	// results grant on any one form would read every submission in the
	// service by guessing identifiers -- the role gate checked the form in the
	// URL, and this is what ties the record to it.
	if s.Form != f.ID {
		a.notFound(w, r)

		return
	}

	view := oneView{
		Form:   f,
		FormID: f.ID.String(),
		ID:     s.ID.String(),
		When:   s.CreatedAt.Local().Format("Monday 2 January 2006, 15:04"),
		Status: s.Status.String(),
		Email:  s.Email.String(),
		Total:  formbus.Show(s.Answers.Total, f.Currency),
		Ref:    s.PaymentRef,
	}

	// Walked in the definition's order rather than the record's, so two
	// submissions to the same form read the same way down the page even when
	// one of them skipped a conditional field.
	for _, fld := range f.Fields {
		ans, answered := s.Answers.Field(fld.Name)

		view.Answers = append(view.Answers, answerView{
			Label:    fld.Label,
			Value:    strings.Join(ans.Values, ", "),
			Answered: answered,
		})
	}

	for _, l := range s.Answers.Lines {
		view.Lines = append(view.Lines, lineView{
			Label:  l.Label,
			Qty:    l.Qty,
			Price:  formbus.Show(l.Price, f.Currency),
			Amount: formbus.Show(l.Amount, f.Currency),
		})
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "submission", view)
}

// export writes the CSV.
//
// encoding/csv, so quoting and embedded commas, quotes and newlines are the
// standard library's problem rather than a string join that works until
// somebody types a comma in the notes field.
func (a app) export(w http.ResponseWriter, r *http.Request) {
	f, subs, ok := a.formAndSubmissions(w, r)
	if !ok {
		return
	}

	// Built in memory first. A CSV of a few hundred rows is small, and writing
	// straight to the ResponseWriter would mean a failure halfway through
	// arriving as a truncated file under a 200 -- a spreadsheet that opens and
	// is quietly missing its last rows, which is worse than an error.
	var body strings.Builder

	out := csv.NewWriter(&body)

	header := []string{"submitted_at", "submission_id", "status", "total", "currency"}
	header = append(header, columnsOf(f)...)
	header = append(header, "payment_reference")

	if err := out.Write(header); err != nil {
		a.oops(w, r, "the export header could not be written", err)

		return
	}

	for _, s := range subs {
		// RFC 3339 in UTC, because a spreadsheet full of local times with no
		// offset is a spreadsheet nobody can compare against anything.
		row := []string{
			s.CreatedAt.UTC().Format(time.RFC3339),
			s.ID.String(),
			s.Status.String(),
			s.Answers.Total.String(),
			s.Answers.Currency,
		}

		row = append(row, cellsOf(f, s)...)
		row = append(row, s.PaymentRef)

		if err := out.Write(row); err != nil {
			a.oops(w, r, "an export row could not be written", err)

			return
		}
	}

	out.Flush()

	if err := out.Error(); err != nil {
		a.oops(w, r, "the export could not be written", err)

		return
	}

	// The visitor's IP address is deliberately not a column. It is kept on the
	// submission for Stripe's fraud checks, and a spreadsheet that gets
	// emailed around a parish office is the wrong place for it.

	name := fmt.Sprintf("%s-submissions-%s.csv", f.ID, time.Now().Format("2006-01-02"))

	h := w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Content-Length", strconv.Itoa(body.Len()))

	// no-store is already set by the surface's policy and is left alone here
	// on purpose: this file is a list of names, addresses and amounts.

	if _, err := io.WriteString(w, body.String()); err != nil {
		a.cfg.Log.Warn("an export was cut off while being sent",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
	}
}

// columnsOf is the column heading per field, in the definition's order,
// followed by one per item.
//
// From the definition rather than from the submissions, which is the whole
// reason formbus.Answers keeps its fields as an ordered slice. Derived from
// the rows instead, a form whose first submission skipped a conditional field
// would export a spreadsheet missing that column entirely, and two exports of
// the same form could have different columns.
func columnsOf(f formbus.Form) []string {
	out := make([]string, 0, len(f.Fields)+len(f.Items))

	for _, fld := range f.Fields {
		out = append(out, fld.Label)
	}

	for _, it := range f.Items {
		out = append(out, it.Label)
	}

	return out
}

// cellsOf is one submission's values, lined up with columnsOf.
func cellsOf(f formbus.Form, s submissionbus.Submission) []string {
	out := make([]string, 0, len(f.Fields)+len(f.Items))

	for _, fld := range f.Fields {
		ans, ok := s.Answers.Field(fld.Name)
		if !ok {
			// Not asked, or left blank. Empty either way in a spreadsheet;
			// the detail page is where the difference is visible.
			out = append(out, "")

			continue
		}

		out = append(out, strings.Join(ans.Values, ", "))
	}

	for _, it := range f.Items {
		qty := ""

		for _, l := range s.Answers.Lines {
			if l.ItemID == it.ID {
				qty = strconv.Itoa(l.Qty)

				break
			}
		}

		out = append(out, qty)
	}

	return out
}

// summarise counts what somebody came to the page to find out.
func summarise(f formbus.Form, subs []submissionbus.Submission) summaryView {
	s := summaryView{Count: len(subs)}

	tally := map[string]*itemTally{}
	order := make([]string, 0, len(f.Items))

	for _, it := range f.Items {
		tally[it.ID] = &itemTally{Label: it.Label}
		order = append(order, it.ID)
	}

	var paid, owed types.Money

	for _, sub := range subs {
		switch sub.Status {
		case submissionbus.StatusPaid:
			s.Paid++
		case submissionbus.StatusPending:
			s.Pending++
		case submissionbus.StatusReceived:
			s.Received++
		case submissionbus.StatusFailed:
			s.Failed++
		}

		settled := sub.Status.Settled()

		if settled {
			paid += sub.Answers.Total
		} else if sub.Status == submissionbus.StatusPending {
			owed += sub.Answers.Total
		}

		for _, l := range sub.Answers.Lines {
			t, known := tally[l.ItemID]
			if !known {
				// An item the definition no longer has, which a submission
				// from before an edit can legitimately hold. Not counted
				// against a column that does not exist; the row still shows
				// it on the detail page.
				continue
			}

			switch {
			case settled:
				t.Confirmed += l.Qty
			case sub.Status == submissionbus.StatusPending:
				t.Pending += l.Qty
			}
		}
	}

	for _, id := range order {
		s.Items = append(s.Items, *tally[id])
	}

	s.PaidTotal = formbus.Show(paid, f.Currency)
	s.OwedTotal = formbus.Show(owed, f.Currency)

	return s
}

// formAndSubmissions resolves the form in the path and reads its submissions.
func (a app) formAndSubmissions(w http.ResponseWriter, r *http.Request) (formbus.Form, []submissionbus.Submission, bool) {
	f, ok := a.form(w, r)
	if !ok {
		return formbus.Form{}, nil, false
	}

	subs, err := a.cfg.Submissions.ByForm(r.Context(), f.ID)
	if err != nil {
		a.oops(w, r, "the submissions could not be listed", err)

		return formbus.Form{}, nil, false
	}

	return f, subs, true
}

// form resolves the slug in the path.
//
// The role gate in front of this route has already checked a grant on that
// slug, so reaching here means the account may read this form -- but the gate
// does not know whether the form exists, because grants are rows and
// definitions are files. So a grant on a form that has since been renamed
// away lands here and gets a 404.
func (a app) form(w http.ResponseWriter, r *http.Request) (formbus.Form, bool) {
	slug, err := types.ParseSlug(r.PathValue(mid.FormSlugParam))
	if err != nil {
		a.notFound(w, r)

		return formbus.Form{}, false
	}

	f, err := a.cfg.Forms.ByID(slug)

	switch {
	case errors.Is(err, formtoml.ErrNotFound):
		a.notFound(w, r)

		return formbus.Form{}, false
	case err != nil:
		a.oops(w, r, "a form could not be read", err)

		return formbus.Form{}, false
	}

	return f, true
}

func (a app) notFound(w http.ResponseWriter, r *http.Request) {
	a.cfg.Render.Render(w, r, http.StatusNotFound, "no-form", struct{}{})
}

// oops logs the detail and shows a sentence. The error never reaches the page:
// this surface is behind a session, but a database error message can still
// name a table or a path, and neither belongs in front of anybody.
func (a app) oops(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.cfg.Log.Error(what,
		"request_id", web.RequestIDFrom(r.Context()), "error", err)
	http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)
}

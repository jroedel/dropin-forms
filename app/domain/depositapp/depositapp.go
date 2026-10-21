// Package depositapp is how somebody keeping the books matches Stripe's
// deposits to the bank against the sales on one form.
//
// A deposit is one line on the checking account's statement and holds a
// week's payments from every form, less Stripe's fees and any refunds, and
// possibly money this service never saw. This page takes one form and shows,
// for every deposit holding any of its money, the deposit's whole amount --
// the number on the statement -- beside this form's sales, refunds, fees and
// net, and the rest split into other forms and money that came from no form.
// The three add up to the bank line, which is what makes the page usable for
// reconciling rather than merely interesting. depositbus says how the numbers
// are made.
//
// # What guards each route
//
//	GET /forms/{slug}/deposits      Require, then RequireFormRole(results)
//	GET /forms/{slug}/deposits.csv  the same
//
// results, because the person reconciling reads and exports and should change
// nothing: no price, no order, nobody's access. Nothing here writes at all.
//
// # Why it is not a page of submissionapp
//
// It reads a different domain. The submissions list is the orders as people
// placed them; this is the money as the bank received it, days later and
// mixed with every other form's. Folding it into submissionapp would hand
// that app the deposit domain for the sake of one page, and the two answer
// different people's questions.
package depositapp

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
	"github.com/jroedel/dropin-forms/business/domain/deposit/depositbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

//go:embed templates
var templates embed.FS

// Templates is this app's own template directory, handed to page.NewRenderer
// by whatever builds the renderer.
var Templates = templates

// Forms is where definitions come from.
type Forms interface {
	ByID(slug types.Slug) (formbus.Form, error)
}

// Deposits is the deposit domain, read-only.
type Deposits interface {
	ForForm(ctx context.Context, form types.Slug) ([]depositbus.Share, error)
	Undeposited(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error)
	Synced() (time.Time, bool)
}

// Orders turns a line's submission into a name. Both lists, because a hidden
// order's money is in the deposit all the same and a line with no name on it
// is the one somebody will ask about.
type Orders interface {
	ByForm(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error)
	Hidden(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error)
}

// Config is what this app needs.
type Config struct {
	Log      *slog.Logger
	Forms    Forms
	Deposits Deposits
	Orders   Orders
	Render   *page.Renderer
}

type app struct {
	cfg Config
}

// Routes mounts this app.
//
// guard is Require and results is RequireFormRole(results), both handed in by
// the muxer: a route's position in the chain is written down in one place.
func Routes(mux *http.ServeMux, cfg Config, guard, results func(http.Handler) http.Handler) {
	a := app{cfg: cfg}

	behind := func(h http.HandlerFunc) http.Handler {
		return guard(results(h))
	}

	mux.Handle("GET /forms/{"+mid.FormSlugParam+"}/deposits.csv", behind(a.export))
	mux.Handle("GET /forms/{"+mid.FormSlugParam+"}/deposits", behind(a.list))
}

// list renders the page.
func (a app) list(w http.ResponseWriter, r *http.Request) {
	f, shares, orders, ok := a.read(w, r)
	if !ok {
		return
	}

	waiting, err := a.cfg.Deposits.Undeposited(r.Context(), f.ID)
	if err != nil {
		a.oops(w, r, "the orders not yet deposited could not be listed", err)

		return
	}

	view := build(f, shares, orders, waiting)

	if at, ok := a.cfg.Deposits.Synced(); ok {
		view.Checked = "Checked with Stripe at " + at.Local().Format("15:04 on 2 Jan 2006") + "."
	} else {
		// True for the seconds after a restart, and for as long as Stripe is
		// refusing -- which the log says at Error. Not "never", because the
		// deposits shown may be from before the restart.
		view.Checked = "Not checked with Stripe since the service last started. If this does not change within the hour, the log says why."
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "deposits", view)
}

// export is one row per line of this form's in any deposit.
//
// Lines rather than deposits, because a spreadsheet can total lines into
// deposits and cannot do the reverse; the deposit's amount and identifier
// are on every row so that grouping needs nothing else.
func (a app) export(w http.ResponseWriter, r *http.Request) {
	f, shares, orders, ok := a.read(w, r)
	if !ok {
		return
	}

	var body bytes.Buffer
	out := csv.NewWriter(&body)

	header := []string{
		"deposit_date", "payout_id", "deposit_amount", "currency", "sent_to", "trace_id",
		"kind", "stripe_type", "created_at", "submission_id", "name", "email",
		"amount", "fee", "net", "payment_reference", "balance_transaction",
	}

	if err := out.Write(header); err != nil {
		a.oops(w, r, "the export header could not be written", err)

		return
	}

	for _, s := range shares {
		for _, l := range s.Lines {
			o := orders[l.SubmissionID]

			// Amounts as plain decimals with a minus sign, the existing
			// export's convention, because a spreadsheet parses that and not
			// a currency symbol. Dates in UTC, for the reason the
			// submissions export gives; the deposit date is a calendar date
			// and has no time to be confused about.
			row := []string{
				s.Payout.ArrivalDate.UTC().Format(time.DateOnly),
				s.Payout.ID,
				s.Payout.Amount.String(),
				s.Payout.Currency,
				destination(s.Payout),
				s.Payout.TraceID,
				string(l.Kind),
				l.Type,
				l.Created.UTC().Format(time.RFC3339),
				l.SubmissionID.String(),
				nameOf(o),
				o.Email.String(),
				l.Amount.String(),
				l.Fee.String(),
				l.Net.String(),
				l.PaymentRef,
				l.ID,
			}

			if err := out.Write(row); err != nil {
				a.oops(w, r, "an export row could not be written", err)

				return
			}
		}
	}

	out.Flush()

	if err := out.Error(); err != nil {
		a.oops(w, r, "the export could not be written", err)

		return
	}

	name := fmt.Sprintf("%s-deposits-%s.csv", f.ID, time.Now().Format(time.DateOnly))

	h := w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Content-Length", strconv.Itoa(body.Len()))

	if _, err := io.Copy(w, &body); err != nil {
		a.cfg.Log.Warn("an export was cut off while being sent",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
	}
}

// read resolves the form in the path and reads its deposits and orders.
func (a app) read(w http.ResponseWriter, r *http.Request) (formbus.Form, []depositbus.Share, map[types.ID]submissionbus.Submission, bool) {
	f, ok := a.form(w, r)
	if !ok {
		return formbus.Form{}, nil, nil, false
	}

	shares, err := a.cfg.Deposits.ForForm(r.Context(), f.ID)
	if err != nil {
		a.oops(w, r, "the deposits could not be read", err)

		return formbus.Form{}, nil, nil, false
	}

	orders := map[types.ID]submissionbus.Submission{}

	for _, list := range []func(context.Context, types.Slug) ([]submissionbus.Submission, error){a.cfg.Orders.ByForm, a.cfg.Orders.Hidden} {
		subs, err := list(r.Context(), f.ID)
		if err != nil {
			a.oops(w, r, "the orders on that form could not be listed", err)

			return formbus.Form{}, nil, nil, false
		}

		for _, s := range subs {
			orders[s.ID] = s
		}
	}

	return f, shares, orders, true
}

// form resolves the form in the path.
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

	return f, true
}

func (a app) oops(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.cfg.Log.Error(what,
		"request_id", web.RequestIDFrom(r.Context()), "error", err)
	http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)
}

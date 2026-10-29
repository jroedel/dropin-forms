// Package reviewapp approves and declines submissions, on a form that holds
// every submission until somebody does -- formbus.Form.NeedsApproval.
//
// # Why it is not a page of submissionapp
//
// hideapp's reason: submissionapp only reads, and this writes. The buttons are
// on submissionapp's page for one submission, because that is where somebody
// is reading the request when they decide it, and they post here.
//
// # What guards each route
//
//	POST /forms/{slug}/submissions/{id}/approve   Require, then RequireFormRole(results)
//	POST /forms/{slug}/submissions/{id}/decline   the same
//
// results, and that is a decision rather than an oversight. Everywhere else
// results reads and changes nothing; approving is the one thing it decides,
// because the person who reads the sign-ups is the person who knows whether
// Monday at nine suits Father -- and making them an administrator of the form
// to say so would also let them change what it asks and who else may see it.
// Door and admin include results, so they may decide too.
//
// Both redirect back to the submission's page, which says where it stands
// and who decided it: that sentence is the confirmation, and a POST answered
// with a page would be repeated by a reload.
package reviewapp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// Forms resolves the slug in the path.
type Forms interface {
	ByID(slug types.Slug) (formbus.Form, error)
}

// Submissions is the slice of the submission domain this app needs.
type Submissions interface {
	ByID(ctx context.Context, id types.ID) (submissionbus.Submission, error)
	Decide(ctx context.Context, now time.Time, d submissionbus.Decision) (submissionbus.Submission, submissionbus.Submission, error)
}

// Notify tells the person what was decided.
type Notify interface {
	Decided(ctx context.Context, id types.ID)
}

// Config is what this app needs. Notify is optional: an installation that
// sends no mail still lets the office decide, and the person finds out the
// way they would have before there was mail -- by being told.
type Config struct {
	Log         *slog.Logger
	Forms       Forms
	Submissions Submissions
	Notify      Notify
	Now         func() time.Time
}

type app struct {
	cfg Config
}

// Routes mounts this app. guard is Require and results is
// RequireFormRole(results), handed in by the muxer.
func Routes(mux *http.ServeMux, cfg Config, guard, results func(http.Handler) http.Handler) {
	a := app{cfg: cfg}

	base := "/forms/{" + mid.FormSlugParam + "}/submissions/{id}"

	mux.Handle("POST "+base+"/approve", guard(results(a.decide(submissionbus.ReviewApproved))))
	mux.Handle("POST "+base+"/decline", guard(results(a.decide(submissionbus.ReviewDeclined))))
}

func (a app) now() time.Time {
	if a.cfg.Now != nil {
		return a.cfg.Now()
	}

	return time.Now()
}

// decide is both routes, which differ only in where the submission ends up.
func (a app) decide(to submissionbus.Review) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := a.form(w, r)
		if !ok {
			return
		}

		me, ok := mid.UserFrom(r.Context())
		if !ok {
			a.oops(w, r, "deciding a submission was reached with no account", nil)

			return
		}

		id, err := types.ParseID(r.PathValue("id"))
		if err != nil {
			a.notFound(w)

			return
		}

		sub, err := a.cfg.Submissions.ByID(r.Context(), id)

		switch {
		case errors.Is(err, submissionbus.ErrNotFound):
			a.notFound(w)

			return
		case err != nil:
			a.oops(w, r, "the submission could not be read", err)

			return
		}

		// The gate authorised the account against the slug in the path; the
		// id is a separate wildcard nothing has looked at. hideapp checks the
		// same thing for the same reason.
		if sub.Form != f.ID {
			a.notFound(w)

			return
		}

		back := "/forms/" + f.ID.String() + "/submissions/" + sub.ID.String()

		_, after, err := a.cfg.Submissions.Decide(r.Context(), a.now(), submissionbus.Decision{
			ID:   sub.ID,
			Form: f.ID,
			To:   to,
			By:   me.ID,
			Note: r.PostFormValue("note"),

			// Counted only when a declined submission is approved after all,
			// against the definition as it stands now.
			Claims: f.Claims(sub.Answers),
		})

		_, full := errors.AsType[submissionbus.OptionFull](err)

		switch {
		case err == nil:

		case full:
			// Its place went to somebody else while it was declined. The page
			// says so; nothing was written.
			http.Redirect(w, r, back+"?taken=1", http.StatusSeeOther)

			return

		case errors.Is(err, submissionbus.ErrNotDecidable), errors.Is(err, submissionbus.ErrChangedMeanwhile):
			// Already where it was being sent, or decided by somebody else a
			// moment ago. Either way the page shows where it now stands, which
			// is the answer, and nobody is told twice.
			http.Redirect(w, r, back, http.StatusSeeOther)

			return

		case errors.Is(err, submissionbus.ErrNotFound):
			a.notFound(w)

			return

		default:
			a.oops(w, r, "the submission could not be decided", err)

			return
		}

		a.cfg.Log.Info("submission decided",
			"request_id", web.RequestIDFrom(r.Context()),
			"form", f.ID.String(), "submission_id", after.ID.String(), "review", after.Review.String())

		// Before the redirect, for the reason embedapp tells before it
		// renders: the response is the end of the handler.
		if a.cfg.Notify != nil {
			a.cfg.Notify.Decided(r.Context(), after.ID)
		}

		http.Redirect(w, r, back, http.StatusSeeOther)
	})
}

func (a app) form(w http.ResponseWriter, r *http.Request) (formbus.Form, bool) {
	slug, err := types.ParseSlug(r.PathValue(mid.FormSlugParam))
	if err != nil {
		a.notFound(w)

		return formbus.Form{}, false
	}

	f, err := a.cfg.Forms.ByID(slug)

	switch {
	case errors.Is(err, formbus.ErrNotFound):
		a.notFound(w)

		return formbus.Form{}, false
	case err != nil:
		a.oops(w, r, "a form could not be read", err)

		return formbus.Form{}, false
	}

	return f, true
}

// notFound is plain text and the same sentence mid.RequireFormRole uses.
func (a app) notFound(w http.ResponseWriter) {
	http.Error(w, "there is no form by that name.", http.StatusNotFound)
}

func (a app) oops(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.cfg.Log.Error(what, "request_id", web.RequestIDFrom(r.Context()), "error", err)
	http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)
}

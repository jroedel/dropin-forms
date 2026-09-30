// Package hideapp takes a submission out of every list, and puts it back.
//
// The case is a test submission -- somebody trying the form before the parish
// sees it -- or a duplicate, or a joke. submissionbus.Hiding says why this
// hides rather than deletes, and which lists a hidden submission leaves.
//
// # Why it is not a page of submissionapp
//
// The same reason willcallapp is not: submissionapp is read-only, says so,
// and sits behind results. This writes, and sits behind admin. The buttons
// are on submissionapp's page for one submission, because that is where
// somebody is looking when they decide a row should go, and they post here.
//
// # What guards each route
//
//	POST /forms/{slug}/submissions/{id}/hide     Require, then RequireFormRole(admin)
//	POST /forms/{slug}/submissions/{id}/unhide   the same
//
// admin, not results -- results cannot write -- and not door, which may mark
// an order collected and nothing else. Taking a row out of the totals changes
// the number somebody orders food against, which is the form's administrator's
// decision.
//
// Both redirect back to the submission's own page, which says whether it is
// hidden and by whom: that sentence is the confirmation, and a POST answered
// with a page would be repeated by a reload.
package hideapp

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
	Hide(ctx context.Context, now time.Time, id, by types.ID) (submissionbus.Hiding, bool, error)
	Unhide(ctx context.Context, id, by types.ID) error
}

// Config is what this app needs.
type Config struct {
	Log         *slog.Logger
	Forms       Forms
	Submissions Submissions
}

type app struct {
	cfg Config
}

// Routes mounts this app. guard is Require and admins is
// RequireFormRole(admin), handed in by the muxer.
func Routes(mux *http.ServeMux, cfg Config, guard, admins func(http.Handler) http.Handler) {
	a := app{cfg: cfg}

	base := "/forms/{" + mid.FormSlugParam + "}/submissions/{id}"

	mux.Handle("POST "+base+"/hide", guard(admins(http.HandlerFunc(a.hide))))
	mux.Handle("POST "+base+"/unhide", guard(admins(http.HandlerFunc(a.unhide))))
}

func (a app) hide(w http.ResponseWriter, r *http.Request) {
	a.change(w, r, func(ctx context.Context, id, by types.ID) error {
		_, _, err := a.cfg.Submissions.Hide(ctx, time.Now(), id, by)

		return err
	})
}

func (a app) unhide(w http.ResponseWriter, r *http.Request) {
	a.change(w, r, a.cfg.Submissions.Unhide)
}

// change resolves the form and the submission, applies do, and goes back to
// the submission's page.
func (a app) change(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, id, by types.ID) error) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	me, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "hiding a submission was reached with no account", nil)

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

	// The gate authorised the account against the slug in the path; the id
	// is a separate wildcard nothing has looked at. Without this an
	// administrator of one form could hide submissions on any other by
	// editing the URL. 404 rather than 403, as willcallapp answers it.
	if sub.Form != f.ID {
		a.notFound(w)

		return
	}

	if err := do(r.Context(), sub.ID, me.ID); err != nil {
		a.oops(w, r, "the submission could not be changed", err)

		return
	}

	http.Redirect(w, r, "/forms/"+f.ID.String()+"/submissions/"+sub.ID.String(), http.StatusSeeOther)
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

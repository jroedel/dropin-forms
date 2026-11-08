// Package refundapp gives money back on a paid submission, all of it or part,
// and cancels the registration with it when asked.
//
// # Why it is not a page of submissionapp
//
// hideapp's reason: submissionapp only reads, and this writes -- and this
// writes to Stripe as well as here. The form is on submissionapp's page for
// one submission, where somebody is reading the order when they decide, and
// it posts here.
//
// # What guards each route
//
//	POST /forms/{slug}/submissions/{id}/refund   Require, then RequireFormRole(admin)
//
// admin, because it sends the parish's money out. Whoever reads the sign-ups
// can see what was refunded and by whom; giving it back is the decision of
// whoever administers the form.
//
// # The order of the three steps
//
// Ask submissionbus whether the refund is one the submission can take, ask
// Stripe to make it, then write it down. Stripe is in the middle because it is
// the one step that cannot be undone, and nothing here should be recorded as
// refunded that Stripe did not refund. When Stripe has refunded and the
// writing-down fails, the page says so, and Stripe's own notification of the
// refund brings the running total up to date: see submissionbus.SyncRefunded.
//
// It redirects back to the submission's page with a word saying what
// happened, which the page turns into a sentence: a fixed word rather than a
// message in the address, so nothing a link carries is shown as if the
// service had said it.
package refundapp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/payment/paybus"
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
	CanRefund(ctx context.Context, r submissionbus.Refund) (submissionbus.Submission, error)
	RecordRefund(ctx context.Context, before submissionbus.Submission, made submissionbus.RefundMade) (submissionbus.Submission, error)
}

// Payments makes the refund at Stripe. Its own interface, and not the
// webhook's or the payment form's, for the reason the muxer gives for keeping
// those two apart: each surface gets the one thing it does.
type Payments interface {
	Refund(ctx context.Context, o paybus.RefundOrder) (paybus.RefundDone, error)
}

// Notify tells the person.
type Notify interface {
	Refunded(ctx context.Context, id types.ID, amount types.Money, note string)
}

// Config is what this app needs. Notify is optional, for reviewapp's reason.
type Config struct {
	Log         *slog.Logger
	Forms       Forms
	Submissions Submissions
	Payments    Payments
	Notify      Notify
	Now         func() time.Time
}

type app struct {
	cfg Config
}

// The words the submission's page turns into a sentence.
const (
	Done       = "done"
	Changed    = "changed"
	TooMuch    = "too-much"
	Nothing    = "nothing"
	NotPaid    = "not-paid"
	Refused    = "refused"
	Unrecorded = "unrecorded"
	BadAmount  = "amount"
)

// Routes mounts this app. guard is Require and admins is
// RequireFormRole(admin), handed in by the muxer.
func Routes(mux *http.ServeMux, cfg Config, guard, admins func(http.Handler) http.Handler) {
	a := app{cfg: cfg}

	mux.Handle("POST /forms/{"+mid.FormSlugParam+"}/submissions/{id}/refund", guard(admins(http.HandlerFunc(a.refund))))
}

func (a app) now() time.Time {
	if a.cfg.Now != nil {
		return a.cfg.Now()
	}

	return time.Now()
}

func (a app) refund(w http.ResponseWriter, r *http.Request) {
	f, sub, ok := a.submission(w, r)
	if !ok {
		return
	}

	me, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "a refund was reached with no account", nil)

		return
	}

	back := "/forms/" + f.ID.String() + "/submissions/" + sub.ID.String()
	answer := func(word string) {
		http.Redirect(w, r, back+"?refund="+word, http.StatusSeeOther)
	}

	// The page carries the running total it was drawn with. If it has moved
	// since -- somebody else refunded from another tab, or a refund made in
	// Stripe's dashboard arrived -- the amount on the page was chosen against
	// a figure that is no longer true, and nothing is sent.
	if drawn, err := strconv.ParseInt(r.PostFormValue("before"), 10, 64); err != nil || types.Money(drawn) != sub.Refunded {
		answer(Changed)

		return
	}

	// Empty is everything that is left, which is what the box says when the
	// page is drawn and what "refund it" means.
	amount := sub.Kept()

	if raw := strings.TrimSpace(r.PostFormValue("amount")); raw != "" {
		m, err := types.ParseMoney(raw)
		if err != nil {
			answer(BadAmount)

			return
		}

		amount = m
	}

	ask := submissionbus.Refund{
		ID:     sub.ID,
		Form:   f.ID,
		By:     me.ID,
		Amount: amount,
		Cancel: r.PostFormValue("cancel") == "yes",
		Note:   r.PostFormValue("note"),
	}

	before, err := a.cfg.Submissions.CanRefund(r.Context(), ask)

	switch {
	case err == nil:

	case errors.Is(err, submissionbus.ErrNotFound):
		a.notFound(w)

		return

	case errors.Is(err, submissionbus.ErrNotRefundable):
		switch {
		case amount > sub.Kept():
			answer(TooMuch)
		case sub.Status != submissionbus.StatusPaid && sub.Status != submissionbus.StatusCancelled:
			answer(NotPaid)
		default:
			answer(Nothing)
		}

		return

	default:
		a.oops(w, r, "whether the submission can be refunded could not be read", err)

		return
	}

	now := a.now()

	made := submissionbus.RefundMade{
		SubmissionID: sub.ID,
		Form:         f.ID,
		Amount:       amount,
		By:           me.ID,
		At:           now,
		Note:         ask.Note,
		Cancelled:    ask.Cancel,

		// A cancellation that gives nothing back has no Stripe refund to name
		// it, and is given a key of its own that cannot collide with one.
		RefundID: "cancel:" + sub.ID.String() + ":" + strconv.FormatInt(now.UnixMilli(), 10),
	}

	if amount > 0 {
		done, err := a.cfg.Payments.Refund(r.Context(), paybus.RefundOrder{
			SubmissionID: sub.ID,
			PaymentRef:   before.PaymentRef,
			Amount:       amount,
			Key:          "refund:" + sub.ID.String() + ":" + strconv.FormatInt(int64(before.Refunded), 10) + ":" + strconv.FormatInt(int64(amount), 10),
		})
		if err != nil {
			a.cfg.Log.Error("Stripe did not make a refund",
				"request_id", web.RequestIDFrom(r.Context()),
				"form", f.ID.String(), "submission_id", sub.ID.String(), "amount", amount.String(), "error", err)
			answer(Refused)

			return
		}

		made.RefundID = done.ID
	}

	if _, err := a.cfg.Submissions.RecordRefund(r.Context(), before, made); err != nil {
		// Error, because money has moved and this service does not yet say
		// so. Stripe's notification of the refund brings the total up to
		// date; a cancellation it cannot bring, and the page says to try
		// that again.
		a.cfg.Log.Error("a refund was made in Stripe and could not be recorded",
			"request_id", web.RequestIDFrom(r.Context()),
			"form", f.ID.String(), "submission_id", sub.ID.String(),
			"refund", made.RefundID, "amount", amount.String(), "cancel", ask.Cancel, "error", err)

		if amount > 0 {
			answer(Unrecorded)
		} else {
			answer(Changed)
		}

		return
	}

	a.cfg.Log.Info("refunded",
		"request_id", web.RequestIDFrom(r.Context()),
		"form", f.ID.String(), "submission_id", sub.ID.String(),
		"amount", amount.String(), "cancel", ask.Cancel, "refund", made.RefundID)

	if a.cfg.Notify != nil {
		a.cfg.Notify.Refunded(r.Context(), sub.ID, amount, ask.Note)
	}

	answer(Done)
}

// submission resolves the form and the submission in the path.
func (a app) submission(w http.ResponseWriter, r *http.Request) (formbus.Form, submissionbus.Submission, bool) {
	slug, err := types.ParseSlug(r.PathValue(mid.FormSlugParam))
	if err != nil {
		a.notFound(w)

		return formbus.Form{}, submissionbus.Submission{}, false
	}

	f, err := a.cfg.Forms.ByID(slug)

	switch {
	case errors.Is(err, formbus.ErrNotFound):
		a.notFound(w)

		return formbus.Form{}, submissionbus.Submission{}, false
	case err != nil:
		a.oops(w, r, "a form could not be read", err)

		return formbus.Form{}, submissionbus.Submission{}, false
	}

	id, err := types.ParseID(r.PathValue("id"))
	if err != nil {
		a.notFound(w)

		return formbus.Form{}, submissionbus.Submission{}, false
	}

	sub, err := a.cfg.Submissions.ByID(r.Context(), id)

	switch {
	case errors.Is(err, submissionbus.ErrNotFound):
		a.notFound(w)

		return formbus.Form{}, submissionbus.Submission{}, false
	case err != nil:
		a.oops(w, r, "the submission could not be read", err)

		return formbus.Form{}, submissionbus.Submission{}, false
	}

	// The gate authorised the account against the slug in the path; the id
	// is a separate wildcard nothing has looked at. Without this an
	// administrator of one form could refund another's orders by editing the
	// URL.
	if sub.Form != f.ID {
		a.notFound(w)

		return formbus.Form{}, submissionbus.Submission{}, false
	}

	return f, sub, true
}

// notFound is plain text and the same sentence mid.RequireFormRole uses.
func (a app) notFound(w http.ResponseWriter) {
	http.Error(w, "there is no form by that name.", http.StatusNotFound)
}

func (a app) oops(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.cfg.Log.Error(what, "request_id", web.RequestIDFrom(r.Context()), "error", err)
	http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)
}

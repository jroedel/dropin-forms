package embedapp

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/payment/paybus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// payField is the hidden input that carries the payment link's token from the
// page to the POST, as answersField does for a change.
const payField = "t"

// payView is the page for paying for a place offered from a waiting list.
//
// One page, several states, because a link in an email is opened whenever the
// person gets to it: the place may still be held, already paid for, or gone,
// and each wants its own sentence rather than an error.
type payView struct {
	Form         formbus.Form
	ParentOrigin string

	// Token is the link's own token, carried into the POST.
	Token string

	// Held is a place still held, until Until, with the lines and the total
	// it comes to. PayURL is Stripe's page, once one has been made; until it
	// has, the page offers the button that makes it.
	Held   bool
	Until  string
	Lines  []lineView
	Total  string
	PayURL string

	// Paid is a place already paid for, and Gone one that is no longer held:
	// its hold ran out, or it was never offered.
	Paid bool
	Gone bool

	// Broken is a link that is not one of ours, and Unavailable a payment
	// page that could not be made just now.
	Broken      bool
	Unavailable bool
}

// payPage shows an offered place: what it comes to, and until when it is held.
//
// It starts nothing. A GET on a link in an email is made by every scanner a
// mail provider runs, and one that made a Stripe session would make them by
// the dozen for nobody.
func (a app) payPage(w http.ResponseWriter, r *http.Request) {
	f, ok := a.lookup(w, r)
	if !ok {
		return
	}

	token := r.URL.Query().Get(payField)

	sub, ok := a.offered(w, r, f, token)
	if !ok {
		return
	}

	a.renderPay(w, r, http.StatusOK, f, token, sub, "")
}

// pay holds the place for the length of a payment page and makes one.
//
// The page is a link the person clicks rather than a redirect, for the reason
// the confirmation page gives: a real click is what gets a top-level
// navigation past every popup blocker, and a cross-origin redirect from a
// form POST is one a form-action policy may refuse.
func (a app) pay(w http.ResponseWriter, r *http.Request) {
	f, ok := a.lookup(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	if err := r.ParseForm(); err != nil {
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "pay", payView{Form: f, Broken: true})

		return
	}

	token := r.PostFormValue(payField)

	id, ok := a.payLink(w, r, f, token)
	if !ok {
		return
	}

	now := a.now()

	// Held until the payment page closes and the sweep's margin after it, so
	// that a page opened in the last minutes of a hold cannot outlive it.
	sub, err := a.cfg.Submissions.Hold(r.Context(), now, f.ID, id, now.Add(paybus.AbandonAfter))

	switch {
	case err == nil:

	case errors.Is(err, submissionbus.ErrOfferLapsed):
		a.renderPay(w, r, http.StatusOK, f, token, sub, "")

		return

	case errors.Is(err, submissionbus.ErrNotFound):
		a.cfg.Render.Render(w, r, http.StatusNotFound, "pay", payView{Form: f, Broken: true})

		return

	default:
		a.cfg.Log.Error("an offered place could not be held for payment",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "submission_id", id.String(), "error", err)
		a.oops(w, r)

		return
	}

	order := paybus.OrderFor(f, sub)

	// Each press of the button is its own payment page, so that somebody
	// who closed the first can be given a second while the place is held.
	order.Attempt = strconv.FormatInt(now.UnixMilli(), 10)

	if a.cfg.Payments == nil {
		a.renderPay(w, r, http.StatusOK, f, token, sub, "unavailable")

		return
	}

	h, err := a.cfg.Payments.Start(r.Context(), order)
	if err != nil {
		a.cfg.Log.Error("a payment could not be started for an offered place",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "submission_id", sub.ID.String(), "error", err)
		a.renderPay(w, r, http.StatusOK, f, token, sub, "unavailable")

		return
	}

	a.renderPay(w, r, http.StatusOK, f, token, sub, h.URL)
}

// offered reads the submission a payment link names, answering the request
// itself when the link is not one of ours.
func (a app) offered(w http.ResponseWriter, r *http.Request, f formbus.Form, token string) (submissionbus.Submission, bool) {
	id, ok := a.payLink(w, r, f, token)
	if !ok {
		return submissionbus.Submission{}, false
	}

	sub, err := a.cfg.Submissions.ForPayment(r.Context(), f.ID, id)

	switch {
	case errors.Is(err, submissionbus.ErrNotFound):
		a.cfg.Render.Render(w, r, http.StatusNotFound, "pay", payView{Form: f, Broken: true})

		return submissionbus.Submission{}, false

	case err != nil:
		a.cfg.Log.Error("an offered place could not be read",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "submission_id", id.String(), "error", err)
		a.oops(w, r)

		return submissionbus.Submission{}, false
	}

	return sub, true
}

// payLink checks a presented payment link against the form in the address.
// Every way it can fail is the same page, for the reason answerLink gives.
func (a app) payLink(w http.ResponseWriter, r *http.Request, f formbus.Form, token string) (types.ID, bool) {
	form, id, err := submissionbus.ReadPayLink(a.cfg.AnswerKey, token)

	switch {
	case errors.Is(err, submissionbus.ErrBadPayLink), err == nil && form != f.ID:
		a.cfg.Log.Info("a payment link was refused",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String())
		a.cfg.Render.Render(w, r, http.StatusNotFound, "pay", payView{Form: f, Broken: true})

		return types.ID{}, false

	case err != nil:
		a.cfg.Log.Error("a payment link could not be checked",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
		a.cfg.Render.Render(w, r, http.StatusNotFound, "pay", payView{Form: f, Broken: true})

		return types.ID{}, false
	}

	return id, true
}

// renderPay draws the page for where the place now stands. payURL is Stripe's
// page once one has been made, "unavailable" when making one failed, or empty.
func (a app) renderPay(w http.ResponseWriter, r *http.Request, status int, f formbus.Form, token string, sub submissionbus.Submission, payURL string) {
	view := payView{Form: f, Token: token}

	switch {
	case sub.Status == submissionbus.StatusPaid:
		view.Paid = true

	case sub.Status == submissionbus.StatusOffered && a.now().Before(sub.OfferExpiresAt):
		view.Held = true
		view.Until = untilWords(sub.OfferExpiresAt)
		view.Lines = viewLines(paybus.OrderFor(f, sub), f.Currency)
		view.Total = totalOf(sub.Answers, f.Currency)

		switch payURL {
		case "":
		case "unavailable":
			view.Unavailable = true
		default:
			view.PayURL = payURL
		}

	default:
		view.Gone = true
	}

	a.cfg.Render.Render(w, r, status, "pay", view)
}

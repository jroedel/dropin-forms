// Package embedapp is the public form: the page a stranger fills in, on
// somebody else's website.
//
// # Everything here arrives with no credential, and that is permanent
//
// This surface is framed cross-site, which makes every cookie either useless
// or a cookie whose only purpose is to be sent in a third-party frame.
// SameSite is evaluated against the site being visited, so a Lax cookie is
// withheld from everything the frame requests -- same-origin POSTs included.
// So there is no session here, no per-browser anchor of any kind, and no CSRF
// to prevent: an attacker cannot make a browser send a credential it does not
// have.
//
// What guards each route, then, in the order the request meets it:
//
//	GET /f/{slug}          nothing. It is a blank form, and it has to be
//	                       readable by anybody, from anywhere, for the service
//	                       to work at all. It mints a submission grant.
//	POST /f/{slug}         web.SameOriginOnly, which refuses a cross-site
//	                       write from a browser but by its own admission lets
//	                       a header-less client through; then the submission
//	                       grant, which pins the form and version and is
//	                       single-use; then the definition's own rules.
//	POST /f/{slug}/edit    the same-origin gate, and nothing else, because it
//	                       stores nothing: it renders the form again with what
//	                       was typed still in it. That is the Back button on
//	                       the confirmation page.
//	GET /f/{slug}/return   nothing, and it must stay that way: this is where
//	                       a browser lands after Stripe, and the one thing it
//	                       carries is a word saying what happened. It decides
//	                       wording and touches no data at all.
//	GET /embed.js          nothing. It is a static file the whole point of
//	                       which is to be loaded by other people's pages.
//
// Read docs/design/drop-in-forms.md section 5.3 before adding a gate here.
// The one thing not to do is mount the parent project's RequireFormToken: it
// passes through when there is no principal, which is correct there and would
// admit every POST unconditionally here, while a chain diagram still listed a
// gate.
//
// # Why the same-origin check does not refuse Stripe, and will not refuse it
//
// Nothing from Stripe arrives on this surface. The webhook is a third
// listener with its own chain, because it authenticates by a signature over
// the raw request body and must not sit behind anything that calls ParseForm.
// Mentioned here because this is where somebody will come looking.
package embedapp

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formtoml"
	"github.com/jroedel/dropin-forms/business/domain/payment/paybus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

//go:embed templates
var templates embed.FS

// Templates is this app's own template directory, handed to page.NewRenderer
// by whatever builds the renderer.
var Templates = templates

//go:embed embed.js
var embedJS []byte

// maxBody is the most a submission may be.
//
// A form intake endpoint is an upload endpoint whether or not it was meant to
// be. 64KB is far more than any definition here can produce -- the longest
// field is 500 characters -- and small enough that a body which reaches the
// limit is not a submission.
const maxBody = 64 << 10

// Forms is where definitions come from. An interface rather than the store,
// because this app reads one form by name and does nothing else with it, and
// because a test wants to supply two forms without writing TOML.
type Forms interface {
	ByID(slug types.Slug) (formbus.Form, error)
}

// Submissions is the slice of the submission domain this app needs.
type Submissions interface {
	Accept(ctx context.Context, now time.Time, g formbus.Grant, ns submissionbus.New) (submissionbus.Submission, error)
}

// Payments is the slice of the payment domain this app needs: it starts a
// payment and can never confirm one.
//
// That narrowness is the point. Confirming a payment is the webhook's job and
// nothing else's -- a browser arriving back from Stripe is a browser saying
// something happened, and this surface is the one strangers talk to. A handler
// here that could mark a submission paid would be a route that marks
// submissions paid.
type Payments interface {
	Start(ctx context.Context, o paybus.Order) (paybus.Handoff, error)
}

// Config is what this app needs.
type Config struct {
	Log         *slog.Logger
	Forms       Forms
	Submissions Submissions
	Render      *page.Renderer

	// Payments is optional, and a nil one is a working service rather than a
	// broken one: a form that sells something still validates, still stores
	// the submission, and still says thank you -- it simply shows no way to
	// pay, and the office sees a pending row. That is what this service did
	// before the payment step existed and is the right thing to degrade to,
	// because the alternative is that a missing Stripe key turns every form
	// into an error page.
	Payments Payments

	// GrantKey signs the submission grants this app mints and redeems.
	GrantKey formbus.GrantKey

	// Now is the clock, injectable so that the open and close windows can be
	// tested without waiting for a date to pass -- and so that the tests
	// against the real feast definition do not start failing the day after
	// the feast. Nil means time.Now.
	//
	// formbus.Form.Validate already takes its own now for the same reason;
	// this is the other half of it.
	Now func() time.Time

	// TrustProxy says whether to believe X-Forwarded-For.
	//
	// It has to be configured rather than sniffed. This process listens on the
	// loopback behind Apache, so the socket's own address is always
	// 127.0.0.1 and the visitor's address is only in a header -- and a header
	// is exactly as trustworthy as whatever is in front of it. Believing one
	// with nothing in front means letting a stranger choose the address that
	// goes to Stripe's fraud checks, which is worse than having no address at
	// all.
	TrustProxy bool
}

type app struct {
	cfg Config
}

// now is the request's own time, read once per handler so that a submission
// validated as open cannot be stored under a later second that has closed.
func (a app) now() time.Time {
	if a.cfg.Now != nil {
		return a.cfg.Now()
	}

	return time.Now()
}

// Routes mounts this app.
//
// writes is the same-origin gate, handed in by the muxer rather than built
// here, and applied to the one route that accepts a write. The chain is
// written down in one place and a route's position in it is not a decision an
// app package gets to make -- which is also what lets the muxer mount the
// Stripe webhook on this listener without that gate in front of it.
//
// A nil writes mounts the POST ungated, and that is for tests of this app
// alone. Every surface built by the muxer passes one.
func Routes(mux *http.ServeMux, cfg Config, writes func(http.Handler) http.Handler) {
	a := app{cfg: cfg}

	if writes == nil {
		writes = func(h http.Handler) http.Handler { return h }
	}

	mux.HandleFunc("GET /f/{slug}", a.blank)
	mux.Handle("POST /f/{slug}", writes(http.HandlerFunc(a.submit)))

	// Back, from the confirmation page. A POST because it carries somebody's
	// answers and a GET would put their name and address in a URL -- and
	// behind the same-origin gate with the write it resembles, even though it
	// writes nothing, because a route that reflects a posted body into a page
	// has no business accepting that body from another site.
	mux.Handle("POST /f/{slug}/edit", writes(http.HandlerFunc(a.edit)))

	// Where Stripe sends the browser back to. Strictly speaking Stripe sends
	// it to the *hosting* page and embed.js brings it here, which is the only
	// reason this can be a page inside the frame rather than a redirect.
	mux.HandleFunc("GET /f/{slug}/return", a.returned)

	// The snippet a site owner pastes names this path, and a pasted path
	// cannot be changed afterwards -- so it is a fixed name rather than a
	// content-hashed one, and it is cached for an hour instead of forever.
	mux.HandleFunc("GET /embed.js", a.script)

	if p := cfg.Render.StylesheetPath(); p != "" {
		mux.HandleFunc("GET "+p, cfg.Render.Stylesheet())
	}
	if p := cfg.Render.ScriptPath(); p != "" {
		mux.HandleFunc("GET "+p, cfg.Render.Script())
	}
}

// formView is what the form template renders.
//
// Fields and Items are precomputed rather than the definition itself, so the
// template renders values and decides nothing. The mapping is in view.go and
// the reasoning is at the top of it.
type formView struct {
	Form  formbus.Form
	Grant string

	// Action is where this form posts, which is this same page plus the
	// parent origin. It is not simply /f/{slug}, and that omission was a
	// visible bug: the POST dropped the parameter, so the confirmation it
	// rendered knew no parent, never posted its height, and the frame kept
	// whatever height the form had had. On the shrine's page that was a short
	// receipt sitting at the top of a tall empty box.
	Action string

	// ParentOrigin is the origin to post height messages to, or empty when
	// this page was not opened in a frame this form permits. The template puts
	// it on the body and the script reads it from there; it is never '*'.
	ParentOrigin string

	Fields []fieldView
	Items  []itemView

	// MinPerOrder and MaxPerOrder are the order's own bounds, as a sentence
	// the template shows above the quantity boxes. A rule enforced on the
	// server and never stated on the page is a refusal somebody cannot
	// anticipate.
	OrderNote string

	// OrderProblems are the violations that belong to the order as a whole
	// rather than to any one field, which have nowhere else to appear.
	OrderProblems []formbus.Violation

	// Problems is why the last attempt was refused; Note is a sentence about
	// the attempt as a whole, used when there is nothing field-specific to
	// say -- a stale grant, or a body we could not read.
	Problems formbus.Invalid
	Note     string

	// Symbol is the currency's symbol, for the amount fields. From formbus,
	// which owns the table, so that a price beside a field and a price inside
	// a violation are written the same way.
	Symbol string
}

// blank renders an empty form and mints the grant it will be submitted with.
func (a app) blank(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	a.render(w, r, http.StatusOK, f, formbus.Values{}, formbus.Invalid{}, "")
}

// submit validates a submission, stores it, and says what happened.
func (a app) submit(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	if err := r.ParseForm(); err != nil {
		// A body too large, or not a form at all. Nothing worth a sentence
		// about which, and nothing to redisplay.
		a.render(w, r, http.StatusBadRequest, f, formbus.Values{}, formbus.Invalid{},
			"We could not read that. Please try again.")

		return
	}

	values := formbus.Values(r.PostForm)
	now := a.now()

	// The grant first, because it decides which definition this submission is
	// answering. Validating against the current form and then discovering the
	// grant was for an older version would mean reporting violations of rules
	// the person was never shown.
	g, err := formbus.Redeem(a.cfg.GrantKey, r.PostFormValue(grantField), f, now)
	if err != nil {
		if !errors.Is(err, formbus.ErrGrantRefused) {
			a.cfg.Log.Error("a grant could not be checked",
				"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
			a.oops(w, r)

			return
		}

		// Not their fault and not worth explaining. A fresh grant and their
		// answers back, with a sentence saying to send it again. The status is
		// 200 rather than 400: nothing about the request was malformed, the
		// page it was made from was simply stale.
		a.cfg.Log.Info("a stale grant was re-rendered",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "reason", err)

		a.render(w, r, http.StatusOK, f, values, formbus.Invalid{},
			"This page had been open a while, so we have refreshed it. Please check your answers and send it again.")

		return
	}

	answers, err := f.Validate(now, values)

	switch {
	case err == nil:

	case isClosed(err):
		// Closed between the page being opened and the form being sent, which
		// is exactly what happens at a deadline.
		a.closed(w, r, f, now)

		return

	default:
		invalid, ok := errors.AsType[formbus.Invalid](err)
		if !ok {
			a.cfg.Log.Error("a submission could not be validated",
				"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
			a.oops(w, r)

			return
		}

		// A fresh grant goes out with the re-rendered page. The old one was
		// not spent -- nothing was stored -- but reusing it would mean the
		// person's second attempt failing for a reason they cannot see if the
		// first attempt happened to be replayed in the meantime.
		a.render(w, r, http.StatusUnprocessableEntity, f, values, invalid, "")

		return
	}

	sub, err := a.cfg.Submissions.Accept(r.Context(), now, g, submissionbus.New{
		Answers:  answers,
		RemoteIP: a.remoteIP(r),
	})

	switch {
	case errors.Is(err, submissionbus.ErrReplayed):
		// The same grant twice: a double-clicked button, or a refreshed POST.
		// Nothing was stored the second time, and the first submission is
		// already safe, so this says so rather than reporting a failure.
		a.cfg.Log.Info("a replayed submission was refused",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String())

		a.cfg.Render.Render(w, r, http.StatusOK, "done", doneView{
			Form:         f,
			ParentOrigin: parentOrigin(f, r),
			Duplicate:    true,
		})

		return

	case err != nil:
		a.cfg.Log.Error("a submission could not be stored",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
		a.oops(w, r)

		return
	}

	a.cfg.Log.Info("submission received",
		"request_id", web.RequestIDFrom(r.Context()),
		"form", f.ID.String(), "submission_id", sub.ID.String(),
		"status", sub.Status, "total", sub.Answers.Total.String())

	// One derivation of the lines, used for both the receipt on this page and
	// the itemisation sent to Stripe. Built here rather than separately for
	// each, because a confirmation that disagrees with the charge is the bug
	// somebody finds by reading their bank statement.
	order := paybus.OrderFor(f, sub)

	view := doneView{
		Form:         f,
		Submission:   sub,
		ParentOrigin: parentOrigin(f, r),
		Lines:        viewLines(order, f.Currency),
		Total:        totalOf(sub.Answers, f.Currency),
	}

	if view.Owed() {
		view.Answers = hiddenAnswers(values)
		view.BackTo = withParent("/f/"+f.ID.String()+"/edit", view.ParentOrigin)
	}

	// The payment is started here, in the POST, and never while rendering the
	// blank form. A Checkout session created on a GET would mean an
	// unauthenticated crawler minting Stripe objects at crawl rate; created
	// here it happens once per validated submission, after a single-use grant
	// has been spent.
	//
	// And it happens after Accept has committed, never inside it. SQLite has
	// one writer, and a network call held inside the write transaction is the
	// mistake the parent project built a linter to prevent.
	if view.Owed() && a.cfg.Payments != nil {
		h, err := a.cfg.Payments.Start(r.Context(), order)
		if err != nil {
			// The submission is stored and safe. So this is not an error page:
			// it is the confirmation, with a sentence saying the payment could
			// not be started. Re-rendering the form instead would invite a
			// second order for something already recorded, and answering 500
			// would tell somebody their order was lost when it was not.
			a.cfg.Log.Error("a payment could not be started",
				"request_id", web.RequestIDFrom(r.Context()),
				"form", f.ID.String(), "submission_id", sub.ID.String(), "error", err)

			view.PaymentUnavailable = true
		} else {
			view.PayURL = h.URL
		}
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "done", view)
}

// edit renders the form again with what was typed still in it.
//
// This is Back on the confirmation page, and it is a re-render rather than any
// kind of undo. Two things follow from that, and both are deliberate.
//
// It stores nothing and changes nothing, so it needs no grant: what it echoes
// is the body of the request it is answering, and the page it produces carries
// a *fresh* grant like any other. The answers are trusted no further than
// being put back in the boxes they came out of -- the next submission is
// validated from scratch against the definition, so an edited hidden field
// buys exactly the same as typing in the box.
//
// And the order that was already stored stays stored, as pending. There is no
// "abandoned" status to move it to and this surface has no business inventing
// one: a stranger holding a submission's id is not proof of anything, and a
// route on the public form that could retire somebody's order is a route that
// retires orders. So going back and ordering again leaves one pending row
// nobody will pay -- which is the same footprint as clicking Continue and then
// closing the Stripe tab, and that already happens. What the office reads a
// pending row as is "not paid", which remains true.
func (a app) edit(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	if err := r.ParseForm(); err != nil {
		// Nothing readable to put back in the boxes. A blank form is still the
		// right answer: they are trying to get back to the form.
		a.render(w, r, http.StatusBadRequest, f, formbus.Values{}, formbus.Invalid{},
			"We could not read that. Here is the form again.")

		return
	}

	// No problems and no note. Coming back to change an answer is not a
	// refusal, and an alert box saying so would be an error message for
	// something that went right.
	a.render(w, r, http.StatusOK, f, formbus.Values(r.PostForm), formbus.Invalid{}, "")
}

// returned is what somebody sees when they come back from Stripe.
//
// # Why a browser telling us it paid is safe here
//
// The state in the query string is not evidence and is not treated as any. It
// is a word chosen from two, it decides which paragraph this page renders, and
// it moves no data whatsoever -- there is no submission id in the URL to move
// it for. Anybody may type ?state=paid and read a thank-you; it tells them
// nothing they did not already have to know to construct it, and it leaves the
// order exactly as pending as it was.
//
// The authority for "this is paid" is the webhook signature, which is a
// different listener and the only thing in this service that can settle an
// order. That separation is the reason this handler can be this relaxed: a
// page that only says words cannot be tricked into anything.
//
// What this page therefore cannot do is tell somebody their *own* order is
// confirmed, because it cannot look one up. The wording is written for that:
// it reports what happened at Stripe, which is what the person just did and
// what they want acknowledged, and it never claims to have checked.
func (a app) returned(w http.ResponseWriter, r *http.Request) {
	f, ok := a.lookup(w, r)
	if !ok {
		return
	}

	state := r.URL.Query().Get("state")

	if state != paybus.StatePaid && state != paybus.StateCancelled {
		// Not a word we recognise, so nothing can be said about it. Falling
		// through to the form is the useful failure: an old bookmark, or a
		// marker we stop sending one day, lands somebody on a working form
		// rather than on a page explaining a parameter to them.
		a.blank(w, r)

		return
	}

	origin := parentOrigin(f, r)

	view := returnedView{
		Form:         f,
		ParentOrigin: origin,
		Paid:         state == paybus.StatePaid,
	}

	if !view.Paid {
		view.FormURL = withParent("/f/"+f.ID.String(), origin)
	}

	a.cfg.Log.Info("a browser came back from the payment page",
		"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "state", state)

	a.cfg.Render.Render(w, r, http.StatusOK, "returned", view)
}

// returnedView is the page inside the frame after Stripe, reached because
// embed.js spotted the marker Stripe sent the hosting page back with.
type returnedView struct {
	Form         formbus.Form
	ParentOrigin string

	// Paid distinguishes the two words Stripe can send back. It is what the
	// browser said happened and not what this service has confirmed, which is
	// why the template's wording never says "we have checked".
	Paid bool

	// FormURL is where a "back to the form" link goes when nothing was paid,
	// carrying the parent origin forward so the page it lands on can still
	// post its height and not be a 120px box.
	FormURL string
}

// doneView is the in-line confirmation, rendered in place of the form inside
// the same frame. No navigation at all on a form that takes no payment, which
// is the whole point of the choice recorded in section 2a of the design.
type doneView struct {
	Form         formbus.Form
	Submission   submissionbus.Submission
	ParentOrigin string

	// Duplicate says this had already been received, so the wording is "we
	// already have this" rather than "thank you". Not an error: the first
	// submission is safe and there is nothing for anybody to do.
	Duplicate bool

	// Lines and Total are the receipt, formatted. Rendered from what was
	// stored rather than from what was posted, so it is what the server
	// derived and not what a browser claimed.
	Lines []lineView
	Total string

	// PayURL is Stripe's hosted page, when there is something to pay and a
	// session was created. Rendered as a link the person clicks rather than a
	// redirect: a real click is a user activation, which is what gets a
	// top-level navigation out of a frame past every popup blocker, and it is
	// honest that they are leaving the site.
	PayURL string

	// PaymentUnavailable says the order is stored and the payment could not be
	// started. Kept apart from every other failure on this page because it is
	// not a failure for the person: their answers are safe, the office can see
	// the order, and telling them to try again would invite a second one.
	PaymentUnavailable bool

	// Answers is what was posted, as hidden inputs behind a Back button, so
	// that somebody who has just read the total and wants three tickets
	// instead of two gets the form back with their name and address still in
	// it. Empty on a form with nothing left to pay -- there is nothing to go
	// back for once the money is in, and offering it would read as an offer to
	// undo something.
	Answers []answerView

	// BackTo is where Back posts, carrying the parent origin onward for the
	// same reason Action does.
	BackTo string
}

// answerView is one posted name and value, on its way back into a hidden
// input. A pair rather than a map because a checkbox group posts one name
// several times, and because a template that ranges over a map renders in a
// different order every time.
type answerView struct {
	Name  string
	Value string
}

// lineView is one priced line of the receipt.
type lineView struct {
	Label  string
	Qty    int
	Amount string
}

// Owed reports whether there is still money to collect, which is what the
// payment step turns into a button.
func (v doneView) Owed() bool {
	return v.Submission.Status == submissionbus.StatusPending
}

// closedView is what a form not taking submissions shows.
type closedView struct {
	Form         formbus.Form
	ParentOrigin string
	Note         string

	// NotYetOpen distinguishes too early from too late, which want different
	// sentences: one of them is worth coming back for.
	NotYetOpen bool

	// OpensWhen is the opening instant as a date somebody can read, or empty.
	// Formatted here rather than in the template, because a template that
	// formats a time is a template that has to know which zone to use.
	OpensWhen string
}

// script serves the parent-side snippet.
func (a app) script(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/javascript; charset=utf-8")

	// An hour, and not immutable. The path is baked into whatever somebody
	// pasted into a Squarespace page and cannot be changed afterwards, so this
	// file has to be replaceable -- but it also must not be re-fetched on
	// every page view of the host site.
	h.Set("Cache-Control", "public, max-age=3600")

	// Overrides the surface's no-store, which is right for a form page
	// carrying a single-use grant and wrong for the one file here that is the
	// same for everybody.
	http.ServeContent(w, r, "embed.js", startup, strings.NewReader(string(embedJS)))
}

// startup is the modification time reported for the embedded script, so that
// conditional requests have something stable to compare against.
var startup = time.Now()

// form resolves the slug in the path and refuses a form not taking
// submissions, answering 404 or the closed page itself when it cannot.
func (a app) form(w http.ResponseWriter, r *http.Request) (formbus.Form, bool) {
	f, ok := a.lookup(w, r)
	if !ok {
		return formbus.Form{}, false
	}

	now := a.now()
	if !f.Open(now) {
		a.closed(w, r, f, now)

		return formbus.Form{}, false
	}

	return f, true
}

// lookup resolves the slug in the path and says nothing about whether the form
// is open, answering 404 itself when there is no such form.
//
// Separate from form because one route must not care: somebody coming back
// from Stripe has already paid, and a form closes on a date. A deadline that
// passes while they are on Stripe's page would otherwise answer "this form is
// no longer taking submissions" to the one person on the site who has just
// been charged -- which reads as "your money went somewhere and we have no
// idea what you are talking about". The close date governs taking new orders,
// and nothing else.
func (a app) lookup(w http.ResponseWriter, r *http.Request) (formbus.Form, bool) {
	slug, err := types.ParseSlug(r.PathValue("slug"))
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
		a.cfg.Log.Error("a form could not be read",
			"request_id", web.RequestIDFrom(r.Context()), "form", slug.String(), "error", err)
		a.oops(w, r)

		return formbus.Form{}, false
	}

	return f, true
}

// render writes the form page, minting a grant for it.
func (a app) render(
	w http.ResponseWriter, r *http.Request, status int,
	f formbus.Form, values formbus.Values, problems formbus.Invalid, note string,
) {
	grant, err := formbus.Mint(a.cfg.GrantKey, f, a.now())
	if err != nil {
		a.cfg.Log.Error("a grant could not be minted",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
		a.oops(w, r)

		return
	}

	origin := parentOrigin(f, r)

	a.cfg.Render.Render(w, r, status, "form", formView{
		Form:          f,
		Grant:         grant,
		Action:        withParent("/f/"+f.ID.String(), origin),
		ParentOrigin:  origin,
		Symbol:        formbus.Symbol(f.Currency),
		Fields:        viewFields(f, values, problems),
		Items:         viewItems(f, values, problems),
		OrderNote:     orderNote(f),
		OrderProblems: problems.For(""),
		Problems:      problems,
		Note:          note,
	})
}

func (a app) closed(w http.ResponseWriter, r *http.Request, f formbus.Form, now time.Time) {
	early := !f.OpensAt.IsZero() && now.Before(f.OpensAt)

	view := closedView{
		Form:         f,
		ParentOrigin: parentOrigin(f, r),
		Note:         f.ClosedNote,
		NotYetOpen:   early,
	}

	if early {
		// In whatever offset the author wrote the instant in, so a form
		// opening at nine in Austin says nine rather than whatever that is in
		// UTC.
		view.OpensWhen = f.OpensAt.Format("Monday 2 January, 3:04pm (MST)")
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "closed", view)
}

func (a app) notFound(w http.ResponseWriter, r *http.Request) {
	// No parent origin, and none is possible: with no form there is no list of
	// permitted embedders to check one against. So this page never posts its
	// height, and a mistyped slug in a snippet is a short box with an
	// explanation in it.
	a.cfg.Render.Render(w, r, http.StatusNotFound, "missing", struct{}{})
}

// hiddenAnswers is what was posted, ready to be put back in the form by Back.
//
// From the posted values rather than from the stored submission on purpose:
// this has to fill in the same boxes the person typed into, and what is stored
// has been through validation and derivation -- normalised, priced, with the
// quantity fields renamed. Round-tripping *that* would hand somebody a form
// subtly different from the one they filled in.
//
// The grant is dropped rather than carried: it has been spent, and the page
// this feeds mints a new one.
func hiddenAnswers(values formbus.Values) []answerView {
	names := make([]string, 0, len(values))

	for name := range values {
		if name == grantField {
			continue
		}

		names = append(names, name)
	}

	// Sorted so the page is the same page twice, which is what makes it
	// testable and what stops a diff of two renders being noise.
	slices.Sort(names)

	out := make([]answerView, 0, len(names))

	for _, name := range names {
		for _, v := range values[name] {
			out = append(out, answerView{Name: name, Value: v})
		}
	}

	return out
}

// viewLines formats the receipt from what was stored.
// viewLines formats the receipt from the order that will be charged.
//
// From the order rather than from formbus.Answers.Lines, which was a quiet bug
// worth naming: Answers.Lines is the priced *items* only, and a donation is an
// amount field that the validator adds into the total separately. A receipt
// built from Answers.Lines therefore showed two lunch tickets and a total five
// dollars larger than they came to, with nothing on the page accounting for
// the difference. paybus.OrderFor walks both, and it is the same list Stripe
// is given.
func viewLines(o paybus.Order, currency string) []lineView {
	out := make([]lineView, 0, len(o.Lines))

	for _, l := range o.Lines {
		out = append(out, lineView{
			Label:  l.Label,
			Qty:    l.Qty,
			Amount: formbus.Show(l.Amount(), currency),
		})
	}

	return out
}

// totalOf formats the total, or returns empty when there is nothing to show,
// which is what keeps a receipt off a form that sells nothing.
func totalOf(a formbus.Answers, currency string) string {
	if a.Total == 0 {
		return ""
	}

	return formbus.Show(a.Total, currency)
}

func (a app) oops(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)
}

// grantField is the hidden input the grant travels in.
const grantField = "_grant"

// GrantField is that name, exported so a test can fill the field in without
// parsing HTML for it.
const GrantField = grantField

// withParent adds the parent origin to one of this app's own addresses, so
// that whatever the browser reaches next can still tell the frame how tall it
// is.
//
// Every page here learns its parent from this parameter and from nothing else
// -- there is no referrer to read, because the surface sets
// Referrer-Policy: no-referrer, and no cookie to keep it in, because the
// surface is cookie-free by construction. So a link or an action that drops it
// is a page that goes silent, and the symptom is not an error: it is a frame
// stuck at the wrong height with the right thing inside it.
//
// The origin has already been checked against the form's own list by
// parentOrigin and is re-escaped here as a query value on the way back out.
func withParent(path, origin string) string {
	if origin == "" {
		return path
	}

	return path + "?parent=" + url.QueryEscape(origin)
}

// parentOrigin decides what the page may post its height to.
//
// The parameter is attacker-supplied -- anybody can open /f/x?parent=anything
// -- so it is checked against this form's own list of permitted embedders and
// re-serialised from the parsed origin rather than echoed. Without that check
// the target would be whatever the caller asked for, which is postMessage to
// '*' with extra steps.
func parentOrigin(f formbus.Form, r *http.Request) string {
	want := r.URL.Query().Get("parent")
	if want == "" {
		return ""
	}

	asked, err := types.ParseOrigin(want)
	if err != nil {
		return ""
	}

	for _, allowed := range f.Origins {
		if allowed == asked {
			// Rendered from the parsed value, so nothing of what was typed
			// reaches the page even when it matches.
			return allowed.String()
		}
	}

	return ""
}

// remoteIP is the visitor's address, for Stripe's fraud checks.
func (a app) remoteIP(r *http.Request) string {
	if a.cfg.TrustProxy {
		// The leftmost entry is the original client, and every entry after it
		// was added by a proxy. Only meaningful because TrustProxy says
		// something is in front of us that overwrites rather than appends --
		// which is why this is configuration and not detection.
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			first, _, _ := strings.Cut(fwd, ",")

			return strings.TrimSpace(first)
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}

// orderNote states the order's own bounds, which are otherwise rules a person
// only discovers by breaking them.
func orderNote(f formbus.Form) string {
	switch {
	case f.MinPerOrder > 0 && f.MaxPerOrder > 0:
		return fmt.Sprintf("Choose between %d and %d in total.", f.MinPerOrder, f.MaxPerOrder)
	case f.MinPerOrder > 0:
		return fmt.Sprintf("Choose at least %d.", f.MinPerOrder)
	case f.MaxPerOrder > 0:
		return fmt.Sprintf("Up to %d in total.", f.MaxPerOrder)
	}

	return ""
}

// isClosed reports whether validation refused the submission because the form
// is not taking any.
func isClosed(err error) bool {
	_, ok := errors.AsType[formbus.Closed](err)

	return ok
}

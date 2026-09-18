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
	"github.com/jroedel/dropin-forms/foundation/alarm"
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

// Notify tells the submitter and the office about a submission that is
// finished.
//
// Finished, and that word is doing the work: this is called for a submission
// with nothing to pay and never for one on its way to Stripe. Somebody who has
// just been handed a payment page has not finished, and the message about
// their order belongs to the webhook that confirms it.
//
// No error, because there is nothing this app could do with one. The
// submission is stored and the page has been rendered; a relay that is down is
// a line in the log, not a different answer to the person who filled the form
// in.
type Notify interface {
	Received(ctx context.Context, id types.ID)
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

	// Notify is optional, and a nil one is a working service: submissions are
	// stored and the office reads them in the management app, which is what
	// this service did before there was any mail at all.
	Notify Notify

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
	//
	// It decides the rate-limit key as well as the address sent to Stripe, and
	// the two failures are the same shape: trust a forgeable header and every
	// bucket is one the sender chose.
	TrustProxy bool

	// Limits are the throttles in front of these routes. The zero value is
	// [DefaultLimits], because a public form with no limit at all is not a
	// configuration anybody should be able to reach by forgetting a field.
	Limits Limits
}

// Limits is how often one visitor may do each of the things on this surface.
//
// Every number here is a judgement about a parish website, and they are
// written down in one place so that the judgement can be re-made by somebody
// who has watched the real traffic rather than guessed at it. See
// docs/design/drop-in-forms.md section 7.6 for what they are defending
// against, which is card testing rather than load.
type Limits struct {
	// Submit is the POST that stores a submission and can create a Stripe
	// object. Keyed by address *and* form, so that somebody filling in two
	// different forms is not held to one allowance, and so that one address
	// cannot spend another's.
	//
	// Deliberately not keyed by form alone. A bucket shared by every visitor
	// to one form is a bucket a stranger can empty, and the person it refuses
	// is the next real buyer -- which is why the per-form control here is an
	// hourly ceiling that alerts and does not refuse.
	Submit web.Rate

	// Read is the pages: a blank form, the page somebody lands on after
	// Stripe, and the Back button that re-renders what was typed. Looser,
	// because none of them stores anything or spends anything, and because a
	// frame in a page being edited is reloaded a lot.
	Read web.Rate

	// PerFormHourly is how many submissions one form may take in an hour
	// before somebody is told. It refuses nothing: a form selling out in an
	// afternoon is the outcome this service exists for, and a limiter that
	// stopped it would be the most expensive bug available here. Zero takes
	// the default; a negative number is how the alarm is switched off.
	PerFormHourly int
}

// DefaultLimits is what this surface uses when a Config says nothing.
//
// The submit allowance is ten at once and then one every six seconds. Ten is
// far more than a person filling in a form needs -- three or four corrections
// is a bad day -- and one every six seconds is ten an hour short of nothing
// for a machine trying cards. The read allowance is a page a second with a
// minute's worth in hand, which is a frame being reloaded rather than a
// visitor.
//
// The hourly ceiling is two hundred submissions on one form. The lunch it was
// written for seats a fraction of that, so anything reaching it is either
// extraordinary news or an attack, and both are worth a line in the log.
func DefaultLimits() Limits {
	return Limits{
		Submit:        web.Rate{Burst: 10, Every: 6 * time.Second},
		Read:          web.Rate{Burst: 60, Every: time.Second},
		PerFormHourly: 200,
	}
}

// withDefaults fills in whatever a caller left unset, one field at a time.
//
// Field by field rather than all-or-nothing, so that a test tightening the
// submit rate does not silently switch off the read rate beside it.
func (l Limits) withDefaults() Limits {
	d := DefaultLimits()

	if l.Submit.Zero() {
		l.Submit = d.Submit
	}
	if l.Read.Zero() {
		l.Read = d.Read
	}
	if l.PerFormHourly == 0 {
		l.PerFormHourly = d.PerFormHourly
	}

	return l
}

type app struct {
	cfg Config

	// busy is the per-form hourly ceiling. It lives on the app rather than in
	// a package variable because two surfaces in one test process must not
	// share a count, and because a counter that outlives its configuration is
	// a counter nobody can reason about.
	busy *alarm.Ceiling
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
	cfg.Limits = cfg.Limits.withDefaults()

	a := app{
		cfg:  cfg,
		busy: alarm.NewCeiling(cfg.Limits.PerFormHourly, time.Hour),
	}

	if writes == nil {
		writes = func(h http.Handler) http.Handler { return h }
	}

	// The throttles are built here rather than handed in by the muxer,
	// unlike the origin gate above, and the difference is what each one
	// knows. The origin gate is a chain decision that has to be withheld from
	// the webhook on the same listener, so it belongs where the chains are
	// written down. A throttle key is route knowledge -- which wildcard names
	// the form, which routes store something -- and that is here.
	submits := web.Throttle(web.Throttling{
		Rate: cfg.Limits.Submit,
		Key:  a.submitKey,
		Log:  cfg.Log,
	})

	reads := web.Throttle(web.Throttling{
		Rate: cfg.Limits.Read,
		Key:  a.readKey,
		Log:  cfg.Log,
	})

	mux.Handle("GET /f/{slug}", reads(http.HandlerFunc(a.blank)))
	mux.Handle("POST /f/{slug}", submits(writes(http.HandlerFunc(a.submit))))

	// Back, from the confirmation page. A POST because it carries somebody's
	// answers and a GET would put their name and address in a URL -- and
	// behind the same-origin gate with the write it resembles, even though it
	// writes nothing, because a route that reflects a posted body into a page
	// has no business accepting that body from another site.
	//
	// On the read throttle rather than the submit one: it stores nothing,
	// spends nothing and reaches nobody's API, so holding it to the allowance
	// that exists to make card testing expensive would only punish somebody
	// changing their mind twice.
	mux.Handle("POST /f/{slug}/edit", reads(writes(http.HandlerFunc(a.edit))))

	// Where Stripe sends the browser back to. Strictly speaking Stripe sends
	// it to the *hosting* page and embed.js brings it here, which is the only
	// reason this can be a page inside the frame rather than a redirect.
	mux.Handle("GET /f/{slug}/return", reads(http.HandlerFunc(a.returned)))

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

		// The definition's own ceiling, which is counted in storage because
		// that is where the day's submissions are. Zero on every form that
		// does not set one, including this one's, and the reasoning for
		// leaving it unset is in the form file.
		DailyCap: f.DailyCap,
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

	case errors.Is(err, submissionbus.ErrDailyCap):
		// The form's own cap, reached. Their answers go back in the boxes with
		// a sentence saying to come back rather than to try again, because
		// trying again is precisely what will not work -- and the 429 is the
		// honest status for "not you, and not now".
		//
		// Nothing is logged here: submissionbus wrote the line, with the count
		// and the cap in it, which is what somebody deciding whether to raise
		// the number actually needs.
		a.render(w, r, http.StatusTooManyRequests, f, values, formbus.Invalid{},
			"We have taken as many of these as we can today. Please try again tomorrow, or write to us and we will sort it out.")

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

	// The hourly ceiling, counted after the submission is safely stored and
	// refusing nothing. A form selling out in an afternoon is the outcome this
	// service exists for; what this says is that somebody should look, and it
	// says it once per hour however far past the line the count goes.
	//
	// Error rather than Warn, for the same reason a dispute is: the levels
	// here are read as "something is watching this one", and a form taking
	// submissions at a rate nobody planned for is either extraordinary news or
	// an attack in progress.
	if crossed, count := a.busy.Count(f.ID.String(), now); crossed {
		a.cfg.Log.Error("a form is taking submissions far faster than expected; check whether they are real",
			"form", f.ID.String(), "in_the_last_hour", count, "expected_at_most", a.cfg.Limits.PerFormHourly)
	}

	// Told about now, before the page is rendered, and only for a submission
	// with nothing left to pay.
	//
	// Before rather than after, because the response is the end of this
	// handler: work started after the last Render is work racing the request's
	// own end. It costs this person the wait -- bounded in notifybus, which
	// detaches from this request's context so that closing the tab cannot
	// cancel the message telling the office what they ordered.
	if sub.Status == submissionbus.StatusReceived && a.cfg.Notify != nil {
		a.cfg.Notify.Received(r.Context(), sub.ID)
	}

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
//
// One line, and it is worth keeping as a method: the address is read on two
// paths that must not disagree -- the one that goes to Stripe and the one that
// becomes a rate-limit key -- and a second call to web.ClientIP with the flag
// spelled differently is exactly the kind of drift that would leave the limit
// keyed on our own proxy.
func (a app) remoteIP(r *http.Request) string {
	return web.ClientIP(r, a.cfg.TrustProxy)
}

// submitKey is the bucket a submission is counted in: this visitor, on this
// form.
//
// The form is part of the key rather than a bucket of its own, for the reason
// [Limits.Submit] gives. The slug is read from the path value rather than
// parsed as a types.Slug, because this runs before the handler has decided
// whether the form exists and an unknown slug still has to be counted --
// otherwise the cheapest way past the limit would be to ask for forms that are
// not there.
func (a app) submitKey(r *http.Request) string {
	return "submit|" + web.IPBucket(a.remoteIP(r)) + "|" + r.PathValue("slug")
}

// readKey is the bucket a page view is counted in: this visitor, across every
// form.
//
// Not per form, unlike the submit key. Reading is cheap and the limit exists
// to bound a crawler rather than to protect one form, so one allowance per
// visitor is both simpler and the tighter of the two.
func (a app) readKey(r *http.Request) string {
	return "read|" + web.IPBucket(a.remoteIP(r))
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

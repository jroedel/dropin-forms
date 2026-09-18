// Package muxer assembles the request chains and mounts the routes, once.
//
// Adding a route should be one entry in a list, and the thing most easily got
// wrong about a new route is what it sits behind -- so the chains are written
// down here, in one place, rather than re-derived at each mount point.
//
// # Three arrival shapes, not one
//
// The parent project had a single chain. This service has three, because it
// has three kinds of caller and they have nothing in common:
//
//	embed    a stranger on somebody else's website. No cookie, ever.
//	admin    the shrine, signed in, holding a __Host- session cookie.
//	webhook  Stripe, authenticated by a signature over the raw body.
//
// They are separated by *listener* rather than by path prefix. Apache's
// ProxyPreserveHost cannot be set in .htaccess, which is all konsoleH offers,
// so a proxied request arrives with Host: 127.0.0.1:<port> whatever the visitor
// typed -- Host is unusable for this, and a header set by the front end would
// be forgeable if the proxy were ever bypassed. Which socket accepted the
// connection is not. See docs/design/drop-in-forms.md section 3.1.
//
// # The chain, and why in that order
//
//	RequestID              mints the id every line of this request will carry
//	Logging                writes the one "request" line, whatever answers
//	Panics                 recovers, logs it, answers 500
//	SecureHeaders          the per-surface header policy
//	  SameOriginOnly       writes only: refuses a cross-site write
//	  FormEncodedOnly      writes only: refuses a body that is not a form
//	    the app
//
// The three above SecureHeaders are not gates and refuse nothing. They are
// there so that whatever the gates below decide is logged, and so that a panic
// beneath them is one Error line and a 500 rather than net/http's unstructured
// "panic serving" past a request line that was never written. Logging is
// outside Panics so the request line records the 500; both are inside
// RequestID so both lines carry the id.
//
// # Two things about the webhook, kept here because this is where a route's
// position in a chain is decided
//
// The grant check must not be the parent project's RequireFormToken. That
// middleware passes through when there is no principal, which is correct there
// and would admit every POST on a permanently cookie-free surface while this
// comment still listed a gate. What guards the embed POST is the submission
// grant, redeemed inside the handler.
//
// And the Stripe webhook must not sit behind anything that reads the body.
// RequireFormToken calls r.ParseForm, which would consume the bytes the
// signature is computed over -- so the webhook has a listener of its own, with
// no origin gate and nothing above it that touches a body. See
// app/domain/paymentapp.
//
// # Where the throttles are, and why they are not here
//
// The rate limits of docs/design/drop-in-forms.md section 7.6 are mounted by
// the apps, beside the routes they hold back: embedapp.Limits and
// authapp.DefaultSignInRate. That is not an exception to the rule above but
// the other half of it. What belongs here is which gate a route sits behind,
// because that is a decision about the chain and about what must be withheld
// from the webhook. A throttle's key is route knowledge -- which wildcard
// names the form, which of two POSTs stores something -- and a copy of it here
// would be a second place that has to be edited when a route is renamed.
package muxer

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jroedel/dropin-forms/app/domain/authapp"
	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/app/domain/notifyapp"
	"github.com/jroedel/dropin-forms/app/domain/paymentapp"
	"github.com/jroedel/dropin-forms/app/domain/submissionapp"
	"github.com/jroedel/dropin-forms/app/sdk/health"
	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/notify/notifybus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/foundation/mail"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// Config is everything the routes need, built once in main.
type Config struct {
	Log      *slog.Logger
	DB       *sql.DB
	Expected sqldb.Expected

	// FrameAncestors answers which origins may frame a given form's page. It
	// is a function because the answer is per form, and main builds one from
	// the form store with page.FormFrameAncestors.
	FrameAncestors page.FrameAncestorsFor

	// Embed is what the public form surface needs. Its own config struct
	// rather than more fields here, because the two surfaces share almost
	// nothing and a flat Config made it easy to hand the embed surface a
	// dependency only the admin surface should have.
	Embed embedapp.Config

	// Users, Access, Mail and Render are what the admin surface needs and the
	// embed surface must not have. Embed ignores them entirely, which is the
	// clearest statement available that the public surface has no session,
	// sends no mail and renders no admin chrome.
	Users  *userbus.Business
	Access *accessbus.Business
	Mail   mail.Sender
	Render *page.Renderer

	// Submissions is what the admin surface reads to show what was submitted.
	// Read-only from here: the app that lists them cannot change one.
	Submissions submissionapp.Submissions

	// Forms is the definition store, needed on both surfaces -- the embed
	// surface renders them and the admin surface names their fields as
	// columns.
	Forms submissionapp.Forms

	// Payments is what the webhook route calls. Embed.Payments is a different
	// and much narrower slice of the same domain: one confirms a payment and
	// the other can only start one, and nothing should be able to do both by
	// accident. They share a listener and not an interface.
	Payments paymentapp.Payments

	// Notify tells people about a submission. One value reaching two surfaces,
	// and each sees only what it is allowed to say: the embed surface can
	// report a submission that needed no payment, and the webhook can report
	// one that Stripe has confirmed. Neither can do the other's.
	//
	// The concrete type rather than an interface, so that a missing
	// notifier is a nil pointer here and not an interface holding one --
	// which is the Go trap main already works around for the payment domain,
	// and which would turn "mail is not configured" into a panic on the first
	// submission.
	Notify *notifybus.Business

	// TrustProxy says whether X-Forwarded-For carries the visitor's address,
	// for both surfaces.
	//
	// One field rather than one per surface, because it is a fact about the
	// deployment -- Apache is in front of both listeners or it is in front of
	// neither -- and two copies of one fact is how the two come to disagree.
	// Embed builds it into the embed app's own config, which keeps that app
	// testable on its own.
	TrustProxy bool

	// AdminBaseURL is the admin surface's own origin, used to build the link
	// that goes in a sign-in email. Configured rather than taken from the
	// request: a link built from a Host header is one an attacker can aim at
	// their own host by sending a single request, and the person who receives
	// the mail cannot tell.
	AdminBaseURL string

	// Bootstrap is the one-time sign-in secret. Empty leaves those routes
	// unmounted.
	Bootstrap string
}

// Embed builds the public, embeddable surface.
//
// Like Admin, it returns an error rather than a handler alone: this surface
// cannot be built without a form store, a place to put submissions, a renderer
// and a grant key, and a Config missing any of them used to be a nil
// dereference on the first request -- a segfault in place of a sentence. A
// surface that cannot be built should say so while the process is starting.
func Embed(cfg Config) (http.Handler, error) {
	switch {
	case cfg.Embed.Forms == nil:
		return nil, errors.New("the embed surface needs somewhere to read form definitions from")
	case cfg.Embed.Submissions == nil:
		return nil, errors.New("the embed surface needs somewhere to put submissions")
	case cfg.Embed.Render == nil:
		return nil, errors.New("the embed surface needs a renderer")
	case cfg.Embed.GrantKey.Zero():
		return nil, errors.New("the embed surface needs a grant signing key; every form page carries a grant")
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", health.Handler(cfg.Log, cfg.DB, cfg.Expected))

	// The form routes. The origin gate goes to embedapp as an argument rather
	// than onto the chain below, so that it covers the one route that accepts
	// a write and nothing else -- in particular not the webhook.
	//
	// No Authenticate and no Require, and that is the whole shape of this
	// surface rather than an omission. It is framed cross-site, so SameSite
	// withholds any cookie it might carry from every request the frame makes
	// -- there is no session to establish and nothing for a CSRF gate to
	// protect. What guards the POST is the submission grant, inside the
	// handler, and embedapp's package comment says so at length.
	//
	// In particular: do not mount the parent project's RequireFormToken here.
	// It passes through when there is no principal, which is correct there and
	// would admit every POST unconditionally here.
	// Writes only. The origin check refuses a cross-site POST from a browser
	// and by its own admission lets a header-less client through, which is why
	// it is one of several things in front of a submission rather than the
	// thing in front of it; the content-type check narrows what those bodies
	// may be to the one shape a form produces, so that nothing on this surface
	// is ever asked to parse a multipart upload.
	//
	// Both are handed to embedapp rather than put on the chain below, and the
	// reason is the same in both cases and is the whole of why this is written
	// here: the Stripe webhook is on this listener, it posts JSON, and it must
	// not be behind either of them.
	cfg.Embed.TrustProxy = cfg.TrustProxy

	if cfg.Notify != nil {
		cfg.Embed.Notify = cfg.Notify
	}

	embedapp.Routes(mux, cfg.Embed, func(next http.Handler) http.Handler {
		return web.Wrap(next, web.SameOriginOnly(), web.FormEncodedOnly())
	})

	// The webhook, on this listener and deliberately *outside* that gate.
	//
	// It shares the listener because it does not need one of its own: nothing
	// on this chain reads a request body, which is the only property the
	// webhook actually requires of what sits above it. An earlier version of
	// this comment claimed the origin gate would refuse Stripe's delivery and
	// that a separate hostname was therefore forced. That was wrong --
	// web.sameOrigin treats a request with neither Sec-Fetch-Site nor Origin
	// as a pass, which is the hole its own comment documents, so a
	// server-to-server POST would go straight through it.
	//
	// But passing a gate through a hole is not the same as not being behind
	// it. Somebody may one day decide that hole should be closed, which would
	// be a defensible change to make for the form POST -- and it would
	// silently stop every payment being confirmed. So the gate is handed to
	// embedapp for its own write route instead of being put on the chain, and
	// this route is genuinely not behind it. That survives somebody tightening
	// the gate without knowing this route exists.
	//
	// Mounted only when there is a payment domain. Without one this would be
	// a public URL that verifies nothing, which is worse than no endpoint.
	if cfg.Payments != nil {
		pc := paymentapp.Config{
			Log:      cfg.Log,
			Payments: cfg.Payments,
		}

		if cfg.Notify != nil {
			pc.Notify = cfg.Notify
		}

		paymentapp.Routes(mux, pc)
	}

	return web.Wrap(mux,
		web.RequestID(),
		web.Logging(cfg.Log),
		web.Panics(cfg.Log),
		web.SecureHeaders(page.EmbedPolicy(cfg.FrameAncestors)),

		// And nothing else. The four above refuse nothing: an id, a log line,
		// a recovered panic and a header set. None of them reads a body, which
		// is what makes it safe for the webhook to sit under them -- the
		// signature covers the exact bytes, so anything calling ParseForm here
		// would destroy the only credential that surface has.
	), nil
}

// Admin builds the management surface.
//
// It answers /healthz too, and that is on purpose: the deploy checks each
// listener separately, because a release where one of the two came up is the
// failure this service is most likely to have.
// Admin returns an error rather than a handler alone, unlike Embed.
//
// The asymmetry is the honest signature: the embed surface can be built from
// nothing but a logger and a database, and this one cannot. It needs a
// renderer and an account domain, and a Config missing either produced a nil
// dereference on the first request -- a segfault in place of a sentence. A
// surface that cannot be built should say so while the process is still
// starting.
func Admin(cfg Config) (http.Handler, error) {
	switch {
	case cfg.Render == nil:
		return nil, errors.New("the admin surface needs a renderer")
	case cfg.Users == nil:
		return nil, errors.New("the admin surface needs the account domain")
	case cfg.Access == nil:
		return nil, errors.New("the admin surface needs the access domain, which is what decides who may read a form's submissions")
	case cfg.Submissions == nil:
		return nil, errors.New("the admin surface needs the submission domain; reading submissions is the whole reason it exists")
	case cfg.Forms == nil:
		return nil, errors.New("the admin surface needs the form definitions, which is where a submission's columns come from")
	case cfg.Mail == nil:
		return nil, errors.New("the admin surface needs somewhere to send mail, even if that is a recorder")
	case cfg.AdminBaseURL == "":
		return nil, errors.New("the admin surface needs its own base URL, which is what goes into a sign-in email")
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", health.Handler(cfg.Log, cfg.DB, cfg.Expected))

	// The stylesheet is outside every gate, and deliberately so: it is a file
	// compiled into the binary rather than anybody's data, and the sign-in
	// page needs it. Put it behind the session and the login page renders
	// unstyled. Its path carries a hash of its content, so it is also the one
	// response on this surface that may be cached.
	mux.HandleFunc("GET "+cfg.Render.StylesheetPath(), cfg.Render.Stylesheet())

	// Where a browser arriving at the bare hostname goes. Answered here
	// rather than left as a 404, because this hostname is what somebody types
	// from memory. {$} rather than / so this matches the root exactly and
	// does not become a catch-all that swallows every typo as a redirect.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		to := signInPath

		// The forms list rather than the account page. Somebody typing this
		// hostname from memory came to look at submissions; the account page
		// is for backup codes, which is a thing you do once.
		if _, ok := mid.UserFrom(r.Context()); ok {
			to = "/forms"
		}

		http.Redirect(w, r, to, http.StatusSeeOther)
	})

	guard := mid.Require(signInPath)

	authapp.Routes(mux, authapp.Config{
		Log:       cfg.Log,
		Users:     cfg.Users,
		Access:    cfg.Access,
		Mail:      cfg.Mail,
		Render:    cfg.Render,
		BaseURL:   cfg.AdminBaseURL,
		Bootstrap: cfg.Bootstrap,

		// Which is what the sign-in throttle counts. The default rate applies
		// unless a test says otherwise; authapp.DefaultSignInRate says what it
		// is and why.
		TrustProxy: cfg.TrustProxy,
	}, guard)

	// results is the second gate, and it is mounted here rather than inside
	// the app for the same reason guard is: a route's position in the chain is
	// written down in one place. "Who is this" is settled by guard before
	// "what may they do" is asked, which is the order the web skill is
	// explicit about.
	//
	// The role is results and not admin. Reading the numbers is not editing
	// the price, and whoever is counting lunches should not have to be able to
	// change what a ticket costs.
	results := mid.RequireFormRole(cfg.Log, cfg.Access, accessbus.RoleResults)

	sc := submissionapp.Config{
		Log:         cfg.Log,
		Forms:       cfg.Forms,
		Submissions: cfg.Submissions,
		Grants:      cfg.Access,
		Render:      cfg.Render,
	}

	if cfg.Notify != nil {
		sc.Notifications = cfg.Notify
	}

	submissionapp.Routes(mux, sc, guard, results)

	// The page that turns email about a form off and on, and the one route on
	// this surface that is deliberately *outside* guard.
	//
	// It has to be, because the link that leads to it is at the bottom of a
	// notification and is opened from a mailbox: behind the session gate it
	// would be a sign-in round trip to stop an email. What stands in for the
	// session is a signed token naming the account and the form, checked in
	// the handler -- see app/domain/notifyapp, which is emphatic about why
	// that check cannot be a middleware and about why its GET changes nothing.
	//
	// Mounted only when there is somewhere to record the preference. Without
	// one this would be a page whose button does nothing.
	if cfg.Notify != nil && cfg.Notify.CanUnsubscribe() {
		notifyapp.Routes(mux, notifyapp.Config{
			Log:         cfg.Log,
			Forms:       cfg.Forms,
			Preferences: cfg.Notify,
			Render:      cfg.Render,
			SignInPath:  signInPath,
		})
	}

	return web.Wrap(mux,
		web.RequestID(),
		web.Logging(cfg.Log),
		web.Panics(cfg.Log),
		web.SecureHeaders(page.AdminPolicy()),
		web.SameOriginOnly(),

		// Every write on this surface is a form this service rendered, and
		// there is no upload anywhere in the management app -- so the set of
		// content types it will parse is exactly one. On the chain here rather
		// than per route, unlike the embed surface, because there is no
		// webhook on this listener and therefore nothing to withhold it from.
		web.FormEncodedOnly(),

		// Below the header and origin gates, above every handler. It refuses
		// nobody, which is what lets the sign-in pages live in the same chain
		// as the account page they lead to.
		mid.Authenticate(cfg.Log, cfg.Users),
	), nil
}

// signInPath is where Require sends a signed-out reader, and it is the one
// route authapp and the muxer both have to agree on.
const signInPath = "/signin"

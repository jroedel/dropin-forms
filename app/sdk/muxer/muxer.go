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
//	    the app
//
// The three above SecureHeaders are not gates and refuse nothing. They are
// there so that whatever the gates below decide is logged, and so that a panic
// beneath them is one Error line and a 500 rather than net/http's unstructured
// "panic serving" past a request line that was never written. Logging is
// outside Panics so the request line records the 500; both are inside
// RequestID so both lines carry the id.
//
// # What is not here yet
//
// Authenticate, Require and RequireFormRole arrive with the management app,
// and the submission grant arrives with the embedded form. When they do, note
// two things the design document is emphatic about:
//
//   - The grant check must not be the parent project's RequireFormToken. That
//     middleware passes through when there is no principal, which is correct
//     there and would admit every POST on a permanently cookie-free surface
//     while this comment still listed a gate.
//   - The Stripe webhook must not sit behind anything that reads the body.
//     RequireFormToken calls r.ParseForm, which would consume the bytes the
//     signature is computed over.
package muxer

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jroedel/dropin-forms/app/domain/authapp"
	"github.com/jroedel/dropin-forms/app/sdk/health"
	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/app/sdk/page"
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
	// is a function because the answer is per form; until the form domain
	// exists, main supplies one backed by the configured list.
	FrameAncestors page.FrameAncestorsFor

	// Users, Mail and Render are what the admin surface needs and the embed
	// surface must not have. Embed ignores them entirely, which is the
	// clearest statement available that the public surface has no session,
	// sends no mail and renders no admin chrome.
	Users  *userbus.Business
	Mail   mail.Sender
	Render *page.Renderer

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
func Embed(cfg Config) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", health.Handler(cfg.Log, cfg.DB, cfg.Expected))

	return web.Wrap(mux,
		web.RequestID(),
		web.Logging(cfg.Log),
		web.Panics(cfg.Log),
		web.SecureHeaders(page.EmbedPolicy(cfg.FrameAncestors)),
		web.SameOriginOnly(),
	)
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
		if _, ok := mid.UserFrom(r.Context()); ok {
			to = "/account"
		}

		http.Redirect(w, r, to, http.StatusSeeOther)
	})

	guard := mid.Require(signInPath)

	authapp.Routes(mux, authapp.Config{
		Log:       cfg.Log,
		Users:     cfg.Users,
		Mail:      cfg.Mail,
		Render:    cfg.Render,
		BaseURL:   cfg.AdminBaseURL,
		Bootstrap: cfg.Bootstrap,
	}, guard)

	return web.Wrap(mux,
		web.RequestID(),
		web.Logging(cfg.Log),
		web.Panics(cfg.Log),
		web.SecureHeaders(page.AdminPolicy()),
		web.SameOriginOnly(),

		// Below the header and origin gates, above every handler. It refuses
		// nobody, which is what lets the sign-in pages live in the same chain
		// as the account page they lead to.
		mid.Authenticate(cfg.Log, cfg.Users),
	), nil
}

// signInPath is where Require sends a signed-out reader, and it is the one
// route authapp and the muxer both have to agree on.
const signInPath = "/signin"

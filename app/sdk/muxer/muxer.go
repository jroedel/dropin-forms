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
	"log/slog"
	"net/http"

	"github.com/jroedel/dropin-forms/app/sdk/health"
	"github.com/jroedel/dropin-forms/app/sdk/page"
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
func Admin(cfg Config) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", health.Handler(cfg.Log, cfg.DB, cfg.Expected))

	return web.Wrap(mux,
		web.RequestID(),
		web.Logging(cfg.Log),
		web.Panics(cfg.Log),
		web.SecureHeaders(page.AdminPolicy()),
		web.SameOriginOnly(),
	)
}

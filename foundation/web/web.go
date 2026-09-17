// Package web is the HTTP plumbing that has no domain knowledge: the
// middleware every request passes through, the response-header policy, and the
// origin check in front of writes.
//
// Nothing here knows what a form or a submission is. When two apps need the
// same request middleware it belongs here; when they need the same page chrome
// it belongs one layer up in app/sdk/page, because a <head> block is
// app-specific and a request log is not.
package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Middleware wraps a handler. Nothing more.
type Middleware func(http.Handler) http.Handler

// Wrap applies middleware so that the first argument is the outermost layer,
// which is the order the chain is written down in and therefore the order it
// should be read in.
func Wrap(h http.Handler, mw ...Middleware) http.Handler {
	for _, m := range slices.Backward(mw) {
		if m != nil {
			h = m(h)
		}
	}

	return h
}

// --- request id -------------------------------------------------------------

type ctxKey int

const requestIDKey ctxKey = iota + 1

// RequestIDFrom returns the id every log line for this request carries, or the
// empty string outside a request.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// RequestID mints the id the rest of the chain logs against.
//
// It is the outermost middleware so that the request line and a panic line
// carry the same id, which is the only thing that lets the two be matched up
// afterwards. The id is ours rather than a client-supplied header: a stranger
// choosing the correlation id in our logs can make two unrelated requests look
// like one.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var b [8]byte
			rand.Read(b[:]) // documented never to fail

			ctx := context.WithValue(r.Context(), requestIDKey, hex.EncodeToString(b[:]))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// --- logging ----------------------------------------------------------------

// recorder remembers what was answered, because the request line is written
// after the handler has finished and the standard ResponseWriter does not
// report either of these back.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (rec *recorder) WriteHeader(status int) {
	if rec.status == 0 {
		rec.status = status
	}
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *recorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += int64(n)

	return n, err
}

// Unwrap keeps http.ResponseController working through this wrapper, so a
// handler that needs to flush or extend a deadline still can.
func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// Logging writes exactly one line per request, whatever answers it.
//
// The query string is never logged. A form page is reached with the slug in
// the path, which is safe to keep, but a query carries exactly the kind of
// detail -- a search term, an email somebody pasted, a receipt id -- that
// should not outlive the request in a file somebody tails.
//
// It sits outside Panics so that a request answered by the panic recovery
// still gets its request line, with the 500 on it.
func Logging(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w}

			next.ServeHTTP(rec, r)

			if rec.status == 0 {
				rec.status = http.StatusOK
			}

			log.InfoContext(r.Context(), "request",
				"id", RequestIDFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"ms", time.Since(start).Milliseconds(),
				"host", r.Host,
			)
		})
	}
}

// --- panics -----------------------------------------------------------------

// Panics turns a panic into one error line and a 500.
//
// Without it net/http prints an unstructured "panic serving" past a request
// line that was never written, which is the worst of both: no correlation id
// and no status. It sits inside Logging so the request line records the 500.
//
// The sentence is deliberately not "try again". A panic in the payment path
// can happen after a card has been charged, and telling somebody to retry is
// how they get charged twice.
func Panics(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}

				log.ErrorContext(r.Context(), "panic",
					"id", RequestIDFrom(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"value", rec,
				)

				http.Error(w,
					"Something went wrong on our end. If you were in the middle of a payment, do not try again -- if you were charged we have your payment and will follow up.",
					http.StatusInternalServerError)
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// --- response headers -------------------------------------------------------

// Policy is the set of response headers a surface answers with.
//
// It is a value rather than a constant because this binary serves two surfaces
// with opposite needs: one is framed by other people's websites and admits
// Stripe, the other holds the session cookie and admits nothing. Building the
// policy is the caller's job, one layer up, because a Content-Security-Policy
// that names a form's allowed embedding origins knows a domain word and
// nothing in foundation/ may.
//
// An empty field is not set at all, which is how the embed surface omits
// X-Frame-Options -- a header that cannot express a list and would kill the
// embed in any browser that honours it.
type Policy struct {
	ContentSecurityPolicy string
	ReferrerPolicy        string
	CacheControl          string
	FrameOptions          string
	StrictTransport       string
}

// PolicyFor chooses the policy for one request.
type PolicyFor func(*http.Request) Policy

// SecureHeaders sets the response headers before the handler can write a body.
//
// Every header is Set and never Add. Two enforced Content-Security-Policy
// headers are evaluated independently -- a request need only violate one to be
// blocked -- so a second, stricter policy arriving alongside ours would block
// everything while looking like our policy was ignored.
func SecureHeaders(policy PolicyFor) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := policy(r)
			head := w.Header()

			set := func(name, value string) {
				if value != "" {
					head.Set(name, value)
				}
			}

			set("Content-Security-Policy", p.ContentSecurityPolicy)
			set("Referrer-Policy", p.ReferrerPolicy)
			set("Cache-Control", p.CacheControl)
			set("X-Frame-Options", p.FrameOptions)
			set("Strict-Transport-Security", p.StrictTransport)
			head.Set("X-Content-Type-Options", "nosniff")

			next.ServeHTTP(w, r)
		})
	}
}

// --- where a write came from ------------------------------------------------

// SameOriginOnly refuses a write that a page of this site did not make.
//
// Safe methods are not checked at all, and that is not an oversight. The
// reason this exists is that writing is the whole attack: a cross-site read of
// a blank form tells nobody anything, and the embedded form *must* be readable
// cross-site or it could not be framed in the first place.
//
// What it actually rules out, and what it does not:
//
//   - Sec-Fetch-Site is sent by every current browser and page script cannot
//     forge it, because any header whose name begins with Sec- is forbidden to
//     script. same-origin is ours. none means somebody typed the address or
//     used a bookmark. same-site and cross-site are refused.
//   - Where that header is absent -- an old browser, or curl -- Origin is
//     compared against Host instead.
//   - A request carrying neither is allowed through. That is the hole in this
//     check, and it is deliberate: it is a command-line client rather than a
//     browser being used against you, and no browser reaches it.
//
// So this is a browser-CSRF control and nothing else. On the public form POST,
// which carries no cookie at all, there is no ambient credential to forge and
// therefore no CSRF to prevent -- what protects that route is the submission
// grant, and docs/design/drop-in-forms.md section 5.3 says exactly what the
// grant does and does not buy. Do not read this function as more than it is.
func SameOriginOnly() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !safeMethod(r.Method) && !sameOrigin(r) {
				http.Error(w,
					"That request did not come from a page on this site. Open the form again and resubmit it.",
					http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "same-site", "cross-site":
		return false
	}

	// No Sec-Fetch-Site. Fall back to Origin, and treat a null Origin as no
	// usable Origin rather than as a mismatch.
	//
	// That distinction is load-bearing and was a live bug in the code this was
	// carried over from. A browser sends `Origin: null` on a native form
	// submission when the referrer policy is no-referrer -- which the policy
	// in the parent project set -- so the fallback rejected the site's own
	// form posts on exactly the legacy clients it was written to serve. It
	// went unnoticed because Sec-Fetch-Site short-circuits first everywhere.
	//
	// We avoid creating the ambiguity in the first place by using
	// Referrer-Policy: same-origin rather than no-referrer, so our own pages
	// send a real Origin. A null one is therefore uninformative, and
	// uninformative falls into the documented hole below rather than into a
	// refusal.
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return true
	}

	return originMatchesHost(origin, r.Host)
}

// originMatchesHost compares an Origin against the Host it arrived on.
//
// The scheme is required to be https, unlike the version this replaces, which
// stripped the scheme and string-compared the remainder -- so
// http://example.test matched Host example.test. That is not exploitable while
// everything is HTTPS-only, and it is one fewer thing that has to stay true.
func originMatchesHost(origin, host string) bool {
	rest, ok := strings.CutPrefix(origin, "https://")
	if !ok {
		return false
	}

	return rest == host
}

// --- body size --------------------------------------------------------------

// MaxBody caps what a request may send.
//
// Every route a stranger can post to needs one, because a form intake endpoint
// is an upload endpoint whether or not it was meant to be. The limit is per
// route rather than global: a form submission and a Stripe webhook are
// different sizes, and sizing both for the larger one is how the smaller one
// becomes a way to spend our memory.
func MaxBody(n int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}

			next.ServeHTTP(w, r)
		})
	}
}

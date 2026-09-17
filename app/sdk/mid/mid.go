// Package mid holds the request gates that need to know about accounts.
//
// The chain, assembled in app/sdk/muxer:
//
//	RequestID        mints the id every line of this request carries
//	Logging          writes the one "request" line, whatever answers
//	Panics           recovers, logs it, answers 500
//	SecureHeaders    the CSP and friends
//	SameOriginOnly   refuses a write that came from somewhere else
//	Authenticate     establishes who is asking, and refuses nobody
//	  Require        refuses a request with no account behind it
//	    the app
//
// [Authenticate] never refuses, which is what lets the sign-in page live in
// the same chain as everything it protects. [Require] comes next, so a route
// mounted without it fails by showing a signed-out page rather than by leaking
// one.
package mid

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// CookieName is the session cookie.
//
// The __Host- prefix is not decoration. It tells the browser to refuse the
// cookie unless it is Secure, has Path=/, and carries no Domain attribute --
// which means no subdomain can set it, and neither can a sibling host that
// somebody else controls. It is also the constraint that forced this service
// onto two hostnames: a cookie that must have Path=/ cannot be scoped away
// from the page third parties embed, so the embed surface had to be a
// different origin instead.
const CookieName = "__Host-session"

// Authenticator is what this package needs in order to recognise a session,
// which is a narrow enough slice of userbus to be worth naming separately.
type Authenticator interface {
	Authenticate(ctx context.Context, now time.Time, presented string) (userbus.User, error)
}

// ctxKey is unexported, so nothing outside this package can put a principal
// into a context. A gate downstream trusts what it finds there, and the only
// way to be sure of that is for there to be exactly one writer.
type ctxKey int

const principalKey ctxKey = iota + 1

// Principal is who is making this request.
type Principal struct {
	User      userbus.User
	SessionID types.ID
}

// UserFrom returns the account behind this request.
//
// The bool is not a formality: on the sign-in page it is legitimately false,
// which is the whole reason Authenticate refuses nobody. A handler that needs
// it to be true belongs behind [Require].
func UserFrom(ctx context.Context) (userbus.User, bool) {
	p, ok := ctx.Value(principalKey).(Principal)

	return p.User, ok
}

// SessionFrom returns the session this request arrived with, for signing out
// and for showing somebody their own devices.
func SessionFrom(ctx context.Context) (types.ID, bool) {
	p, ok := ctx.Value(principalKey).(Principal)

	return p.SessionID, ok
}

// Authenticate establishes who is asking, and refuses nobody.
//
// A cookie that no longer resolves is cleared on the way past. Without that, a
// session that expired while a tab was open leaves a cookie the browser keeps
// sending, Require keeps redirecting, and the sign-in page keeps being reached
// with a credential it ignores -- which looks to the person like a redirect
// loop with no explanation.
func Authenticate(log *slog.Logger, auth Authenticator) web.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(CookieName)
			if err != nil || c.Value == "" {
				next.ServeHTTP(w, r)

				return
			}

			u, err := auth.Authenticate(r.Context(), time.Now(), c.Value)

			switch {
			case errors.Is(err, userbus.ErrDenied):
				ClearSession(w)
				next.ServeHTTP(w, r)

				return

			case err != nil:
				// Not treated as "not signed in". An unreadable database
				// would otherwise present as a sign-in page, and somebody
				// would sign in again, and again, with nothing saying why.
				log.Error("the session could not be checked",
					"request_id", web.RequestIDFrom(r.Context()), "error", err)
				http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)

				return
			}

			// The session id comes from the cookie's own identifier half,
			// which Authenticate has just proved belongs to this account.
			id, _, _ := strings.Cut(c.Value, ".")

			sessionID, err := types.ParseID(id)
			if err != nil {
				// Unreachable: a cookie that authenticated has a parseable
				// identifier. Handled rather than ignored so that a future
				// change to the credential format fails loudly here instead
				// of silently producing a zero session id.
				log.Error("an authenticated session has an unreadable identifier",
					"request_id", web.RequestIDFrom(r.Context()))
				http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)

				return
			}

			ctx := context.WithValue(r.Context(), principalKey, Principal{
				User:      u,
				SessionID: sessionID,
			})

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Require refuses a request with no account behind it.
//
// A readable request is sent to the sign-in page, carrying where it was going
// so that signing in lands somebody where they meant to be. Anything else gets
// a bare 403: a POST redirected to a login page loses its body, and answering
// a form submission with a page is worse than answering it with a refusal.
func Require(signInPath string) web.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := UserFrom(r.Context()); ok {
				next.ServeHTTP(w, r)

				return
			}

			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "you need to be signed in to do that.", http.StatusForbidden)

				return
			}

			to := signInPath
			if want := SafeNext(r.URL.RequestURI()); want != "" {
				to += "?next=" + url.QueryEscape(want)
			}

			// 303, so that the browser follows with a GET whatever this was.
			http.Redirect(w, r, to, http.StatusSeeOther)
		})
	}
}

// SafeNext returns raw if it is somewhere on this site, and "" otherwise.
//
// This is the open-redirect guard, and it is a denylist of the shapes that
// smuggle another origin past a naive "starts with a slash" check:
//
//	//evil.test/       a protocol-relative URL: a browser reads this as
//	                   https://evil.test/ even though it starts with a slash
//	/\evil.test/       browsers normalise a backslash to a slash, so this is
//	                   the same attack with one character changed
//	https://evil.test  an absolute URL, which has a scheme
//
// Returning "" rather than an error, because there is nothing to tell anybody:
// a hand-made `next` is either a bug in our own links or an attempt, and the
// answer to both is to land on the default page.
func SafeNext(raw string) string {
	switch {
	case raw == "", raw == "/":
		return ""
	case !strings.HasPrefix(raw, "/"):
		return ""
	case strings.HasPrefix(raw, "//"):
		return ""
	case strings.HasPrefix(raw, "/\\"):
		return ""
	case strings.ContainsAny(raw, "\r\n"):
		// A newline in a Location header ends it and starts another.
		return ""
	}

	// Parsed as well as prefix-checked, so that anything with a scheme or a
	// host in it is refused however it was spelled.
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return ""
	}

	return raw
}

// SetSession writes the session cookie.
//
// SameSite=Lax rather than Strict. Strict withholds the cookie on a
// cross-site top-level navigation, which is exactly what following a sign-in
// link out of a mail client is -- so Strict would mean clicking the link,
// signing in, and arriving signed out.
func SetSession(w http.ResponseWriter, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSession removes it.
//
// Both an empty value and a past expiry, because the two are handled
// differently by different browsers and the cost of setting both is nothing.
// The remaining attributes have to match the ones it was set with or the
// browser treats it as a different cookie and keeps the original.
func ClearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

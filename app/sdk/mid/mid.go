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
//	    RequireFormRole  refuses an account with no grant on this form
//	    RequireSiteAdmin refuses an account that does not hold the service
//	      the app
//
// The last two are alternatives rather than a pair: a route names a form in
// its path and takes the first, or it does not and takes the second. Only the
// routes that create a form are in the second case, because a form that does
// not exist yet has no grant anybody could hold.
//
// [Authenticate] never refuses, which is what lets the sign-in page live in
// the same chain as everything it protects. [Require] comes next, so a route
// mounted without it fails by showing a signed-out page rather than by leaking
// one.
//
// [RequireFormRole] is last, because "who is this" has to be settled before
// "what may they do", and because the two refusals are different answers: one
// sends somebody to the sign-in page and the other tells them they are signed
// in as the wrong person.
package mid

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
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

// FormSlugParam is the path wildcard [RequireFormRole] reads. A route behind
// that gate must name its wildcard this, and the gate says so loudly rather
// than failing open when it does not.
const FormSlugParam = "slug"

// Authorizer is what this package needs in order to answer "may they", which
// is a narrow enough slice of accessbus to be worth naming separately -- and
// narrow enough that a test can supply it in three lines.
type Authorizer interface {
	Allowed(ctx context.Context, userID types.ID, form types.Slug, want accessbus.Role) (bool, error)
}

// RequireFormRole refuses a signed-in account that holds no sufficient grant
// on the form this route names.
//
// It must be mounted under [Require]. Without a principal there is nothing to
// check, and rather than fall through to the handler this refuses and logs it
// as the mounting mistake it is -- a gate whose absent input makes it a no-op
// is the trap the parent project's form-token middleware set, and it is
// written down in app/sdk/muxer for the same reason.
//
// Three refusals, and they are deliberately not the same:
//
//	404  the slug in the path is not a form name at all, so no such page
//	     could exist. Answered here rather than let through, because the
//	     handler beneath would have to re-derive it to say the same thing.
//	403  a real form, and this account may not have it. It says which
//	     account, because the reader is signed in and the usual cause is
//	     being signed in as the wrong one.
//	500  the grant could not be read. Never a refusal: an unreachable
//	     database must not present as "you are not allowed", or somebody
//	     spends the afternoon wondering what they did wrong.
func RequireFormRole(log *slog.Logger, auth Authorizer, want accessbus.Role) web.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := web.RequestIDFrom(r.Context())

			u, ok := UserFrom(r.Context())
			if !ok {
				log.Error("a route behind RequireFormRole is not behind Require",
					"request_id", requestID, "path", r.URL.Path)
				http.Error(w, "you need to be signed in to do that.", http.StatusForbidden)

				return
			}

			raw := r.PathValue(FormSlugParam)
			if raw == "" {
				// A mounting mistake again, and a 500 rather than a refusal
				// because refusing would look like a permission problem and
				// send somebody looking in the wrong place.
				log.Error("a route behind RequireFormRole has no {"+FormSlugParam+"} in its pattern",
					"request_id", requestID, "path", r.URL.Path)
				http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)

				return
			}

			form, err := types.ParseSlug(raw)
			if err != nil {
				http.Error(w, "there is no form by that name.", http.StatusNotFound)

				return
			}

			allowed, err := auth.Allowed(r.Context(), u.ID, form, want)
			if err != nil {
				log.Error("the grant could not be checked",
					"request_id", requestID, "user_id", u.ID, "form", form.String(), "error", err)
				http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)

				return
			}

			if !allowed {
				// Logged, because a refusal here is either somebody signed in
				// on the wrong account or somebody trying slugs, and the two
				// are told apart by how many lines there are.
				log.Info("refused for want of a grant",
					"request_id", requestID, "user_id", u.ID, "form", form.String(), "want", want)
				http.Error(w, "you are signed in as "+u.Email.String()+", which does not have access to this form.", http.StatusForbidden)

				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RequireSiteAdmin refuses a signed-in account that does not administer the
// whole service.
//
// It exists for exactly one shape of route: the one that creates a form. Every
// other gated route in this service names a form in its path and asks
// [RequireFormRole] whether this account holds a role on that form, but a form
// that does not exist yet has no grant anybody could hold -- so the question
// has to be the other one, and the site-wide grant is the only answer to it.
//
// accessbus.Allowed already consults the site-wide grant as a fallback for
// every form, so this is the same read with the form left out rather than a
// second authority. The refusals match RequireFormRole's, minus the 404: there
// is no slug here to be wrong about.
func RequireSiteAdmin(log *slog.Logger, auth Authorizer, want accessbus.Role) web.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := web.RequestIDFrom(r.Context())

			u, ok := UserFrom(r.Context())
			if !ok {
				log.Error("a route behind RequireSiteAdmin is not behind Require",
					"request_id", requestID, "path", r.URL.Path)
				http.Error(w, "you need to be signed in to do that.", http.StatusForbidden)

				return
			}

			// The zero slug is the site-wide grant's own form, which is what
			// accessbus.Grant.SiteWide reports on. Asking Allowed about it is
			// asking whether this account holds the service rather than any
			// one form.
			allowed, err := auth.Allowed(r.Context(), u.ID, types.Slug{}, want)
			if err != nil {
				log.Error("the site-wide grant could not be checked",
					"request_id", requestID, "user_id", u.ID, "error", err)
				http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)

				return
			}

			if !allowed {
				log.Info("refused for want of a site-wide grant",
					"request_id", requestID, "user_id", u.ID, "want", want)
				http.Error(w, "you are signed in as "+u.Email.String()+", which does not administer this service. Whoever does can make a form for you, or give you the run of the place.", http.StatusForbidden)

				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// FormCreator is the one thing [RequireFormCreator] needs.
type FormCreator interface {
	CanCreateForms(ctx context.Context, userID types.ID) (bool, error)
}

// RequireFormCreator refuses a signed-in account that may not make a new
// form.
//
// It exists because [RequireSiteAdmin] answers a different question: whether
// this account holds the service, which is more than "may it make a form".
// accessbus.RoleCreator is a site-wide grant that says only the narrower
// thing, and CanCreateForms is where that is decided -- see its comment for
// why Allowed is not asked instead. The refusals otherwise match
// RequireSiteAdmin's.
func RequireFormCreator(log *slog.Logger, can FormCreator) web.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := web.RequestIDFrom(r.Context())

			u, ok := UserFrom(r.Context())
			if !ok {
				log.Error("a route behind RequireFormCreator is not behind Require",
					"request_id", requestID, "path", r.URL.Path)
				http.Error(w, "you need to be signed in to do that.", http.StatusForbidden)

				return
			}

			allowed, err := can.CanCreateForms(r.Context(), u.ID)
			if err != nil {
				log.Error("the site-wide grant could not be checked",
					"request_id", requestID, "user_id", u.ID, "error", err)
				http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)

				return
			}

			if !allowed {
				log.Info("refused for want of a form-creator grant",
					"request_id", requestID, "user_id", u.ID)
				http.Error(w, "you are signed in as "+u.Email.String()+", which may not create a new form. Whoever administers this service can give you that.", http.StatusForbidden)

				return
			}

			next.ServeHTTP(w, r)
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

package mid_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// fakeAuth answers however a test needs it to.
type fakeAuth struct {
	user userbus.User
	err  error

	presented []string
}

func (f *fakeAuth) Authenticate(_ context.Context, _ time.Time, presented string) (userbus.User, error) {
	f.presented = append(f.presented, presented)

	return f.user, f.err
}

// signInPath is where Require sends a signed-out reader. One of the tests
// below declares its own local copy, which predates this and is left alone.
const signInPath = "/signin"

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func someone() userbus.User {
	return userbus.User{ID: types.NewID(), Enabled: true}
}

// handler records what it saw in the context, so a test can assert on the
// principal rather than on a page.
type seen struct {
	reached   bool
	user      userbus.User
	haveUser  bool
	sessionID types.ID
}

func probe(got *seen) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.reached = true
		got.user, got.haveUser = mid.UserFrom(r.Context())
		got.sessionID, _ = mid.SessionFrom(r.Context())

		w.WriteHeader(http.StatusOK)
	})
}

func cookieFor(value string) *http.Cookie {
	return &http.Cookie{Name: mid.CookieName, Value: value}
}

func TestAuthenticateRefusesNobody(t *testing.T) {
	u := someone()
	sessionID := types.NewID()

	tests := []struct {
		name     string
		cookie   *http.Cookie
		auth     *fakeAuth
		wantUser bool
		status   int
		cleared  bool
	}{
		{
			name:   "no cookie at all",
			auth:   &fakeAuth{err: userbus.ErrDenied},
			status: http.StatusOK,
		},
		{
			name:   "an empty cookie",
			cookie: cookieFor(""),
			auth:   &fakeAuth{err: userbus.ErrDenied},
			status: http.StatusOK,
		},
		{
			name:     "a good session",
			cookie:   cookieFor(sessionID.String() + ".averylongsecretvaluehere"),
			auth:     &fakeAuth{user: u},
			wantUser: true,
			status:   http.StatusOK,
		},
		{
			// A session that expired while a tab was open. The request must
			// still reach the handler signed out, and the cookie must be
			// cleared on the way past -- otherwise the browser keeps sending
			// it, Require keeps redirecting, and it looks like a redirect loop
			// with no explanation.
			name:    "a session that no longer resolves",
			cookie:  cookieFor(sessionID.String() + ".averylongsecretvaluehere"),
			auth:    &fakeAuth{err: userbus.ErrDenied},
			status:  http.StatusOK,
			cleared: true,
		},
		{
			name:    "junk in the cookie",
			cookie:  cookieFor("not-a-credential"),
			auth:    &fakeAuth{err: userbus.ErrDenied},
			status:  http.StatusOK,
			cleared: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got seen

			h := web.Wrap(probe(&got), mid.Authenticate(discard(), tt.auth))

			r := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
			if tt.cookie != nil {
				r.AddCookie(tt.cookie)
			}

			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != tt.status {
				t.Errorf("status = %d, want %d", w.Code, tt.status)
			}
			if !got.reached {
				t.Fatal("Authenticate refused the request; it must refuse nobody")
			}
			if got.haveUser != tt.wantUser {
				t.Errorf("a principal reached the handler: %v, want %v", got.haveUser, tt.wantUser)
			}
			if tt.wantUser && got.user.ID != u.ID {
				t.Errorf("the handler saw %q, want %q", got.user.ID, u.ID)
			}
			if tt.wantUser && got.sessionID != sessionID {
				t.Errorf("the session id is %q, want %q", got.sessionID, sessionID)
			}

			cleared := strings.Contains(w.Header().Get("Set-Cookie"), mid.CookieName+"=;")
			if cleared != tt.cleared {
				t.Errorf("the cookie was cleared: %v, want %v (Set-Cookie: %q)",
					cleared, tt.cleared, w.Header().Get("Set-Cookie"))
			}
		})
	}
}

// An unreadable database must not present as a sign-in page. Somebody would
// sign in again, and again, with nothing saying why.
func TestAuthenticateAnswersAnInfrastructureFailure(t *testing.T) {
	var got seen

	auth := &fakeAuth{err: errors.New("the database is on fire")}
	h := web.Wrap(probe(&got), mid.Authenticate(discard(), auth))

	r := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	r.AddCookie(cookieFor(types.NewID().String() + ".averylongsecretvaluehere"))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if got.reached {
		t.Error("the handler ran although the session could not be checked")
	}

	// And the sentence says what to do, and names no Go package.
	body := w.Body.String()
	for _, forbidden := range []string{"userbus", "sql", "database is on fire", "error"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the page leaks %q: %q", forbidden, body)
		}
	}
}

func TestRequire(t *testing.T) {
	const signIn = "/signin"

	tests := []struct {
		name     string
		method   string
		target   string
		signedIn bool
		status   int
		location string
	}{
		{
			name: "a signed-in read", method: http.MethodGet, target: "/dashboard",
			signedIn: true, status: http.StatusOK,
		},
		{
			name: "a signed-in write", method: http.MethodPost, target: "/forms",
			signedIn: true, status: http.StatusOK,
		},
		{
			// Carrying where they were going, so that signing in lands them
			// where they meant to be.
			name: "a signed-out read", method: http.MethodGet, target: "/submissions/feast-lunch-2026",
			status:   http.StatusSeeOther,
			location: "/signin?next=%2Fsubmissions%2Ffeast-lunch-2026",
		},
		{
			name: "a signed-out read of the root", method: http.MethodGet, target: "/",
			status:   http.StatusSeeOther,
			location: "/signin",
		},
		{
			// A POST redirected to a login page loses its body, and answering
			// a form submission with a page is worse than refusing it.
			name: "a signed-out write", method: http.MethodPost, target: "/forms",
			status: http.StatusForbidden,
		},
		{
			name: "a signed-out delete", method: http.MethodDelete, target: "/forms/x",
			status: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got seen

			auth := &fakeAuth{err: userbus.ErrDenied}
			if tt.signedIn {
				auth = &fakeAuth{user: someone()}
			}

			h := web.Wrap(probe(&got),
				mid.Authenticate(discard(), auth),
				mid.Require(signIn),
			)

			r := httptest.NewRequest(tt.method, tt.target, nil)
			if tt.signedIn {
				r.AddCookie(cookieFor(types.NewID().String() + ".averylongsecretvaluehere"))
			}

			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != tt.status {
				t.Errorf("status = %d, want %d", w.Code, tt.status)
			}
			if got.reached != (tt.status == http.StatusOK) {
				t.Errorf("the handler ran: %v", got.reached)
			}
			if tt.location != "" && w.Header().Get("Location") != tt.location {
				t.Errorf("Location = %q, want %q", w.Header().Get("Location"), tt.location)
			}
		})
	}
}

// The open-redirect guard. Every rejected shape here gets past a naive
// "starts with a slash" check.
func TestSafeNext(t *testing.T) {
	keep := []string{
		"/dashboard",
		"/submissions/feast-lunch-2026",
		"/submissions?form=feast-lunch-2026&page=2",
		"/a/b/c#section",
		"/path%20with%20escapes",
	}

	for _, in := range keep {
		t.Run("keeps "+in, func(t *testing.T) {
			if got := mid.SafeNext(in); got != in {
				t.Errorf("SafeNext(%q) = %q, want it kept", in, got)
			}
		})
	}

	drop := []struct {
		name string
		in   string
	}{
		{"blank", ""},
		{"the root, which is the default anyway", "/"},

		// A browser reads this as https://evil.test/ although it starts with
		// a slash.
		{"protocol-relative", "//evil.test/"},
		{"protocol-relative with a path", "//evil.test/dashboard"},

		// Browsers normalise a backslash to a slash, so this is the same
		// attack with one character changed.
		{"backslash-relative", "/\\evil.test/"},
		{"backslash-relative, doubled", "/\\\\evil.test"},

		{"absolute https", "https://evil.test/dashboard"},
		{"absolute http", "http://evil.test/"},
		{"a scheme with no host", "javascript:alert(1)"},
		{"a data URL", "data:text/html,<script>alert(1)</script>"},

		{"relative, with no leading slash", "dashboard"},
		{"parent-relative", "../dashboard"},

		// A newline in a Location header ends it and starts another.
		{"a newline", "/dashboard\nSet-Cookie: x=y"},
		{"a carriage return", "/dashboard\rSet-Cookie: x=y"},
	}

	for _, tt := range drop {
		t.Run("drops "+tt.name, func(t *testing.T) {
			if got := mid.SafeNext(tt.in); got != "" {
				t.Errorf("SafeNext(%q) = %q, want it dropped", tt.in, got)
			}
		})
	}
}

// Whatever Require puts in the Location header must survive being parsed back
// as a URL and still point at this site. This is the assertion that would
// catch a guard that let something through in an encoding nobody thought of.
func TestRequireNeverRedirectsOffSite(t *testing.T) {
	for _, target := range []string{
		"/dashboard",
		"//evil.test/",
		"/\\evil.test/",
		"/dashboard?next=https://evil.test",
	} {
		t.Run(target, func(t *testing.T) {
			var got seen

			h := web.Wrap(probe(&got),
				mid.Authenticate(discard(), &fakeAuth{err: userbus.ErrDenied}),
				mid.Require("/signin"),
			)

			// Built by hand rather than with httptest.NewRequest, which
			// refuses some of these before the handler ever sees them.
			r := &http.Request{
				Method: http.MethodGet,
				URL:    &url.URL{Path: "/x"},
				Header: http.Header{},
			}
			r.URL.Opaque = ""
			if u, err := url.Parse(target); err == nil {
				r.URL = u
			}

			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			loc := w.Header().Get("Location")
			if loc == "" {
				t.Fatal("no Location header")
			}

			parsed, err := url.Parse(loc)
			if err != nil {
				t.Fatalf("the Location header is not a URL: %q", loc)
			}
			if parsed.Scheme != "" || parsed.Host != "" {
				t.Errorf("Location points off site: %q", loc)
			}
			if !strings.HasPrefix(parsed.Path, "/signin") {
				t.Errorf("Location is not the sign-in page: %q", loc)
			}

			// And the preserved destination, if there is one, is also on this
			// site.
			if next := parsed.Query().Get("next"); next != "" {
				n, err := url.Parse(next)
				if err != nil || n.Scheme != "" || n.Host != "" {
					t.Errorf("the preserved destination points off site: %q", next)
				}
			}
		})
	}
}

// The cookie attributes are the __Host- prefix's requirements, and a browser
// silently refuses the whole cookie if any of them is missing.
func TestSessionCookieAttributes(t *testing.T) {
	w := httptest.NewRecorder()
	expires := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

	mid.SetSession(w, "abc.def", expires)

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}

	c := cookies[0]

	switch {
	case c.Name != "__Host-session":
		t.Errorf("Name = %q", c.Name)
	case c.Value != "abc.def":
		t.Errorf("Value = %q", c.Value)

	// All three are required by the __Host- prefix. Without Path=/ or
	// without Secure, or with a Domain, the browser drops it entirely.
	case c.Path != "/":
		t.Errorf("Path = %q, want /", c.Path)
	case !c.Secure:
		t.Error("the cookie is not Secure")
	case c.Domain != "":
		t.Errorf("the cookie has a Domain of %q, which the __Host- prefix forbids", c.Domain)

	case !c.HttpOnly:
		t.Error("the cookie is readable by script")

	// Lax rather than Strict. Strict withholds the cookie on a cross-site
	// top-level navigation, which is exactly what following a sign-in link
	// out of a mail client is -- so Strict means signing in and arriving
	// signed out.
	case c.SameSite != http.SameSiteLaxMode:
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
}

func TestClearSessionMatchesTheAttributesItWasSetWith(t *testing.T) {
	set := httptest.NewRecorder()
	mid.SetSession(set, "abc.def", time.Now().Add(time.Hour))

	clear := httptest.NewRecorder()
	mid.ClearSession(clear)

	was := set.Result().Cookies()[0]
	now := clear.Result().Cookies()[0]

	// A browser matches a cookie by name, domain and path. Change any of them
	// and it treats this as a different cookie and keeps the original.
	switch {
	case now.Name != was.Name:
		t.Errorf("Name = %q, want %q", now.Name, was.Name)
	case now.Path != was.Path:
		t.Errorf("Path = %q, want %q", now.Path, was.Path)
	case now.Secure != was.Secure:
		t.Error("Secure differs from the cookie being cleared")
	case now.SameSite != was.SameSite:
		t.Error("SameSite differs from the cookie being cleared")
	}

	if now.Value != "" {
		t.Errorf("Value = %q, want empty", now.Value)
	}

	// Both an empty value and a past expiry, because different browsers have
	// honoured different ones and setting both costs nothing.
	if now.MaxAge >= 0 && !now.Expires.Before(time.Now()) {
		t.Errorf("the cookie is not expired: MaxAge=%d Expires=%s", now.MaxAge, now.Expires)
	}
}

// outsiderKey imitates what code in another package would have to do to plant
// a principal: declare its own key type. mid's key type is unexported, so even
// an identical underlying value is a different type and the lookup misses.
type outsiderKey int

const outsiderPrincipalKey outsiderKey = 1

// A gate downstream trusts whatever principal it finds in the context, so the
// only thing making that safe is that nothing outside this package can put one
// there.
func TestAPrincipalCannotBeForgedFromOutside(t *testing.T) {
	forged := mid.Principal{User: someone()}

	// The same underlying value mid uses, under a type an outsider can
	// declare.
	if _, ok := mid.UserFrom(context.WithValue(t.Context(), outsiderPrincipalKey, forged)); ok {
		t.Error("a principal planted under an outsider's key type was accepted")
	}

	if _, ok := mid.UserFrom(t.Context()); ok {
		t.Error("an empty context produced a principal")
	}
	if _, ok := mid.SessionFrom(t.Context()); ok {
		t.Error("an empty context produced a session")
	}
}

// fakeAuthz answers however a test needs it to, and records what it was asked.
type fakeAuthz struct {
	allowed bool
	err     error

	asked []string
	want  accessbus.Role
}

func (f *fakeAuthz) Allowed(_ context.Context, userID types.ID, form types.Slug, want accessbus.Role) (bool, error) {
	f.asked = append(f.asked, userID.String()+" "+form.String())
	f.want = want

	return f.allowed, f.err
}

// roleChain mounts a route the way a real one is mounted: a pattern with the
// wildcard the gate reads, behind Require, behind Authenticate. Testing the
// gate through the chain rather than alone is the point -- what sits in front
// of a route is the thing most easily got wrong about it.
func roleChain(pattern string, signedIn bool, authz mid.Authorizer, got *seen) http.Handler {
	auth := &fakeAuth{err: userbus.ErrDenied}
	if signedIn {
		auth = &fakeAuth{user: userbus.User{
			ID:      types.NewID(),
			Email:   mustEmail("reader@example.org"),
			Enabled: true,
		}}
	}

	mux := http.NewServeMux()
	mux.Handle(pattern, mid.Require(signInPath)(
		mid.RequireFormRole(discard(), authz, accessbus.RoleResults)(probe(got)),
	))

	return web.Wrap(mux, mid.Authenticate(discard(), auth))
}

func mustEmail(s string) types.Email {
	e, err := types.ParseEmail(s)
	if err != nil {
		panic(err)
	}

	return e
}

func TestRequireFormRoleAdmitsAGrantHolder(t *testing.T) {
	var got seen

	authz := &fakeAuthz{allowed: true}
	h := roleChain("GET /forms/{slug}/submissions", true, authz, &got)

	r := httptest.NewRequest(http.MethodGet, "/forms/feast-lunch-2026/submissions", nil)
	r.AddCookie(cookieFor(types.NewID().String() + ".averylongsecretvaluehere"))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !got.reached {
		t.Error("the handler was not reached")
	}

	// The slug came from the path, and the role asked for is the route's own.
	if len(authz.asked) != 1 || !strings.HasSuffix(authz.asked[0], " feast-lunch-2026") {
		t.Errorf("asked = %v, want one question about feast-lunch-2026", authz.asked)
	}
	if authz.want != accessbus.RoleResults {
		t.Errorf("asked for %q, want %q", authz.want, accessbus.RoleResults)
	}
}

func TestRequireFormRoleRefusesAnAccountWithNoGrant(t *testing.T) {
	var got seen

	h := roleChain("GET /forms/{slug}/submissions", true, &fakeAuthz{allowed: false}, &got)

	r := httptest.NewRequest(http.MethodGet, "/forms/feast-lunch-2026/submissions", nil)
	r.AddCookie(cookieFor(types.NewID().String() + ".averylongsecretvaluehere"))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if got.reached {
		t.Error("the handler was reached without a grant")
	}

	// It names the account, because the reader is signed in and the usual
	// cause of this is being signed in as the wrong person.
	if !strings.Contains(w.Body.String(), "reader@example.org") {
		t.Errorf("the refusal does not say who is signed in: %q", w.Body.String())
	}
}

// A refusal must not be how an unreachable database presents. Somebody told
// "you are not allowed" goes looking for a permission problem, and there is
// not one.
func TestRequireFormRoleAnswers500WhenTheGrantCannotBeRead(t *testing.T) {
	var got seen

	authz := &fakeAuthz{err: errors.New("the disk is on fire")}
	h := roleChain("GET /forms/{slug}/submissions", true, authz, &got)

	r := httptest.NewRequest(http.MethodGet, "/forms/feast-lunch-2026/submissions", nil)
	r.AddCookie(cookieFor(types.NewID().String() + ".averylongsecretvaluehere"))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if got.reached {
		t.Error("the handler was reached after the check failed")
	}
	if strings.Contains(w.Body.String(), "disk") {
		t.Errorf("the error reached the reader: %q", w.Body.String())
	}
}

func TestRequireFormRoleRefusesASlugThatIsNotAFormName(t *testing.T) {
	var got seen

	// allowed: true, so that the only thing that can refuse this is the slug.
	authz := &fakeAuthz{allowed: true}
	h := roleChain("GET /forms/{slug}/submissions", true, authz, &got)

	r := httptest.NewRequest(http.MethodGet, "/forms/Feast_Lunch!/submissions", nil)
	r.AddCookie(cookieFor(types.NewID().String() + ".averylongsecretvaluehere"))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if got.reached {
		t.Error("the handler was reached with an unparseable slug")
	}
	if len(authz.asked) != 0 {
		t.Errorf("the store was consulted about a slug that cannot exist: %v", authz.asked)
	}
}

// The trap this gate is written against: a middleware whose absent input makes
// it a no-op, while the chain diagram still lists it as a gate.
func TestRequireFormRoleRefusesWhenThereIsNoPrincipal(t *testing.T) {
	var got seen

	authz := &fakeAuthz{allowed: true}

	// Mounted without Require, which is the mistake. Deliberately reached
	// with no session at all.
	mux := http.NewServeMux()
	mux.Handle("GET /forms/{slug}/submissions",
		mid.RequireFormRole(discard(), authz, accessbus.RoleResults)(probe(&got)))

	h := web.Wrap(mux, mid.Authenticate(discard(), &fakeAuth{err: userbus.ErrDenied}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/forms/feast-lunch-2026/submissions", nil))

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if got.reached {
		t.Error("an unauthenticated request reached a handler behind RequireFormRole")
	}
	if len(authz.asked) != 0 {
		t.Errorf("the store was consulted with no account: %v", authz.asked)
	}
}

// A route mounted behind this gate with no {slug} in its pattern is a mounting
// mistake, and a 500 rather than a refusal so that it is not mistaken for a
// permission problem.
func TestRequireFormRoleAnswers500WhenTheRouteNamesNoForm(t *testing.T) {
	var got seen

	authz := &fakeAuthz{allowed: true}
	h := roleChain("GET /forms/all/submissions", true, authz, &got)

	r := httptest.NewRequest(http.MethodGet, "/forms/all/submissions", nil)
	r.AddCookie(cookieFor(types.NewID().String() + ".averylongsecretvaluehere"))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if got.reached {
		t.Error("the handler was reached from a route with no form in it")
	}
}

// Require decides first, and its answer for a readable request is the sign-in
// page rather than a refusal -- so a signed-out reader is redirected, not told
// they lack a grant they could not possibly hold.
func TestRequireDecidesBeforeRequireFormRole(t *testing.T) {
	var got seen

	authz := &fakeAuthz{allowed: true}
	h := roleChain("GET /forms/{slug}/submissions", false, authz, &got)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/forms/feast-lunch-2026/submissions", nil))

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if to := w.Header().Get("Location"); !strings.HasPrefix(to, signInPath) {
		t.Errorf("Location = %q, want the sign-in page", to)
	}
	if len(authz.asked) != 0 {
		t.Errorf("the grant was checked for a signed-out reader: %v", authz.asked)
	}
}

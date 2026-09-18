package muxer_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/app/domain/authapp"
	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/app/domain/submissionapp"
	"github.com/jroedel/dropin-forms/app/sdk/muxer"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/access/stores/accessdb"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formtoml"
	"github.com/jroedel/dropin-forms/business/domain/submission/stores/submissiondb"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/stores/userdb"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/forms"
	"github.com/jroedel/dropin-forms/foundation/logger"
	"github.com/jroedel/dropin-forms/foundation/mail"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// Tests go through the mounted mux rather than against a bare handler,
// because the thing most worth testing about a route is the chain in front of
// it. Each test below names the gate that should have decided the answer.

func newConfig(t *testing.T, origins []types.Origin, expected sqldb.Expected) muxer.Config {
	t.Helper()

	db, err := sqldb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := sqldb.Init(t.Context(), db); err != nil {
		t.Fatalf("initialising the test database: %v", err)
	}

	log := logger.New(io.Discard, slog.LevelError)

	// userdb's tables as well, because the admin surface mounts the sign-in
	// routes and they need somewhere to write, and accessdb's because the
	// bootstrap sign-in grants the account it creates.
	if err := userdb.Init(t.Context(), db); err != nil {
		t.Fatalf("initialising the account tables: %v", err)
	}
	if err := accessdb.Init(t.Context(), db); err != nil {
		t.Fatalf("initialising the grant table: %v", err)
	}

	renderer, err := page.NewRenderer(log, page.AdminChrome(), authapp.Templates, submissionapp.Templates)
	if err != nil {
		t.Fatalf("building the renderer: %v", err)
	}

	embedPages, err := page.NewRenderer(log, page.EmbedChrome(), embedapp.Templates)
	if err != nil {
		t.Fatalf("building the embed renderer: %v", err)
	}

	// submissiondb's tables too, because the embed surface accepts writes.
	if err := submissiondb.Init(t.Context(), db); err != nil {
		t.Fatalf("initialising the submission tables: %v", err)
	}

	return muxer.Config{
		Log:      log,
		DB:       db,
		Expected: expected,
		// The real resolver, so that the mounted surface in a test chooses
		// its policy the way the mounted surface in production does. Tests
		// that are about the policy itself rather than about the lookup
		// replace this with a function of their own.
		FrameAncestors: page.FormFrameAncestors(definitions, origins),

		Users:  userbus.NewBusiness(log, userdb.NewStore(db)),
		Access: accessbus.NewBusiness(log, accessdb.NewStore(db)),

		// The admin surface reads submissions, so it needs both the rows
		// and the definitions the rows are columns of.
		Submissions: submissionbus.NewBusiness(log, submissiondb.NewStore(db)),
		Forms:       definitions,

		Embed: embedapp.Config{
			Log:         log,
			Forms:       definitions,
			Submissions: submissionbus.NewBusiness(log, submissiondb.NewStore(db)),
			Render:      embedPages,
			GrantKey:    grantKey,
		},
		Mail:         &mail.Recorder{},
		Render:       renderer,
		AdminBaseURL: "https://forms.test",
	}
}

// definitions is the real form set, loaded once, so that these tests mount
// what production mounts. Package level rather than per-harness because both
// surfaces need it and both test files build one: the embed surface to render
// and price a form, the admin surface because a submission's columns come from
// the form it was made against.
var definitions = func() *formtoml.Store {
	s, err := formtoml.Load(forms.FS)
	if err != nil {
		panic(err)
	}

	return s
}()

// grantKey is a signing key for the tests. Not a secret: it signs grants for
// a database in a temporary directory that is deleted when the test ends.
var grantKey = func() formbus.GrantKey {
	k, err := formbus.ParseGrantKey("a-test-grant-signing-key-long-enough")
	if err != nil {
		panic(err)
	}

	return k
}()

// embedOf builds the embed surface, failing the test rather than returning an
// error to every caller -- the same shape as adminOf, and for the same reason.
func embedOf(t *testing.T, cfg muxer.Config) http.Handler {
	t.Helper()

	h, err := muxer.Embed(cfg)
	if err != nil {
		t.Fatalf("muxer.Embed: %v", err)
	}

	return h
}

// adminOf builds the admin surface, failing the test rather than returning an
// error to every caller. Admin reports a Config missing a dependency instead
// of dereferencing nil on the first request, and in a test that report is a
// setup mistake.
func adminOf(t *testing.T, cfg muxer.Config) http.Handler {
	t.Helper()

	h, err := muxer.Admin(cfg)
	if err != nil {
		t.Fatalf("muxer.Admin: %v", err)
	}

	return h
}

func mustOrigin(t *testing.T, s string) types.Origin {
	t.Helper()

	o, err := types.ParseOrigin(s)
	if err != nil {
		t.Fatalf("ParseOrigin(%q) = %v", s, err)
	}

	return o
}

// --- the header policy, per surface -----------------------------------------

// TestSurfacesEmitOneCSPEach is the test for Set-not-Add.
//
// Two enforced Content-Security-Policy headers are evaluated independently --
// a request need only violate one to be blocked -- so a second policy arriving
// alongside ours would block everything while looking like ours was ignored.
// That failure is very hard to read in a browser and trivial to assert here.
func TestSurfacesEmitOneCSPEach(t *testing.T) {
	cfg := newConfig(t, []types.Origin{mustOrigin(t, "https://schoenstatt-austin.us")}, sqldb.Infrastructure)

	for name, h := range map[string]http.Handler{
		"embed": embedOf(t, cfg),
		"admin": adminOf(t, cfg),
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

			if got := w.Header().Values("Content-Security-Policy"); len(got) != 1 {
				t.Fatalf("got %d Content-Security-Policy headers %q, want exactly 1", len(got), got)
			}
		})
	}
}

// TestEmbedSurfaceIsFrameable asserts the framing decision in
// docs/design/drop-in-forms.md: the embedded form names its allowed origins
// and, critically, sends no X-Frame-Options at all. That header cannot express
// a list, so any value would kill the embed.
func TestEmbedSurfaceIsFrameable(t *testing.T) {
	cfg := newConfig(t, []types.Origin{mustOrigin(t, "https://schoenstatt-austin.us")}, sqldb.Infrastructure)

	// A form page, not /healthz. frame-ancestors is now answered per form, so
	// only a form page has an answer other than 'none' -- which is the point
	// of the change and worth asserting on the page that is actually framed.
	w := httptest.NewRecorder()
	embedOf(t, cfg).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/f/feast-lunch-2026", nil))

	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors https://schoenstatt-austin.us") {
		t.Errorf("embed CSP = %q, want it to allow framing by the site", csp)
	}

	if got := w.Header().Get("X-Frame-Options"); got != "" {
		t.Errorf("embed sent X-Frame-Options: %q, want it absent -- any value breaks the embed", got)
	}
}

// TestAdminSurfaceRefusesFramingAndScripts asserts the management app keeps the
// strict policy the web skill ships. There is no script-src at all, and adding
// one should require changing this test, which is the point of asserting it.
func TestAdminSurfaceRefusesFramingAndScripts(t *testing.T) {
	cfg := newConfig(t, []types.Origin{mustOrigin(t, "https://schoenstatt-austin.us")}, sqldb.Infrastructure)

	w := httptest.NewRecorder()
	adminOf(t, cfg).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("admin CSP = %q, want frame-ancestors 'none'", csp)
	}
	if strings.Contains(csp, "script-src") {
		t.Errorf("admin CSP = %q, want no script-src -- the management app is server-rendered", csp)
	}
	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("admin X-Frame-Options = %q, want DENY", got)
	}
}

// TestFrameAncestorsFailsClosed is the one that matters most in this file.
//
// A CSP that merely omits frame-ancestors is frameable by the entire web --
// default-src 'none' does not restrict framing. So every way of ending up with
// no origins must produce 'none' rather than an absent directive.
func TestFrameAncestorsFailsClosed(t *testing.T) {
	cfg := newConfig(t, nil, sqldb.Infrastructure)

	cases := map[string][]types.Origin{
		"nil slice":    nil,
		"empty slice":  {},
		"zero origins": {{}, {}},
	}

	for name, origins := range cases {
		t.Run(name, func(t *testing.T) {
			c := cfg
			c.FrameAncestors = func(*http.Request) []types.Origin { return origins }

			w := httptest.NewRecorder()
			embedOf(t, c).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

			csp := w.Header().Get("Content-Security-Policy")
			if !strings.Contains(csp, "frame-ancestors 'none'") {
				t.Errorf("CSP = %q, want frame-ancestors 'none' when no origin is allowed", csp)
			}
		})
	}

	t.Run("nil function", func(t *testing.T) {
		c := cfg
		c.FrameAncestors = nil

		w := httptest.NewRecorder()
		embedOf(t, c).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("CSP = %q, want frame-ancestors 'none' when no resolver is configured", csp)
		}
	})
}

// --- which gate decided, per arrival shape ----------------------------------

// TestSameOriginOnlyDecidesWrites walks every arrival shape and asserts what
// the origin check does with it.
//
// 403 means SameOriginOnly refused it. 200 means it allowed it and the request
// reached the handler, which answers a grant-less POST by re-rendering the
// form with a fresh grant -- so 200 is the "the gate let this through" answer.
//
// It probes the real write route rather than an arbitrary one, because the
// gate is no longer global on this surface: it is handed to embedapp and wraps
// the form POST alone, so that the Stripe webhook can share this listener
// without sitting behind it. An earlier version of this test posted to
// /healthz and read a 405 as "allowed", which stopped meaning anything the
// moment the gate became per-route -- and worse, would have gone on passing if
// the gate had been dropped from the form POST altogether.
func TestSameOriginOnlyDecidesWrites(t *testing.T) {
	cfg := newConfig(t, []types.Origin{mustOrigin(t, "https://schoenstatt-austin.us")}, sqldb.Infrastructure)
	h := embedOf(t, cfg)

	const host = "f.schoenstatt.link"

	tests := []struct {
		name    string
		method  string
		headers map[string]string
		want    int
		why     string
	}{
		{
			name:   "cross-site GET of a blank form is allowed",
			method: http.MethodGet,
			headers: map[string]string{
				"Sec-Fetch-Site": "cross-site",
				"Sec-Fetch-Dest": "iframe",
			},
			want: http.StatusOK,
			why:  "the embedded form is framed from another site, so this must never be refused",
		},
		{
			name:    "cross-site POST is refused",
			method:  http.MethodPost,
			headers: map[string]string{"Sec-Fetch-Site": "cross-site"},
			want:    http.StatusForbidden,
			why:     "writing is the whole attack this check exists for",
		},
		{
			name:    "same-site POST is refused",
			method:  http.MethodPost,
			headers: map[string]string{"Sec-Fetch-Site": "same-site"},
			want:    http.StatusForbidden,
			why:     "a sibling subdomain is not us",
		},
		{
			name:    "same-origin POST is allowed",
			method:  http.MethodPost,
			headers: map[string]string{"Sec-Fetch-Site": "same-origin"},
			want:    http.StatusOK,
			why:     "our own iframe posting to our own origin is the normal case",
		},
		{
			name:    "directly navigated POST is allowed",
			method:  http.MethodPost,
			headers: map[string]string{"Sec-Fetch-Site": "none"},
			want:    http.StatusOK,
			why:     "none means the address was typed or bookmarked",
		},
		{
			name:    "matching Origin with no fetch metadata is allowed",
			method:  http.MethodPost,
			headers: map[string]string{"Origin": "https://" + host},
			want:    http.StatusOK,
			why:     "the fallback for a browser too old to send Sec-Fetch-Site",
		},
		{
			name:    "mismatched Origin with no fetch metadata is refused",
			method:  http.MethodPost,
			headers: map[string]string{"Origin": "https://evil.test"},
			want:    http.StatusForbidden,
			why:     "the fallback still has to refuse the obvious case",
		},
		{
			name:    "plaintext Origin is refused",
			method:  http.MethodPost,
			headers: map[string]string{"Origin": "http://" + host},
			want:    http.StatusForbidden,
			why:     "everything here is HTTPS-only, so the scheme is compared rather than stripped",
		},
		{
			name:    "null Origin is allowed",
			method:  http.MethodPost,
			headers: map[string]string{"Origin": "null"},
			want:    http.StatusOK,
			why:     "a null Origin is uninformative, not a mismatch -- refusing it broke the parent project's own form posts",
		},
		{
			name:    "no metadata at all is allowed",
			method:  http.MethodPost,
			headers: nil,
			want:    http.StatusOK,
			why:     "the documented hole: a command-line client, not a browser being used against somebody. The submission grant is what guards this route",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "https://"+host+"/f/"+theForm, nil)
			r.Host = host

			// The content type every one of these carries, so that the gate
			// being tested is the origin one. Without it the writes are
			// refused a step earlier, by the allowlist that exists to keep
			// this surface from ever parsing a multipart body, and every row
			// here would assert 415 about the wrong thing.
			r.Header.Set("Content-Type", web.FormEncoded)

			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}

			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != tc.want {
				t.Errorf("got %d, want %d\n  %s", w.Code, tc.want, tc.why)
			}
		})
	}
}

// --- readiness --------------------------------------------------------------

// TestHealthFailsOnSchemaMismatch is the test for the silent rollback.
//
// In the sibling project a rollback restored the binary onto a newer schema;
// the old binary started cleanly because its DDL was idempotent, and its
// health check was a ping, so the deploy saw 200 while every real request
// failed on a missing column. A health check that cannot fail is not a health
// check, so this asserts that it can.
func TestHealthFailsOnSchemaMismatch(t *testing.T) {
	cfg := newConfig(t, nil, sqldb.Expected{
		"schema_meta": {"id", "version", "applied_at", "a_column_a_later_release_adds"},
	})

	w := httptest.NewRecorder()
	embedOf(t, cfg).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503 when the database does not match the binary", w.Code)
	}

	// The detail belongs in the log, not in a body a stranger can fetch.
	if body := w.Body.String(); strings.Contains(body, "a_column_a_later_release_adds") {
		t.Errorf("health body = %q, want it to name nothing about the schema", body)
	}
}

func TestHealthPassesOnMatchingSchema(t *testing.T) {
	cfg := newConfig(t, nil, sqldb.Infrastructure)

	w := httptest.NewRecorder()
	adminOf(t, cfg).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200 on a schema this binary understands", w.Code)
	}
}

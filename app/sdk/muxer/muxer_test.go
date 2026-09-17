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
	"github.com/jroedel/dropin-forms/app/sdk/muxer"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/access/stores/accessdb"
	"github.com/jroedel/dropin-forms/business/domain/user/stores/userdb"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/logger"
	"github.com/jroedel/dropin-forms/foundation/mail"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
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

	renderer, err := page.NewRenderer(log, authapp.Templates)
	if err != nil {
		t.Fatalf("building the renderer: %v", err)
	}

	return muxer.Config{
		Log:      log,
		DB:       db,
		Expected: expected,
		FrameAncestors: func(*http.Request) []types.Origin {
			return origins
		},

		Users:        userbus.NewBusiness(log, userdb.NewStore(db)),
		Access:       accessbus.NewBusiness(log, accessdb.NewStore(db)),
		Mail:         &mail.Recorder{},
		Render:       renderer,
		AdminBaseURL: "https://forms.test",
	}
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
		"embed": muxer.Embed(cfg),
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

	w := httptest.NewRecorder()
	muxer.Embed(cfg).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

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
			muxer.Embed(c).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

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
		muxer.Embed(c).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

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
// 403 means SameOriginOnly refused it. 405 means SameOriginOnly allowed it and
// the request reached the mux, which has no POST route yet -- so 405 is the
// "the gate let this through" answer, and it tests the gate independently of
// any route.
func TestSameOriginOnlyDecidesWrites(t *testing.T) {
	cfg := newConfig(t, []types.Origin{mustOrigin(t, "https://schoenstatt-austin.us")}, sqldb.Infrastructure)
	h := muxer.Embed(cfg)

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
			want:    http.StatusMethodNotAllowed,
			why:     "our own iframe posting to our own origin is the normal case",
		},
		{
			name:    "directly navigated POST is allowed",
			method:  http.MethodPost,
			headers: map[string]string{"Sec-Fetch-Site": "none"},
			want:    http.StatusMethodNotAllowed,
			why:     "none means the address was typed or bookmarked",
		},
		{
			name:    "matching Origin with no fetch metadata is allowed",
			method:  http.MethodPost,
			headers: map[string]string{"Origin": "https://" + host},
			want:    http.StatusMethodNotAllowed,
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
			want:    http.StatusMethodNotAllowed,
			why:     "a null Origin is uninformative, not a mismatch -- refusing it broke the parent project's own form posts",
		},
		{
			name:    "no metadata at all is allowed",
			method:  http.MethodPost,
			headers: nil,
			want:    http.StatusMethodNotAllowed,
			why:     "the documented hole: a command-line client, not a browser being used against somebody. The submission grant is what guards this route",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "https://"+host+"/healthz", nil)
			r.Host = host
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
	muxer.Embed(cfg).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

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

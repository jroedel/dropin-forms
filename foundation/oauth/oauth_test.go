package oauth_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/foundation/oauth"
)

// documents serves a metadata document per path. Each describes itself by
// its own URL unless the path says otherwise, which is the one thing a
// document must get right.
func documents(t *testing.T) (*httptest.Server, *oauth.Fetcher) {
	t.Helper()

	var srv *httptest.Server

	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		self := srv.URL + r.URL.Path

		switch r.URL.Path {
		case "/good":
			json.NewEncoder(w).Encode(oauth.Client{ID: self, Name: "Claude", RedirectURIs: []string{"https://claude.ai/api/mcp/auth_callback"}})
		case "/impostor":
			json.NewEncoder(w).Encode(oauth.Client{ID: "https://claude.ai/oauth/real", RedirectURIs: []string{"https://evil.example/cb"}})
		case "/no-redirects":
			json.NewEncoder(w).Encode(oauth.Client{ID: self})
		case "/moved":
			http.Redirect(w, r, "/good", http.StatusFound)
		case "/huge":
			w.Write([]byte(`{"client_id": "` + strings.Repeat("x", 70<<10) + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	// The test server's own client, for its certificate, with this
	// package's rule about redirects.
	hc := srv.Client()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return srv, oauth.NewFetcher(hc)
}

func TestFetchReadsADocumentThatDescribesItself(t *testing.T) {
	srv, f := documents(t)

	c, err := f.Fetch(t.Context(), srv.URL+"/good")
	if err != nil || c.Name != "Claude" || len(c.RedirectURIs) != 1 {
		t.Fatalf("Fetch: %+v %v", c, err)
	}

	for _, path := range []string{"/impostor", "/no-redirects", "/moved", "/huge", "/missing"} {
		if _, err := f.Fetch(t.Context(), srv.URL+path); !errors.Is(err, oauth.ErrBadClient) {
			t.Errorf("%s: %v", path, err)
		}
	}

	// Not even asked for: anything but a plain https URL.
	for _, id := range []string{"http://claude.ai/x", "claude", "https://user@claude.ai/x", "https://claude.ai/x#frag", ""} {
		if _, err := f.Fetch(t.Context(), id); !errors.Is(err, oauth.ErrBadClient) {
			t.Errorf("%q: %v", id, err)
		}
	}
}

func TestARedirectMatchesExactlyOrAsALoopbackOnAnyPort(t *testing.T) {
	c := oauth.Client{RedirectURIs: []string{
		"https://claude.ai/api/mcp/auth_callback",
		"http://localhost/callback",
		"http://127.0.0.1/callback",
	}}

	for uri, want := range map[string]bool{
		"https://claude.ai/api/mcp/auth_callback":      true,
		"http://localhost:3118/callback":               true,
		"http://127.0.0.1:50211/callback":              true,
		"http://localhost/callback":                    true,
		"https://claude.ai/api/mcp/auth_callback?x=1":  false,
		"https://claude.ai:8443/api/mcp/auth_callback": false,
		"http://localhost:3118/elsewhere":              false,
		"https://localhost:3118/callback":              false,
		"http://127.0.0.2:3118/callback":               false, // loopback, but not the host it listed
		"http://evil.example:3118/callback":            false,
		"http://localhost.evil.example/callback":       false,
	} {
		if got := c.RedirectAllowed(uri); got != want {
			t.Errorf("%s: %v, want %v", uri, got, want)
		}
	}
}

func TestPKCEIsS256(t *testing.T) {
	// RFC 7636, appendix B.
	const (
		verifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
		challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	)

	if !oauth.ValidChallenge(challenge) || oauth.ValidChallenge("too-short") || oauth.ValidChallenge(challenge+"=") {
		t.Error("ValidChallenge")
	}

	if !oauth.VerifyPKCE(challenge, verifier) {
		t.Error("the RFC's own pair does not verify")
	}

	for _, v := range []string{verifier[1:], strings.Repeat("a", 129), verifier[:42] + "!"} {
		if oauth.VerifyPKCE(challenge, v) {
			t.Errorf("%q verified", v)
		}
	}
}

func TestRedirectWithKeepsTheRedirectsOwnQuery(t *testing.T) {
	got, err := oauth.RedirectWith("https://claude.ai/cb?keep=1", url.Values{"code": {"abc"}, "state": {""}})
	if err != nil || got != "https://claude.ai/cb?code=abc&keep=1" {
		t.Errorf("%q %v", got, err)
	}
}

func TestServerMetadataOffersWhatClaudeLooksFor(t *testing.T) {
	m := oauth.NewServerMetadata("https://forms.example/", "/oauth/authorize", "/oauth/token")

	raw, _ := json.Marshal(m)

	var doc map[string]any
	json.Unmarshal(raw, &doc)

	// The two Claude checks before it uses a metadata document rather than
	// looking for a registration endpoint that is not there.
	if doc["client_id_metadata_document_supported"] != true || doc["token_endpoint_auth_methods_supported"].([]any)[0] != "none" {
		t.Errorf("%s", raw)
	}

	if m.Issuer != "https://forms.example" || m.TokenEndpoint != "https://forms.example/oauth/token" {
		t.Errorf("%+v", m)
	}
}

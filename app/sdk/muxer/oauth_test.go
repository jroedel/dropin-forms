package muxer_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/foundation/oauth"
)

// Connecting Claude on claude.ai: the discovery documents, the page where
// somebody agrees, and the token endpoint, through the admin surface as it is
// mounted.

const (
	claudeClient   = "https://claude.ai/oauth/test-client-metadata"
	claudeCallback = "https://claude.ai/api/mcp/auth_callback"

	// RFC 7636, appendix B.
	pkceVerifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	pkceChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
)

// claudeDocs is the metadata documents these tests know, in place of the
// network: Claude's, and nothing else. A document on any other host must
// never be read at all, so asking for one fails the test.
type claudeDocs struct{ t *testing.T }

func (c claudeDocs) Fetch(_ context.Context, id string) (oauth.Client, error) {
	if id != claudeClient {
		c.t.Errorf("a metadata document was read for %s", id)

		return oauth.Client{}, oauth.ErrBadClient
	}

	return oauth.Client{ID: claudeClient, Name: "Claude", RedirectURIs: []string{claudeCallback}}, nil
}

func authorizeQuery(client, redirect string) url.Values {
	return url.Values{
		"client_id":             {client},
		"redirect_uri":          {redirect},
		"state":                 {"st-123"},
		"code_challenge":        {pkceChallenge},
		"code_challenge_method": {"S256"},
		"response_type":         {"code"},
	}
}

// token is what Claude's servers do: a form POST with no cookie and no Origin.
func token(t *testing.T, a harness, code string) *httptest.ResponseRecorder {
	t.Helper()

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {claudeClient},
		"redirect_uri":  {claudeCallback},
		"code_verifier": {pkceVerifier},
	}

	r := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)

	return w
}

// agree presses Allow on the page and returns the code Claude is sent back
// with.
func agree(t *testing.T, a harness, cookie string) string {
	t.Helper()

	form := authorizeQuery(claudeClient, claudeCallback)
	form.Set("answer", "allow")

	w := a.post(t, "/oauth/authorize", form, cookie)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("Allow = %d:\n%s", w.Code, short(w.Body.String()))
	}

	back, err := url.Parse(w.Header().Get("Location"))
	if err != nil || back.Scheme+"://"+back.Host+back.Path != claudeCallback {
		t.Fatalf("Allow went to %q", w.Header().Get("Location"))
	}

	q := back.Query()
	if q.Get("state") != "st-123" || q.Get("iss") != "https://forms.test" || q.Get("code") == "" {
		t.Fatalf("sent back with %v", q)
	}

	return q.Get("code")
}

// The whole connection, in the order claude.ai makes it.
func TestClaudeAIConnectsBySigningIn(t *testing.T) {
	a := newAdmin(t, "")
	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")

	// 1. The MCP endpoint, with nothing: refused, with where to sign in.
	w := rpc(t, a, "", map[string]any{"id": 1, "method": "initialize"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("MCP with no key = %d", w.Code)
	}

	const resourceDoc = "https://forms.test/.well-known/oauth-protected-resource/api/mcp"
	if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, `resource_metadata="`+resourceDoc+`"`) {
		t.Errorf("challenge = %q, want it to name %s", got, resourceDoc)
	}

	// 2. The two documents that say where.
	var resource map[string]any
	json.Unmarshal(a.get(t, "/.well-known/oauth-protected-resource/api/mcp", "").Body.Bytes(), &resource)

	if resource["resource"] != "https://forms.test/api/mcp" || resource["authorization_servers"].([]any)[0] != "https://forms.test" {
		t.Errorf("protected resource = %v", resource)
	}

	var server map[string]any
	json.Unmarshal(a.get(t, "/.well-known/oauth-authorization-server", "").Body.Bytes(), &server)

	if server["issuer"] != "https://forms.test" || server["token_endpoint"] != "https://forms.test/oauth/token" ||
		server["client_id_metadata_document_supported"] != true {
		t.Errorf("authorization server = %v", server)
	}

	// 3. The page, signed out: sent to sign in, and brought back after.
	page := "/oauth/authorize?" + authorizeQuery(claudeClient, claudeCallback).Encode()

	w = a.get(t, page, "")
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/signin?next=") {
		t.Fatalf("the page signed out = %d to %q, want the sign-in page", w.Code, w.Header().Get("Location"))
	}

	// 4. Signed in: the question, with a policy that lets Allow reach Claude.
	cookie := sessionFor(t, a, volunteer)

	w = a.get(t, page, cookie)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "volunteer@schoenstatt.test") || !strings.Contains(w.Body.String(), "Claude (claude.ai)") {
		t.Fatalf("the page = %d:\n%s", w.Code, short(w.Body.String()))
	}

	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https://claude.ai") {
		t.Errorf("CSP = %q; Allow would go nowhere", csp)
	}

	// 5. Allow, and the code traded for a key.
	code := agree(t, a, cookie)

	w = token(t, a, code)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("token = %d (%q):\n%s", w.Code, w.Header().Get("Cache-Control"), w.Body.String())
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	json.Unmarshal(w.Body.Bytes(), &tok)

	if !strings.HasPrefix(tok.AccessToken, "dfa_") || tok.TokenType != "Bearer" || tok.ExpiresIn < 89*24*3600 {
		t.Fatalf("token = %+v", tok)
	}

	// 6. Claude uses it, as the volunteer.
	tr, body := useTool(t, a, tok.AccessToken, "whoami", nil)
	if tr.IsError || body["email"] != "volunteer@schoenstatt.test" {
		t.Errorf("whoami = %v", body)
	}

	// And the code is spent.
	if w := token(t, a, code); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_grant") {
		t.Errorf("a spent code = %d %s", w.Code, w.Body.String())
	}

	// 7. Listed on the key page as Claude's, and revoked from there.
	keysPage := a.get(t, "/account/keys", cookie).Body.String()
	if !strings.Contains(keysPage, "Claude (claude.ai)") {
		t.Errorf("the key page does not list Claude's key:\n%s", short(keysPage))
	}
}

func TestReconnectingReplacesClaudesKeyAndTheOldOneSaysSignInAgain(t *testing.T) {
	a := newAdmin(t, "")
	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	first := tokenOf(t, token(t, a, agree(t, a, cookie)))
	second := tokenOf(t, token(t, a, agree(t, a, cookie)))

	w := rpc(t, a, first, map[string]any{"id": 1, "method": "tools/list"})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Errorf("the replaced key = %d %q, want 401 invalid_token", w.Code, w.Header().Get("WWW-Authenticate"))
	}

	rpcResult(t, rpc(t, a, second, map[string]any{"id": 1, "method": "tools/list"}))
}

func tokenOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()

	var tok struct {
		AccessToken string `json:"access_token"`
	}

	if err := json.Unmarshal(w.Body.Bytes(), &tok); err != nil || tok.AccessToken == "" {
		t.Fatalf("token = %d %s", w.Code, w.Body.String())
	}

	return tok.AccessToken
}

// A program that cannot be trusted, or a redirect its document does not
// list, is answered on our own page and never sent anywhere: a redirect with
// an error to an unchecked address is an open redirect with our name on it.
func TestAnUntrustedRequestIsAnsweredHereAndNeverRedirected(t *testing.T) {
	a := newAdmin(t, "")
	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	for name, q := range map[string]url.Values{
		"a program on another host":       authorizeQuery("https://evil.example/client", "https://evil.example/cb"),
		"a lookalike host":                authorizeQuery("https://claude.ai.evil.example/client", "https://evil.example/cb"),
		"a redirect Claude does not list": authorizeQuery(claudeClient, "https://evil.example/cb"),
	} {
		t.Run(name, func(t *testing.T) {
			w := a.get(t, "/oauth/authorize?"+q.Encode(), cookie)
			if w.Code != http.StatusBadRequest || w.Header().Get("Location") != "" {
				t.Errorf("= %d to %q, want 400 here", w.Code, w.Header().Get("Location"))
			}

			q.Set("answer", "allow")
			if w := a.post(t, "/oauth/authorize", q, cookie); w.Code != http.StatusBadRequest || w.Header().Get("Location") != "" {
				t.Errorf("Allow = %d to %q, want 400 here", w.Code, w.Header().Get("Location"))
			}
		})
	}
}

// Every other refusal goes back to Claude, which is how it finds out.
func TestARefusalOfATrustedRequestGoesBackToClaude(t *testing.T) {
	a := newAdmin(t, "")
	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	no := authorizeQuery(claudeClient, claudeCallback)
	no.Set("answer", "deny")

	noPKCE := authorizeQuery(claudeClient, claudeCallback)
	noPKCE.Del("code_challenge")
	noPKCE.Set("answer", "allow")

	for name, tc := range map[string]struct {
		form url.Values
		want string
	}{
		"saying no": {no, "access_denied"},
		"no PKCE":   {noPKCE, "invalid_request"},
	} {
		t.Run(name, func(t *testing.T) {
			w := a.post(t, "/oauth/authorize", tc.form, cookie)

			back, _ := url.Parse(w.Header().Get("Location"))
			if w.Code != http.StatusSeeOther || back == nil || back.Query().Get("error") != tc.want || back.Query().Get("code") != "" {
				t.Errorf("= %d to %q, want an %s back to Claude", w.Code, w.Header().Get("Location"), tc.want)
			}
		})
	}
}

// The answer is a write a person makes, and is behind the same check as every
// other: another site cannot press Allow for somebody who is signed in.
func TestAnotherSiteCannotPressAllow(t *testing.T) {
	a := newAdmin(t, "")
	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	form := authorizeQuery(claudeClient, claudeCallback)
	form.Set("answer", "allow")

	r := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.AddCookie(&http.Cookie{Name: mid.CookieName, Value: cookie})

	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("a cross-site Allow = %d, want 403", w.Code)
	}
}

// Package oauthapp is how Claude on claude.ai signs in as somebody: the
// discovery documents, the page where the person agrees, and the token
// endpoint that hands Claude its key.
//
// It exists because a claude.ai custom connector can only reach a server that
// signs in this way: there is nowhere in one to paste a key. What Claude is
// given is an ordinary personal key (apikeybus/oauth.go), so the API and MCP
// endpoint it then uses are exactly what a key from the key page reaches,
// behind the same gates.
//
// This is /opt/projects/stewards' oauthapp, carried over: the same protocol,
// the same refusals, and the same reasoning, which is repeated here because a
// reader of this repository should not have to open another to find it.
//
// # Who may ask
//
// Programs are known by a Client ID Metadata Document (foundation/oauth): the
// client_id is an HTTPS URL, and the document there says where the code may
// be sent. Only documents on Anthropic's hosts are read. Any program could
// publish a document, and somebody who is shown "Let some-app.example work as
// you?" by a link in an email is the phishing this page would otherwise be.
// Claude is the program this was built for; another is a decision, and a line
// in trustedHosts.
//
// # The two kinds of refusal
//
// RFC 6749 is particular about this, and it is a security rule rather than a
// style. A request whose program or redirect cannot be trusted is answered
// here, on our own page, and never sent anywhere: redirecting with an error to
// an unchecked redirect_uri is an open redirect with our name on it. Every
// other refusal -- a missing PKCE challenge, the person saying no -- goes back
// to the program at its checked redirect, which is how it finds out.
//
// # What guards each route
//
//	GET  /.well-known/oauth-authorization-server   nothing: a public document
//	GET  /oauth/authorize                           Require: the page
//	POST /oauth/authorize                           Require: the answer
//	POST /oauth/token                               nothing but the code
//
// The page and its answer are on the browser's chain, so a person who is not
// signed in is sent to sign in and brought back, and the answer is behind the
// same origin and form checks as every other write a person makes. The token
// endpoint is called by Claude's servers rather than by a browser, carries no
// cookie, and is mounted beside the API rather than on the browser's chain;
// the code it is handed is the whole of its credential.
package oauthapp

import (
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/apikey/apikeybus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/oauth"
	"github.com/jroedel/dropin-forms/foundation/web"
)

//go:embed templates
var templates embed.FS

// Templates is this app's template directory, for page.NewRenderer.
var Templates = templates

// The paths. The discovery document's is fixed by RFC 8414 for an issuer with
// no path, which the admin surface's origin is.
const (
	MetadataPath  = "/.well-known/oauth-authorization-server"
	AuthorizePath = "/oauth/authorize"
	TokenPath     = "/oauth/token"
)

// trustedHosts are the hosts whose metadata documents are read, with their
// subdomains. Claude's are on claude.ai today (Claude Code's is
// https://claude.ai/oauth/claude-code-client-metadata); the other two are
// Anthropic's own, so that Claude moving its document between them is not a
// failed connection to debug.
var trustedHosts = []string{"claude.ai", "claude.com", "anthropic.com"}

// Grants is the slice of the key domain this app uses.
type Grants interface {
	GrantAccess(ctx context.Context, now time.Time, userID types.ID, clientID, name, redirect, challenge string) (string, error)
	RedeemGrant(ctx context.Context, now time.Time, code, clientID, redirect, verifier string) (apikeybus.Key, string, error)
}

// Clients reads a program's metadata document. foundation/oauth.Fetcher, or a
// test's stand-in for the network.
type Clients interface {
	Fetch(ctx context.Context, clientID string) (oauth.Client, error)
}

// Config is what this app needs.
type Config struct {
	Log     *slog.Logger
	Render  *page.Renderer
	Grants  Grants
	Clients Clients

	// BaseURL is the admin surface's origin, which is the issuer: the
	// discovery document and the iss on every answer say it, and a program
	// checks that they agree.
	BaseURL string

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

type app struct {
	cfg  Config
	meta oauth.ServerMetadata
}

func (a app) now() time.Time {
	if a.cfg.Now != nil {
		return a.cfg.Now()
	}

	return time.Now()
}

func newApp(cfg Config) app {
	return app{cfg: cfg, meta: oauth.NewServerMetadata(cfg.BaseURL, AuthorizePath, TokenPath)}
}

// Routes mounts the discovery document and the page on the browser's mux.
// guard is Require.
func Routes(mux *http.ServeMux, cfg Config, guard func(http.Handler) http.Handler) {
	a := newApp(cfg)

	mux.HandleFunc("GET "+MetadataPath, a.metadata)
	mux.Handle("GET "+AuthorizePath, guard(http.HandlerFunc(a.ask)))
	mux.Handle("POST "+AuthorizePath, guard(http.HandlerFunc(a.answer)))
}

// TokenHandler is the token endpoint, for the muxer to mount off the
// browser's chain. See the package comment.
func TokenHandler(cfg Config) http.Handler {
	return http.HandlerFunc(newApp(cfg).token)
}

func (a app) metadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.meta)
}

// --- the request ----------------------------------------------------------------

// request is an authorization request, from the query on the way in and from
// the form's hidden fields on the way out. Both are read and checked the same
// way: the hidden fields are only the query carried across one page, and the
// person's browser can change them as easily.
type request struct {
	ClientID, Redirect, State, Challenge, Method, ResponseType string
}

func requestFrom(v url.Values) request {
	return request{
		ClientID:     v.Get("client_id"),
		Redirect:     v.Get("redirect_uri"),
		State:        v.Get("state"),
		Challenge:    v.Get("code_challenge"),
		Method:       v.Get("code_challenge_method"),
		ResponseType: v.Get("response_type"),
	}
}

// The sentences for a request that is answered on our own page.
const (
	sayNotClaude        = "Only Claude can connect to this service this way. If you did not start this, close the page: nothing has happened."
	sayCannotReadClient = "We could not check who is asking to connect. Go back to Claude and try connecting again in a minute."
	sayBadRedirect      = "This connection asked to send you somewhere the program does not list as its own, so we stopped it. Go back to Claude and try connecting again."
	sayCannotRead       = "We could not read that. Go back to Claude and try connecting again."
)

// client reads and checks the program and its redirect: the checks whose
// failure is answered on our own page.
func (a app) client(ctx context.Context, req request) (oauth.Client, string, error) {
	if !trusted(req.ClientID) {
		a.cfg.Log.Warn("an oauth request from a program that is not trusted", "client_id", req.ClientID)

		return oauth.Client{}, sayNotClaude, errUntrusted
	}

	c, err := a.cfg.Clients.Fetch(ctx, req.ClientID)
	if err != nil {
		a.cfg.Log.Warn("an oauth program's metadata document could not be used", "client_id", req.ClientID, "error", err)

		return oauth.Client{}, sayCannotReadClient, err
	}

	if !c.RedirectAllowed(req.Redirect) {
		a.cfg.Log.Warn("an oauth request asked to go back somewhere its program does not list", "client_id", req.ClientID, "redirect_uri", req.Redirect)

		return oauth.Client{}, sayBadRedirect, errBadRedirect
	}

	return c, "", nil
}

var (
	errUntrusted   = errors.New("the client is not on a trusted host")
	errBadRedirect = errors.New("the redirect is not one the client lists")
)

func trusted(clientID string) bool {
	u, err := url.Parse(clientID)
	if err != nil || u.Scheme != "https" {
		return false
	}

	host := u.Hostname()
	for _, t := range trustedHosts {
		if host == t || strings.HasSuffix(host, "."+t) {
			return true
		}
	}

	return false
}

// refusal is what a request that passed client() lacks, as the OAuth error to
// send back, or "" for nothing.
func (req request) refusal() (code, description string) {
	switch {
	case req.ResponseType != "code":
		return "unsupported_response_type", "only the authorization code flow is offered"
	case req.Method != "S256" || !oauth.ValidChallenge(req.Challenge):
		return "invalid_request", "a PKCE code_challenge made with S256 is required"
	}

	return "", ""
}

// back sends the person to the program with params, and the state and issuer
// every answer carries.
func (a app) back(w http.ResponseWriter, r *http.Request, req request, params url.Values) {
	params.Set("state", req.State)
	params.Set("iss", a.meta.Issuer)

	to, err := oauth.RedirectWith(req.Redirect, params)
	if err != nil {
		// Checked against the program's document already, so this is a
		// document listing something that is not a URL.
		a.refuse(w, r, sayBadRedirect)

		return
	}

	http.Redirect(w, r, to, http.StatusSeeOther)
}

// --- the page -------------------------------------------------------------------

type connectView struct {
	Request request

	Name    string // what the document calls itself
	Host    string // who is asking, as the client_id says
	KeyName string // what the key page will list
	BackTo  string // where Allow sends the person
	Email   string
	Days    int

	Problem string
}

// ask is the page where somebody agrees, or not.
func (a app) ask(w http.ResponseWriter, r *http.Request) {
	req := requestFrom(r.URL.Query())

	c, say, err := a.client(r.Context(), req)
	if err != nil {
		a.refuse(w, r, say)

		return
	}

	if code, desc := req.refusal(); code != "" {
		a.back(w, r, req, url.Values{"error": {code}, "error_description": {desc}})

		return
	}

	a.show(w, r, http.StatusOK, c, req, "")
}

// answer is the person's yes or no.
func (a app) answer(w http.ResponseWriter, r *http.Request) {
	me, ok := mid.UserFrom(r.Context())
	if !ok {
		a.cfg.Log.Error("the oauth page was answered with no account", "request_id", web.RequestIDFrom(r.Context()))
		http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)

		return
	}

	if err := r.ParseForm(); err != nil {
		a.refuse(w, r, sayCannotRead)

		return
	}

	req := requestFrom(r.PostForm)

	c, say, err := a.client(r.Context(), req)
	if err != nil {
		a.refuse(w, r, say)

		return
	}

	if code, desc := req.refusal(); code != "" {
		a.back(w, r, req, url.Values{"error": {code}, "error_description": {desc}})

		return
	}

	if r.PostForm.Get("answer") != "allow" {
		a.cfg.Log.Info("somebody said no to an oauth program", "user_id", me.ID.String(), "client_id", req.ClientID)
		a.back(w, r, req, url.Values{"error": {"access_denied"}, "error_description": {"the person said no"}})

		return
	}

	code, err := a.cfg.Grants.GrantAccess(r.Context(), a.now(), me.ID, req.ClientID, keyName(c), req.Redirect, req.Challenge)

	switch {
	case errors.Is(err, apikeybus.ErrTooManyGrants):
		a.show(w, r, http.StatusTooManyRequests, c, req, "You have agreed several times in the last few minutes. Wait five minutes, then go back to Claude and connect again.")

		return
	case err != nil:
		a.cfg.Log.Error("an oauth grant could not be made", "request_id", web.RequestIDFrom(r.Context()), "error", err)
		a.back(w, r, req, url.Values{"error": {"server_error"}})

		return
	}

	a.back(w, r, req, url.Values{"code": {code}})
}

func (a app) show(w http.ResponseWriter, r *http.Request, status int, c oauth.Client, req request, problem string) {
	me, _ := mid.UserFrom(r.Context())

	back, _ := url.Parse(req.Redirect) // checked in client()

	// The form's answer is a redirect to the program, which form-action must
	// allow; see page.AllowFormTo.
	page.AllowFormTo(w.Header(), back.Scheme+"://"+back.Host)

	a.cfg.Render.Render(w, r, status, "connect", connectView{
		Request: req,
		Name:    cmp.Or(c.Name, c.Host()),
		Host:    c.Host(),
		KeyName: keyName(c),
		BackTo:  back.Host,
		Email:   me.Email.String(),
		Days:    int(apikeybus.OAuthKeyLife / (24 * time.Hour)),
		Problem: problem,
	})
}

func (a app) refuse(w http.ResponseWriter, r *http.Request, say string) {
	a.cfg.Render.Render(w, r, http.StatusBadRequest, "connect", connectView{Problem: say})
}

// keyName is what the key page lists the program's key as: its own name, and
// the host its document is on, since the name is only what the document says
// about itself.
func keyName(c oauth.Client) string {
	if c.Name == "" {
		return c.Host()
	}

	return c.Name + " (" + c.Host() + ")"
}

// --- the token ------------------------------------------------------------------

// maxTokenBody is far more than a token request needs: four short fields.
const maxTokenBody = 16 << 10

// token trades a code for the key. A program's request, not a person's: the
// answers are the RFC's JSON, never a page.
func (a app) token(w http.ResponseWriter, r *http.Request) {
	// A token is a credential. Neither it nor a refusal may be cached by
	// anything between here and the program.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")

	r.Body = http.MaxBytesReader(w, r.Body, maxTokenBody)

	if !strings.HasPrefix(r.Header.Get("Content-Type"), web.FormEncoded) {
		writeJSON(w, http.StatusBadRequest, oauth.TokenError{Code: oauth.InvalidRequest, Description: "send the request as " + web.FormEncoded})

		return
	}

	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, oauth.TokenError{Code: oauth.InvalidRequest, Description: "the body could not be read as a form"})

		return
	}

	f := r.PostForm

	if f.Get("grant_type") != "authorization_code" {
		writeJSON(w, http.StatusBadRequest, oauth.TokenError{Code: oauth.UnsupportedGrantType, Description: "only authorization_code is offered"})

		return
	}

	for _, name := range []string{"code", "client_id", "redirect_uri", "code_verifier"} {
		if f.Get(name) == "" {
			writeJSON(w, http.StatusBadRequest, oauth.TokenError{Code: oauth.InvalidRequest, Description: name + " is required"})

			return
		}
	}

	now := a.now()

	k, key, err := a.cfg.Grants.RedeemGrant(r.Context(), now, f.Get("code"), f.Get("client_id"), f.Get("redirect_uri"), f.Get("code_verifier"))

	switch {
	case errors.Is(err, apikeybus.ErrRefused):
		writeJSON(w, http.StatusBadRequest, oauth.TokenError{Code: oauth.InvalidGrant, Description: "the code is not valid: it may have been used, have expired, or belong to another request"})

		return
	case err != nil:
		a.cfg.Log.Error("an oauth code could not be traded for a key", "request_id", web.RequestIDFrom(r.Context()), "error", err)
		writeJSON(w, http.StatusInternalServerError, oauth.TokenError{Code: "server_error"})

		return
	}

	writeJSON(w, http.StatusOK, oauth.Token{
		AccessToken: key,
		TokenType:   "Bearer",
		ExpiresIn:   int64(k.ExpiresAt.Sub(now) / time.Second),
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Package oauth is the plumbing of an OAuth 2.1 authorization server that
// knows its clients by a Client ID Metadata Document: reading the document a
// client_id names, matching a redirect against it, and checking a PKCE
// verifier. Which clients to trust, who is signing in and what they are
// given are the caller's; nothing here knows any of them.
//
// # Why a metadata document, and not registration
//
// A client_id here is an HTTPS URL, and the client's details -- its name and
// where a code may be sent -- are the JSON at that URL
// (draft-ietf-oauth-client-id-metadata-document). The alternative, Dynamic
// Client Registration, is an endpoint anyone may post to and a table of
// clients that grows by one every time somebody connects. With a document
// there is nothing to register and nothing to keep: the server reads it when
// a person is asked to agree, and the code it hands out is bound to the
// client_id and redirect it was asked for.
//
// Carried over unchanged from /opt/projects/stewards, which connected to
// Claude this way first. The two copies are kept alike on purpose: a fix to
// one is very likely a fix to the other.
package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// ServerMetadata is an authorization server's RFC 8414 discovery document,
// as much of it as a server issuing codes for public clients needs to say.
type ServerMetadata struct {
	Issuer                   string   `json:"issuer"`
	AuthorizationEndpoint    string   `json:"authorization_endpoint"`
	TokenEndpoint            string   `json:"token_endpoint"`
	ResponseTypes            []string `json:"response_types_supported"`
	GrantTypes               []string `json:"grant_types_supported"`
	CodeChallengeMethods     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethods []string `json:"token_endpoint_auth_methods_supported"`
	ClientIDMetadataDocument bool     `json:"client_id_metadata_document_supported"`
	IssuerParameter          bool     `json:"authorization_response_iss_parameter_supported"`
}

// NewServerMetadata describes a server at issuer whose two endpoints are at
// the paths given: the authorization code grant with S256 PKCE, public
// clients only, and clients known by their metadata documents.
//
// "none" as the only way to authenticate at the token endpoint is what makes
// a client that has no secret -- a browser's, a phone's -- use its document
// rather than look for a registration endpoint that is not there.
func NewServerMetadata(issuer, authorizePath, tokenPath string) ServerMetadata {
	issuer = strings.TrimSuffix(issuer, "/")

	return ServerMetadata{
		Issuer:                   issuer,
		AuthorizationEndpoint:    issuer + authorizePath,
		TokenEndpoint:            issuer + tokenPath,
		ResponseTypes:            []string{"code"},
		GrantTypes:               []string{"authorization_code"},
		CodeChallengeMethods:     []string{"S256"},
		TokenEndpointAuthMethods: []string{"none"},
		ClientIDMetadataDocument: true,
		IssuerParameter:          true,
	}
}

// Client is what a client's metadata document says about it.
type Client struct {
	ID           string   `json:"client_id"`
	Name         string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
}

// Host is the client_id's host: the thing to show a person as who is asking,
// since the document's name is whatever the document says.
func (c Client) Host() string {
	u, err := url.Parse(c.ID)
	if err != nil {
		return ""
	}

	return u.Hostname()
}

// ErrBadClient is a client_id that is not a usable metadata document: not an
// HTTPS URL, not reachable, or a document that does not describe itself.
var ErrBadClient = errors.New("the client_id is not a usable client metadata document")

// maxDocument is far more than a metadata document needs. Claude Code's is
// about 300 bytes; the limit is so a URL that answers with something huge
// costs a read of 64 KB and no more.
const maxDocument = 64 << 10

// fetchTime is under the ten seconds Claude gives the whole of a sign-in
// step, and a document is one small GET.
const fetchTime = 5 * time.Second

// Fetcher reads metadata documents.
type Fetcher struct {
	client *http.Client
}

// NewFetcher builds one. hc may be nil for a client with this package's own
// timeout and no redirects followed: a document must be at the URL that is
// its client_id, since that URL is the client's identity, and a redirect to
// anywhere else is not that document.
func NewFetcher(hc *http.Client) *Fetcher {
	if hc == nil {
		hc = &http.Client{
			Timeout: fetchTime,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	return &Fetcher{client: hc}
}

// Fetch reads the document clientID names and checks that it describes
// itself: its own client_id must be the URL it was read from, and it must
// list at least one redirect. Every failure is ErrBadClient, wrapped with
// what went wrong, for the log.
func (f *Fetcher) Fetch(ctx context.Context, clientID string) (Client, error) {
	u, err := url.Parse(clientID)

	switch {
	case err != nil:
		return Client{}, fmt.Errorf("%w: %w", ErrBadClient, err)
	case u.Scheme != "https", u.Host == "", u.User != nil, u.Fragment != "":
		return Client{}, fmt.Errorf("%w: it is not a plain https URL", ErrBadClient)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return Client{}, fmt.Errorf("%w: %w", ErrBadClient, err)
	}

	req.Header.Set("Accept", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		return Client{}, fmt.Errorf("%w: %w", ErrBadClient, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Client{}, fmt.Errorf("%w: it answered %d", ErrBadClient, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument+1))

	switch {
	case err != nil:
		return Client{}, fmt.Errorf("%w: %w", ErrBadClient, err)
	case len(body) > maxDocument:
		return Client{}, fmt.Errorf("%w: the document is larger than %d bytes", ErrBadClient, maxDocument)
	}

	var c Client
	if err := json.Unmarshal(body, &c); err != nil {
		return Client{}, fmt.Errorf("%w: %w", ErrBadClient, err)
	}

	switch {
	case c.ID != clientID:
		return Client{}, fmt.Errorf("%w: its client_id is %q, not the URL it was read from", ErrBadClient, c.ID)
	case len(c.RedirectURIs) == 0:
		return Client{}, fmt.Errorf("%w: it lists no redirect_uris", ErrBadClient)
	}

	return c, nil
}

// RedirectAllowed is whether presented is one of the client's redirects.
//
// Exactly, with one exception: a loopback redirect matches whatever its port.
// A program on somebody's computer -- Claude Code -- listens on a port it is
// given when it starts, so its document can only say "http://127.0.0.1/callback"
// and mean any port (RFC 8252, 7.3). localhost is matched the same way,
// although RFC 8252 discourages it, because Claude Code's document lists it.
func (c Client) RedirectAllowed(presented string) bool {
	if slices.Contains(c.RedirectURIs, presented) {
		return true
	}

	p, err := url.Parse(presented)
	if err != nil || !loopback(p) {
		return false
	}

	return slices.ContainsFunc(c.RedirectURIs, func(registered string) bool {
		r, err := url.Parse(registered)
		if err != nil || !loopback(r) {
			return false
		}

		return r.Hostname() == p.Hostname() && r.Path == p.Path && r.RawQuery == p.RawQuery
	})
}

func loopback(u *url.URL) bool {
	if u.Scheme != "http" || u.User != nil || u.Fragment != "" {
		return false
	}

	host := u.Hostname()
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// ValidChallenge is whether a code_challenge is the shape S256 makes: the
// base64url of a SHA-256, 43 characters with no padding.
func ValidChallenge(challenge string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(challenge)

	return err == nil && len(raw) == sha256.Size
}

// VerifyPKCE is whether verifier is the one challenge was made from, by S256.
//
// The verifier's own rules (RFC 7636, 4.1: 43 to 128 characters of a small
// alphabet) are checked too. A short verifier hashes as well as a long one;
// refusing it is the client being told its PKCE is weaker than it should be,
// rather than finding out from somebody else.
func VerifyPKCE(challenge, verifier string) bool {
	if len(verifier) < 43 || len(verifier) > 128 || strings.ContainsFunc(verifier, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r))
	}) {
		return false
	}

	sum := sha256.Sum256([]byte(verifier))
	made := base64.RawURLEncoding.EncodeToString(sum[:])

	return subtle.ConstantTimeCompare([]byte(made), []byte(challenge)) == 1
}

// RedirectWith is redirect with params added to its query, keeping what it
// already had: the shape of every answer the authorization endpoint sends
// back to a client, a code or an error.
func RedirectWith(redirect string, params url.Values) (string, error) {
	u, err := url.Parse(redirect)
	if err != nil {
		return "", err
	}

	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			if v != "" {
				q.Add(k, v)
			}
		}
	}

	u.RawQuery = q.Encode()

	return u.String(), nil
}

// TokenError is the body of a refusal from the token endpoint (RFC 6749,
// 5.2). The codes are the RFC's, because a client acts on them: Claude
// treats invalid_grant as "sign in again", and anything else as a fault.
type TokenError struct {
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

// The token endpoint's refusals that this package's callers send.
const (
	InvalidRequest       = "invalid_request"
	InvalidGrant         = "invalid_grant"
	UnsupportedGrantType = "unsupported_grant_type"
)

// Token is the body of a token endpoint's success. No refresh token: see
// the caller for why.
type Token struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

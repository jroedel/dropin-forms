package google

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Signing in to Google, as a client: Google's "OAuth 2.0 for Web Server
// Applications", with PKCE.

// Verifier makes a PKCE code verifier: 32 random bytes, which is 43
// characters, inside the 43 to 128 that RFC 7636 allows.
func Verifier() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails, per its documentation.

	return base64.RawURLEncoding.EncodeToString(b)
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))

	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthURL is where to send the browser to ask the person for permission.
//
// access_type=offline asks for a refresh token, which is the whole point: the
// spreadsheet is kept up to date when nobody is signed in. prompt=consent
// asks Google to show the consent screen even to somebody who has granted it
// before, because only that screen issues a new refresh token -- without it,
// reconnecting after a revoke returns an access token alone and the
// connection dies an hour later.
func (c *Client) AuthURL(state, verifier, loginHint string) string {
	v := url.Values{
		"client_id":             {c.cfg.ClientID},
		"redirect_uri":          {c.cfg.RedirectURL},
		"response_type":         {"code"},
		"scope":                 {scopes},
		"state":                 {state},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"code_challenge":        {challenge(verifier)},
		"code_challenge_method": {"S256"},
	}

	if loginHint != "" {
		v.Set("login_hint", loginHint)
	}

	return c.cfg.AuthURL + "?" + v.Encode()
}

// Grant is what Google gave back for a code.
type Grant struct {
	Refresh string
	Access  string
	Expires time.Time

	// Email is the Google account the person chose, from the id token.
	Email string

	// Files says whether they allowed this app its files. Google's consent
	// screen lets a person untick each permission separately, and somebody
	// who unticked this one has connected an account the service can do
	// nothing with.
	Files bool
}

type tokenReply struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	IDToken      string `json:"id_token"`
}

// Exchange trades the code Google sent the browser back with for tokens.
func (c *Client) Exchange(ctx context.Context, now time.Time, code, verifier string) (Grant, error) {
	var r tokenReply

	err := c.form(ctx, c.cfg.TokenURL, url.Values{
		"code":          {code},
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
		"redirect_uri":  {c.cfg.RedirectURL},
		"grant_type":    {"authorization_code"},
		"code_verifier": {verifier},
	}, &r)
	if err != nil {
		return Grant{}, err
	}

	if r.RefreshToken == "" {
		return Grant{}, errors.New("google gave no refresh token, so the sheet could not be kept up to date")
	}

	email, err := emailOf(r.IDToken)
	if err != nil {
		return Grant{}, err
	}

	return Grant{
		Refresh: r.RefreshToken,
		Access:  r.AccessToken,
		Expires: now.Add(time.Duration(r.ExpiresIn) * time.Second),
		Email:   email,
		Files:   strings.Contains(" "+r.Scope+" ", " "+ScopeDriveFile+" "),
	}, nil
}

// Access is a short-lived token for the APIs.
type Access struct {
	Token   string
	Expires time.Time
}

// Refresh gets a fresh access token. [ErrRevoked] is the person having
// withdrawn permission, or Google having expired the connection.
func (c *Client) Refresh(ctx context.Context, now time.Time, refresh string) (Access, error) {
	var r tokenReply

	err := c.form(ctx, c.cfg.TokenURL, url.Values{
		"refresh_token": {refresh},
		"client_id":     {c.cfg.ClientID},
		"client_secret": {c.cfg.ClientSecret},
		"grant_type":    {"refresh_token"},
	}, &r)
	if err != nil {
		return Access{}, err
	}

	if r.AccessToken == "" {
		return Access{}, errors.New("google answered a refresh with no access token")
	}

	return Access{Token: r.AccessToken, Expires: now.Add(time.Duration(r.ExpiresIn) * time.Second)}, nil
}

// Revoke withdraws a token, so that Google lists this app no longer having
// access. A token already revoked is not an error.
func (c *Client) Revoke(ctx context.Context, token string) error {
	err := c.form(ctx, c.cfg.RevokeURL, url.Values{"token": {token}}, nil)
	if errors.Is(err, ErrRevoked) {
		return nil
	}

	if err != nil {
		// The revoke endpoint answers 400 invalid_token for a token it does
		// not know, which is the same outcome.
		if strings.Contains(err.Error(), "invalid_token") {
			return nil
		}

		return err
	}

	return nil
}

// emailOf reads the email claim from an id token.
//
// The signature is not checked, and that is OpenID Connect's own rule rather
// than a shortcut: a token received directly from the token endpoint over
// TLS, in answer to a request this server made with its client secret, may be
// trusted on the strength of the TLS connection (OpenID Connect Core,
// section 3.1.3.7). Nothing else here would ever accept an id token.
func emailOf(idToken string) (string, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", errors.New("google's answer has no readable id token")
	}

	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("google's id token is unreadable: %w", err)
	}

	var claims struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}

	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", fmt.Errorf("google's id token is unreadable: %w", err)
	}

	if claims.Email == "" {
		return "", errors.New("google did not say which account was connected")
	}

	return claims.Email, nil
}

package apikeybus

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/oauth"
)

// OAuth is a second way to hand a program a key. Instead of somebody copying
// one off the key page into the program's settings, the program sends them
// here to agree, and is given the key itself. It exists for Claude on
// claude.ai and its phone app, whose custom connectors can only reach a
// server that signs in this way: there is nowhere in them to paste a key. The
// program gets nothing a key made on the page would not give it -- the key is
// an ordinary one, listed on that page and revoked from it -- so everything
// apikeybus.go says about what a key can do applies unchanged.
//
// A grant is the authorization code: the person agreed, and this is the
// one-time proof of it that the program trades for the key. Kept as a key is:
// its SHA-256, never the code.
//
// This is /opt/projects/stewards' userbus/oauth.go, carried over with the
// storage changed to this package's.
//
// # One key per program, and no refresh token
//
// Connecting a program again revokes the key it was given before rather than
// adding another beside it: somebody who reconnects Claude a few times should
// not find their key page filling with keys nothing holds any more.
//
// The key lasts ninety days, and then the program must ask again. OAuth would
// have a short-lived token and a refresh token swapped for a new one each
// time; that is a second credential to store, rotate and revoke, for a
// program somebody can reconnect in two clicks four times a year. If the
// asking ever becomes a nuisance, a refresh token is the place to start.

const (
	// GrantLife is how long a program has to trade a code for its key. It
	// does so the moment it has the code; RFC 6749 asks for ten minutes at
	// most, and nothing needs more than one.
	GrantLife = 5 * time.Minute

	// OAuthKeyLife is how long a key given through OAuth works. A key made
	// on the page has no end, because the person holding it chose where to
	// put it; this one is held by a program the person never sees the inside
	// of, and its ending on its own is the backstop for the connection
	// somebody forgot they made.
	OAuthKeyLife = 90 * 24 * time.Hour

	// maxLiveGrants is how many unspent codes one account may hold. Pressing
	// Allow again on an old tab is the most anybody does by mistake.
	maxLiveGrants = 5
)

// ErrTooManyGrants is an account that has agreed more often in the last few
// minutes than anybody would on purpose.
var ErrTooManyGrants = errors.New("you have agreed several times in the last few minutes; wait five minutes and connect again")

// Grant is one authorization code, without the code.
type Grant struct {
	ID     types.ID
	UserID types.ID

	// What the code is bound to: the program it was given to, the name its
	// key will have, where it was sent, and the PKCE challenge the trade must
	// answer.
	ClientID    string
	ClientName  string
	RedirectURI string
	Challenge   string

	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    time.Time // zero while unspent
}

// GrantAccess records that somebody agreed to let a program act as them, and
// returns the code to send it back with. The caller has read the program's
// metadata document and checked the redirect against it; this is the rule
// about what a code is bound to, not about which programs may ask.
func (b *Business) GrantAccess(ctx context.Context, now time.Time, userID types.ID, clientID, name, redirect, challenge string) (string, error) {
	switch {
	case clientID == "", redirect == "":
		return "", errors.New("a grant needs the program and where to send the code")
	case !oauth.ValidChallenge(challenge):
		return "", errors.New("a grant needs an S256 PKCE challenge")
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("drawing a code: %w", err)
	}

	code := base64.RawURLEncoding.EncodeToString(raw)

	g := Grant{
		ID:          types.NewID(),
		UserID:      userID,
		ClientID:    clientID,
		ClientName:  programName(name),
		RedirectURI: redirect,
		Challenge:   challenge,
		CreatedAt:   now.UTC(),
		ExpiresAt:   now.UTC().Add(GrantLife),
	}

	made, err := b.store.CreateGrant(ctx, g, hash(code), maxLiveGrants)

	switch {
	case err != nil:
		return "", fmt.Errorf("storing the grant: %w", err)
	case !made:
		return "", ErrTooManyGrants
	}

	b.log.Info("oauth access granted", "user_id", userID.String(), "grant_id", g.ID.String(), "client_id", clientID)

	return code, nil
}

// RedeemGrant trades a code for a key, once. Every refusal is ErrRefused: the
// token endpoint's answer to all of them is invalid_grant, and the log says
// which it was.
//
// clientID and redirect must be what the code was given for, and verifier
// must answer its challenge: a code stolen on its way back to the program is
// worth nothing without the verifier, which never left the program.
func (b *Business) RedeemGrant(ctx context.Context, now time.Time, code, clientID, redirect, verifier string) (Key, string, error) {
	g, err := b.store.GrantByHash(ctx, hash(code))

	switch {
	case errors.Is(err, ErrNotFound):
		return Key{}, "", ErrRefused
	case err != nil:
		return Key{}, "", fmt.Errorf("reading the grant: %w", err)
	}

	switch {
	case !g.UsedAt.IsZero():
		b.log.Warn("an oauth code was presented twice", "grant_id", g.ID.String(), "user_id", g.UserID.String())

		return Key{}, "", ErrRefused
	case !now.Before(g.ExpiresAt):
		return Key{}, "", ErrRefused
	case g.ClientID != clientID, g.RedirectURI != redirect:
		b.log.Warn("an oauth code was presented by another program, or for another redirect", "grant_id", g.ID.String(), "client_id", clientID)

		return Key{}, "", ErrRefused
	case !oauth.VerifyPKCE(g.Challenge, verifier):
		b.log.Warn("an oauth code was presented with the wrong PKCE verifier", "grant_id", g.ID.String())

		return Key{}, "", ErrRefused
	}

	// The claim, after every check, so that a wrong verifier does not spend
	// the code: a program with a bug may try again, and a thief without the
	// verifier gets nowhere however often they try.
	claimed, err := b.store.UseGrant(ctx, g.ID, now.UTC())

	switch {
	case err != nil:
		return Key{}, "", fmt.Errorf("spending the grant: %w", err)
	case !claimed:
		return Key{}, "", ErrRefused
	}

	u, err := b.accounts.ByID(ctx, g.UserID)

	switch {
	case errors.Is(err, userbus.ErrNotFound):
		return Key{}, "", ErrRefused
	case err != nil:
		return Key{}, "", fmt.Errorf("reading the grant's account: %w", err)
	case !u.Enabled:
		return Key{}, "", ErrRefused
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Key{}, "", fmt.Errorf("drawing a key: %w", err)
	}

	secret := keyPrefix + base64.RawURLEncoding.EncodeToString(raw)

	k := Key{
		ID:        types.NewID(),
		UserID:    u.ID,
		Label:     g.ClientName,
		CreatedAt: now.UTC(),
		ExpiresAt: now.UTC().Add(OAuthKeyLife),
		Client:    g.ClientID,
	}

	if err := b.store.Replace(ctx, k, hash(secret), now.UTC()); err != nil {
		return Key{}, "", fmt.Errorf("storing the key: %w", err)
	}

	b.log.Info("api key given through oauth", "user_id", u.ID.String(), "key_id", k.ID.String(), "client_id", g.ClientID)

	return k, secret, nil
}

// programName fits what a program calls itself to a key's label: one line,
// within labelMax, and something rather than nothing.
func programName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "A program"
	}

	if utf8.RuneCountInString(name) > labelMax {
		name = string([]rune(name)[:labelMax-1]) + "…"
	}

	return name
}

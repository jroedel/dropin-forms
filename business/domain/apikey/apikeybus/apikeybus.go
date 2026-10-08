// Package apikeybus issues and checks the personal keys that let a program --
// a script, or Claude acting for somebody -- use the service as the account
// that made the key.
//
// # Why there is a second kind of key
//
// CLAUDE.md lists credential management among the things deliberately not
// imported from the project this was seeded from, and feedbus was the first
// narrow exception: a read-only key on one form, for a spreadsheet. This is the
// second, made on purpose and for the same underlying reason. Somebody who
// builds forms wants to have a program build them, and a program cannot hold a
// session: it cannot read a sign-in code out of a mailbox, and it would have
// to every fourteen days.
//
// What keeps it ordinary rather than a return of the parent project's
// credential domain is that it adds no authority of its own:
//   - a key is an account, nothing more. It holds no scopes and no roles; every
//     route it reaches asks the same accessbus question a session would, so a
//     key can do exactly what its owner can do in a browser, and the day the
//     owner loses a grant the key loses it with them;
//   - a key belongs to the account that made it, which is the only account
//     that can list or revoke it. No administrator mints keys for anybody;
//   - a key cannot make a key. Keys are made and revoked on the account page,
//     behind a session, so a key that leaks cannot be used to leave a second
//     one behind;
//   - a disabled account's keys stop working on the next request, for the
//     reason userbus.Authenticate checks Enabled on every session.
//
// # How a key is kept
//
// Exactly as feedbus keeps its keys, and for the same reasons: shown once,
// stored only as its SHA-256, looked up by that hash. A key is 32 random bytes,
// so a fast hash is the right one -- there is nothing to stretch.
package apikeybus

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// keyPrefix starts every key, so that one pasted somewhere it should not be is
// recognisable as ours -- by a person, and by a secret scanner. Different from
// the feed's dfk_, so that which kind of key leaked is visible at a glance:
// one reads a form, the other is somebody's whole account.
const keyPrefix = "dfa_"

// labelMax bounds what somebody calls a key. The page's input says the same;
// this is what holds when a request does not come from the page.
const labelMax = 80

// usedEvery is how stale the last-used time may get before a request writes
// it again.
//
// Not every request, which is what feedbus does and is right there: a sheet
// reads once every few minutes. A program building a form makes a burst of
// calls, and on a single-writer SQLite database a write per read is how a
// listing starts queueing behind a submission -- which is why
// userbus.Authenticate does not touch a session on every request either. The
// page shows the date to the minute, so a minute is all the precision there is
// to lose.
const usedEvery = time.Minute

// ErrRefused is a key that is not one of ours, has been revoked, or belongs to
// an account that is gone or disabled. One error for all of them, because the
// answer to each is the same 401 and the reason is nobody's business who does
// not already know it.
var ErrRefused = errors.New("that key is not accepted")

// ErrNotFound is a key looked up by hash or identifier that matched nothing.
var ErrNotFound = errors.New("no such key")

// Key is a key's record. The key itself is not in it, and is never stored.
type Key struct {
	ID     types.ID
	UserID types.ID

	// Label is what the person who made it called it -- "Claude on my laptop"
	// -- so that the page listing them says which one to revoke.
	Label string

	CreatedAt time.Time

	// LastUsedAt is zero for a key never used, and otherwise accurate to
	// [usedEvery].
	LastUsedAt time.Time

	// RevokedAt is zero for a key that still works.
	RevokedAt time.Time

	// ExpiresAt is when the key stops working on its own; zero for never,
	// which is every key made on the page. A key handed to a program through
	// OAuth has one -- see oauth.go.
	ExpiresAt time.Time

	// Client is the program the key was given to through OAuth, as its
	// client_id; empty for a key somebody made on the page.
	Client string
}

// Live reports whether the key still works at now.
func (k Key) Live(now time.Time) bool {
	return k.RevokedAt.IsZero() && (k.ExpiresAt.IsZero() || now.Before(k.ExpiresAt))
}

// Storer is what this package needs from storage.
type Storer interface {
	Create(ctx context.Context, k Key, hash string) error
	ByHash(ctx context.Context, hash string) (Key, error)
	ForUser(ctx context.Context, userID types.ID) ([]Key, error)
	Used(ctx context.Context, id types.ID, at time.Time) error
	Revoke(ctx context.Context, userID, id types.ID, at time.Time) error

	// Replace revokes the account's live keys for k.Client and stores k, in
	// one transaction. See oauth.go.
	Replace(ctx context.Context, k Key, hash string, at time.Time) error

	// The authorization codes of oauth.go. CreateGrant stores g unless the
	// account already holds limit unspent, unexpired codes, and reports
	// whether it did. UseGrant spends a code, and reports false for one
	// already spent: it is the claim, so it must be one statement.
	CreateGrant(ctx context.Context, g Grant, hash string, limit int) (bool, error)
	GrantByHash(ctx context.Context, hash string) (Grant, error)
	UseGrant(ctx context.Context, id types.ID, at time.Time) (bool, error)
}

// Accounts is the one thing this package needs from the account domain: who a
// key belongs to, and whether that account may still do anything at all.
type Accounts interface {
	ByID(ctx context.Context, id types.ID) (userbus.User, error)
}

// Business is the set of operations on keys.
type Business struct {
	log      *slog.Logger
	store    Storer
	accounts Accounts
}

// NewBusiness constructs one.
func NewBusiness(log *slog.Logger, store Storer, accounts Accounts) *Business {
	return &Business{log: log, store: store, accounts: accounts}
}

// Create makes a key for an account and returns it once. The caller shows it
// and forgets it; there is no way to read it again.
func (b *Business) Create(ctx context.Context, now time.Time, userID types.ID, label string) (Key, string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		label = "API key"
	}

	if r := []rune(label); len(r) > labelMax {
		label = string(r[:labelMax])
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Key{}, "", fmt.Errorf("drawing a key: %w", err)
	}

	secret := keyPrefix + base64.RawURLEncoding.EncodeToString(raw)

	k := Key{
		ID:        types.NewID(),
		UserID:    userID,
		Label:     label,
		CreatedAt: now.UTC(),
	}

	if err := b.store.Create(ctx, k, hash(secret)); err != nil {
		return Key{}, "", fmt.Errorf("storing the key: %w", err)
	}

	b.log.Info("api key created", "key_id", k.ID.String(), "user_id", userID.String())

	return k, secret, nil
}

// Authenticate decides whose key this is, and records that it was used.
//
// The account is read and checked here rather than by whoever called, because
// "a disabled account's key does not work" is a rule and not a lookup -- and
// a middleware that forgot the second step would be a key that outlived the
// person it belonged to.
func (b *Business) Authenticate(ctx context.Context, now time.Time, presented string) (userbus.User, Key, error) {
	if !strings.HasPrefix(presented, keyPrefix) {
		return userbus.User{}, Key{}, ErrRefused
	}

	k, err := b.store.ByHash(ctx, hash(presented))

	switch {
	case errors.Is(err, ErrNotFound):
		return userbus.User{}, Key{}, ErrRefused
	case err != nil:
		return userbus.User{}, Key{}, fmt.Errorf("reading the key: %w", err)
	case !k.Live(now):
		return userbus.User{}, Key{}, ErrRefused
	}

	u, err := b.accounts.ByID(ctx, k.UserID)

	switch {
	case errors.Is(err, userbus.ErrNotFound):
		return userbus.User{}, Key{}, ErrRefused
	case err != nil:
		return userbus.User{}, Key{}, fmt.Errorf("reading the key's account: %w", err)
	case !u.Enabled:
		return userbus.User{}, Key{}, ErrRefused
	}

	// A failure to record the use is logged and the request goes ahead: the
	// date beside a key is a convenience, and a program refused because of it
	// would be the wrong way round.
	if now.Sub(k.LastUsedAt) >= usedEvery {
		if err := b.store.Used(ctx, k.ID, now.UTC()); err != nil {
			b.log.Error("an api key's use could not be recorded", "key_id", k.ID.String(), "error", err)
		}
	}

	return u, k, nil
}

// ForUser lists an account's keys, revoked ones included, newest first.
func (b *Business) ForUser(ctx context.Context, userID types.ID) ([]Key, error) {
	keys, err := b.store.ForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("listing the keys: %w", err)
	}

	return keys, nil
}

// Revoke stops one of an account's keys working. The account is part of the
// condition, so a key somebody else owns is left alone rather than revoked by
// whoever edited the identifier in the URL. Revoking one already revoked, or
// one that is not theirs, is not an error.
func (b *Business) Revoke(ctx context.Context, now time.Time, userID, id types.ID) error {
	if err := b.store.Revoke(ctx, userID, id, now.UTC()); err != nil {
		return fmt.Errorf("revoking the key: %w", err)
	}

	b.log.Info("api key revoked", "key_id", id.String(), "user_id", userID.String())

	return nil
}

func hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))

	return hex.EncodeToString(sum[:])
}

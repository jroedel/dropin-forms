// Package feedbus issues and checks the keys that let a spreadsheet read one
// form's submissions.
//
// # Why there is a key at all, on a service that left key management behind
//
// CLAUDE.md lists credential management among the things deliberately not
// imported from the project this was seeded from, and this is a narrow
// re-entry of it, made on purpose. The need is a Google Sheet that keeps
// itself up to date with an ordination's answers. A sheet's script cannot hold
// a session -- it cannot sign in by email, and it would have to every
// fourteen days -- so it needs something it can carry, and that is a key.
//
// What keeps it narrow is everything it cannot do:
//   - one form: a key names the form it reads, and is refused on any other;
//   - read only: there is no route anywhere that a key can write through;
//   - no scopes, roles or expiry: there is one thing to be allowed, and a key
//     is revoked by a form administrator from the same page that made it.
//
// # How a key is kept
//
// Shown once, when it is made, and stored only as its SHA-256. A key is 32
// random bytes, so a fast hash is the right one: there is nothing to stretch,
// and the lookup is by the hash itself, which is what lets a presented key be
// found without comparing it against every row. Someone who reads the database
// learns which forms have keys and when they were last used, and cannot read a
// single answer with what they learn.
package feedbus

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

	"github.com/jroedel/dropin-forms/business/types"
)

// keyPrefix starts every key, so that one pasted somewhere it should not be
// is recognisable as ours -- by a person, and by a secret scanner.
const keyPrefix = "dfk_"

// ErrRefused is a key that is not one of ours, has been revoked, or is for a
// different form. One error for all three, because the answer to each is the
// same 401 and the reason is nobody's business who does not already know it.
var ErrRefused = errors.New("that key does not read this form")

// ErrNotFound is a key looked up by identifier that matched nothing on the
// form named.
var ErrNotFound = errors.New("no such key")

// Key is a key's record. The key itself is not in it, and is never stored.
type Key struct {
	ID   types.ID
	Form types.Slug

	// Label is what the person who made it called it -- "Office sheet" --
	// so that the page listing them says which one to revoke.
	Label string

	CreatedBy types.ID
	CreatedAt time.Time

	// LastUsedAt is zero for a key never used. Shown beside it, because "this
	// one has not been used since March" is how somebody decides it can go.
	LastUsedAt time.Time

	// RevokedAt is zero for a key that still works.
	RevokedAt time.Time
}

// Live reports whether the key still works.
func (k Key) Live() bool { return k.RevokedAt.IsZero() }

// Storer is what this package needs from storage.
type Storer interface {
	Create(ctx context.Context, k Key, hash string) error
	ByHash(ctx context.Context, hash string) (Key, error)
	ForForm(ctx context.Context, form types.Slug) ([]Key, error)
	Used(ctx context.Context, id types.ID, at time.Time) error
	Revoke(ctx context.Context, form types.Slug, id types.ID, at time.Time) error
}

// Business is the set of operations on keys.
type Business struct {
	log   *slog.Logger
	store Storer
}

// NewBusiness constructs one.
func NewBusiness(log *slog.Logger, store Storer) *Business {
	return &Business{log: log, store: store}
}

// Create makes a key for a form and returns it once. The caller shows it and
// forgets it; there is no way to read it again.
func (b *Business) Create(ctx context.Context, now time.Time, form types.Slug, label string, by types.ID) (Key, string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		label = "Spreadsheet"
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Key{}, "", fmt.Errorf("drawing a key: %w", err)
	}

	secret := keyPrefix + base64.RawURLEncoding.EncodeToString(raw)

	k := Key{
		ID:        types.NewID(),
		Form:      form,
		Label:     label,
		CreatedBy: by,
		CreatedAt: now.UTC(),
	}

	if err := b.store.Create(ctx, k, hash(secret)); err != nil {
		return Key{}, "", fmt.Errorf("storing the key: %w", err)
	}

	b.log.Info("feed key created", "form", form.String(), "key_id", k.ID.String(), "by", by.String())

	return k, secret, nil
}

// Check decides whether a presented key reads a form, and records that it was
// used.
func (b *Business) Check(ctx context.Context, now time.Time, form types.Slug, presented string) (Key, error) {
	if !strings.HasPrefix(presented, keyPrefix) {
		return Key{}, ErrRefused
	}

	k, err := b.store.ByHash(ctx, hash(presented))

	switch {
	case errors.Is(err, ErrNotFound):
		return Key{}, ErrRefused
	case err != nil:
		return Key{}, fmt.Errorf("reading the key: %w", err)
	case !k.Live() || k.Form != form:
		return Key{}, ErrRefused
	}

	// A failure to record the use is logged and the read goes ahead: the
	// date beside a key is a convenience, and a sheet that stopped updating
	// because of it would be the wrong way round.
	if err := b.store.Used(ctx, k.ID, now.UTC()); err != nil {
		b.log.Error("a feed key's use could not be recorded", "key_id", k.ID.String(), "error", err)
	}

	return k, nil
}

// ForForm lists a form's keys, revoked ones included, newest first.
func (b *Business) ForForm(ctx context.Context, form types.Slug) ([]Key, error) {
	keys, err := b.store.ForForm(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("listing the keys: %w", err)
	}

	return keys, nil
}

// Revoke stops a key working. Revoking one already revoked is not an error.
func (b *Business) Revoke(ctx context.Context, now time.Time, form types.Slug, id, by types.ID) error {
	if err := b.store.Revoke(ctx, form, id, now.UTC()); err != nil {
		return fmt.Errorf("revoking the key: %w", err)
	}

	b.log.Info("feed key revoked", "form", form.String(), "key_id", id.String(), "by", by.String())

	return nil
}

func hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))

	return hex.EncodeToString(sum[:])
}

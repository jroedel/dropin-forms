// Package feeddb stores the keys a spreadsheet reads a form's submissions
// with -- their hashes, never the keys themselves. feedbus says why.
//
// Times are Unix milliseconds in an INTEGER column, for the reason userdb's
// package comment sets out.
package feeddb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/feed/feedbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// Store is the SQLite implementation of feedbus.Storer.
type Store struct {
	db *sql.DB
}

// NewStore constructs one.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Expected is what CheckSchema verifies at startup.
var Expected = sqldb.Expected{
	"feed_keys": {"id", "form_slug", "label", "hash", "created_by", "created_at", "last_used_at", "revoked_at"},
}

// Init creates this domain's table. Idempotent, and run at every startup.
func Init(ctx context.Context, db *sql.DB) error {
	const schema = `
-- One row per key a spreadsheet reads a form with. hash is the SHA-256 of the
-- key, and the key itself is stored nowhere: it is shown once when it is made.
-- The unique index on hash is the lookup a presented key is checked by.
--
-- last_used_at and revoked_at are 0 for never, rather than NULL, as the other
-- tables here say "none" with an empty value.
CREATE TABLE IF NOT EXISTS feed_keys (
    id            TEXT    PRIMARY KEY,
    form_slug     TEXT    NOT NULL,
    label         TEXT    NOT NULL,
    hash          TEXT    NOT NULL UNIQUE,
    created_by    TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    last_used_at  INTEGER NOT NULL,
    revoked_at    INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS feed_keys_form ON feed_keys (form_slug);
`

	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("creating the feed key table: %w", err)
	}

	return nil
}

// Create stores a new key's record and hash.
func (s *Store) Create(ctx context.Context, k feedbus.Key, hash string) error {
	const q = `
INSERT INTO feed_keys (id, form_slug, label, hash, created_by, created_at, last_used_at, revoked_at)
VALUES (?, ?, ?, ?, ?, ?, 0, 0)`

	_, err := s.db.ExecContext(ctx, q,
		k.ID.String(), k.Form.String(), k.Label, hash, k.CreatedBy.String(), msOf(k.CreatedAt))
	if err != nil {
		return fmt.Errorf("inserting the key: %w", err)
	}

	return nil
}

const columns = `SELECT id, form_slug, label, created_by, created_at, last_used_at, revoked_at FROM feed_keys`

// ByHash finds the key with this hash.
func (s *Store) ByHash(ctx context.Context, hash string) (feedbus.Key, error) {
	k, err := scan(s.db.QueryRowContext(ctx, columns+` WHERE hash = ?`, hash))

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return feedbus.Key{}, feedbus.ErrNotFound
	case err != nil:
		return feedbus.Key{}, fmt.Errorf("reading the key: %w", err)
	}

	return k, nil
}

// ForForm lists a form's keys, newest first.
func (s *Store) ForForm(ctx context.Context, form types.Slug) ([]feedbus.Key, error) {
	rows, err := s.db.QueryContext(ctx, columns+` WHERE form_slug = ? ORDER BY created_at DESC, id`, form.String())
	if err != nil {
		return nil, fmt.Errorf("querying the keys: %w", err)
	}
	defer rows.Close()

	var out []feedbus.Key
	for rows.Next() {
		k, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("reading a key: %w", err)
		}

		out = append(out, k)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the keys: %w", err)
	}

	return out, nil
}

// Used records that a key was used.
func (s *Store) Used(ctx context.Context, id types.ID, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE feed_keys SET last_used_at = ? WHERE id = ?`, msOf(at), id.String()); err != nil {
		return fmt.Errorf("recording the key's use: %w", err)
	}

	return nil
}

// Revoke stops a key on a form working. The form is in the condition so that
// an administrator of one form cannot revoke another's key by its id; a key
// that is not there, or is already revoked, is left as it is.
func (s *Store) Revoke(ctx context.Context, form types.Slug, id types.ID, at time.Time) error {
	const q = `UPDATE feed_keys SET revoked_at = ? WHERE id = ? AND form_slug = ? AND revoked_at = 0`

	if _, err := s.db.ExecContext(ctx, q, msOf(at), id.String(), form.String()); err != nil {
		return fmt.Errorf("revoking the key: %w", err)
	}

	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scan(row scanner) (feedbus.Key, error) {
	var (
		id, form, label, by string
		created, used, gone int64
	)

	if err := row.Scan(&id, &form, &label, &by, &created, &used, &gone); err != nil {
		return feedbus.Key{}, err
	}

	kid, err := types.ParseID(id)
	if err != nil {
		return feedbus.Key{}, fmt.Errorf("the key's identifier is unreadable: %w", err)
	}

	slug, err := types.ParseSlug(form)
	if err != nil {
		return feedbus.Key{}, fmt.Errorf("the key's form is unreadable: %w", err)
	}

	maker, err := types.ParseID(by)
	if err != nil {
		return feedbus.Key{}, fmt.Errorf("who made the key is unreadable: %w", err)
	}

	return feedbus.Key{
		ID:         kid,
		Form:       slug,
		Label:      label,
		CreatedBy:  maker,
		CreatedAt:  timeOf(created),
		LastUsedAt: timeOf(used),
		RevokedAt:  timeOf(gone),
	}, nil
}

func msOf(t time.Time) int64 { return t.UTC().UnixMilli() }

// timeOf is the zero time for 0, which is how never is stored.
func timeOf(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}

	return time.UnixMilli(ms).UTC()
}

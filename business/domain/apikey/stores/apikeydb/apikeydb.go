// Package apikeydb stores the personal keys a program uses the service with --
// their hashes, never the keys themselves. apikeybus says why.
//
// Times are Unix milliseconds in an INTEGER column, for the reason userdb's
// package comment sets out.
package apikeydb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/apikey/apikeybus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// Store is the SQLite implementation of apikeybus.Storer.
type Store struct {
	db *sql.DB
}

// NewStore constructs one.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Expected is what CheckSchema verifies at startup.
var Expected = sqldb.Expected{
	"api_keys": {"id", "user_id", "label", "hash", "created_at", "last_used_at", "revoked_at", "expires_at", "client"},
	"oauth_grants": {
		"id", "user_id", "hash", "client_id", "client_name", "redirect_uri", "challenge",
		"created_at", "expires_at", "used_at",
	},
}

// Init creates this domain's table. Idempotent, and run at every startup.
//
// After userdb, because a key belongs to an account and its row references
// one. ON DELETE CASCADE, so that an account removed by hand takes its keys
// with it rather than leaving rows that authenticate as nobody.
func Init(ctx context.Context, db *sql.DB) error {
	const schema = `
-- One row per personal API key. hash is the SHA-256 of the key, and the key
-- itself is stored nowhere: it is shown once when it is made. The unique index
-- on hash is the lookup a presented key is checked by.
--
-- last_used_at, revoked_at and expires_at are 0 for never, rather than NULL,
-- as the other tables here say "none" with an empty value. client is the
-- program a key was given to through OAuth, as its client_id, and '' for a
-- key somebody made on the page.
CREATE TABLE IF NOT EXISTS api_keys (
    id            TEXT    PRIMARY KEY,
    user_id       TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label         TEXT    NOT NULL,
    hash          TEXT    NOT NULL UNIQUE,
    created_at    INTEGER NOT NULL,
    last_used_at  INTEGER NOT NULL,
    revoked_at    INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL DEFAULT 0,
    client        TEXT    NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX IF NOT EXISTS api_keys_user ON api_keys (user_id);

-- The code a person's agreeing hands to a program signing in through OAuth
-- (apikeybus/oauth.go), traded once for a key. Kept as a key is: the hash,
-- never the code, and used_at as the claim. 0 for never, as above.
CREATE TABLE IF NOT EXISTS oauth_grants (
    id            TEXT    PRIMARY KEY,
    user_id       TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    hash          TEXT    NOT NULL UNIQUE,
    client_id     TEXT    NOT NULL,
    client_name   TEXT    NOT NULL,
    redirect_uri  TEXT    NOT NULL,
    challenge     TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL,
    used_at       INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS oauth_grants_user ON oauth_grants (user_id, expires_at);
`

	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("creating the api key table: %w", err)
	}

	return nil
}

// Create stores a new key's record and hash.
func (s *Store) Create(ctx context.Context, k apikeybus.Key, hash string) error {
	return insert(ctx, s.db, k, hash)
}

// execer is a database or a transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insert(ctx context.Context, db execer, k apikeybus.Key, hash string) error {
	const q = `
INSERT INTO api_keys (id, user_id, label, hash, created_at, last_used_at, revoked_at, expires_at, client)
VALUES (?, ?, ?, ?, ?, 0, 0, ?, ?)`

	_, err := db.ExecContext(ctx, q, k.ID.String(), k.UserID.String(), k.Label, hash, msOf(k.CreatedAt), msOf0(k.ExpiresAt), k.Client)
	if err != nil {
		return fmt.Errorf("inserting the key: %w", err)
	}

	return nil
}

// Replace revokes the account's live keys for k.Client and stores k. In one
// transaction, so that a failure leaves the connection somebody had rather
// than none.
func (s *Store) Replace(ctx context.Context, k apikeybus.Key, hash string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting to replace the key: %w", err)
	}
	defer tx.Rollback()

	const q = `UPDATE api_keys SET revoked_at = ? WHERE user_id = ? AND client = ? AND revoked_at = 0`

	if _, err := tx.ExecContext(ctx, q, msOf(at), k.UserID.String(), k.Client); err != nil {
		return fmt.Errorf("revoking the earlier key: %w", err)
	}

	if err := insert(ctx, tx, k, hash); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replacing the key: %w", err)
	}

	return nil
}

const columns = `SELECT id, user_id, label, created_at, last_used_at, revoked_at, expires_at, client FROM api_keys`

// ByHash finds the key with this hash.
func (s *Store) ByHash(ctx context.Context, hash string) (apikeybus.Key, error) {
	k, err := scan(s.db.QueryRowContext(ctx, columns+` WHERE hash = ?`, hash))

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return apikeybus.Key{}, apikeybus.ErrNotFound
	case err != nil:
		return apikeybus.Key{}, fmt.Errorf("reading the key: %w", err)
	}

	return k, nil
}

// ForUser lists an account's keys, newest first.
func (s *Store) ForUser(ctx context.Context, userID types.ID) ([]apikeybus.Key, error) {
	rows, err := s.db.QueryContext(ctx, columns+` WHERE user_id = ? ORDER BY created_at DESC, id`, userID.String())
	if err != nil {
		return nil, fmt.Errorf("querying the keys: %w", err)
	}
	defer rows.Close()

	var out []apikeybus.Key
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
	if _, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`, msOf(at), id.String()); err != nil {
		return fmt.Errorf("recording the key's use: %w", err)
	}

	return nil
}

// Revoke stops one of an account's keys working. The account is in the
// condition so that nobody revokes a key they do not own by its id; a key that
// is not there, or is already revoked, is left as it is.
func (s *Store) Revoke(ctx context.Context, userID, id types.ID, at time.Time) error {
	const q = `UPDATE api_keys SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at = 0`

	if _, err := s.db.ExecContext(ctx, q, msOf(at), id.String(), userID.String()); err != nil {
		return fmt.Errorf("revoking the key: %w", err)
	}

	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scan(row scanner) (apikeybus.Key, error) {
	var (
		id, user, label, client      string
		created, used, gone, expires int64
	)

	if err := row.Scan(&id, &user, &label, &created, &used, &gone, &expires, &client); err != nil {
		return apikeybus.Key{}, err
	}

	kid, err := types.ParseID(id)
	if err != nil {
		return apikeybus.Key{}, fmt.Errorf("the key's identifier is unreadable: %w", err)
	}

	uid, err := types.ParseID(user)
	if err != nil {
		return apikeybus.Key{}, fmt.Errorf("the key's account is unreadable: %w", err)
	}

	return apikeybus.Key{
		ID:         kid,
		UserID:     uid,
		Label:      label,
		CreatedAt:  timeOf(created),
		LastUsedAt: timeOf(used),
		RevokedAt:  timeOf(gone),
		ExpiresAt:  timeOf(expires),
		Client:     client,
	}, nil
}

// --- grants ------------------------------------------------------------------

// CreateGrant stores a grant unless the account already holds limit unspent,
// unexpired ones. The account's expired grants are cleared on the way, which
// is all the sweeping this table needs: a grant is dead five minutes after it
// is made, and the next one an account makes is when its old ones matter.
func (s *Store) CreateGrant(ctx context.Context, g apikeybus.Grant, hash string, limit int) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("starting to store the grant: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_grants WHERE user_id = ? AND expires_at <= ?`, g.UserID.String(), msOf(g.CreatedAt)); err != nil {
		return false, fmt.Errorf("clearing expired grants: %w", err)
	}

	const q = `
INSERT INTO oauth_grants (id, user_id, hash, client_id, client_name, redirect_uri, challenge, created_at, expires_at, used_at)
SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, 0
WHERE (SELECT count(*) FROM oauth_grants WHERE user_id = ? AND used_at = 0) < ?`

	res, err := tx.ExecContext(ctx, q,
		g.ID.String(), g.UserID.String(), hash, g.ClientID, g.ClientName, g.RedirectURI, g.Challenge,
		msOf(g.CreatedAt), msOf(g.ExpiresAt), g.UserID.String(), limit)
	if err != nil {
		return false, fmt.Errorf("inserting the grant: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inserting the grant: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("storing the grant: %w", err)
	}

	return n == 1, nil
}

// GrantByHash finds the grant with this hash.
func (s *Store) GrantByHash(ctx context.Context, hash string) (apikeybus.Grant, error) {
	const q = `
SELECT id, user_id, client_id, client_name, redirect_uri, challenge, created_at, expires_at, used_at
FROM oauth_grants WHERE hash = ?`

	var (
		id, user, client, name, redirect, challenge string
		created, expires, used                      int64
	)

	err := s.db.QueryRowContext(ctx, q, hash).Scan(&id, &user, &client, &name, &redirect, &challenge, &created, &expires, &used)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return apikeybus.Grant{}, apikeybus.ErrNotFound
	case err != nil:
		return apikeybus.Grant{}, fmt.Errorf("reading the grant: %w", err)
	}

	gid, err := types.ParseID(id)
	if err != nil {
		return apikeybus.Grant{}, fmt.Errorf("the grant's identifier is unreadable: %w", err)
	}

	uid, err := types.ParseID(user)
	if err != nil {
		return apikeybus.Grant{}, fmt.Errorf("the grant's account is unreadable: %w", err)
	}

	return apikeybus.Grant{
		ID:          gid,
		UserID:      uid,
		ClientID:    client,
		ClientName:  name,
		RedirectURI: redirect,
		Challenge:   challenge,
		CreatedAt:   timeOf(created),
		ExpiresAt:   timeOf(expires),
		UsedAt:      timeOf(used),
	}, nil
}

// UseGrant spends a grant, and reports false if it was already spent. One
// statement, so two requests racing with the same code cannot both win.
func (s *Store) UseGrant(ctx context.Context, id types.ID, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE oauth_grants SET used_at = ? WHERE id = ? AND used_at = 0`, msOf(at), id.String())
	if err != nil {
		return false, fmt.Errorf("spending the grant: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("spending the grant: %w", err)
	}

	return n == 1, nil
}

func msOf(t time.Time) int64 { return t.UTC().UnixMilli() }

// msOf0 is msOf with the zero time as 0, which is how never is stored.
func msOf0(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}

	return msOf(t)
}

// timeOf is the zero time for 0, which is how never is stored.
func timeOf(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}

	return time.UnixMilli(ms).UTC()
}

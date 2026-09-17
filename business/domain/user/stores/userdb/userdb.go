// Package userdb stores accounts and credentials in SQLite.
//
// # Times are integers, and that is not an aesthetic choice
//
// Every instant here is stored as Unix milliseconds in an INTEGER column, with
// NULL for the zero time. The obvious alternative -- RFC 3339 text, which is
// readable in a sqlite3 shell -- is a trap, and the trap is worth writing down
// because the fix looks like a regression.
//
// Go's time.RFC3339Nano omits trailing zeros in the fractional second, so one
// instant may be written "2026-01-01T00:00:00Z" and another
// "2026-01-01T00:00:00.5Z". Compared as text, '.' (0x2E) sorts before 'Z'
// (0x5A), so the later instant is the smaller string -- and every
// `WHERE expires_at < ?` in this file would silently get the wrong answer for
// some fraction of rows. A fixed-width text layout would fix it; integers
// avoid the question, sort correctly by construction, and cost nothing.
//
// # The three atomic claims
//
// UseToken, UseBackupCode and ClaimBootstrap are each one statement whose
// WHERE clause only matches an unclaimed row, and each reports whether it was
// the statement that matched. That is the contract userbus.Storer documents,
// and it is the reason those three do not read the row first: a SELECT
// followed by an UPDATE lets two simultaneous requests both see an unused
// sign-in link and both succeed, which is the single-use property gone.
package userdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"

	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// sqliteConstraintUnique is SQLITE_CONSTRAINT_UNIQUE, the extended result code
// for a UNIQUE violation.
//
// Written out rather than imported from modernc.org/sqlite/lib, which is the
// whole translated SQLite library and a heavy import for one integer. The
// driver's own Error type is imported, so the code is compared rather than the
// message -- an error string is not an API and changes without notice.
const sqliteConstraintUnique = 2067

// Store is the SQLite implementation of userbus.Storer.
type Store struct {
	db *sql.DB
}

// NewStore constructs one.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Expected is what CheckSchema verifies at startup: the columns this binary
// will read. A column missing from the database is a startup failure rather
// than a 500 in front of somebody trying to sign in.
var Expected = sqldb.Expected{
	"users":         {"id", "email", "name", "enabled", "created_at", "updated_at"},
	"signin_tokens": {"id", "user_id", "hash", "created_at", "expires_at", "used_at"},
	"backup_codes":  {"id", "user_id", "hash", "created_at", "used_at"},
	"sessions":      {"id", "user_id", "hash", "created_at", "expires_at", "last_seen_at"},
	"bootstrap":     {"id", "claimed_at"},
}

// Init creates this domain's tables. Idempotent, and run at every startup,
// which is the migration story the rest of this repository uses.
func Init(ctx context.Context, db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS users (
    id          TEXT    PRIMARY KEY,
    email       TEXT    NOT NULL UNIQUE,
    name        TEXT    NOT NULL,
    enabled     INTEGER NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
) STRICT;

-- The UNIQUE above uses SQLite's default BINARY collation, so it is
-- case-sensitive. That is correct here rather than a gap: types.Email folds
-- case before an address ever reaches this layer, so there is exactly one
-- spelling of any address, and a NOCASE collation would only hide a caller
-- that had skipped the parser.

CREATE TABLE IF NOT EXISTS signin_tokens (
    id          TEXT    PRIMARY KEY,
    user_id     TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    hash        BLOB    NOT NULL,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    used_at     INTEGER
) STRICT;

CREATE INDEX IF NOT EXISTS signin_tokens_expires_at ON signin_tokens (expires_at);

CREATE TABLE IF NOT EXISTS backup_codes (
    id          TEXT    PRIMARY KEY,
    user_id     TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    hash        BLOB    NOT NULL,
    created_at  INTEGER NOT NULL,
    used_at     INTEGER
) STRICT;

CREATE INDEX IF NOT EXISTS backup_codes_user_id ON backup_codes (user_id);

CREATE TABLE IF NOT EXISTS sessions (
    id            TEXT    PRIMARY KEY,
    user_id       TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    hash          BLOB    NOT NULL,
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL,
    last_seen_at  INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS sessions_user_id ON sessions (user_id);
CREATE INDEX IF NOT EXISTS sessions_expires_at ON sessions (expires_at);

-- One row, ever. The CHECK is what makes "has the bootstrap been spent" a
-- primary key conflict rather than a count, so claiming it is one statement
-- with no race.
CREATE TABLE IF NOT EXISTS bootstrap (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    claimed_at  INTEGER NOT NULL
) STRICT;
`

	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("creating the account tables: %w", err)
	}

	return nil
}

// CreateUser inserts an account.
func (s *Store) CreateUser(ctx context.Context, u userbus.User) error {
	const q = `
INSERT INTO users (id, email, name, enabled, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, q,
		u.ID.String(), u.Email.String(), u.Name, boolOf(u.Enabled), msOf(u.CreatedAt), msOf(u.UpdatedAt))

	switch {
	case isUniqueViolation(err):
		// Reachable only by a race with another Create, since userbus checks
		// first. Translated anyway, so the race produces the same error as
		// the check rather than an opaque one.
		return userbus.ErrEmailTaken
	case err != nil:
		return fmt.Errorf("inserting the account: %w", err)
	}

	return nil
}

// UpdateUser replaces the mutable fields of an account.
func (s *Store) UpdateUser(ctx context.Context, u userbus.User) error {
	const q = `
UPDATE users SET email = ?, name = ?, enabled = ?, updated_at = ?
WHERE id = ?`

	res, err := s.db.ExecContext(ctx, q,
		u.Email.String(), u.Name, boolOf(u.Enabled), msOf(u.UpdatedAt), u.ID.String())

	switch {
	case isUniqueViolation(err):
		return userbus.ErrEmailTaken
	case err != nil:
		return fmt.Errorf("updating the account: %w", err)
	}

	return oneRow(res, "the account")
}

const userColumns = `id, email, name, enabled, created_at, updated_at`

// UserByID finds an account by identifier.
func (s *Store) UserByID(ctx context.Context, id types.ID) (userbus.User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id.String())

	return scanUser(row)
}

// UserByEmail finds an account by address.
func (s *Store) UserByEmail(ctx context.Context, email types.Email) (userbus.User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = ?`, email.String())

	return scanUser(row)
}

// Users returns every account, oldest first, for the administration screen.
func (s *Store) Users(ctx context.Context) ([]userbus.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("reading the accounts: %w", err)
	}
	defer rows.Close()

	var out []userbus.User

	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}

		out = append(out, u)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the accounts: %w", err)
	}

	return out, nil
}

// CreateToken records a sign-in link.
func (s *Store) CreateToken(ctx context.Context, t userbus.Token) error {
	const q = `
INSERT INTO signin_tokens (id, user_id, hash, created_at, expires_at)
VALUES (?, ?, ?, ?, ?)`

	if _, err := s.db.ExecContext(ctx, q,
		t.ID.String(), t.UserID.String(), t.Hash, msOf(t.CreatedAt), msOf(t.ExpiresAt)); err != nil {
		return fmt.Errorf("inserting the sign-in link: %w", err)
	}

	return nil
}

// TokenByID finds a sign-in link by identifier.
func (s *Store) TokenByID(ctx context.Context, id types.ID) (userbus.Token, error) {
	const q = `
SELECT id, user_id, hash, created_at, expires_at, used_at
FROM signin_tokens WHERE id = ?`

	var (
		t      userbus.Token
		rawID  string
		rawUID string
		used   sql.NullInt64
		made   int64
		expiry int64
	)

	err := s.db.QueryRowContext(ctx, q, id.String()).
		Scan(&rawID, &rawUID, &t.Hash, &made, &expiry, &used)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return userbus.Token{}, userbus.ErrNotFound
	case err != nil:
		return userbus.Token{}, fmt.Errorf("reading the sign-in link: %w", err)
	}

	if t.ID, err = types.ParseID(rawID); err != nil {
		return userbus.Token{}, fmt.Errorf("the stored sign-in link has a bad identifier: %w", err)
	}
	if t.UserID, err = types.ParseID(rawUID); err != nil {
		return userbus.Token{}, fmt.Errorf("the stored sign-in link names a bad account: %w", err)
	}

	t.CreatedAt = timeOf(made)
	t.ExpiresAt = timeOf(expiry)
	t.UsedAt = timeOfNull(used)

	return t, nil
}

// UseToken spends a sign-in link, reporting whether this call was the one that
// spent it.
//
// One statement. `used_at IS NULL` is what makes it a claim rather than a
// write: two simultaneous requests holding the same link both run this, and
// exactly one of them affects a row.
func (s *Store) UseToken(ctx context.Context, id types.ID, at time.Time) (bool, error) {
	const q = `UPDATE signin_tokens SET used_at = ? WHERE id = ? AND used_at IS NULL`

	res, err := s.db.ExecContext(ctx, q, msOf(at), id.String())
	if err != nil {
		return false, fmt.Errorf("spending the sign-in link: %w", err)
	}

	return affected(res)
}

// ReplaceBackupCodes swaps a user's whole set of codes for a new one, in a
// transaction so that a failure cannot leave somebody with no codes at all.
func (s *Store) ReplaceBackupCodes(ctx context.Context, userID types.ID, codes []userbus.BackupCode) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replacing the backup codes: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM backup_codes WHERE user_id = ?`, userID.String()); err != nil {
		return fmt.Errorf("removing the old backup codes: %w", err)
	}

	const q = `
INSERT INTO backup_codes (id, user_id, hash, created_at)
VALUES (?, ?, ?, ?)`

	for _, c := range codes {
		if _, err := tx.ExecContext(ctx, q, c.ID.String(), userID.String(), c.Hash, msOf(c.CreatedAt)); err != nil {
			return fmt.Errorf("inserting a backup code: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replacing the backup codes: %w", err)
	}

	return nil
}

// BackupCodes returns every code an account holds, spent or not. userbus skips
// the spent ones; they are returned so that a screen can say how many are
// left.
func (s *Store) BackupCodes(ctx context.Context, userID types.ID) ([]userbus.BackupCode, error) {
	const q = `
SELECT id, user_id, hash, created_at, used_at
FROM backup_codes WHERE user_id = ? ORDER BY created_at, id`

	rows, err := s.db.QueryContext(ctx, q, userID.String())
	if err != nil {
		return nil, fmt.Errorf("reading the backup codes: %w", err)
	}
	defer rows.Close()

	var out []userbus.BackupCode

	for rows.Next() {
		var (
			c      userbus.BackupCode
			rawID  string
			rawUID string
			made   int64
			used   sql.NullInt64
		)

		if err := rows.Scan(&rawID, &rawUID, &c.Hash, &made, &used); err != nil {
			return nil, fmt.Errorf("reading a backup code: %w", err)
		}

		if c.ID, err = types.ParseID(rawID); err != nil {
			return nil, fmt.Errorf("a stored backup code has a bad identifier: %w", err)
		}
		if c.UserID, err = types.ParseID(rawUID); err != nil {
			return nil, fmt.Errorf("a stored backup code names a bad account: %w", err)
		}

		c.CreatedAt = timeOf(made)
		c.UsedAt = timeOfNull(used)

		out = append(out, c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the backup codes: %w", err)
	}

	return out, nil
}

// UseBackupCode spends one code, reporting whether this call spent it.
func (s *Store) UseBackupCode(ctx context.Context, id types.ID, at time.Time) (bool, error) {
	const q = `UPDATE backup_codes SET used_at = ? WHERE id = ? AND used_at IS NULL`

	res, err := s.db.ExecContext(ctx, q, msOf(at), id.String())
	if err != nil {
		return false, fmt.Errorf("spending the backup code: %w", err)
	}

	return affected(res)
}

// CreateSession records a signed-in browser.
func (s *Store) CreateSession(ctx context.Context, sess userbus.Session) error {
	const q = `
INSERT INTO sessions (id, user_id, hash, created_at, expires_at, last_seen_at)
VALUES (?, ?, ?, ?, ?, ?)`

	if _, err := s.db.ExecContext(ctx, q,
		sess.ID.String(), sess.UserID.String(), sess.Hash,
		msOf(sess.CreatedAt), msOf(sess.ExpiresAt), msOf(sess.LastSeenAt)); err != nil {
		return fmt.Errorf("inserting the session: %w", err)
	}

	return nil
}

// SessionByID finds a session by identifier. This is the read on the hot path:
// one indexed lookup per request.
func (s *Store) SessionByID(ctx context.Context, id types.ID) (userbus.Session, error) {
	const q = `
SELECT id, user_id, hash, created_at, expires_at, last_seen_at
FROM sessions WHERE id = ?`

	var (
		sess   userbus.Session
		rawID  string
		rawUID string
		made   int64
		expiry int64
		seen   int64
	)

	err := s.db.QueryRowContext(ctx, q, id.String()).
		Scan(&rawID, &rawUID, &sess.Hash, &made, &expiry, &seen)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return userbus.Session{}, userbus.ErrNotFound
	case err != nil:
		return userbus.Session{}, fmt.Errorf("reading the session: %w", err)
	}

	if sess.ID, err = types.ParseID(rawID); err != nil {
		return userbus.Session{}, fmt.Errorf("the stored session has a bad identifier: %w", err)
	}
	if sess.UserID, err = types.ParseID(rawUID); err != nil {
		return userbus.Session{}, fmt.Errorf("the stored session names a bad account: %w", err)
	}

	sess.CreatedAt = timeOf(made)
	sess.ExpiresAt = timeOf(expiry)
	sess.LastSeenAt = timeOf(seen)

	return sess, nil
}

// TouchSession records that a session was used.
func (s *Store) TouchSession(ctx context.Context, id types.ID, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ? WHERE id = ?`, msOf(at), id.String())
	if err != nil {
		return fmt.Errorf("updating the session: %w", err)
	}

	return oneRow(res, "the session")
}

// DeleteSession ends one session. Deleting one that is not there is not an
// error: the caller wanted it gone, and it is.
func (s *Store) DeleteSession(ctx context.Context, id types.ID) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id.String()); err != nil {
		return fmt.Errorf("ending the session: %w", err)
	}

	return nil
}

// DeleteUserSessions ends every session an account has.
func (s *Store) DeleteUserSessions(ctx context.Context, userID types.ID) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID.String()); err != nil {
		return fmt.Errorf("ending the sessions: %w", err)
	}

	return nil
}

// ClaimBootstrap records that the bootstrap secret has been spent, reporting
// false if it already had been.
//
// An INSERT against a table that can hold exactly one row, so "already
// claimed" is a primary key conflict. ON CONFLICT DO NOTHING turns that into
// zero rows affected rather than an error, which makes the whole question one
// statement with no read and no race.
func (s *Store) ClaimBootstrap(ctx context.Context, at time.Time) (bool, error) {
	const q = `
INSERT INTO bootstrap (id, claimed_at) VALUES (1, ?)
ON CONFLICT (id) DO NOTHING`

	res, err := s.db.ExecContext(ctx, q, msOf(at))
	if err != nil {
		return false, fmt.Errorf("recording the bootstrap: %w", err)
	}

	return affected(res)
}

// PruneExpired removes credentials that can no longer be used.
//
// Spent sign-in links are kept until they expire rather than deleted on use,
// so that presenting one twice is a refusal we can log rather than an unknown
// identifier indistinguishable from a guess.
func (s *Store) PruneExpired(ctx context.Context, before time.Time) error {
	cutoff := msOf(before)

	for _, q := range []string{
		`DELETE FROM signin_tokens WHERE expires_at < ?`,
		`DELETE FROM sessions WHERE expires_at < ?`,
	} {
		if _, err := s.db.ExecContext(ctx, q, cutoff); err != nil {
			return fmt.Errorf("removing expired credentials: %w", err)
		}
	}

	return nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so one scanUser
// serves the single-row lookups and the listing.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(row rowScanner) (userbus.User, error) {
	var (
		u       userbus.User
		rawID   string
		rawMail string
		enabled int64
		made    int64
		updated int64
	)

	err := row.Scan(&rawID, &rawMail, &u.Name, &enabled, &made, &updated)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return userbus.User{}, userbus.ErrNotFound
	case err != nil:
		return userbus.User{}, fmt.Errorf("reading the account: %w", err)
	}

	if u.ID, err = types.ParseID(rawID); err != nil {
		return userbus.User{}, fmt.Errorf("the stored account has a bad identifier: %w", err)
	}

	// Parsed rather than trusted. A row written by an older binary, or by hand
	// in a shell, could hold something the current parser refuses -- and an
	// address that cannot be parsed must not become an account that can sign
	// in.
	if u.Email, err = types.ParseEmail(rawMail); err != nil {
		return userbus.User{}, fmt.Errorf("the stored account has a bad address: %w", err)
	}

	u.Enabled = enabled != 0
	u.CreatedAt = timeOf(made)
	u.UpdatedAt = timeOf(updated)

	return u, nil
}

// msOf converts an instant to Unix milliseconds. See the package comment for
// why this is not text.
func msOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}

	return t.UTC().UnixMilli()
}

func timeOf(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}

	return time.UnixMilli(ms).UTC()
}

func timeOfNull(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}

	return timeOf(n.Int64)
}

func boolOf(b bool) int64 {
	if b {
		return 1
	}

	return 0
}

func affected(res sql.Result) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checking what changed: %w", err)
	}

	return n == 1, nil
}

func oneRow(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	switch {
	case err != nil:
		return fmt.Errorf("checking what changed: %w", err)
	case n == 0:
		return fmt.Errorf("%w: %s", userbus.ErrNotFound, what)
	}

	return nil
}

func isUniqueViolation(err error) bool {
	e, ok := errors.AsType[*sqlite.Error](err)

	return ok && e.Code() == sqliteConstraintUnique
}

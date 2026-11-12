// Package sheetdb stores the Google Sheets people keep of forms: one row per
// connection, the rows and columns written to each, and what has happened to
// it. sheetbus says why each exists.
//
// Times are Unix milliseconds in an INTEGER column, for the reason userdb's
// package comment sets out; 0 is never.
package sheetdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/sheet/sheetbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// Store is the SQLite implementation of sheetbus.Storer.
type Store struct {
	db *sql.DB
}

// NewStore constructs one.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Expected is what CheckSchema verifies at startup.
var Expected = sqldb.Expected{
	"sheet_connections": {"id", "user_id", "form_slug", "account", "spreadsheet_id", "sheet_id", "url", "token",
		"state", "problem", "told", "digest", "created_at", "synced_at", "checked_at", "next_at", "failing_since"},
	"sheet_rows":    {"connection_id", "submission_id"},
	"sheet_columns": {"connection_id", "column_key"},
	"sheet_events":  {"connection_id", "at", "text"},
}

// Init creates this domain's tables. Idempotent, and run at every startup.
func Init(ctx context.Context, db *sql.DB) error {
	const schema = `
-- One row per person per form whose Google Sheet is kept. token is the
-- refresh token sealed with AES-GCM under a key from configuration, never the
-- token itself: a copy of this database alone does not open it.
CREATE TABLE IF NOT EXISTS sheet_connections (
    id              TEXT    PRIMARY KEY,
    user_id         TEXT    NOT NULL,
    form_slug       TEXT    NOT NULL,
    account         TEXT    NOT NULL,
    spreadsheet_id  TEXT    NOT NULL,
    sheet_id        INTEGER NOT NULL,
    url             TEXT    NOT NULL,
    token           BLOB    NOT NULL,
    state           TEXT    NOT NULL,
    problem         TEXT    NOT NULL,
    told            TEXT    NOT NULL,
    digest          TEXT    NOT NULL,
    created_at      INTEGER NOT NULL,
    synced_at       INTEGER NOT NULL,
    checked_at      INTEGER NOT NULL,
    next_at         INTEGER NOT NULL,
    failing_since   INTEGER NOT NULL,
    UNIQUE (user_id, form_slug)
) STRICT;

CREATE INDEX IF NOT EXISTS sheet_connections_due ON sheet_connections (state, next_at);

-- Every submission written to a connection's sheet, once. One written and no
-- longer in the sheet was deleted by its owner, and is not put back.
CREATE TABLE IF NOT EXISTS sheet_rows (
    connection_id  TEXT NOT NULL,
    submission_id  TEXT NOT NULL,
    PRIMARY KEY (connection_id, submission_id)
) STRICT, WITHOUT ROWID;

-- The same for columns, by submissionbus.Column's key.
CREATE TABLE IF NOT EXISTS sheet_columns (
    connection_id  TEXT NOT NULL,
    column_key     TEXT NOT NULL,
    PRIMARY KEY (connection_id, column_key)
) STRICT, WITHOUT ROWID;

-- What happened, in a sentence, for the owner's page.
CREATE TABLE IF NOT EXISTS sheet_events (
    connection_id  TEXT    NOT NULL,
    at             INTEGER NOT NULL,
    text           TEXT    NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS sheet_events_connection ON sheet_events (connection_id, at);
`

	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("creating the Google Sheet tables: %w", err)
	}

	return nil
}

// Save writes a connection, new or not.
func (s *Store) Save(ctx context.Context, c sheetbus.Connection) error {
	const q = `
INSERT INTO sheet_connections (id, user_id, form_slug, account, spreadsheet_id, sheet_id, url, token,
    state, problem, told, digest, created_at, synced_at, checked_at, next_at, failing_since)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    account = excluded.account, spreadsheet_id = excluded.spreadsheet_id, sheet_id = excluded.sheet_id,
    url = excluded.url, token = excluded.token, state = excluded.state, problem = excluded.problem,
    told = excluded.told, digest = excluded.digest, synced_at = excluded.synced_at,
    checked_at = excluded.checked_at, next_at = excluded.next_at, failing_since = excluded.failing_since`

	_, err := s.db.ExecContext(ctx, q,
		c.ID.String(), c.User.String(), c.Form.String(), c.Account, c.Spreadsheet, c.Sheet, c.URL, c.Token,
		string(c.State), c.Problem, c.Told, c.Digest,
		msOf(c.CreatedAt), msOf(c.SyncedAt), msOf(c.CheckedAt), msOf(c.NextAt), msOf(c.FailingSince))
	if err != nil {
		return fmt.Errorf("saving the Google Sheet connection: %w", err)
	}

	return nil
}

const columns = `SELECT id, user_id, form_slug, account, spreadsheet_id, sheet_id, url, token,
    state, problem, told, digest, created_at, synced_at, checked_at, next_at, failing_since FROM sheet_connections`

// ByID reads one connection.
func (s *Store) ByID(ctx context.Context, id types.ID) (sheetbus.Connection, error) {
	return s.one(ctx, columns+` WHERE id = ?`, id.String())
}

// Of reads a person's connection to a form.
func (s *Store) Of(ctx context.Context, user types.ID, form types.Slug) (sheetbus.Connection, error) {
	return s.one(ctx, columns+` WHERE user_id = ? AND form_slug = ?`, user.String(), form.String())
}

func (s *Store) one(ctx context.Context, q string, args ...any) (sheetbus.Connection, error) {
	c, err := scan(s.db.QueryRowContext(ctx, q, args...))

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return sheetbus.Connection{}, sheetbus.ErrNotFound
	case err != nil:
		return sheetbus.Connection{}, fmt.Errorf("reading the Google Sheet connection: %w", err)
	}

	return c, nil
}

// Due lists the connections being kept whose next pass has come, plus the
// stopped ones, whose access may have come back.
func (s *Store) Due(ctx context.Context, now time.Time) ([]sheetbus.Connection, error) {
	rows, err := s.db.QueryContext(ctx, columns+` WHERE state IN ('ok', 'stopped') AND next_at <= ? ORDER BY next_at`, msOf(now))
	if err != nil {
		return nil, fmt.Errorf("listing the Google Sheets due: %w", err)
	}
	defer rows.Close()

	var out []sheetbus.Connection
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("reading a Google Sheet connection: %w", err)
		}

		out = append(out, c)
	}

	return out, rows.Err()
}

// Delete removes a connection and everything recorded about it.
func (s *Store) Delete(ctx context.Context, id types.ID) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM sheet_rows WHERE connection_id = ?`,
			`DELETE FROM sheet_columns WHERE connection_id = ?`,
			`DELETE FROM sheet_events WHERE connection_id = ?`,
			`DELETE FROM sheet_connections WHERE id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, id.String()); err != nil {
				return fmt.Errorf("removing the Google Sheet connection: %w", err)
			}
		}

		return nil
	})
}

// Rows is every submission written to a connection's sheet.
func (s *Store) Rows(ctx context.Context, id types.ID) (map[types.ID]bool, error) {
	out := map[types.ID]bool{}

	err := s.each(ctx, `SELECT submission_id FROM sheet_rows WHERE connection_id = ?`, id, func(v string) error {
		sid, err := types.ParseID(v)
		if err != nil {
			return err
		}

		out[sid] = true

		return nil
	})

	return out, err
}

// AddRows records submissions as written.
func (s *Store) AddRows(ctx context.Context, id types.ID, rows []types.ID) error {
	vals := make([]string, len(rows))
	for i, r := range rows {
		vals[i] = r.String()
	}

	return s.add(ctx, `INSERT OR IGNORE INTO sheet_rows (connection_id, submission_id) VALUES (?, ?)`, id, vals)
}

// Columns is every column key written to a connection's sheet.
func (s *Store) Columns(ctx context.Context, id types.ID) (map[string]bool, error) {
	out := map[string]bool{}

	err := s.each(ctx, `SELECT column_key FROM sheet_columns WHERE connection_id = ?`, id, func(v string) error {
		out[v] = true

		return nil
	})

	return out, err
}

// AddColumns records columns as written.
func (s *Store) AddColumns(ctx context.Context, id types.ID, keys []string) error {
	return s.add(ctx, `INSERT OR IGNORE INTO sheet_columns (connection_id, column_key) VALUES (?, ?)`, id, keys)
}

// Forget clears what was written, for a new spreadsheet or tab.
func (s *Store) Forget(ctx context.Context, id types.ID) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM sheet_rows WHERE connection_id = ?`, `DELETE FROM sheet_columns WHERE connection_id = ?`} {
			if _, err := tx.ExecContext(ctx, q, id.String()); err != nil {
				return fmt.Errorf("forgetting what was written: %w", err)
			}
		}

		return nil
	})
}

// eventsKept is how many events a connection keeps: enough for the page.
const eventsKept = 50

// Log records an event, and lets the oldest go.
func (s *Store) Log(ctx context.Context, id types.ID, at time.Time, text string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sheet_events (connection_id, at, text) VALUES (?, ?, ?)`, id.String(), msOf(at), text); err != nil {
			return fmt.Errorf("recording the event: %w", err)
		}

		const prune = `
DELETE FROM sheet_events WHERE connection_id = ? AND rowid NOT IN (
    SELECT rowid FROM sheet_events WHERE connection_id = ? ORDER BY at DESC, rowid DESC LIMIT ?)`

		if _, err := tx.ExecContext(ctx, prune, id.String(), id.String(), eventsKept); err != nil {
			return fmt.Errorf("pruning the events: %w", err)
		}

		return nil
	})
}

// Events is a connection's events, newest first.
func (s *Store) Events(ctx context.Context, id types.ID, limit int) ([]sheetbus.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, text FROM sheet_events WHERE connection_id = ? ORDER BY at DESC, rowid DESC LIMIT ?`, id.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("listing the events: %w", err)
	}
	defer rows.Close()

	var out []sheetbus.Event
	for rows.Next() {
		var (
			at   int64
			text string
		)

		if err := rows.Scan(&at, &text); err != nil {
			return nil, fmt.Errorf("reading an event: %w", err)
		}

		out = append(out, sheetbus.Event{At: timeOf(at), Text: text})
	}

	return out, rows.Err()
}

func (s *Store) each(ctx context.Context, q string, id types.ID, fn func(string) error) error {
	rows, err := s.db.QueryContext(ctx, q, id.String())
	if err != nil {
		return fmt.Errorf("reading what was written: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return fmt.Errorf("reading what was written: %w", err)
		}

		if err := fn(v); err != nil {
			return fmt.Errorf("reading what was written: %w", err)
		}
	}

	return rows.Err()
}

func (s *Store) add(ctx context.Context, q string, id types.ID, vals []string) error {
	if len(vals) == 0 {
		return nil
	}

	return s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, q)
		if err != nil {
			return fmt.Errorf("recording what was written: %w", err)
		}
		defer stmt.Close()

		for _, v := range vals {
			if _, err := stmt.ExecContext(ctx, id.String(), v); err != nil {
				return fmt.Errorf("recording what was written: %w", err)
			}
		}

		return nil
	})
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting a transaction: %w", err)
	}

	if err := fn(tx); err != nil {
		_ = tx.Rollback()

		return err
	}

	return tx.Commit()
}

type scanner interface {
	Scan(dest ...any) error
}

func scan(row scanner) (sheetbus.Connection, error) {
	var (
		id, user, form, state                   string
		c                                       sheetbus.Connection
		created, synced, checked, next, failing int64
	)

	err := row.Scan(&id, &user, &form, &c.Account, &c.Spreadsheet, &c.Sheet, &c.URL, &c.Token,
		&state, &c.Problem, &c.Told, &c.Digest, &created, &synced, &checked, &next, &failing)
	if err != nil {
		return sheetbus.Connection{}, err
	}

	if c.ID, err = types.ParseID(id); err != nil {
		return sheetbus.Connection{}, fmt.Errorf("the connection's identifier is unreadable: %w", err)
	}

	if c.User, err = types.ParseID(user); err != nil {
		return sheetbus.Connection{}, fmt.Errorf("the connection's account is unreadable: %w", err)
	}

	if c.Form, err = types.ParseSlug(form); err != nil {
		return sheetbus.Connection{}, fmt.Errorf("the connection's form is unreadable: %w", err)
	}

	c.State = sheetbus.State(state)
	c.CreatedAt, c.SyncedAt, c.CheckedAt = timeOf(created), timeOf(synced), timeOf(checked)
	c.NextAt, c.FailingSince = timeOf(next), timeOf(failing)

	return c, nil
}

func msOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}

	return t.UTC().UnixMilli()
}

// timeOf is the zero time for 0, which is how never is stored.
func timeOf(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}

	return time.UnixMilli(ms).UTC()
}

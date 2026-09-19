// Package notifydb stores who has turned off email about which form, and what
// the office has already been told.
//
// One row per (account, form), and a row means silence. The default is
// therefore "notify me", which is the important half: somebody given the job
// of reading a form's submissions hears about them without doing anything, and
// the only way to stop is to say so. A table of subscriptions rather than of
// mutes would invert that -- a new grant would arrive silent, and nobody would
// find out until an order went unnoticed.
//
// Times are Unix milliseconds in an INTEGER column, for the reason userdb's
// package comment sets out: Go's RFC 3339 formatting drops trailing zeros from
// the fractional second, and '.' sorts before 'Z', so text timestamps do not
// compare in chronological order.
package notifydb

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// Store is the SQLite implementation of notifybus.Mutes and notifybus.Reports.
type Store struct {
	db *sql.DB
}

// NewStore constructs one.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Expected is what CheckSchema verifies at startup: the columns this binary
// will read.
var Expected = sqldb.Expected{
	"notification_mutes": {"user_id", "form_slug", "muted_at"},
	"unpaid_notices":     {"submission_id", "sent_at"},
}

// Init creates this domain's table. Idempotent, and run at every startup.
func Init(ctx context.Context, db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS notification_mutes (
    user_id    TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    form_slug  TEXT    NOT NULL,
    muted_at   INTEGER NOT NULL,

    PRIMARY KEY (user_id, form_slug)
) STRICT;

-- ON DELETE CASCADE, unlike the grant table's audit column and for the
-- opposite reason: a preference belongs to the account that set it and means
-- nothing without one, so deleting the account should take it with them.
--
-- form_slug is not a foreign key: forms are defined in TOML and loaded at
-- startup, so there is no table to point at. A row for a form that no longer
-- exists is inert and costs nothing.

CREATE INDEX IF NOT EXISTS notification_mutes_form ON notification_mutes (form_slug);

-- One row per order the office has been told was started and never paid for.
--
-- This table is what stops the same order being reported every hour until
-- somebody deals with it. The primary key is the whole mechanism: claiming a
-- notice is an INSERT that either inserts or does not, so "has this been
-- reported" and "record that it has" are one statement rather than a read
-- followed by a write that a restart can land between.
--
-- Losing it costs one repeated message per order still unpaid inside the
-- window submissionbus.Unpaid looks at, which is why that window has a floor.
CREATE TABLE IF NOT EXISTS unpaid_notices (
    submission_id  TEXT    PRIMARY KEY,
    sent_at        INTEGER NOT NULL
) STRICT;

-- No foreign key on submission_id, unlike user_id above, and the difference is
-- worth the line. A preference is meaningless without the account that set it,
-- so that one cascades. Nothing deletes a submission -- there is no path in
-- this service that does -- so a reference here would buy nothing, and it
-- would make this domain's Init depend on another domain's having run first.

CREATE INDEX IF NOT EXISTS unpaid_notices_sent_at ON unpaid_notices (sent_at);
`

	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("creating the notification tables: %w", err)
	}

	return nil
}

// Muted reports whether this account has turned off email about this form.
func (s *Store) Muted(ctx context.Context, userID types.ID, form types.Slug) (bool, error) {
	const q = `SELECT 1 FROM notification_mutes WHERE user_id = ? AND form_slug = ?`

	var one int

	switch err := s.db.QueryRowContext(ctx, q, userID.String(), form.String()).Scan(&one); {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("reading the notification preference: %w", err)
	}

	return true, nil
}

// MutedForForm lists the accounts that have turned off email about one form.
//
// One query rather than one per recipient, because this is read while a
// submission is being announced -- on the paid path that is inside the request
// Stripe is waiting on, and a query per grant holder is a cost that grows with
// the number of people doing their job.
func (s *Store) MutedForForm(ctx context.Context, form types.Slug) ([]types.ID, error) {
	const q = `SELECT user_id FROM notification_mutes WHERE form_slug = ?`

	rows, err := s.db.QueryContext(ctx, q, form.String())
	if err != nil {
		return nil, fmt.Errorf("listing the notification preferences: %w", err)
	}

	defer rows.Close()

	var out []types.ID

	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("reading a notification preference: %w", err)
		}

		id, err := types.ParseID(raw)
		if err != nil {
			// A row this binary cannot read. Skipped rather than fatal: the
			// consequence of ignoring it is one person getting mail they asked
			// not to, and the consequence of failing here is nobody being told
			// about a submission at all.
			continue
		}

		out = append(out, id)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing the notification preferences: %w", err)
	}

	return out, nil
}

// MutedForUser lists the forms one account has turned off email about, for the
// page that shows them all.
func (s *Store) MutedForUser(ctx context.Context, userID types.ID) ([]types.Slug, error) {
	const q = `SELECT form_slug FROM notification_mutes WHERE user_id = ?`

	rows, err := s.db.QueryContext(ctx, q, userID.String())
	if err != nil {
		return nil, fmt.Errorf("listing the notification preferences: %w", err)
	}

	defer rows.Close()

	var out []types.Slug

	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("reading a notification preference: %w", err)
		}

		slug, err := types.ParseSlug(raw)
		if err != nil {
			continue
		}

		out = append(out, slug)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing the notification preferences: %w", err)
	}

	return out, nil
}

// Mute turns email off. Idempotent: muting twice is one row, and the timestamp
// is when it was first asked for.
func (s *Store) Mute(ctx context.Context, userID types.ID, form types.Slug, at time.Time) error {
	const q = `
INSERT INTO notification_mutes (user_id, form_slug, muted_at)
VALUES (?, ?, ?)
ON CONFLICT (user_id, form_slug) DO NOTHING`

	if _, err := s.db.ExecContext(ctx, q, userID.String(), form.String(), at.UTC().UnixMilli()); err != nil {
		return fmt.Errorf("storing the notification preference: %w", err)
	}

	return nil
}

// Unmute turns email back on, and says nothing about whether it was off.
// Asking to be told about something you are already told about is not a
// mistake worth reporting.
func (s *Store) Unmute(ctx context.Context, userID types.ID, form types.Slug) error {
	const q = `DELETE FROM notification_mutes WHERE user_id = ? AND form_slug = ?`

	if _, err := s.db.ExecContext(ctx, q, userID.String(), form.String()); err != nil {
		return fmt.Errorf("clearing the notification preference: %w", err)
	}

	return nil
}

// ClaimUnpaidNotice records that the office is being told about an unpaid
// order, and reports false if it already has been.
//
// A single atomic claim, in the shape userbus uses for its single-use
// credentials and for the same reason: a SELECT followed by an INSERT is two
// statements a restart can land between, and the visible consequence here is
// the office getting the same message every hour.
//
// Claimed before the message is sent rather than after. Both orders lose
// something -- this one loses a notice when the relay is down, and the other
// repeats one every hour until the write succeeds. A missing message is a
// loud line in the log; a message that arrives hourly forever is how somebody
// decides these notifications are not worth reading.
func (s *Store) ClaimUnpaidNotice(ctx context.Context, id types.ID, at time.Time) (bool, error) {
	const q = `
INSERT INTO unpaid_notices (submission_id, sent_at)
VALUES (?, ?)
ON CONFLICT (submission_id) DO NOTHING`

	res, err := s.db.ExecContext(ctx, q, id.String(), at.UTC().UnixMilli())
	if err != nil {
		return false, fmt.Errorf("claiming the unpaid notice: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claiming the unpaid notice: %w", err)
	}

	return n == 1, nil
}

// ForgetUnpaidNotices drops the record of what has been reported, for orders
// old enough that nothing will look at them again.
//
// The retention has to be longer than the window submissionbus.Unpaid
// considers, or forgetting a notice would be the same as re-sending it.
func (s *Store) ForgetUnpaidNotices(ctx context.Context, before time.Time) error {
	const q = `DELETE FROM unpaid_notices WHERE sent_at < ?`

	if _, err := s.db.ExecContext(ctx, q, before.UTC().UnixMilli()); err != nil {
		return fmt.Errorf("forgetting the unpaid notices: %w", err)
	}

	return nil
}

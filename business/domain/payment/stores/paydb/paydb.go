// Package paydb remembers which payment notifications have been acted on.
//
// One table, and it exists for one reason: Stripe delivers an event at least
// once, and sometimes more than once, so "have we already done this" has to be
// answerable. The identifier is Stripe's own, which is what makes two
// deliveries of one event recognisable as one event.
//
// The insert's conflict is the check. A read followed by a write would let two
// simultaneous deliveries both find nothing and both proceed, which is exactly
// the case this table exists to prevent -- Stripe's retries are not
// coordinated with its first attempt.
//
// Times are Unix milliseconds in an INTEGER column, for the reason userdb's
// package comment sets out: Go's RFC 3339 formatting drops trailing zeros from
// the fractional second, and '.' sorts before 'Z', so instants stored as text
// do not compare in chronological order.
package paydb

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// Store is the SQLite implementation of paybus.Seen.
type Store struct {
	db *sql.DB
}

// NewStore constructs one.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Expected is what CheckSchema verifies at startup: the columns this binary
// will read.
var Expected = sqldb.Expected{
	"payment_events": {"id", "kind", "handled_at"},
}

// Init creates this domain's table. Idempotent, and run at every startup.
func Init(ctx context.Context, db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS payment_events (
    id         TEXT    NOT NULL PRIMARY KEY,
    kind       TEXT    NOT NULL,
    handled_at INTEGER NOT NULL
) STRICT;

-- id is Stripe's event identifier, and it is the primary key rather than a
-- unique index on a surrogate one, so that the duplicate insert fails on the
-- thing being deduplicated.
--
-- There is no foreign key to submissions. An event can legitimately name no
-- submission of ours -- a payment made in the Stripe dashboard, or another
-- integration on the same account -- and a constraint here would turn a row
-- worth recording into an error worth retrying forever. What the event was
-- about is on the submission, which is the side that has somewhere to put it.

CREATE INDEX IF NOT EXISTS payment_events_handled_at ON payment_events (handled_at);
`

	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("creating the payment event table: %w", err)
	}

	return nil
}

// Record writes an event identifier, reporting false when it was already there.
func (s *Store) Record(ctx context.Context, id string, kind string, at time.Time) (bool, error) {
	const q = `INSERT INTO payment_events (id, kind, handled_at) VALUES (?, ?, ?)`

	_, err := s.db.ExecContext(ctx, q, id, kind, at.UTC().UnixMilli())

	switch {
	case sqldb.IsPrimaryKeyViolation(err):
		// Already handled. Nothing is written, and the caller turns this into
		// an acknowledgement rather than a failure: Stripe is telling us
		// something we already know, which is what its retries are for.
		return false, nil

	case err != nil:
		return false, fmt.Errorf("recording the payment notification: %w", err)
	}

	return true, nil
}

// Prune forgets events older than before.
func (s *Store) Prune(ctx context.Context, before time.Time) error {
	const q = `DELETE FROM payment_events WHERE handled_at < ?`

	if _, err := s.db.ExecContext(ctx, q, before.UTC().UnixMilli()); err != nil {
		return fmt.Errorf("pruning the payment notifications: %w", err)
	}

	return nil
}

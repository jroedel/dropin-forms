// Package depositdb keeps the deposits Stripe has paid into the bank, with
// what each one held.
//
// Two tables, a payout and its lines, and nothing in either is ever updated
// except a line's order once it can be traced (depositbus.Business.Sync says
// when that happens). A paid deposit does not change, and the rows are the
// ledger the deposits page is read from, so nothing prunes them either.
//
// Times are Unix milliseconds in an INTEGER column, for the reason userdb's
// package comment sets out.
package depositdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/deposit/depositbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// Store is the SQLite implementation of depositbus.Storer.
type Store struct {
	db *sql.DB
}

// NewStore constructs one.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Expected is what CheckSchema verifies at startup: the columns this binary
// will read.
var Expected = sqldb.Expected{
	"payouts": {"id", "amount", "currency", "created_at", "arrival_date", "automatic", "mismatch", "fetched_at",
		"destination", "bank", "last4", "trace_id", "trace_status"},
	"payout_lines": {"id", "payout_id", "kind", "type", "description", "amount", "fee", "net", "created_at",
		"payment_ref", "form_slug", "submission_id"},
}

// Init creates this domain's tables. Idempotent, and run at every startup.
func Init(ctx context.Context, db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS payouts (
    id            TEXT    PRIMARY KEY,
    amount        INTEGER NOT NULL,
    currency      TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    arrival_date  INTEGER NOT NULL,
    automatic     INTEGER NOT NULL,
    mismatch      INTEGER NOT NULL,
    fetched_at    INTEGER NOT NULL
) STRICT;

-- id is Stripe's, po_..., and the primary key for the reason payment_events
-- uses Stripe's event id: keeping the same payout twice is a constraint
-- violation rather than a check a sync has to remember.

CREATE INDEX IF NOT EXISTS payouts_created ON payouts (created_at);

CREATE TABLE IF NOT EXISTS payout_lines (
    id             TEXT    PRIMARY KEY,
    payout_id      TEXT    NOT NULL REFERENCES payouts(id) ON DELETE CASCADE,
    kind           TEXT    NOT NULL,
    type           TEXT    NOT NULL,
    description    TEXT    NOT NULL,
    amount         INTEGER NOT NULL,
    fee            INTEGER NOT NULL,
    net            INTEGER NOT NULL,
    created_at     INTEGER NOT NULL,
    payment_ref    TEXT    NOT NULL,
    form_slug      TEXT    NOT NULL,
    submission_id  TEXT    NOT NULL
) STRICT;

-- id is Stripe's balance transaction, txn_..., which belongs to exactly one
-- payout.
--
-- form_slug and submission_id are empty, not NULL, for a line that was not one
-- of this service's orders, as submissions.payment_ref is. They are not
-- foreign keys: a line is a fact about money that reached the bank, and it
-- stays one if the submission it was traced to is ever removed.

CREATE INDEX IF NOT EXISTS payout_lines_payout ON payout_lines (payout_id);
CREATE INDEX IF NOT EXISTS payout_lines_form ON payout_lines (form_slug, payout_id);

-- The lines a sync tries to trace again. Partial for the reason
-- submissions_payment_ref is, and Unattributed repeats the condition so the
-- index is usable.
CREATE INDEX IF NOT EXISTS payout_lines_unattributed ON payout_lines (payment_ref) WHERE form_slug = '' AND payment_ref != '';
`

	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("creating the deposit tables: %w", err)
	}

	// Where each payout went and the bank's reference for it, added after the
	// first deposits were kept. Only here and not also in the CREATE above,
	// so that a fresh database and an upgraded one get the columns from the
	// same five lines and cannot drift apart.
	//
	// Empty rather than NULL for "not known yet", as everywhere in this
	// schema; depositbus.Payout.Described says when a row is complete.
	for _, c := range [][2]string{
		{"destination", "TEXT NOT NULL DEFAULT ''"},
		{"bank", "TEXT NOT NULL DEFAULT ''"},
		{"last4", "TEXT NOT NULL DEFAULT ''"},
		{"trace_id", "TEXT NOT NULL DEFAULT ''"},
		{"trace_status", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := sqldb.AddColumn(ctx, db, "payouts", c[0], c[1]); err != nil {
			return err
		}
	}

	return nil
}

// Has reports whether a payout is kept.
func (s *Store) Has(ctx context.Context, id string) (bool, error) {
	const q = `SELECT 1 FROM payouts WHERE id = ?`

	var one int
	err := s.db.QueryRowContext(ctx, q, id).Scan(&one)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("looking for the deposit: %w", err)
	}

	return true, nil
}

// Save keeps a payout and its lines in one transaction.
func (s *Store) Save(ctx context.Context, p depositbus.Payout, lines []depositbus.Line) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting to keep the deposit: %w", err)
	}
	defer tx.Rollback()

	const qp = `INSERT INTO payouts (id, amount, currency, created_at, arrival_date, automatic, mismatch, fetched_at,
destination, bank, last4, trace_id, trace_status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err = tx.ExecContext(ctx, qp, p.ID, int64(p.Amount), p.Currency, msOf(p.Created), msOf(p.ArrivalDate),
		boolOf(p.Automatic), boolOf(p.Mismatch), msOf(p.FetchedAt),
		p.Destination, p.Bank, p.Last4, p.TraceID, p.TraceStatus)

	switch {
	case sqldb.IsPrimaryKeyViolation(err):
		// Kept already, by a sync that overlapped this one. A paid payout
		// does not change, so what is there is what this would have written.
		return nil
	case err != nil:
		return fmt.Errorf("keeping the deposit: %w", err)
	}

	const ql = `INSERT INTO payout_lines (id, payout_id, kind, type, description, amount, fee, net, created_at,
payment_ref, form_slug, submission_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	for _, l := range lines {
		_, err := tx.ExecContext(ctx, ql, l.ID, p.ID, string(l.Kind), l.Type, l.Description,
			int64(l.Amount), int64(l.Fee), int64(l.Net), msOf(l.Created),
			l.PaymentRef, l.Form.String(), l.SubmissionID.String())
		if err != nil {
			return fmt.Errorf("keeping deposit line %s: %w", l.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("keeping the deposit: %w", err)
	}

	return nil
}

// undescribed is the condition for a payout not yet depositbus.Payout.Described,
// written once because two statements carry it and must agree.
const undescribed = `(destination = '' OR trace_status = '' OR trace_status = 'pending')`

// Undescribed is when the oldest payout created at or after since that is not
// yet described was created.
func (s *Store) Undescribed(ctx context.Context, since time.Time) (time.Time, bool, error) {
	const q = `SELECT MIN(created_at) FROM payouts WHERE created_at >= ? AND ` + undescribed

	var ms sql.NullInt64
	if err := s.db.QueryRowContext(ctx, q, msOf(since)).Scan(&ms); err != nil {
		return time.Time{}, false, fmt.Errorf("reading the deposits without a trace: %w", err)
	}

	if !ms.Valid {
		return time.Time{}, false, nil
	}

	return timeOf(ms.Int64), true, nil
}

// Describe records where a kept payout went and its trace, if the row is not
// yet described.
func (s *Store) Describe(ctx context.Context, p depositbus.Payout) error {
	const q = `UPDATE payouts SET destination = ?, bank = ?, last4 = ?, trace_id = ?, trace_status = ?
WHERE id = ? AND ` + undescribed

	if _, err := s.db.ExecContext(ctx, q, p.Destination, p.Bank, p.Last4, p.TraceID, p.TraceStatus, p.ID); err != nil {
		return fmt.Errorf("recording where the deposit went: %w", err)
	}

	return nil
}

// Latest is when the newest kept payout was created.
func (s *Store) Latest(ctx context.Context) (time.Time, bool, error) {
	const q = `SELECT MAX(created_at) FROM payouts`

	var ms sql.NullInt64
	if err := s.db.QueryRowContext(ctx, q).Scan(&ms); err != nil {
		return time.Time{}, false, fmt.Errorf("reading the newest deposit: %w", err)
	}

	if !ms.Valid {
		return time.Time{}, false, nil
	}

	return timeOf(ms.Int64), true, nil
}

// Unattributed lists the lines that name a payment and no form.
func (s *Store) Unattributed(ctx context.Context) ([]depositbus.Line, error) {
	const q = selectLines + ` WHERE form_slug = '' AND payment_ref != ''`

	return s.lines(ctx, q)
}

// Attribute records which order a line was.
func (s *Store) Attribute(ctx context.Context, line string, form types.Slug, submission types.ID) error {
	const q = `UPDATE payout_lines SET form_slug = ?, submission_id = ? WHERE id = ?`

	if _, err := s.db.ExecContext(ctx, q, form.String(), submission.String(), line); err != nil {
		return fmt.Errorf("recording the order for a deposit line: %w", err)
	}

	return nil
}

// ForForm lists the payouts holding any of a form's lines, newest arrival
// first, with every line of each.
//
// Two queries rather than one per payout: the lines of every payout touching
// the form come back in one read and are grouped here.
func (s *Store) ForForm(ctx context.Context, form types.Slug) ([]depositbus.Payout, map[string][]depositbus.Line, error) {
	const touching = `SELECT DISTINCT payout_id FROM payout_lines WHERE form_slug = ?`

	const qp = `SELECT id, amount, currency, created_at, arrival_date, automatic, mismatch, fetched_at,
destination, bank, last4, trace_id, trace_status
FROM payouts WHERE id IN (` + touching + `) ORDER BY arrival_date DESC, id DESC`

	rows, err := s.db.QueryContext(ctx, qp, form.String())
	if err != nil {
		return nil, nil, fmt.Errorf("querying the deposits: %w", err)
	}
	defer rows.Close()

	var payouts []depositbus.Payout
	for rows.Next() {
		var (
			p                                 depositbus.Payout
			amount, created, arrival, fetched int64
			automatic, mismatch               int64
		)

		if err := rows.Scan(&p.ID, &amount, &p.Currency, &created, &arrival, &automatic, &mismatch, &fetched,
			&p.Destination, &p.Bank, &p.Last4, &p.TraceID, &p.TraceStatus); err != nil {
			return nil, nil, fmt.Errorf("reading a deposit: %w", err)
		}

		p.Amount = types.Money(amount)
		p.Created, p.ArrivalDate, p.FetchedAt = timeOf(created), timeOf(arrival), timeOf(fetched)
		p.Automatic, p.Mismatch = automatic != 0, mismatch != 0

		// A kept payout is paid and, if automatic, reconciled; see Payout.
		p.Paid, p.Reconciled = true, p.Automatic

		payouts = append(payouts, p)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading the deposits: %w", err)
	}

	lines, err := s.lines(ctx, selectLines+` WHERE payout_id IN (`+touching+`) ORDER BY created_at, id`, form.String())
	if err != nil {
		return nil, nil, err
	}

	byPayout := map[string][]depositbus.Line{}
	for _, l := range lines {
		byPayout[l.Payout] = append(byPayout[l.Payout], l)
	}

	return payouts, byPayout, nil
}

// Deposited is the set of payment references of a form's sales in a kept
// payout.
func (s *Store) Deposited(ctx context.Context, form types.Slug) (map[string]bool, error) {
	const q = `SELECT DISTINCT payment_ref FROM payout_lines WHERE form_slug = ? AND kind = ? AND payment_ref != ''`

	rows, err := s.db.QueryContext(ctx, q, form.String(), string(depositbus.KindSale))
	if err != nil {
		return nil, fmt.Errorf("querying the deposited payments: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("reading a deposited payment: %w", err)
		}

		out[ref] = true
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the deposited payments: %w", err)
	}

	return out, nil
}

const selectLines = `
SELECT id, payout_id, kind, type, description, amount, fee, net, created_at, payment_ref, form_slug, submission_id
FROM payout_lines`

// lines runs a query over selectLines and reads every row back through its
// own type. A row this binary cannot read is an error, not a partial line.
func (s *Store) lines(ctx context.Context, q string, args ...any) ([]depositbus.Line, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("querying the deposit lines: %w", err)
	}
	defer rows.Close()

	var out []depositbus.Line
	for rows.Next() {
		var (
			l                         depositbus.Line
			kind, form, submission    string
			amount, fee, net, created int64
		)

		if err := rows.Scan(&l.ID, &l.Payout, &kind, &l.Type, &l.Description, &amount, &fee, &net, &created,
			&l.PaymentRef, &form, &submission); err != nil {
			return nil, fmt.Errorf("reading a deposit line: %w", err)
		}

		l.Kind = depositbus.Kind(kind)
		l.Amount, l.Fee, l.Net = types.Money(amount), types.Money(fee), types.Money(net)
		l.Created = timeOf(created)

		if form != "" {
			if l.Form, err = types.ParseSlug(form); err != nil {
				return nil, fmt.Errorf("deposit line %s names a form this binary cannot read: %w", l.ID, err)
			}
		}

		if submission != "" {
			if l.SubmissionID, err = types.ParseID(submission); err != nil {
				return nil, fmt.Errorf("deposit line %s names an order this binary cannot read: %w", l.ID, err)
			}
		}

		out = append(out, l)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the deposit lines: %w", err)
	}

	return out, nil
}

func msOf(t time.Time) int64    { return t.UTC().UnixMilli() }
func timeOf(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func boolOf(b bool) int64 {
	if b {
		return 1
	}

	return 0
}

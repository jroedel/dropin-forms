// Package submissiondb stores submissions in SQLite.
//
// # The answers are one JSON column, and that is deliberate
//
// A submission's shape is the form's shape, and every form is different. The
// alternatives are a column per field -- which means altering the table every
// time somebody adds a question -- or a row per answer, which turns reading
// one submission into a join and an ordering problem and still cannot hold a
// priced line item. So the answers, the ordered lines and the derived total
// are marshalled into one document, with the few things that are queried
// (form, status, total, email, timestamps) kept as real columns beside it.
//
// The document is written through an explicit wire type rather than by
// marshalling formbus.Answers directly. Two reasons, and the first is a bug
// avoided rather than a preference: types.Slug and types.Money are structs
// with unexported fields, so encoding/json would write {} for a form name and
// silently lose it. The second is that a stored record outlives the Go type it
// came from, and a wire type is where a rename is absorbed instead of
// corrupting every row written before it.
//
// # Accepting is one transaction
//
// Accept inserts the submission and the spent nonce together, with the nonce
// insert carrying the primary-key conflict that makes a grant single-use. A
// SELECT to check the nonce followed by an INSERT would let two simultaneous
// POSTs with one grant both see it free; on a form selling tickets that is a
// double charge.
//
// Times are Unix milliseconds in INTEGER columns, for the reason userdb's
// package comment sets out: Go's RFC 3339 formatting drops trailing zeros from
// the fractional second, and '.' sorts before 'Z', so text timestamps do not
// compare in chronological order.
package submissiondb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// sqliteConstraintPrimaryKey is SQLITE_CONSTRAINT_PRIMARYKEY, which is what a
// replayed nonce produces. Written out rather than imported from
// modernc.org/sqlite/lib, which is the whole translated library and a heavy
// import for one integer.
//
// Verified against the driver rather than assumed, because the two plausible
// codes are easy to mix up and the message actively misleads: a duplicate
// TEXT PRIMARY KEY on a rowid table is enforced by a unique index, and the
// driver reports `UNIQUE constraint failed: spent_grants.nonce (1555)` -- the
// words say UNIQUE (2067) while the extended code is PRIMARYKEY. Matching on
// the code is right; matching on the message would have been wrong.
const sqliteConstraintPrimaryKey = 1555

// Store is the SQLite implementation of submissionbus.Storer.
type Store struct {
	db *sql.DB
}

// NewStore constructs one.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Expected is what CheckSchema verifies at startup: the columns this binary
// will read.
var Expected = sqldb.Expected{
	"submissions":  {"id", "form_slug", "version", "status", "answers", "email", "total", "currency", "remote_ip", "payment_ref", "created_at", "updated_at"},
	"spent_grants": {"nonce", "form_slug", "spent_at"},
}

// Init creates this domain's tables. Idempotent, and run at every startup.
func Init(ctx context.Context, db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS submissions (
    id           TEXT    PRIMARY KEY,
    form_slug    TEXT    NOT NULL,
    version      TEXT    NOT NULL,
    status       TEXT    NOT NULL,
    answers      TEXT    NOT NULL,
    email        TEXT    NOT NULL,
    total        INTEGER NOT NULL,
    currency     TEXT    NOT NULL,
    remote_ip    TEXT    NOT NULL,
    payment_ref  TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
) STRICT;

-- form_slug is not a foreign key: forms are defined in TOML and loaded at
-- startup, so there is no table to point at. A submission keeps the form name
-- and the version it was accepted against, which is what makes it readable
-- after the definition has moved on.
--
-- email and payment_ref are NOT NULL and empty rather than nullable. A form
-- need not ask for an address and a free form has no payment, and "" says
-- that as well as NULL does while sparing every read a null check.

CREATE INDEX IF NOT EXISTS submissions_form_created ON submissions (form_slug, created_at DESC);
CREATE INDEX IF NOT EXISTS submissions_status ON submissions (status);

-- One row per grant that has been spent. The primary key is the whole
-- mechanism: redeeming a grant twice is a constraint violation rather than a
-- check somebody has to remember to perform.
CREATE TABLE IF NOT EXISTS spent_grants (
    nonce      TEXT    PRIMARY KEY,
    form_slug  TEXT    NOT NULL,
    spent_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS spent_grants_spent_at ON spent_grants (spent_at);
`

	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("creating the submission tables: %w", err)
	}

	return nil
}

// Accept writes the submission and spends the nonce in one transaction.
//
// The nonce goes in first, so that a replay does no work beyond the failed
// insert and so that the submission is never written for a grant that turns
// out to have been spent. The rollback on the way out is unconditional: after
// a successful Commit it is a no-op, which is the idiom that makes every
// failure path below leave nothing behind.
func (s *Store) Accept(ctx context.Context, sub submissionbus.Submission, nonce string) (bool, error) {
	doc, err := marshalAnswers(sub.Answers)
	if err != nil {
		return false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("beginning the transaction: %w", err)
	}

	defer tx.Rollback()

	const spend = `INSERT INTO spent_grants (nonce, form_slug, spent_at) VALUES (?, ?, ?)`

	_, err = tx.ExecContext(ctx, spend, nonce, sub.Form.String(), msOf(sub.CreatedAt))

	switch {
	case isPrimaryKeyViolation(err):
		// Already spent. Nothing is written, and the caller turns this into
		// the same answer as any other stale grant.
		return false, nil

	case err != nil:
		return false, fmt.Errorf("spending the grant: %w", err)
	}

	const insert = `
INSERT INTO submissions
    (id, form_slug, version, status, answers, email, total, currency, remote_ip, payment_ref, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err = tx.ExecContext(ctx, insert,
		sub.ID.String(), sub.Form.String(), sub.Version, sub.Status.String(), doc,
		sub.Email.String(), int64(sub.Answers.Total), sub.Answers.Currency,
		sub.RemoteIP, sub.PaymentRef, msOf(sub.CreatedAt), msOf(sub.UpdatedAt))
	if err != nil {
		return false, fmt.Errorf("inserting the submission: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("committing the submission: %w", err)
	}

	return true, nil
}

// ByID reads one submission.
func (s *Store) ByID(ctx context.Context, id types.ID) (submissionbus.Submission, error) {
	const q = selectColumns + ` WHERE id = ?`

	sub, err := scan(s.db.QueryRowContext(ctx, q, id.String()))

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return submissionbus.Submission{}, submissionbus.ErrNotFound
	case err != nil:
		return submissionbus.Submission{}, fmt.Errorf("reading the submission: %w", err)
	}

	return sub, nil
}

// ByForm lists a form's submissions, newest first, which is the order the
// index is built for and the order a page wants.
func (s *Store) ByForm(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error) {
	const q = selectColumns + ` WHERE form_slug = ? ORDER BY created_at DESC, id`

	rows, err := s.db.QueryContext(ctx, q, form.String())
	if err != nil {
		return nil, fmt.Errorf("querying the submissions: %w", err)
	}
	defer rows.Close()

	var out []submissionbus.Submission
	for rows.Next() {
		sub, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("reading a submission: %w", err)
		}

		out = append(out, sub)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the submissions: %w", err)
	}

	return out, nil
}

// SetStatus moves a submission forward.
func (s *Store) SetStatus(ctx context.Context, id types.ID, to submissionbus.Status, ref string, at time.Time) error {
	const q = `UPDATE submissions SET status = ?, payment_ref = ?, updated_at = ? WHERE id = ?`

	res, err := s.db.ExecContext(ctx, q, to.String(), ref, msOf(at), id.String())
	if err != nil {
		return fmt.Errorf("updating the submission: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("counting the updated rows: %w", err)
	}

	if n == 0 {
		return submissionbus.ErrNotFound
	}

	return nil
}

// PruneNonces forgets spent nonces older than before.
func (s *Store) PruneNonces(ctx context.Context, before time.Time) error {
	const q = `DELETE FROM spent_grants WHERE spent_at < ?`

	if _, err := s.db.ExecContext(ctx, q, msOf(before)); err != nil {
		return fmt.Errorf("pruning the spent grants: %w", err)
	}

	return nil
}

const selectColumns = `
SELECT id, form_slug, version, status, answers, email, total, currency, remote_ip, payment_ref, created_at, updated_at
FROM submissions`

// scanner is what QueryRow and Rows have in common.
type scanner interface {
	Scan(dest ...any) error
}

// scan turns one row into a Submission, parsing every stored value back
// through its own type rather than casting. A row this binary cannot read is
// an error rather than a partial submission.
func scan(row scanner) (submissionbus.Submission, error) {
	var (
		id, formSlug, version, status string
		doc, email, currency          string
		remoteIP, paymentRef          string
		total                         int64
		createdAt, updatedAt          int64
	)

	err := row.Scan(&id, &formSlug, &version, &status, &doc, &email,
		&total, &currency, &remoteIP, &paymentRef, &createdAt, &updatedAt)
	if err != nil {
		return submissionbus.Submission{}, err
	}

	subID, err := types.ParseID(id)
	if err != nil {
		return submissionbus.Submission{}, fmt.Errorf("the submission identifier is unreadable: %w", err)
	}

	form, err := types.ParseSlug(formSlug)
	if err != nil {
		return submissionbus.Submission{}, fmt.Errorf("the form name on a submission is unreadable: %w", err)
	}

	st, err := submissionbus.ParseStatus(status)
	if err != nil {
		return submissionbus.Submission{}, fmt.Errorf("the status on a submission is unreadable: %w", err)
	}

	answers, err := unmarshalAnswers(doc, form, version, currency, types.Money(total))
	if err != nil {
		return submissionbus.Submission{}, err
	}

	sub := submissionbus.Submission{
		ID:         subID,
		Form:       form,
		Version:    version,
		Status:     st,
		Answers:    answers,
		RemoteIP:   remoteIP,
		PaymentRef: paymentRef,
		CreatedAt:  timeOf(createdAt),
		UpdatedAt:  timeOf(updatedAt),
	}

	// A form need not ask for an address, so an empty one is not a failure.
	if email != "" {
		sub.Email, err = types.ParseEmail(email)
		if err != nil {
			return submissionbus.Submission{}, fmt.Errorf("the address on a submission is unreadable: %w", err)
		}
	}

	return sub, nil
}

// The stored document. Field names are short because there will be one of
// these per submission and they are never read by a person, and every one is
// spelled out rather than inherited from the domain type -- see the package
// comment.
type answersDoc struct {
	V      int        `json:"v"`
	Fields []fieldDoc `json:"fields,omitempty"`
	Lines  []lineDoc  `json:"lines,omitempty"`
}

type fieldDoc struct {
	Name   string   `json:"name"`
	Label  string   `json:"label"`
	Kind   string   `json:"kind"`
	Values []string `json:"values,omitempty"`
}

type lineDoc struct {
	ItemID string `json:"item"`
	Label  string `json:"label"`
	Price  int64  `json:"price"`
	Qty    int    `json:"qty"`
	Amount int64  `json:"amount"`
}

// docVersion is the document's own format number, so that a future change of
// shape is a refusal rather than a misread.
const docVersion = 1

func marshalAnswers(a formbus.Answers) (string, error) {
	doc := answersDoc{V: docVersion}

	for _, f := range a.Fields {
		doc.Fields = append(doc.Fields, fieldDoc{
			Name:   f.Name,
			Label:  f.Label,
			Kind:   string(f.Kind),
			Values: f.Values,
		})
	}

	for _, l := range a.Lines {
		doc.Lines = append(doc.Lines, lineDoc{
			ItemID: l.ItemID,
			Label:  l.Label,
			Price:  int64(l.Price),
			Qty:    l.Qty,
			Amount: int64(l.Amount),
		})
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("encoding the answers: %w", err)
	}

	return string(out), nil
}

// unmarshalAnswers rebuilds the answers, taking the form, version, currency
// and total from their own columns rather than from the document.
//
// Those four are stored twice -- once as a column that can be queried and
// indexed, once implied by the record -- and this is where the duplication is
// resolved in favour of the column. A total in a JSON blob is not something a
// report can sum.
func unmarshalAnswers(raw string, form types.Slug, version, currency string, total types.Money) (formbus.Answers, error) {
	var doc answersDoc

	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return formbus.Answers{}, fmt.Errorf("decoding the answers: %w", err)
	}

	if doc.V != docVersion {
		return formbus.Answers{}, fmt.Errorf("the answers are format %d and this binary reads %d", doc.V, docVersion)
	}

	out := formbus.Answers{
		FormID:   form,
		Version:  version,
		Currency: currency,
		Total:    total,
	}

	for _, f := range doc.Fields {
		out.Fields = append(out.Fields, formbus.Answer{
			Name:   f.Name,
			Label:  f.Label,
			Kind:   formbus.Kind(f.Kind),
			Values: f.Values,
		})
	}

	for _, l := range doc.Lines {
		out.Lines = append(out.Lines, formbus.Line{
			ItemID: l.ItemID,
			Label:  l.Label,
			Price:  types.Money(l.Price),
			Qty:    l.Qty,
			Amount: types.Money(l.Amount),
		})
	}

	return out, nil
}

// isPrimaryKeyViolation reports whether err is the conflict that makes a grant
// single-use. The driver's own error type, so the extended result code is
// compared rather than the message -- an error string is not an API.
func isPrimaryKeyViolation(err error) bool {
	e, ok := errors.AsType[*sqlite.Error](err)

	return ok && e.Code() == sqliteConstraintPrimaryKey
}

func msOf(t time.Time) int64    { return t.UTC().UnixMilli() }
func timeOf(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

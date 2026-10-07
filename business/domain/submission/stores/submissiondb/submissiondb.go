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
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// Store is the SQLite implementation of submissionbus.Storer.
type Store struct {
	db *sql.DB
}

// NewStore constructs one.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Expected is what CheckSchema verifies at startup: the columns this binary
// will read.
var Expected = sqldb.Expected{
	"submissions":        {"id", "form_slug", "version", "status", "answers", "email", "total", "currency", "remote_ip", "payment_ref", "created_at", "updated_at"},
	"spent_grants":       {"nonce", "form_slug", "spent_at"},
	"collections":        {"submission_id", "form_slug", "collected_at", "collected_by"},
	"hidden_submissions": {"submission_id", "form_slug", "hidden_at", "hidden_by"},
	"submission_revisions": {"submission_id", "revision", "version", "answers", "email", "total", "currency",
		"remote_ip", "answered_at", "replaced_at"},
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

-- One row per order whose tokens have been handed over at the will-call
-- table. A table of its own rather than a column on submissions, because a
-- submission is immutable apart from its status -- submissionbus says so in
-- its first paragraph -- and collecting is not a payment status. It is
-- somebody standing at a table on the morning of the feast.
--
-- The primary key is the whole of the once-only property, exactly as it is on
-- spent_grants: two volunteers tapping Collect on the same order from two
-- phones is a constraint violation rather than a check either of them has to
-- remember to perform, and the one that lost is told who got there first.
--
-- collected_by records which of them it was. That is the reason the table
-- exists rather than a flag: the alternative considered was one shared
-- account, which loses exactly this on the one surface where it is the point.
CREATE TABLE IF NOT EXISTS collections (
    submission_id  TEXT    PRIMARY KEY REFERENCES submissions(id) ON DELETE CASCADE,
    form_slug      TEXT    NOT NULL,
    collected_at   INTEGER NOT NULL,
    collected_by   TEXT    NOT NULL
) STRICT;

-- The will-call page reads every collection on one form in one query, because
-- a query per row is how a page that is fast with two orders is slow with two
-- hundred on a phone in a car park.
CREATE INDEX IF NOT EXISTS collections_form ON collections (form_slug);

-- One row per submission somebody has taken out of the lists: a test, a
-- duplicate. Its own table for the reason collections is -- a submission is
-- immutable apart from its status -- and a row rather than a delete, because
-- a hidden submission may be a real payment and must stay explicable.
-- submissionbus.Hiding says which readers leave these out.
--
-- The primary key makes hiding twice a no-op rather than two rows, and the
-- listing queries test membership with NOT IN against it, which SQLite
-- answers from this primary key's index.
CREATE TABLE IF NOT EXISTS hidden_submissions (
    submission_id  TEXT    PRIMARY KEY REFERENCES submissions(id) ON DELETE CASCADE,
    form_slug      TEXT    NOT NULL,
    hidden_at      INTEGER NOT NULL,
    hidden_by      TEXT    NOT NULL
) STRICT;

-- One row per set of answers a submission has held and no longer does,
-- written when somebody changes their answers through the link in a message
-- we sent them. The submissions row always holds the current answers, so
-- every list, the CSV and the public listing read one row per submission as
-- they always have; this table is the history behind it, and nothing in it is
-- ever updated or removed except with the submission itself.
--
-- revision counts from 1, which is what was first submitted. answered_at is
-- when these answers became the current ones and replaced_at is when they
-- stopped being, so a row says on its own which stretch of time it covers.
-- total and currency are kept, though a form whose answers can change sells
-- nothing, so that a row reads back through the same code as a submission.
CREATE TABLE IF NOT EXISTS submission_revisions (
    submission_id  TEXT    NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    revision       INTEGER NOT NULL,
    version        TEXT    NOT NULL,
    answers        TEXT    NOT NULL,
    email          TEXT    NOT NULL,
    total          INTEGER NOT NULL,
    currency       TEXT    NOT NULL,
    remote_ip      TEXT    NOT NULL,
    answered_at    INTEGER NOT NULL,
    replaced_at    INTEGER NOT NULL,
    PRIMARY KEY (submission_id, revision)
) STRICT;
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
	case sqldb.IsPrimaryKeyViolation(err):
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

// Change replaces a submission's answers, keeping the ones it held as a
// revision, and spends the nonce -- in one transaction, for the reason Accept
// is one.
//
// The UPDATE is conditional on the row being the one the caller read: the
// same updated_at, and not hidden. Two tabs open on the same link, both
// submitting, is the case that matters. Without the condition the second
// would silently replace the first, and the revision it wrote would claim the
// answers it replaced were the ones from before either of them. With it, the
// second is told [submissionbus.ErrChangedMeanwhile] and nothing is written.
// A hiding that lands between the caller's read and this write is the same
// answer, for the same reason.
func (s *Store) Change(ctx context.Context, before, after submissionbus.Submission, nonce string) (bool, error) {
	was, err := marshalAnswers(before.Answers)
	if err != nil {
		return false, err
	}

	now, err := marshalAnswers(after.Answers)
	if err != nil {
		return false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("beginning the transaction: %w", err)
	}

	defer tx.Rollback()

	const spend = `INSERT INTO spent_grants (nonce, form_slug, spent_at) VALUES (?, ?, ?)`

	_, err = tx.ExecContext(ctx, spend, nonce, after.Form.String(), msOf(after.UpdatedAt))

	switch {
	case sqldb.IsPrimaryKeyViolation(err):
		return false, nil

	case err != nil:
		return false, fmt.Errorf("spending the grant: %w", err)
	}

	const update = `
UPDATE submissions
SET version = ?, answers = ?, email = ?, total = ?, currency = ?, remote_ip = ?, updated_at = ?
WHERE id = ? AND form_slug = ? AND updated_at = ? AND ` + notHidden

	res, err := tx.ExecContext(ctx, update,
		after.Version, now, after.Email.String(), int64(after.Answers.Total), after.Answers.Currency,
		after.RemoteIP, msOf(after.UpdatedAt),
		before.ID.String(), before.Form.String(), msOf(before.UpdatedAt))
	if err != nil {
		return false, fmt.Errorf("changing the submission: %w", err)
	}

	n, err := res.RowsAffected()
	switch {
	case err != nil:
		return false, fmt.Errorf("changing the submission: %w", err)
	case n == 0:
		return false, submissionbus.ErrChangedMeanwhile
	}

	// Numbered inside the transaction, so two changes cannot both take the
	// same number -- and the conditional UPDATE above already means only one
	// of two could reach here.
	const keep = `
INSERT INTO submission_revisions
    (submission_id, revision, version, answers, email, total, currency, remote_ip, answered_at, replaced_at)
SELECT ?, COALESCE(MAX(revision), 0) + 1, ?, ?, ?, ?, ?, ?, ?, ?
FROM submission_revisions WHERE submission_id = ?`

	_, err = tx.ExecContext(ctx, keep,
		before.ID.String(), before.Version, was, before.Email.String(), int64(before.Answers.Total),
		before.Answers.Currency, before.RemoteIP, msOf(before.UpdatedAt), msOf(after.UpdatedAt),
		before.ID.String())
	if err != nil {
		return false, fmt.Errorf("keeping the earlier answers: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("committing the change: %w", err)
	}

	return true, nil
}

// ChangeCounts is how many times each of a form's changed submissions has
// been changed, in one query. A submission never changed is absent.
func (s *Store) ChangeCounts(ctx context.Context, form types.Slug) (map[types.ID]int, error) {
	const q = `
SELECT r.submission_id, COUNT(*)
FROM submission_revisions r JOIN submissions s ON s.id = r.submission_id
WHERE s.form_slug = ?
GROUP BY r.submission_id`

	rows, err := s.db.QueryContext(ctx, q, form.String())
	if err != nil {
		return nil, fmt.Errorf("counting the changes: %w", err)
	}
	defer rows.Close()

	out := make(map[types.ID]int)
	for rows.Next() {
		var (
			raw string
			n   int
		)

		if err := rows.Scan(&raw, &n); err != nil {
			return nil, fmt.Errorf("reading a count of changes: %w", err)
		}

		id, err := types.ParseID(raw)
		if err != nil {
			return nil, fmt.Errorf("the submission identifier on a revision is unreadable: %w", err)
		}

		out[id] = n
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the counts of changes: %w", err)
	}

	return out, nil
}

// ByEmail lists a form's submissions from one address that are not hidden,
// newest first: the ones a "send me my link" request may be answered with.
//
// Scanned rather than indexed. It is one form's rows, read by a person typing
// their address into a page behind the submit throttle, and an index on
// (form_slug, email) would be paid for on every insert to serve that.
func (s *Store) ByEmail(ctx context.Context, form types.Slug, email types.Email) ([]submissionbus.Submission, error) {
	const q = selectColumns + ` WHERE form_slug = ? AND email = ? AND ` + notHidden + ` ORDER BY created_at DESC, id`

	rows, err := s.db.QueryContext(ctx, q, form.String(), email.String())
	if err != nil {
		return nil, fmt.Errorf("querying the submissions from an address: %w", err)
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
		return nil, fmt.Errorf("reading the submissions from an address: %w", err)
	}

	return out, nil
}

// Revisions reads every set of answers a submission has held and no longer
// does, oldest first. Empty for one that has never been changed.
func (s *Store) Revisions(ctx context.Context, sub submissionbus.Submission) ([]submissionbus.Revision, error) {
	const q = `
SELECT revision, version, answers, email, total, currency, remote_ip, answered_at, replaced_at
FROM submission_revisions
WHERE submission_id = ?
ORDER BY revision`

	rows, err := s.db.QueryContext(ctx, q, sub.ID.String())
	if err != nil {
		return nil, fmt.Errorf("querying the earlier answers: %w", err)
	}
	defer rows.Close()

	var out []submissionbus.Revision
	for rows.Next() {
		var (
			number                   int
			version, doc, email, cur string
			remoteIP                 string
			total, from, until       int64
		)

		if err := rows.Scan(&number, &version, &doc, &email, &total, &cur, &remoteIP, &from, &until); err != nil {
			return nil, fmt.Errorf("reading earlier answers: %w", err)
		}

		answers, err := unmarshalAnswers(doc, sub.Form, version, cur, types.Money(total))
		if err != nil {
			return nil, err
		}

		rev := submissionbus.Revision{
			SubmissionID: sub.ID,
			Number:       number,
			Version:      version,
			Answers:      answers,
			RemoteIP:     remoteIP,
			From:         timeOf(from),
			Until:        timeOf(until),
		}

		if email != "" {
			rev.Email, err = types.ParseEmail(email)
			if err != nil {
				return nil, fmt.Errorf("the address on earlier answers is unreadable: %w", err)
			}
		}

		out = append(out, rev)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the earlier answers: %w", err)
	}

	return out, nil
}

// CountSince is how many submissions a form has taken since an instant.
//
// Answered by the (form_slug, created_at) index above, which exists for the
// listing screen and covers this exactly: it is a count of a contiguous range
// of one form's rows, so it is read from the index and never touches the
// table.
func (s *Store) CountSince(ctx context.Context, form types.Slug, since time.Time) (int, error) {
	const q = `SELECT COUNT(*) FROM submissions WHERE form_slug = ? AND created_at >= ?`

	var n int
	if err := s.db.QueryRowContext(ctx, q, form.String(), msOf(since)).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting the submissions: %w", err)
	}

	return n, nil
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

// notHidden is the condition every listing query carries, so that a hidden
// submission is left out by the store rather than by each reader remembering
// to -- a new page listing submissions is correct by default. ByID and
// CountSince do not carry it, on purpose: see submissionbus.Hiding.
const notHidden = `id NOT IN (SELECT submission_id FROM hidden_submissions)`

// ByForm lists a form's submissions, newest first, which is the order the
// index is built for and the order a page wants.
func (s *Store) ByForm(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error) {
	const q = selectColumns + ` WHERE form_slug = ? AND ` + notHidden + ` ORDER BY created_at DESC, id`

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

// Settled lists at most limit of a form's settled submissions, newest first.
//
// Walks the (form_slug, created_at) index in order and stops at the limit, so
// the cost is the rows shown plus whatever unsettled ones lie between them --
// not every row the form has ever taken, which is what ByForm reads.
//
// The statuses come from submissionbus.SettledStatuses rather than being
// written into the SQL, so that what "settled" means is stated in one place,
// beside Status.Settled, and a status added there cannot be forgotten here.
func (s *Store) Settled(ctx context.Context, form types.Slug, limit int) ([]submissionbus.Submission, error) {
	settled := submissionbus.SettledStatuses()

	args := []any{form.String()}
	for _, st := range settled {
		args = append(args, st.String())
	}
	args = append(args, limit)

	q := selectColumns + ` WHERE form_slug = ? AND ` + notHidden + ` AND status IN (?` +
		strings.Repeat(", ?", len(settled)-1) + `) ORDER BY created_at DESC, id LIMIT ?`

	rows, err := s.db.QueryContext(ctx, q, args...)
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

// Unpaid lists submissions still waiting for money, oldest first, within a
// window.
//
// Two bounds rather than one. The upper is the grace period -- an order placed
// four minutes ago is somebody typing a card number, not an abandoned one. The
// lower is a floor under how far back this ever looks, so that a service whose
// record of what it has already reported is lost cannot mail the office about
// every order anybody ever abandoned.
//
// Oldest first, because the list goes into a message somebody reads top to
// bottom and the one most likely to matter is the one that has been waiting
// longest.
func (s *Store) Unpaid(ctx context.Context, from, before time.Time) ([]submissionbus.Submission, error) {
	const q = selectColumns + ` WHERE status = ? AND ` + notHidden + ` AND created_at >= ? AND created_at < ? ORDER BY created_at, id`

	rows, err := s.db.QueryContext(ctx, q, submissionbus.StatusPending.String(), msOf(from), msOf(before))
	if err != nil {
		return nil, fmt.Errorf("querying the unpaid submissions: %w", err)
	}
	defer rows.Close()

	var out []submissionbus.Submission
	for rows.Next() {
		sub, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("reading an unpaid submission: %w", err)
		}

		out = append(out, sub)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the unpaid submissions: %w", err)
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

// Collect records that an order's tokens were handed over, and reports false
// when somebody had already recorded it.
//
// The bool rather than an error for that case is the shape Accept uses and for
// the same reason: "somebody else got there first" is an outcome rather than a
// fault. The caller wants to show who, so the row that won comes back either
// way.
func (s *Store) Collect(ctx context.Context, c submissionbus.Collection) (submissionbus.Collection, bool, error) {
	const insert = `
INSERT INTO collections (submission_id, form_slug, collected_at, collected_by)
VALUES (?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, insert,
		c.SubmissionID.String(), c.Form.String(), msOf(c.CollectedAt), c.CollectedBy.String())

	switch {
	case sqldb.IsPrimaryKeyViolation(err):
		won, err := s.collection(ctx, c.SubmissionID)
		if err != nil {
			return submissionbus.Collection{}, false, err
		}

		return won, false, nil

	case err != nil:
		return submissionbus.Collection{}, false, fmt.Errorf("recording the collection: %w", err)
	}

	return c, true, nil
}

// Uncollect takes the mark off again, for the mis-tap that is going to happen
// at a table at eight in the morning.
//
// Removing nothing is not an error, for the reason accessdb.Delete gives: the
// caller wanted the mark gone and it is gone.
func (s *Store) Uncollect(ctx context.Context, id types.ID) error {
	const q = `DELETE FROM collections WHERE submission_id = ?`

	if _, err := s.db.ExecContext(ctx, q, id.String()); err != nil {
		return fmt.Errorf("removing the collection: %w", err)
	}

	return nil
}

// CollectionsForForm reads every collection on one form, keyed by submission.
//
// One query for the whole page rather than one per row, which is the shape
// peopleapp and submissionapp already use for the same kind of lookup.
func (s *Store) CollectionsForForm(ctx context.Context, form types.Slug) (map[types.ID]submissionbus.Collection, error) {
	const q = `
SELECT submission_id, form_slug, collected_at, collected_by
FROM collections
WHERE form_slug = ?`

	rows, err := s.db.QueryContext(ctx, q, form.String())
	if err != nil {
		return nil, fmt.Errorf("reading the collections: %w", err)
	}
	defer rows.Close()

	out := map[types.ID]submissionbus.Collection{}

	for rows.Next() {
		c, err := scanCollection(rows.Scan)
		if err != nil {
			return nil, err
		}

		out[c.SubmissionID] = c
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the collections: %w", err)
	}

	return out, nil
}

// collection reads one, which is only ever the row that won a race.
func (s *Store) collection(ctx context.Context, id types.ID) (submissionbus.Collection, error) {
	const q = `
SELECT submission_id, form_slug, collected_at, collected_by
FROM collections
WHERE submission_id = ?`

	c, err := scanCollection(s.db.QueryRowContext(ctx, q, id.String()).Scan)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// The row that violated the primary key a moment ago and is not there
		// now. Only reachable if somebody uncollected it in between, which is
		// a person at a table rather than a fault.
		return submissionbus.Collection{}, fmt.Errorf("%w: the collection was removed while it was being read", submissionbus.ErrNotFound)
	case err != nil:
		return submissionbus.Collection{}, err
	}

	return c, nil
}

// scanCollection decodes one row, from either the single-row or the many-row
// path -- the scan function is the only thing that differs between them.
func scanCollection(scan func(...any) error) (submissionbus.Collection, error) {
	var (
		id     string
		form   string
		at     int64
		byWhom string
	)

	if err := scan(&id, &form, &at, &byWhom); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return submissionbus.Collection{}, err
		}

		return submissionbus.Collection{}, fmt.Errorf("reading a collection: %w", err)
	}

	submissionID, err := types.ParseID(id)
	if err != nil {
		return submissionbus.Collection{}, fmt.Errorf("a collection names an unreadable submission %q: %w", id, err)
	}

	slug, err := types.ParseSlug(form)
	if err != nil {
		return submissionbus.Collection{}, fmt.Errorf("a collection names an unreadable form %q: %w", form, err)
	}

	// Not parsed strictly: the column records which volunteer it was, and a
	// page that refused to show an order because that string had stopped being
	// an id would be refusing over an audit field.
	by, _ := types.ParseID(byWhom)

	return submissionbus.Collection{
		SubmissionID: submissionID,
		Form:         slug,
		CollectedAt:  timeOf(at),
		CollectedBy:  by,
	}, nil
}

func msOf(t time.Time) int64    { return t.UTC().UnixMilli() }
func timeOf(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// Hide records a hiding, or reports the one already there.
func (s *Store) Hide(ctx context.Context, h submissionbus.Hiding) (submissionbus.Hiding, bool, error) {
	const insert = `
INSERT INTO hidden_submissions (submission_id, form_slug, hidden_at, hidden_by)
VALUES (?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, insert,
		h.SubmissionID.String(), h.Form.String(), msOf(h.HiddenAt), h.HiddenBy.String())

	switch {
	case sqldb.IsPrimaryKeyViolation(err):
		was, ok, err := s.HidingOf(ctx, h.SubmissionID)
		switch {
		case err != nil:
			return submissionbus.Hiding{}, false, err
		case !ok:
			// Unhidden in the moment between the two statements, which is two
			// people disagreeing rather than a fault.
			return submissionbus.Hiding{}, false, fmt.Errorf("%w: the hiding was removed while it was being read", submissionbus.ErrNotFound)
		}

		return was, false, nil

	case err != nil:
		return submissionbus.Hiding{}, false, fmt.Errorf("recording the hiding: %w", err)
	}

	return h, true, nil
}

// Unhide removes a hiding. Removing nothing is not an error.
func (s *Store) Unhide(ctx context.Context, id types.ID) error {
	const q = `DELETE FROM hidden_submissions WHERE submission_id = ?`

	if _, err := s.db.ExecContext(ctx, q, id.String()); err != nil {
		return fmt.Errorf("removing the hiding: %w", err)
	}

	return nil
}

// Hidden lists a form's hidden submissions, newest first.
func (s *Store) Hidden(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error) {
	const q = selectColumns + ` WHERE form_slug = ? AND id IN (SELECT submission_id FROM hidden_submissions) ORDER BY created_at DESC, id`

	rows, err := s.db.QueryContext(ctx, q, form.String())
	if err != nil {
		return nil, fmt.Errorf("querying the hidden submissions: %w", err)
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
		return nil, fmt.Errorf("reading the hidden submissions: %w", err)
	}

	return out, nil
}

// HidingOf reads one submission's hiding, and whether there is one.
func (s *Store) HidingOf(ctx context.Context, id types.ID) (submissionbus.Hiding, bool, error) {
	const q = `
SELECT submission_id, form_slug, hidden_at, hidden_by
FROM hidden_submissions
WHERE submission_id = ?`

	var (
		sid, form, byWhom string
		at                int64
	)

	err := s.db.QueryRowContext(ctx, q, id.String()).Scan(&sid, &form, &at, &byWhom)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return submissionbus.Hiding{}, false, nil
	case err != nil:
		return submissionbus.Hiding{}, false, fmt.Errorf("reading the hiding: %w", err)
	}

	slug, err := types.ParseSlug(form)
	if err != nil {
		return submissionbus.Hiding{}, false, fmt.Errorf("a hiding names an unreadable form %q: %w", form, err)
	}

	// Not parsed strictly, for the reason scanCollection gives: it is an
	// audit field, and refusing to say a row is hidden over it would put the
	// row back in every list.
	by, _ := types.ParseID(byWhom)

	return submissionbus.Hiding{SubmissionID: id, Form: slug, HiddenAt: timeOf(at), HiddenBy: by}, true, nil
}

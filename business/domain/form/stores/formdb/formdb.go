// Package formdb keeps the form definitions that were authored in a browser.
//
// The other store, formtoml, reads definitions out of files compiled into the
// binary and can only be written by a pull request. This one is what the
// visual builder writes to. Both produce the same [formbus.Form] and both go
// through Stamp and Check before anything is served, so neither can put a
// definition in front of somebody that the other would have refused.
//
// # One JSON column, not five tables
//
// A definition is a document. It is read whole, written whole, and nothing
// anywhere queries inside one: no page asks which forms have a field called
// email, and no report groups by option value. Normalising fields, options and
// items into their own tables would buy an index nobody reads, and cost four
// joins on every read, a transaction on every save, and a migration every time
// the definition grows an attribute -- which, for a form builder, is the thing
// most likely to keep happening.
//
// # The wire shape is its own type
//
// Same bargain formtoml makes, for the same reason: renaming a field on
// [formbus.Form] must not rewrite every row already in the database, and a
// column added to the domain type must not silently read as its zero value out
// of documents written before it existed. The JSON names here are a
// compatibility surface and change only deliberately.
//
// Two places where this shape deliberately differs from the TOML one. Money is
// stored as whole minor units rather than as "5.00", because nobody types this
// document -- the strict parser that string exists for has already run in the
// app layer, and storing the number it produced is one fewer chance to
// re-parse it differently. And a definition carries no version, because a
// version is derived from the content: it is recomputed by Stamp on the way
// out, so a stored copy of it could only ever be a second answer waiting to
// disagree with the first.
package formdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// Store is the SQLite implementation of formbus.Storer.
type Store struct {
	db *sql.DB
}

// NewStore constructs one.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Expected is what CheckSchema verifies at startup: the columns this binary
// will read.
var Expected = sqldb.Expected{
	"form_definitions": {"slug", "definition", "live", "created_at", "updated_at", "updated_by", "published_at"},
}

// Init creates this store's table. Idempotent, and run at every startup.
func Init(ctx context.Context, db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS form_definitions (
    slug         TEXT    PRIMARY KEY,
    definition   TEXT    NOT NULL,
    live         INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    updated_by   TEXT    NOT NULL,
    published_at INTEGER
) STRICT;

-- slug is the row's identity and is deliberately not repeated inside the
-- definition column. One name, in one place, so the two cannot drift; a form
-- is renamed by moving the row, which is a thing nothing here offers because
-- the slug is in the URL somebody has already pasted into their website.
--
-- updated_by is not a foreign key, for the same reason accessdb.granted_by is
-- not: ON DELETE CASCADE on an audit field would mean removing an account
-- deleted the forms that account had last edited. It holds '' when nobody in
-- particular did the editing.
--
-- published_at is NULL until the first time a definition goes live and stays
-- set after it is taken down again. formbus.Delete reads it, and nothing else
-- does: a form that has ever been served may have submissions pointing at it,
-- and those rows are unreadable without the definition that names their
-- columns.
`

	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("creating the form definition table: %w", err)
	}

	return nil
}

// All reads every authored definition, live or not.
func (s *Store) All(ctx context.Context) ([]formbus.Stored, error) {
	const q = `
SELECT slug, definition, live, created_at, updated_at, updated_by, published_at
FROM form_definitions
ORDER BY slug`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("reading the form definitions: %w", err)
	}
	defer rows.Close()

	var out []formbus.Stored

	for rows.Next() {
		st, err := scan(rows)
		if err != nil {
			return nil, err
		}

		out = append(out, st)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the form definitions: %w", err)
	}

	return out, nil
}

// ByID reads one, and returns formbus.ErrNotFound when there is none.
func (s *Store) ByID(ctx context.Context, slug types.Slug) (formbus.Stored, error) {
	const q = `
SELECT slug, definition, live, created_at, updated_at, updated_by, published_at
FROM form_definitions
WHERE slug = ?`

	st, err := scan(s.db.QueryRowContext(ctx, q, slug.String()))

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return formbus.Stored{}, fmt.Errorf("%w: %s", formbus.ErrNotFound, slug)
	case err != nil:
		return formbus.Stored{}, err
	}

	return st, nil
}

// Upsert writes a definition, replacing whatever was there.
//
// The whole row, including created_at, because the caller read it first and is
// handing back what it read. A partial update that left created_at alone would
// be the same write with one more thing this statement has to be right about.
func (s *Store) Upsert(ctx context.Context, st formbus.Stored) error {
	body, err := encode(st.Form)
	if err != nil {
		return err
	}

	const q = `
INSERT INTO form_definitions (slug, definition, live, created_at, updated_at, updated_by, published_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (slug) DO UPDATE SET
    definition   = excluded.definition,
    live         = excluded.live,
    updated_at   = excluded.updated_at,
    updated_by   = excluded.updated_by,
    published_at = excluded.published_at`

	_, err = s.db.ExecContext(ctx, q,
		st.Form.ID.String(), body, st.Live,
		msOf(st.CreatedAt), msOf(st.UpdatedAt), st.UpdatedBy.String(), msOrNull(st.PublishedAt))
	if err != nil {
		return fmt.Errorf("writing the form %s: %w", st.Form.ID, err)
	}

	return nil
}

// Delete removes a definition.
//
// Deleting nothing is not an error, for the reason accessdb.Delete gives:
// the caller wanted the row gone and it is gone, and reporting "no such row"
// would make every caller need a story for somebody else having got there
// first.
func (s *Store) Delete(ctx context.Context, slug types.Slug) error {
	const q = `DELETE FROM form_definitions WHERE slug = ?`

	if _, err := s.db.ExecContext(ctx, q, slug.String()); err != nil {
		return fmt.Errorf("deleting the form %s: %w", slug, err)
	}

	return nil
}

// row is what scan reads from, so that the single-row and many-row paths share
// one decoder rather than two copies of the same seven columns.
type row interface {
	Scan(dest ...any) error
}

func scan(r row) (formbus.Stored, error) {
	var (
		slug        string
		body        string
		live        bool
		createdAt   int64
		updatedAt   int64
		updatedBy   string
		publishedAt sql.NullInt64
	)

	if err := r.Scan(&slug, &body, &live, &createdAt, &updatedAt, &updatedBy, &publishedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return formbus.Stored{}, err
		}

		return formbus.Stored{}, fmt.Errorf("reading a form definition: %w", err)
	}

	id, err := types.ParseSlug(slug)
	if err != nil {
		return formbus.Stored{}, fmt.Errorf("the stored form %q does not have a usable name: %w", slug, err)
	}

	f, err := decode(body)
	if err != nil {
		return formbus.Stored{}, fmt.Errorf("the stored form %s could not be read: %w", slug, err)
	}

	f.ID = id

	// Not parsed strictly, and not refused if it is empty. The column records
	// who last edited a definition; a service that would not serve a form
	// because that string has stopped being an id would be refusing to show
	// somebody their own work over an audit field.
	by, _ := types.ParseID(updatedBy)

	st := formbus.Stored{
		Form:      f,
		Live:      live,
		CreatedAt: timeOf(createdAt),
		UpdatedAt: timeOf(updatedAt),
		UpdatedBy: by,
	}

	if publishedAt.Valid {
		st.PublishedAt = timeOf(publishedAt.Int64)
	}

	return st, nil
}

func msOf(t time.Time) int64 { return t.UTC().UnixMilli() }

func timeOf(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// msOrNull keeps "never" out of the column as NULL rather than as the Unix
// epoch, which is a real instant and would read as a form published in 1970.
func msOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}

	return msOf(t)
}

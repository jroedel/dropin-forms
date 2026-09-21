// Package accessdb stores per-form grants in SQLite.
//
// One row per (account, form), because a grant is keyed by the pair and the
// roles imply one another -- holding admin and results at once would be two
// rows saying the same thing, with the second one free to disagree.
//
// Times are Unix milliseconds in an INTEGER column, for the reason userdb's
// package comment sets out at length: Go's RFC 3339 formatting drops trailing
// zeros from the fractional second, and '.' sorts before 'Z', so instants
// stored as text do not compare in chronological order.
package accessdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// Store is the SQLite implementation of accessbus.Storer.
type Store struct {
	db *sql.DB
}

// NewStore constructs one.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Expected is what CheckSchema verifies at startup: the columns this binary
// will read.
var Expected = sqldb.Expected{
	"grants": {"user_id", "form_slug", "role", "granted_by", "granted_at"},
}

// Init creates this domain's table. Idempotent, and run at every startup.
func Init(ctx context.Context, db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS grants (
    user_id     TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    form_slug   TEXT    NOT NULL,
    role        TEXT    NOT NULL,
    granted_by  TEXT    NOT NULL,
    granted_at  INTEGER NOT NULL,

    PRIMARY KEY (user_id, form_slug)
) STRICT;

-- form_slug is '' for a site-wide grant, and is not a foreign key: forms are
-- defined in TOML and loaded at startup, so there is no table to point at.
--
-- granted_by is not a foreign key either, and that one is a decision rather
-- than a consequence. ON DELETE CASCADE on it would mean that deleting an
-- account silently withdrew every grant that account had ever made, which is
-- an audit field deleting real authority. It holds '' when the bootstrap
-- secret did the granting, because no person did.

CREATE INDEX IF NOT EXISTS grants_form_slug ON grants (form_slug);
`

	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("creating the grant table: %w", err)
	}

	return nil
}

// Upsert writes a grant, replacing whatever role the account held on that form.
func (s *Store) Upsert(ctx context.Context, g accessbus.Grant) error {
	const q = `
INSERT INTO grants (user_id, form_slug, role, granted_by, granted_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (user_id, form_slug) DO UPDATE SET
    role       = excluded.role,
    granted_by = excluded.granted_by,
    granted_at = excluded.granted_at`

	_, err := s.db.ExecContext(ctx, q,
		g.UserID.String(), g.Form.String(), g.Role.String(), g.GrantedBy.String(), msOf(g.GrantedAt))
	if err != nil {
		return fmt.Errorf("inserting the grant: %w", err)
	}

	return nil
}

// Delete removes whatever grant an account holds on a form.
//
// Deleting nothing is not an error. The caller wanted the grant gone and it is
// gone; reporting "no such row" would make every revoke button need a story
// for the case where somebody else clicked it first.
func (s *Store) Delete(ctx context.Context, userID types.ID, form types.Slug) error {
	const q = `DELETE FROM grants WHERE user_id = ? AND form_slug = ?`

	if _, err := s.db.ExecContext(ctx, q, userID.String(), form.String()); err != nil {
		return fmt.Errorf("deleting the grant: %w", err)
	}

	return nil
}

// ByUserAndForm reads one grant, and returns accessbus.ErrNotFound if there is
// none. This is the read on the authorisation path, and it is a primary-key
// lookup.
func (s *Store) ByUserAndForm(ctx context.Context, userID types.ID, form types.Slug) (accessbus.Grant, error) {
	const q = `
SELECT user_id, form_slug, role, granted_by, granted_at
FROM grants
WHERE user_id = ? AND form_slug = ?`

	g, err := s.one(ctx, q, userID.String(), form.String())
	if err != nil {
		return accessbus.Grant{}, err
	}

	return g, nil
}

// ByUser lists every grant an account holds, site-wide first and then by form.
func (s *Store) ByUser(ctx context.Context, userID types.ID) ([]accessbus.Grant, error) {
	const q = `
SELECT user_id, form_slug, role, granted_by, granted_at
FROM grants
WHERE user_id = ?
ORDER BY form_slug`

	return s.many(ctx, q, userID.String())
}

// ByForm lists every grant on one form. Passing the zero slug lists the
// site-wide grants, which is how accessbus counts administrators.
func (s *Store) ByForm(ctx context.Context, form types.Slug) ([]accessbus.Grant, error) {
	const q = `
SELECT user_id, form_slug, role, granted_by, granted_at
FROM grants
WHERE form_slug = ?
ORDER BY user_id`

	return s.many(ctx, q, form.String())
}

// All lists every grant.
func (s *Store) All(ctx context.Context) ([]accessbus.Grant, error) {
	const q = `
SELECT user_id, form_slug, role, granted_by, granted_at
FROM grants
ORDER BY form_slug, user_id`

	return s.many(ctx, q)
}

func (s *Store) one(ctx context.Context, q string, args ...any) (accessbus.Grant, error) {
	g, err := scan(s.db.QueryRowContext(ctx, q, args...))

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return accessbus.Grant{}, accessbus.ErrNotFound
	case err != nil:
		return accessbus.Grant{}, fmt.Errorf("reading the grant: %w", err)
	}

	return g, nil
}

func (s *Store) many(ctx context.Context, q string, args ...any) ([]accessbus.Grant, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("querying the grants: %w", err)
	}
	defer rows.Close()

	var out []accessbus.Grant
	for rows.Next() {
		g, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("reading a grant: %w", err)
		}

		out = append(out, g)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the grants: %w", err)
	}

	return out, nil
}

// scanner is what QueryRow and Rows have in common, so one scan function
// serves both.
type scanner interface {
	Scan(dest ...any) error
}

// scan turns one row into a Grant, parsing every stored value back through
// its own type rather than casting.
//
// A row this binary cannot read is an error rather than a partial Grant. The
// case that matters is role: a value written by a newer binary would otherwise
// arrive as a Role this one does not recognise, and while Role.Includes
// refuses an unrecognised role, a permission decision resting on a value
// nobody parsed is not one to leave standing.
//
// ParseStoredRole and not ParseRole, because the question here is "is this a
// role at all" rather than "is this a role somebody may be given on one form".
// The two differ by RoleCreator, which only a site-wide row holds, and asking
// the narrower question of storage makes that row writable but unreadable.
func scan(row scanner) (accessbus.Grant, error) {
	var (
		userID    string
		formSlug  string
		role      string
		grantedBy string
		grantedAt int64
	)

	if err := row.Scan(&userID, &formSlug, &role, &grantedBy, &grantedAt); err != nil {
		return accessbus.Grant{}, err
	}

	id, err := types.ParseID(userID)
	if err != nil {
		return accessbus.Grant{}, fmt.Errorf("the account identifier on a grant is unreadable: %w", err)
	}

	r, err := accessbus.ParseStoredRole(role)
	if err != nil {
		return accessbus.Grant{}, fmt.Errorf("the role on a grant is unreadable: %w", err)
	}

	g := accessbus.Grant{
		UserID:    id,
		Role:      r,
		GrantedAt: timeOf(grantedAt),
	}

	// The empty slug is the site-wide grant and is not a parse failure; every
	// other value has to be a slug this binary would accept.
	if formSlug != "" {
		g.Form, err = types.ParseSlug(formSlug)
		if err != nil {
			return accessbus.Grant{}, fmt.Errorf("the form name on a grant is unreadable: %w", err)
		}
	}

	// Likewise the empty granter, which means the configuration did it.
	if grantedBy != "" {
		g.GrantedBy, err = types.ParseID(grantedBy)
		if err != nil {
			return accessbus.Grant{}, fmt.Errorf("the granter on a grant is unreadable: %w", err)
		}
	}

	return g, nil
}

// msOf converts an instant to Unix milliseconds.
func msOf(t time.Time) int64 { return t.UTC().UnixMilli() }

// timeOf converts back, in UTC.
func timeOf(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

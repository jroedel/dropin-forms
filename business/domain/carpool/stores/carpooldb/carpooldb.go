// Package carpooldb stores who is riding in whose car.
//
// One row per rider, keyed by their submission, naming the driver's
// submission or nobody while they wait. No foreign keys to submissions: a
// submission is never deleted, only hidden, and carpoolbus forgets a rider
// whose submission stops counting on its next pass -- so a key would protect
// against nothing, and would make the order the two domains' tables are
// created in matter.
//
// Times are Unix milliseconds in an INTEGER column, for userdb's reason.
package carpooldb

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/carpool/carpoolbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// Store is the SQLite implementation of carpoolbus.Storer.
type Store struct {
	db *sql.DB
}

// NewStore constructs one.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Expected is what CheckSchema verifies at startup.
var Expected = sqldb.Expected{
	"rides": {"rider_id", "form_slug", "driver_id", "name", "since", "placed_at", "placed_by",
		"rider_texted_at", "driver_texted_at", "problem"},
	"carpool_choices": {"sub_id", "form_slug", "choice", "seats", "at"},
	"cars":            {"driver_id", "form_slug", "changed_at", "summarised_at", "problem"},
}

// Init creates this domain's table. Idempotent, and run at every startup.
func Init(ctx context.Context, db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS rides (
    rider_id          TEXT    PRIMARY KEY,
    form_slug         TEXT    NOT NULL,
    driver_id         TEXT    NOT NULL DEFAULT '',
    name              TEXT    NOT NULL DEFAULT '',
    since             INTEGER NOT NULL,
    placed_at         INTEGER NOT NULL DEFAULT 0,
    placed_by         TEXT    NOT NULL DEFAULT '',
    rider_texted_at   INTEGER NOT NULL DEFAULT 0,
    driver_texted_at  INTEGER NOT NULL DEFAULT 0,
    problem           TEXT    NOT NULL DEFAULT ''
) STRICT;

-- driver_id is '' while the rider waits. The count of a car's riders is the
-- count of rows naming its driver, which is what the index is for.

CREATE INDEX IF NOT EXISTS rides_form ON rides (form_slug);
CREATE INDEX IF NOT EXISTS rides_driver ON rides (driver_id);

-- What somebody said on their own ride page, which takes the place of their
-- carpool answer until they change their answers again. One row per
-- submission: the latest word is the only one that counts.
CREATE TABLE IF NOT EXISTS carpool_choices (
    sub_id     TEXT    PRIMARY KEY,
    form_slug  TEXT    NOT NULL,
    choice     TEXT    NOT NULL,
    seats      INTEGER NOT NULL DEFAULT 0,
    at         INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS carpool_choices_form ON carpool_choices (form_slug);

-- One row per driver once they have been welcomed: when their car's riders
-- last changed, and when they were last told who is in it. A summary is due
-- when the first is later than the second and the car has been quiet a while.
CREATE TABLE IF NOT EXISTS cars (
    driver_id      TEXT    PRIMARY KEY,
    form_slug      TEXT    NOT NULL,
    changed_at     INTEGER NOT NULL DEFAULT 0,
    summarised_at  INTEGER NOT NULL DEFAULT 0,
    problem        TEXT    NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX IF NOT EXISTS cars_form ON cars (form_slug);
`

	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("creating the rides table: %w", err)
	}

	return nil
}

// ByForm lists a form's rides, oldest first.
func (s *Store) ByForm(ctx context.Context, form types.Slug) ([]carpoolbus.Ride, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT rider_id, form_slug, driver_id, name, since, placed_at, placed_by, rider_texted_at, driver_texted_at, problem
FROM rides WHERE form_slug = ? ORDER BY since, rider_id`, form.String())
	if err != nil {
		return nil, fmt.Errorf("reading the rides: %w", err)
	}
	defer rows.Close()

	var out []carpoolbus.Ride

	for rows.Next() {
		var (
			r                                     carpoolbus.Ride
			rider, slug, driver, by               string
			since, placed, riderTexted, drvTexted int64
		)

		if err := rows.Scan(&rider, &slug, &driver, &r.Name, &since, &placed, &by, &riderTexted, &drvTexted, &r.Problem); err != nil {
			return nil, fmt.Errorf("reading a ride: %w", err)
		}

		if r.Rider, err = types.ParseID(rider); err != nil {
			return nil, fmt.Errorf("a stored ride has a bad rider: %w", err)
		}

		if r.Form, err = types.ParseSlug(slug); err != nil {
			return nil, fmt.Errorf("a stored ride has a bad form: %w", err)
		}

		if r.Driver, err = optionalID(driver); err != nil {
			return nil, fmt.Errorf("a stored ride has a bad driver: %w", err)
		}

		if r.PlacedBy, err = optionalID(by); err != nil {
			return nil, fmt.Errorf("a stored ride has a bad organiser: %w", err)
		}

		r.Since, r.PlacedAt = timeOf(since), timeOf(placed)
		r.RiderTexted, r.DriverTexted = timeOf(riderTexted), timeOf(drvTexted)

		out = append(out, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the rides: %w", err)
	}

	return out, nil
}

// Wait records a rider as waiting, keeping when they started if they already
// were, and reports whether the row is new.
func (s *Store) Wait(ctx context.Context, form types.Slug, rider types.ID, name string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
INSERT INTO rides (rider_id, form_slug, name, since) VALUES (?, ?, ?, ?)
ON CONFLICT (rider_id) DO NOTHING`, rider.String(), form.String(), name, msOf(now))
	if err != nil {
		return false, fmt.Errorf("recording a rider as waiting: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("recording a rider as waiting: %w", err)
	}

	return n == 1, nil
}

// Place puts a rider in a car if it has a seat, in one transaction with the
// count. The service holds one connection to the database
// (foundation/sqldb), so nothing can be written between the count and the
// write; the transaction is what makes the two one step even so.
func (s *Store) Place(ctx context.Context, form types.Slug, rider, driver types.ID, name string, seats int, now time.Time, by types.ID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting to place a rider: %w", err)
	}
	defer tx.Rollback()

	var in int

	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM rides WHERE driver_id = ? AND rider_id <> ?`,
		driver.String(), rider.String()).Scan(&in); err != nil {
		return fmt.Errorf("counting a car's riders: %w", err)
	}

	if in >= seats {
		return carpoolbus.ErrCarFull
	}

	byText := ""
	if !by.Zero() {
		byText = by.String()
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO rides (rider_id, form_slug, driver_id, name, since, placed_at, placed_by)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (rider_id) DO UPDATE SET
    driver_id = excluded.driver_id,
    name = excluded.name,
    placed_at = excluded.placed_at,
    placed_by = excluded.placed_by,
    rider_texted_at = 0,
    driver_texted_at = 0,
    problem = ''`,
		rider.String(), form.String(), driver.String(), name, msOf(now), msOf(now), byText); err != nil {
		return fmt.Errorf("placing a rider: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("placing a rider: %w", err)
	}

	return nil
}

// Release takes a rider out of their car and back to waiting.
func (s *Store) Release(ctx context.Context, rider types.ID) error {
	if _, err := s.db.ExecContext(ctx, `
UPDATE rides SET driver_id = '', placed_at = 0, placed_by = '', rider_texted_at = 0, driver_texted_at = 0, problem = ''
WHERE rider_id = ?`, rider.String()); err != nil {
		return fmt.Errorf("taking a rider out of a car: %w", err)
	}

	return nil
}

// Remove forgets a rider.
func (s *Store) Remove(ctx context.Context, rider types.ID) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM rides WHERE rider_id = ?`, rider.String()); err != nil {
		return fmt.Errorf("forgetting a rider: %w", err)
	}

	return nil
}

// Texted records who was told about a ride and what went wrong.
func (s *Store) Texted(ctx context.Context, rider types.ID, riderAt, driverAt time.Time, problem string) error {
	if _, err := s.db.ExecContext(ctx, `
UPDATE rides SET rider_texted_at = ?, driver_texted_at = ?, problem = ? WHERE rider_id = ?`,
		msOf(riderAt), msOf(driverAt), problem, rider.String()); err != nil {
		return fmt.Errorf("recording a ride's texts: %w", err)
	}

	return nil
}

func optionalID(s string) (types.ID, error) {
	if s == "" {
		return types.ID{}, nil
	}

	return types.ParseID(s)
}

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

// Choices lists what the people on a form said on their ride pages.
func (s *Store) Choices(ctx context.Context, form types.Slug) ([]carpoolbus.Choice, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT sub_id, choice, seats, at FROM carpool_choices WHERE form_slug = ?`, form.String())
	if err != nil {
		return nil, fmt.Errorf("reading the carpool choices: %w", err)
	}
	defer rows.Close()

	var out []carpoolbus.Choice

	for rows.Next() {
		var (
			c   carpoolbus.Choice
			sub string
			at  int64
		)

		if err := rows.Scan(&sub, &c.Choice, &c.Seats, &at); err != nil {
			return nil, fmt.Errorf("reading a carpool choice: %w", err)
		}

		if c.Sub, err = types.ParseID(sub); err != nil {
			return nil, fmt.Errorf("a stored carpool choice has a bad submission: %w", err)
		}

		c.Form, c.At = form, timeOf(at)
		out = append(out, c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the carpool choices: %w", err)
	}

	return out, nil
}

// SetChoice records what somebody said on their ride page, in place of
// anything they said there before.
func (s *Store) SetChoice(ctx context.Context, c carpoolbus.Choice) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO carpool_choices (sub_id, form_slug, choice, seats, at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (sub_id) DO UPDATE SET choice = excluded.choice, seats = excluded.seats, at = excluded.at`,
		c.Sub.String(), c.Form.String(), c.Choice, c.Seats, msOf(c.At))
	if err != nil {
		return fmt.Errorf("recording a carpool choice: %w", err)
	}

	return nil
}

// Cars lists what has been texted to a form's drivers.
func (s *Store) Cars(ctx context.Context, form types.Slug) ([]carpoolbus.CarTexts, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT driver_id, changed_at, summarised_at, problem FROM cars WHERE form_slug = ?`, form.String())
	if err != nil {
		return nil, fmt.Errorf("reading the cars: %w", err)
	}
	defer rows.Close()

	var out []carpoolbus.CarTexts

	for rows.Next() {
		var (
			c                   carpoolbus.CarTexts
			driver              string
			changed, summarised int64
		)

		if err := rows.Scan(&driver, &changed, &summarised, &c.Problem); err != nil {
			return nil, fmt.Errorf("reading a car: %w", err)
		}

		if c.Driver, err = types.ParseID(driver); err != nil {
			return nil, fmt.Errorf("a stored car has a bad driver: %w", err)
		}

		c.Form, c.ChangedAt, c.SummarisedAt = form, timeOf(changed), timeOf(summarised)
		out = append(out, c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the cars: %w", err)
	}

	return out, nil
}

// Changed records that a driver's riders changed. A driver not yet welcomed
// has no row and nothing to record: their welcome says who is in the car.
func (s *Store) Changed(ctx context.Context, driver types.ID, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE cars SET changed_at = ? WHERE driver_id = ?`, msOf(at), driver.String()); err != nil {
		return fmt.Errorf("recording that a car changed: %w", err)
	}

	return nil
}

// Summarised records that a driver was told who is in their car, or why the
// text did not go. The first time, it is the welcome, and the row is made.
func (s *Store) Summarised(ctx context.Context, form types.Slug, driver types.ID, at time.Time, problem string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO cars (driver_id, form_slug, changed_at, summarised_at, problem) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (driver_id) DO UPDATE SET summarised_at = excluded.summarised_at, problem = excluded.problem`,
		driver.String(), form.String(), msOf(at), msOf(at), problem)
	if err != nil {
		return fmt.Errorf("recording a driver's text: %w", err)
	}

	return nil
}

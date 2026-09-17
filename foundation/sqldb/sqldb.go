// Package sqldb opens the one SQLite database this service uses, and answers
// whether its schema is the one the running binary expects.
//
// The driver is modernc.org/sqlite, which is a dependency because the standard
// library has no SQL driver. Pure Go specifically, rather than the cgo one: the
// release build is CGO_ENABLED=0, so that the artefact is a single file with no
// libc on the server to match against. An ordinary build links the builder's
// glibc and dies on the host with GLIBC_2.xx not found.
package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"

	_ "modernc.org/sqlite"
)

// Open returns the shared handle, with the pragmas this service needs set in
// the DSN so that they apply to every connection rather than to whichever one
// happened to run a SET statement.
//
// MaxOpenConns(1) is deliberate and is not a performance oversight. SQLite has
// one writer; allowing several connections means "database is locked" becomes a
// thing that happens under concurrency, and the fix is a retry loop in every
// caller. One connection makes the queue explicit and the failure mode absent.
// A form-intake service for a shrine does not need more, and the day it does,
// the change is a considered one rather than a surprise.
func Open(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?" + strings.Join([]string{
		// Readers do not block the writer and the writer does not block
		// readers. Also the reason the deploy copies the database only while
		// the app is stopped: a live copy can catch a torn page set mid
		// transaction.
		"_pragma=journal_mode(WAL)",

		// Wait rather than failing instantly if a write is in progress. With
		// one connection this should never fire; it is here for the backup
		// process, which is a second writer by definition.
		"_pragma=busy_timeout(5000)",

		// Off by default in SQLite, which surprises everybody exactly once.
		"_pragma=foreign_keys(on)",

		// WAL plus NORMAL is the usual pairing: a crash can lose the last
		// transactions but cannot corrupt the file, and the alternative costs
		// an fsync per commit on a shared disk.
		"_pragma=synchronous(normal)",
	}, "&")

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	db.SetMaxOpenConns(1)

	return db, nil
}

// Init creates the infrastructure table. Domain tables belong to their own
// stores; this one is about the database rather than about forms.
//
// The DDL is idempotent and runs at every startup, which is the migration
// story in both sibling projects and is inherited on purpose. A column added
// later arrives as an ALTER with a DEFAULT, so that on a fresh database the
// CREATE already made it and the ALTER finds nothing to do, and on an existing
// one the CREATE is the no-op.
func Init(ctx context.Context, db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS schema_meta (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    version     INTEGER NOT NULL,
    applied_at  TEXT    NOT NULL
) STRICT;

INSERT INTO schema_meta (id, version, applied_at)
VALUES (1, 1, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
ON CONFLICT (id) DO NOTHING;
`

	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("applying schema: %w", err)
	}

	return nil
}

// Expected is the schema the running binary needs: for each table, the columns
// it will read. Each store contributes its own entry.
type Expected map[string][]string

// Infrastructure is what this package itself requires.
var Infrastructure = Expected{
	"schema_meta": {"id", "version", "applied_at"},
}

// CheckSchema reports whether every expected table exists and carries every
// expected column.
//
// This is what the health endpoint runs, and the reason it is not a ping is
// worth stating, because it cost the sibling project a silent outage. A
// rollback restores the binary and not the database. The old binary's
// CREATE TABLE IF NOT EXISTS is a no-op, so it starts cleanly; its health
// check was db.PingContext, so the probe answered 200; and every real request
// then failed on a column that no longer existed. A health check that cannot
// fail is not a health check.
func CheckSchema(ctx context.Context, db *sql.DB, want Expected) error {
	for _, table := range slices.Sorted(maps.Keys(want)) {
		if !safeIdentifier(table) {
			return fmt.Errorf("table name %q is not a plain identifier", table)
		}

		// A PRAGMA takes no bind parameters, so the name is interpolated. It
		// comes from a compiled-in constant rather than from a request, and
		// safeIdentifier above is the belt to that braces.
		rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info('`+table+`')`)
		if err != nil {
			return fmt.Errorf("reading the columns of %s: %w", table, err)
		}

		var have []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return fmt.Errorf("reading the columns of %s: %w", table, err)
			}
			have = append(have, name)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("reading the columns of %s: %w", table, err)
		}
		rows.Close()

		if len(have) == 0 {
			return fmt.Errorf("table %s is missing", table)
		}

		for _, col := range want[table] {
			if !slices.Contains(have, col) {
				return fmt.Errorf("table %s has no column %s", table, col)
			}
		}
	}

	return nil
}

func safeIdentifier(s string) bool {
	if s == "" {
		return false
	}

	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9':
		default:
			return false
		}
	}

	return true
}

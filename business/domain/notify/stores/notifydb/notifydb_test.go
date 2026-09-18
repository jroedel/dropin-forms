package notifydb_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/notify/stores/notifydb"
	"github.com/jroedel/dropin-forms/business/domain/user/stores/userdb"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

var now = time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

// A row means silence, so the assertions here are mostly about absence: what
// the store says about somebody who has never touched the page.
func open(t *testing.T) (*sql.DB, *notifydb.Store) {
	t.Helper()

	db, err := sqldb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	if err := sqldb.Init(t.Context(), db); err != nil {
		t.Fatalf("sqldb.Init: %v", err)
	}

	// The account tables too: a preference row references one, which is what
	// makes deleting an account take its preferences with it.
	if err := userdb.Init(t.Context(), db); err != nil {
		t.Fatalf("userdb.Init: %v", err)
	}
	if err := notifydb.Init(t.Context(), db); err != nil {
		t.Fatalf("notifydb.Init: %v", err)
	}

	return db, notifydb.NewStore(db)
}

func account(t *testing.T, db *sql.DB, address string) types.ID {
	t.Helper()

	email, err := types.ParseEmail(address)
	if err != nil {
		t.Fatalf("ParseEmail(%q): %v", address, err)
	}

	u := userbus.User{
		ID:        types.NewID(),
		Email:     email,
		Name:      "A Reader",
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := userdb.NewStore(db).CreateUser(t.Context(), u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	return u.ID
}

func mustSlug(t *testing.T, s string) types.Slug {
	t.Helper()

	slug, err := types.ParseSlug(s)
	if err != nil {
		t.Fatalf("ParseSlug(%q): %v", s, err)
	}

	return slug
}

func TestSilenceIsARowAndTheDefaultIsNone(t *testing.T) {
	db, store := open(t)

	reader := account(t, db, "reader@example.test")
	feast := mustSlug(t, "feast-lunch-2026")

	muted, err := store.Muted(t.Context(), reader, feast)
	if err != nil {
		t.Fatalf("Muted: %v", err)
	}
	if muted {
		t.Fatal("an account nobody has recorded anything about starts silent, which means a new grant would never hear about a submission")
	}

	if err := store.Mute(t.Context(), reader, feast, now); err != nil {
		t.Fatalf("Mute: %v", err)
	}

	// Twice, because the link can be pressed twice and a second row would be a
	// constraint violation rather than a no-op.
	if err := store.Mute(t.Context(), reader, feast, now.Add(time.Hour)); err != nil {
		t.Fatalf("Mute again: %v", err)
	}

	muted, err = store.Muted(t.Context(), reader, feast)
	if err != nil {
		t.Fatalf("Muted: %v", err)
	}
	if !muted {
		t.Error("the preference did not stick")
	}

	if err := store.Unmute(t.Context(), reader, feast); err != nil {
		t.Fatalf("Unmute: %v", err)
	}

	// And unmuting something that was never muted is not a mistake worth
	// reporting: asking to be told about what you are already told about.
	if err := store.Unmute(t.Context(), reader, mustSlug(t, "never-touched")); err != nil {
		t.Fatalf("Unmute of an untouched pair: %v", err)
	}

	muted, err = store.Muted(t.Context(), reader, feast)
	if err != nil {
		t.Fatalf("Muted: %v", err)
	}
	if muted {
		t.Error("it is still muted after being unmuted")
	}
}

func TestThePreferenceIsPerPersonAndPerForm(t *testing.T) {
	db, store := open(t)

	one := account(t, db, "one@example.test")
	two := account(t, db, "two@example.test")

	feast := mustSlug(t, "feast-lunch-2026")
	raffle := mustSlug(t, "raffle-2026")

	if err := store.Mute(t.Context(), one, feast, now); err != nil {
		t.Fatalf("Mute: %v", err)
	}
	if err := store.Mute(t.Context(), one, raffle, now); err != nil {
		t.Fatalf("Mute: %v", err)
	}
	if err := store.Mute(t.Context(), two, feast, now); err != nil {
		t.Fatalf("Mute: %v", err)
	}

	// Who is quiet on one form, which is the question asked while a submission
	// is being announced.
	quiet, err := store.MutedForForm(t.Context(), feast)
	if err != nil {
		t.Fatalf("MutedForForm: %v", err)
	}
	if len(quiet) != 2 {
		t.Errorf("MutedForForm = %v, want both accounts", quiet)
	}

	quiet, err = store.MutedForForm(t.Context(), raffle)
	if err != nil {
		t.Fatalf("MutedForForm: %v", err)
	}
	if len(quiet) != 1 || quiet[0] != one {
		t.Errorf("MutedForForm = %v, want only the one account that asked", quiet)
	}

	// And which forms one person is quiet on, which is the question the list
	// page asks.
	forms, err := store.MutedForUser(t.Context(), one)
	if err != nil {
		t.Fatalf("MutedForUser: %v", err)
	}
	if len(forms) != 2 {
		t.Errorf("MutedForUser = %v, want both forms", forms)
	}

	forms, err = store.MutedForUser(t.Context(), two)
	if err != nil {
		t.Fatalf("MutedForUser: %v", err)
	}
	if len(forms) != 1 || forms[0] != feast {
		t.Errorf("MutedForUser = %v, want only the feast", forms)
	}
}

// A preference belongs to the account that set it and means nothing without
// one, which is why this table cascades where the grant table deliberately
// does not.
func TestDeletingAnAccountTakesItsPreferencesWithIt(t *testing.T) {
	db, store := open(t)

	reader := account(t, db, "reader@example.test")
	feast := mustSlug(t, "feast-lunch-2026")

	if err := store.Mute(t.Context(), reader, feast, now); err != nil {
		t.Fatalf("Mute: %v", err)
	}

	if _, err := db.ExecContext(t.Context(), `DELETE FROM users WHERE id = ?`, reader.String()); err != nil {
		t.Fatalf("deleting the account: %v", err)
	}

	quiet, err := store.MutedForForm(t.Context(), feast)
	if err != nil {
		t.Fatalf("MutedForForm: %v", err)
	}
	if len(quiet) != 0 {
		t.Errorf("%v is still recorded against a deleted account", quiet)
	}
}

// The schema this binary expects is the schema Init writes. Asserted here
// because CheckSchema runs at startup and on every health request, and a
// column named in one place and not the other is a service that will not
// start.
func TestTheSchemaMatchesWhatIsExpected(t *testing.T) {
	db, _ := open(t)

	if err := sqldb.CheckSchema(t.Context(), db, notifydb.Expected); err != nil {
		t.Fatalf("CheckSchema: %v", err)
	}
}

package accessdb_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/access/stores/accessdb"
	"github.com/jroedel/dropin-forms/business/domain/user/stores/userdb"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

var now = time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

// open makes a real database in a temporary directory. A file rather than
// :memory:, so the pragmas sqldb.Open sets -- WAL, foreign keys, busy timeout
// -- are the ones production runs with. Foreign keys in particular, since the
// cascade from users is one of the things asserted here.
func open(t *testing.T) (*sql.DB, *accessdb.Store) {
	t.Helper()

	db, err := sqldb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	if err := sqldb.Init(t.Context(), db); err != nil {
		t.Fatalf("sqldb.Init: %v", err)
	}
	if err := userdb.Init(t.Context(), db); err != nil {
		t.Fatalf("userdb.Init: %v", err)
	}
	if err := accessdb.Init(t.Context(), db); err != nil {
		t.Fatalf("accessdb.Init: %v", err)
	}

	return db, accessdb.NewStore(db)
}

// account inserts a real user row, because grants reference one.
func account(t *testing.T, db *sql.DB, address string) types.ID {
	t.Helper()

	email, err := types.ParseEmail(address)
	if err != nil {
		t.Fatalf("ParseEmail(%q): %v", address, err)
	}

	u := userbus.User{
		ID:        types.NewID(),
		Email:     email,
		Name:      "Somebody",
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

func TestSchemaMatchesWhatTheStoreReads(t *testing.T) {
	db, _ := open(t)

	if err := sqldb.CheckSchema(t.Context(), db, accessdb.Expected); err != nil {
		t.Errorf("CheckSchema: %v", err)
	}
}

func TestAGrantRoundTrips(t *testing.T) {
	db, store := open(t)
	user := account(t, db, "reader@example.org")
	granter := account(t, db, "admin@example.org")
	lunch := mustSlug(t, "feast-lunch-2026")

	want := accessbus.Grant{
		UserID:    user,
		Form:      lunch,
		Role:      accessbus.RoleResults,
		GrantedBy: granter,
		GrantedAt: now,
	}

	if err := store.Upsert(t.Context(), want); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := store.ByUserAndForm(t.Context(), user, lunch)
	if err != nil {
		t.Fatalf("ByUserAndForm: %v", err)
	}

	if got.UserID != want.UserID || got.Form != want.Form || got.Role != want.Role || got.GrantedBy != want.GrantedBy {
		t.Errorf("read back %+v, want %+v", got, want)
	}
	if !got.GrantedAt.Equal(now) {
		t.Errorf("GrantedAt = %v, want %v", got.GrantedAt, now)
	}
}

func TestASiteWideGrantRoundTripsAsTheZeroSlug(t *testing.T) {
	db, store := open(t)
	user := account(t, db, "admin@example.org")

	// The zero granter too, which is what the bootstrap writes: no person did
	// it, the configuration did.
	g := accessbus.Grant{
		UserID:    user,
		Role:      accessbus.RoleAdmin,
		GrantedAt: now,
	}

	if err := store.Upsert(t.Context(), g); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := store.ByUserAndForm(t.Context(), user, types.Slug{})
	if err != nil {
		t.Fatalf("ByUserAndForm: %v", err)
	}

	if !got.SiteWide() {
		t.Errorf("Form = %q, want the zero slug", got.Form)
	}
	if !got.GrantedBy.Zero() {
		t.Errorf("GrantedBy = %q, want the zero id", got.GrantedBy)
	}
	if got.Role != accessbus.RoleAdmin {
		t.Errorf("Role = %q", got.Role)
	}
}

func TestAMissingGrantIsErrNotFound(t *testing.T) {
	db, store := open(t)
	user := account(t, db, "nobody@example.org")

	_, err := store.ByUserAndForm(t.Context(), user, mustSlug(t, "feast-lunch-2026"))
	if !errors.Is(err, accessbus.ErrNotFound) {
		t.Errorf("ByUserAndForm = %v, want ErrNotFound", err)
	}
}

func TestUpsertReplacesTheRoleRatherThanAddingARow(t *testing.T) {
	db, store := open(t)
	user := account(t, db, "reader@example.org")
	lunch := mustSlug(t, "feast-lunch-2026")

	for _, role := range []accessbus.Role{accessbus.RoleResults, accessbus.RoleAdmin, accessbus.RoleResults} {
		err := store.Upsert(t.Context(), accessbus.Grant{
			UserID: user, Form: lunch, Role: role, GrantedAt: now,
		})
		if err != nil {
			t.Fatalf("Upsert(%s): %v", role, err)
		}
	}

	grants, err := store.ByUser(t.Context(), user)
	if err != nil {
		t.Fatalf("ByUser: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("%d rows for one (account, form) pair, want 1", len(grants))
	}
	if grants[0].Role != accessbus.RoleResults {
		t.Errorf("Role = %q, want the last one written", grants[0].Role)
	}
}

func TestDeletingNothingIsNotAnError(t *testing.T) {
	db, store := open(t)
	user := account(t, db, "reader@example.org")

	if err := store.Delete(t.Context(), user, mustSlug(t, "never-granted")); err != nil {
		t.Errorf("Delete of a grant that does not exist: %v", err)
	}
}

func TestListingsAreScopedAndOrdered(t *testing.T) {
	db, store := open(t)

	admin := account(t, db, "admin@example.org")
	reader := account(t, db, "reader@example.org")

	lunch := mustSlug(t, "feast-lunch-2026")
	retreat := mustSlug(t, "fall-retreat")

	rows := []accessbus.Grant{
		{UserID: admin, Role: accessbus.RoleAdmin, GrantedAt: now},
		{UserID: admin, Form: lunch, Role: accessbus.RoleAdmin, GrantedAt: now},
		{UserID: reader, Form: lunch, Role: accessbus.RoleResults, GrantedAt: now},
		{UserID: reader, Form: retreat, Role: accessbus.RoleResults, GrantedAt: now},
	}

	for _, g := range rows {
		if err := store.Upsert(t.Context(), g); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	byUser, err := store.ByUser(t.Context(), reader)
	if err != nil {
		t.Fatalf("ByUser: %v", err)
	}
	if len(byUser) != 2 {
		t.Errorf("%d grants for the reader, want 2", len(byUser))
	}

	byForm, err := store.ByForm(t.Context(), lunch)
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(byForm) != 2 {
		t.Errorf("%d grants on the lunch form, want 2", len(byForm))
	}

	// The zero slug lists the site-wide grants, which is how accessbus counts
	// administrators.
	site, err := store.ByForm(t.Context(), types.Slug{})
	if err != nil {
		t.Fatalf("ByForm(zero): %v", err)
	}
	if len(site) != 1 {
		t.Errorf("%d site-wide grants, want 1", len(site))
	}

	all, err := store.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != len(rows) {
		t.Fatalf("%d grants in all, want %d", len(all), len(rows))
	}

	// Ordered by form, so the site-wide grants -- the empty string -- come
	// first, which is the order a page listing them wants.
	if !all[0].SiteWide() {
		t.Errorf("All did not begin with the site-wide grant: %+v", all[0])
	}
}

func TestDeletingAnAccountTakesItsGrantsWithIt(t *testing.T) {
	db, store := open(t)
	user := account(t, db, "reader@example.org")
	lunch := mustSlug(t, "feast-lunch-2026")

	err := store.Upsert(t.Context(), accessbus.Grant{
		UserID: user, Form: lunch, Role: accessbus.RoleResults, GrantedAt: now,
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if _, err := db.ExecContext(t.Context(), `DELETE FROM users WHERE id = ?`, user.String()); err != nil {
		t.Fatalf("deleting the account: %v", err)
	}

	// ON DELETE CASCADE, and it only works because sqldb.Open turns foreign
	// keys on -- SQLite ignores the clause otherwise, which would leave a
	// grant pointing at nobody.
	if _, err := store.ByUserAndForm(t.Context(), user, lunch); !errors.Is(err, accessbus.ErrNotFound) {
		t.Errorf("the grant outlived the account: %v", err)
	}
}

func TestAGrantForNoAccountIsRefusedByTheDatabase(t *testing.T) {
	_, store := open(t)

	err := store.Upsert(t.Context(), accessbus.Grant{
		UserID: types.NewID(), Form: mustSlug(t, "feast-lunch-2026"),
		Role: accessbus.RoleResults, GrantedAt: now,
	})
	if err == nil {
		t.Error("a grant referencing no account was accepted")
	}
}

// A role this binary does not understand must not read back as a Grant. The
// case is a row written by a newer release and then rolled back to this one,
// and a permission decision resting on a value nobody parsed is not one to
// leave standing.
func TestARoleThisBinaryCannotReadIsAnError(t *testing.T) {
	db, store := open(t)
	user := account(t, db, "reader@example.org")

	_, err := db.ExecContext(t.Context(),
		`INSERT INTO grants (user_id, form_slug, role, granted_by, granted_at) VALUES (?, ?, ?, ?, ?)`,
		user.String(), "feast-lunch-2026", "owner", "", now.UnixMilli())
	if err != nil {
		t.Fatalf("inserting the row by hand: %v", err)
	}

	_, err = store.ByUserAndForm(t.Context(), user, mustSlug(t, "feast-lunch-2026"))
	if err == nil {
		t.Fatal("a grant with an unknown role was read as valid")
	}
	if errors.Is(err, accessbus.ErrNotFound) {
		t.Error("an unreadable row reported itself as no row at all")
	}
	if !strings.Contains(err.Error(), "role") {
		t.Errorf("the error does not say what was wrong: %v", err)
	}
}

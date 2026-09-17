package userdb_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/user/stores/userdb"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

var now = time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

// open makes a real database in a temporary directory. A file rather than
// :memory:, so the pragmas sqldb.Open sets -- WAL, foreign keys, busy timeout
// -- are the ones production runs with.
func open(t *testing.T) (*sql.DB, *userdb.Store) {
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

	return db, userdb.NewStore(db)
}

func mustEmail(t *testing.T, s string) types.Email {
	t.Helper()

	e, err := types.ParseEmail(s)
	if err != nil {
		t.Fatalf("ParseEmail(%q): %v", s, err)
	}

	return e
}

func mustUser(t *testing.T, s *userdb.Store, addr string) userbus.User {
	t.Helper()

	u := userbus.User{
		ID:        types.NewID(),
		Email:     mustEmail(t, addr),
		Name:      "Somebody",
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.CreateUser(t.Context(), u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	return u
}

// The schema this binary creates must be the schema it claims to need, or
// CheckSchema is decoration. A column named in Expected but never created
// fails here rather than at startup on the server.
func TestInitSatisfiesItsOwnExpectations(t *testing.T) {
	db, _ := open(t)

	if err := sqldb.CheckSchema(t.Context(), db, userdb.Expected); err != nil {
		t.Fatalf("the tables Init creates do not match what Expected names: %v", err)
	}

	// Idempotent, because it runs at every startup.
	for range 3 {
		if err := userdb.Init(t.Context(), db); err != nil {
			t.Fatalf("Init is not idempotent: %v", err)
		}
	}

	if err := sqldb.CheckSchema(t.Context(), db, userdb.Expected); err != nil {
		t.Fatalf("after re-running Init: %v", err)
	}
}

func TestUserRoundTrip(t *testing.T) {
	_, s := open(t)

	want := mustUser(t, s, "frjeff@schoenstatt.us")

	got, err := s.UserByID(t.Context(), want.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}

	switch {
	case got.ID != want.ID:
		t.Errorf("ID = %q, want %q", got.ID, want.ID)
	case got.Email != want.Email:
		t.Errorf("Email = %q, want %q", got.Email, want.Email)
	case got.Name != want.Name:
		t.Errorf("Name = %q, want %q", got.Name, want.Name)
	case !got.Enabled:
		t.Error("Enabled came back false")
	case !got.CreatedAt.Equal(want.CreatedAt):
		t.Errorf("CreatedAt = %s, want %s", got.CreatedAt, want.CreatedAt)
	}

	byEmail, err := s.UserByEmail(t.Context(), want.Email)
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	if byEmail.ID != want.ID {
		t.Errorf("UserByEmail found %q, want %q", byEmail.ID, want.ID)
	}

	// Not found is a typed error, because userbus branches on it.
	if _, err := s.UserByID(t.Context(), types.NewID()); !errors.Is(err, userbus.ErrNotFound) {
		t.Errorf("UserByID for an absent account returned %v, want ErrNotFound", err)
	}
	if _, err := s.UserByEmail(t.Context(), mustEmail(t, "nobody@example.org")); !errors.Is(err, userbus.ErrNotFound) {
		t.Errorf("UserByEmail for an absent account returned %v, want ErrNotFound", err)
	}

	// The unique address, reported as the error userbus already uses for the
	// same collision found by its own check.
	dup := want
	dup.ID = types.NewID()

	if err := s.CreateUser(t.Context(), dup); !errors.Is(err, userbus.ErrEmailTaken) {
		t.Errorf("a duplicate address returned %v, want ErrEmailTaken", err)
	}

	// Update, including disabling.
	want.Name = "Fr Jeff"
	want.Enabled = false
	want.UpdatedAt = now.Add(time.Hour)

	if err := s.UpdateUser(t.Context(), want); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	got, err = s.UserByID(t.Context(), want.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if got.Name != "Fr Jeff" || got.Enabled {
		t.Errorf("the update did not take: %+v", got)
	}

	absent := want
	absent.ID = types.NewID()

	if err := s.UpdateUser(t.Context(), absent); !errors.Is(err, userbus.ErrNotFound) {
		t.Errorf("updating an absent account returned %v, want ErrNotFound", err)
	}
}

func TestUsersAreOrderedOldestFirst(t *testing.T) {
	_, s := open(t)

	for i, addr := range []string{"c@example.org", "a@example.org", "b@example.org"} {
		u := userbus.User{
			ID:        types.NewID(),
			Email:     mustEmail(t, addr),
			Enabled:   true,
			CreatedAt: now.Add(time.Duration(i) * time.Minute),
			UpdatedAt: now,
		}

		if err := s.CreateUser(t.Context(), u); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
	}

	users, err := s.Users(t.Context())
	if err != nil {
		t.Fatalf("Users: %v", err)
	}

	var got []string
	for _, u := range users {
		got = append(got, u.Email.String())
	}

	if want := "c@example.org,a@example.org,b@example.org"; strings.Join(got, ",") != want {
		t.Errorf("Users returned %q, want %q in creation order", strings.Join(got, ","), want)
	}
}

func TestTokenRoundTripAndSingleUse(t *testing.T) {
	_, s := open(t)
	u := mustUser(t, s, "frjeff@schoenstatt.us")

	want := userbus.Token{
		ID:        types.NewID(),
		UserID:    u.ID,
		Hash:      []byte("a thirty-two byte hash goes here"),
		CreatedAt: now,
		ExpiresAt: now.Add(15 * time.Minute),
	}

	if err := s.CreateToken(t.Context(), want); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	got, err := s.TokenByID(t.Context(), want.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}

	switch {
	case got.UserID != u.ID:
		t.Errorf("UserID = %q, want %q", got.UserID, u.ID)
	case string(got.Hash) != string(want.Hash):
		t.Errorf("Hash = %q, want %q", got.Hash, want.Hash)
	case !got.ExpiresAt.Equal(want.ExpiresAt):
		t.Errorf("ExpiresAt = %s, want %s", got.ExpiresAt, want.ExpiresAt)

	// The zero time must come back as the zero time. It is stored as NULL,
	// and a sentinel like the Unix epoch would compare as "used in 1970".
	case !got.UsedAt.IsZero():
		t.Errorf("UsedAt = %s, want the zero time for an unused link", got.UsedAt)
	}

	claimed, err := s.UseToken(t.Context(), want.ID, now)
	switch {
	case err != nil:
		t.Fatalf("UseToken: %v", err)
	case !claimed:
		t.Fatal("UseToken did not claim an unused link")
	}

	again, err := s.UseToken(t.Context(), want.ID, now)
	switch {
	case err != nil:
		t.Fatalf("UseToken: %v", err)
	case again:
		t.Error("UseToken claimed a link that had already been spent")
	}

	got, err = s.TokenByID(t.Context(), want.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if !got.UsedAt.Equal(now) {
		t.Errorf("UsedAt = %s, want %s", got.UsedAt, now)
	}

	if _, err := s.TokenByID(t.Context(), types.NewID()); !errors.Is(err, userbus.ErrNotFound) {
		t.Errorf("TokenByID for an absent link returned %v, want ErrNotFound", err)
	}

	// Claiming something that is not there is false rather than an error:
	// indistinguishable from losing the race, which is what the caller needs.
	if claimed, err := s.UseToken(t.Context(), types.NewID(), now); err != nil || claimed {
		t.Errorf("UseToken on an absent link = %v, %v; want false, nil", claimed, err)
	}
}

// The property userbus.Storer documents and that a SELECT-then-UPDATE would
// fail. Real SQLite this time rather than a mutex in a test double.
func TestUseTokenIsClaimedOnceUnderConcurrency(t *testing.T) {
	_, s := open(t)
	u := mustUser(t, s, "frjeff@schoenstatt.us")

	id := types.NewID()

	if err := s.CreateToken(t.Context(), userbus.Token{
		ID: id, UserID: u.ID, Hash: []byte("hash"),
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	const racers = 20

	var (
		start  sync.WaitGroup
		wg     sync.WaitGroup
		mu     sync.Mutex
		claims int
	)

	start.Add(1)

	for range racers {
		wg.Go(func() {
			start.Wait()

			claimed, err := s.UseToken(t.Context(), id, now)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				t.Errorf("UseToken: %v", err)

				return
			}
			if claimed {
				claims++
			}
		})
	}

	start.Done()
	wg.Wait()

	if claims != 1 {
		t.Errorf("%d of %d racing claims succeeded, want exactly 1", claims, racers)
	}
}

func TestBackupCodes(t *testing.T) {
	_, s := open(t)
	u := mustUser(t, s, "frjeff@schoenstatt.us")
	other := mustUser(t, s, "someone@schoenstatt.us")

	make10 := func(owner types.ID) []userbus.BackupCode {
		out := make([]userbus.BackupCode, 0, 10)
		for i := range 10 {
			out = append(out, userbus.BackupCode{
				ID:        types.NewID(),
				UserID:    owner,
				Hash:      []byte{byte(i)},
				CreatedAt: now.Add(time.Duration(i) * time.Second),
			})
		}

		return out
	}

	first := make10(u.ID)
	if err := s.ReplaceBackupCodes(t.Context(), u.ID, first); err != nil {
		t.Fatalf("ReplaceBackupCodes: %v", err)
	}
	if err := s.ReplaceBackupCodes(t.Context(), other.ID, make10(other.ID)); err != nil {
		t.Fatalf("ReplaceBackupCodes: %v", err)
	}

	got, err := s.BackupCodes(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("BackupCodes: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("got %d codes, want 10", len(got))
	}

	for _, c := range got {
		if c.UserID != u.ID {
			t.Errorf("a code belongs to %q, want %q", c.UserID, u.ID)
		}
		if !c.UsedAt.IsZero() {
			t.Errorf("a fresh code is marked used at %s", c.UsedAt)
		}
	}

	// Spend one, once.
	if claimed, err := s.UseBackupCode(t.Context(), got[0].ID, now); err != nil || !claimed {
		t.Fatalf("UseBackupCode = %v, %v", claimed, err)
	}
	if claimed, err := s.UseBackupCode(t.Context(), got[0].ID, now); err != nil || claimed {
		t.Errorf("a code was spent twice: %v, %v", claimed, err)
	}

	// Replacing removes the whole previous set, and touches nobody else's.
	if err := s.ReplaceBackupCodes(t.Context(), u.ID, make10(u.ID)); err != nil {
		t.Fatalf("ReplaceBackupCodes: %v", err)
	}

	after, err := s.BackupCodes(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("BackupCodes: %v", err)
	}
	if len(after) != 10 {
		t.Errorf("after replacing there are %d codes, want 10", len(after))
	}

	for _, old := range first {
		for _, c := range after {
			if c.ID == old.ID {
				t.Errorf("a code from the old set survived: %q", c.ID)
			}
		}
	}

	theirs, err := s.BackupCodes(t.Context(), other.ID)
	if err != nil {
		t.Fatalf("BackupCodes: %v", err)
	}
	if len(theirs) != 10 {
		t.Errorf("replacing one account's codes changed another's: %d remain", len(theirs))
	}
}

func TestSessions(t *testing.T) {
	_, s := open(t)
	u := mustUser(t, s, "frjeff@schoenstatt.us")

	want := userbus.Session{
		ID:         types.NewID(),
		UserID:     u.ID,
		Hash:       []byte("session hash"),
		CreatedAt:  now,
		ExpiresAt:  now.Add(14 * 24 * time.Hour),
		LastSeenAt: now,
	}

	if err := s.CreateSession(t.Context(), want); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := s.SessionByID(t.Context(), want.ID)
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if got.UserID != u.ID || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("the session came back wrong: %+v", got)
	}

	later := now.Add(time.Hour)
	if err := s.TouchSession(t.Context(), want.ID, later); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}

	got, err = s.SessionByID(t.Context(), want.ID)
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if !got.LastSeenAt.Equal(later) {
		t.Errorf("LastSeenAt = %s, want %s", got.LastSeenAt, later)
	}

	if err := s.TouchSession(t.Context(), types.NewID(), later); !errors.Is(err, userbus.ErrNotFound) {
		t.Errorf("touching an absent session returned %v, want ErrNotFound", err)
	}

	// Deleting one that is not there is not an error: the caller wanted it
	// gone, and it is.
	if err := s.DeleteSession(t.Context(), types.NewID()); err != nil {
		t.Errorf("deleting an absent session returned %v", err)
	}

	if err := s.DeleteSession(t.Context(), want.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.SessionByID(t.Context(), want.ID); !errors.Is(err, userbus.ErrNotFound) {
		t.Errorf("a deleted session was still found: %v", err)
	}
}

func TestDeleteUserSessions(t *testing.T) {
	_, s := open(t)
	u := mustUser(t, s, "frjeff@schoenstatt.us")
	other := mustUser(t, s, "someone@schoenstatt.us")

	add := func(owner types.ID) types.ID {
		id := types.NewID()

		if err := s.CreateSession(t.Context(), userbus.Session{
			ID: id, UserID: owner, Hash: []byte("h"),
			CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}

		return id
	}

	mine, alsoMine, theirs := add(u.ID), add(u.ID), add(other.ID)

	if err := s.DeleteUserSessions(t.Context(), u.ID); err != nil {
		t.Fatalf("DeleteUserSessions: %v", err)
	}

	for _, id := range []types.ID{mine, alsoMine} {
		if _, err := s.SessionByID(t.Context(), id); !errors.Is(err, userbus.ErrNotFound) {
			t.Errorf("session %q survived: %v", id, err)
		}
	}

	if _, err := s.SessionByID(t.Context(), theirs); err != nil {
		t.Errorf("another account's session was deleted: %v", err)
	}
}

func TestClaimBootstrapHappensOnce(t *testing.T) {
	_, s := open(t)

	claimed, err := s.ClaimBootstrap(t.Context(), now)
	switch {
	case err != nil:
		t.Fatalf("ClaimBootstrap: %v", err)
	case !claimed:
		t.Fatal("the first claim did not succeed")
	}

	for range 5 {
		claimed, err := s.ClaimBootstrap(t.Context(), now.Add(time.Hour))
		switch {
		case err != nil:
			t.Fatalf("ClaimBootstrap: %v", err)
		case claimed:
			t.Fatal("the bootstrap was claimed more than once")
		}
	}
}

// Foreign keys are on, so removing an account takes its credentials with it.
// Without ON DELETE CASCADE these rows would outlive the account and a session
// would name a user that no longer exists.
func TestDeletingAnAccountRemovesItsCredentials(t *testing.T) {
	db, s := open(t)
	u := mustUser(t, s, "frjeff@schoenstatt.us")

	tokenID, sessionID := types.NewID(), types.NewID()

	if err := s.CreateToken(t.Context(), userbus.Token{
		ID: tokenID, UserID: u.ID, Hash: []byte("h"),
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if err := s.CreateSession(t.Context(), userbus.Session{
		ID: sessionID, UserID: u.ID, Hash: []byte("h"),
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.ReplaceBackupCodes(t.Context(), u.ID, []userbus.BackupCode{
		{ID: types.NewID(), UserID: u.ID, Hash: []byte("h"), CreatedAt: now},
	}); err != nil {
		t.Fatalf("ReplaceBackupCodes: %v", err)
	}

	if _, err := db.ExecContext(t.Context(), `DELETE FROM users WHERE id = ?`, u.ID.String()); err != nil {
		t.Fatalf("deleting the account: %v", err)
	}

	if _, err := s.TokenByID(t.Context(), tokenID); !errors.Is(err, userbus.ErrNotFound) {
		t.Errorf("the sign-in link outlived the account: %v", err)
	}
	if _, err := s.SessionByID(t.Context(), sessionID); !errors.Is(err, userbus.ErrNotFound) {
		t.Errorf("the session outlived the account: %v", err)
	}

	codes, err := s.BackupCodes(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("BackupCodes: %v", err)
	}
	if len(codes) != 0 {
		t.Errorf("%d backup codes outlived the account", len(codes))
	}

	// And a credential cannot be created for an account that never existed.
	err = s.CreateToken(t.Context(), userbus.Token{
		ID: types.NewID(), UserID: types.NewID(), Hash: []byte("h"),
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err == nil {
		t.Error("a sign-in link was created for an account that does not exist")
	}
}

// A direct guard on the trap the package comment describes. These two
// instants differ only in their fractional second, and as RFC 3339 Nano text
// the *later* one sorts first -- '.' is 0x2E and 'Z' is 0x5A -- so a text
// column would prune the wrong row. As integers the comparison is arithmetic
// and cannot be wrong.
func TestExpiryComparesCorrectlyAcrossFractionalSeconds(t *testing.T) {
	_, s := open(t)
	u := mustUser(t, s, "frjeff@schoenstatt.us")

	base := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

	earlier := base                           // ...T15:00:00Z
	later := base.Add(500 * time.Millisecond) // ...T15:00:00.5Z

	if earlier.Format(time.RFC3339Nano) >= later.Format(time.RFC3339Nano) {
		t.Logf("as text, %q sorts at or after %q -- which is the trap",
			earlier.Format(time.RFC3339Nano), later.Format(time.RFC3339Nano))
	}

	early, late := types.NewID(), types.NewID()

	for id, expiry := range map[types.ID]time.Time{early: earlier, late: later} {
		if err := s.CreateToken(t.Context(), userbus.Token{
			ID: id, UserID: u.ID, Hash: []byte("h"),
			CreatedAt: base.Add(-time.Hour), ExpiresAt: expiry,
		}); err != nil {
			t.Fatalf("CreateToken: %v", err)
		}
	}

	// A cutoff between the two: the earlier link is expired, the later is not.
	cutoff := base.Add(250 * time.Millisecond)

	if err := s.PruneExpired(t.Context(), cutoff); err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}

	if _, err := s.TokenByID(t.Context(), early); !errors.Is(err, userbus.ErrNotFound) {
		t.Errorf("the expired link survived pruning: %v", err)
	}
	if _, err := s.TokenByID(t.Context(), late); err != nil {
		t.Errorf("a link that had not expired was pruned: %v", err)
	}
}

func TestPruneExpired(t *testing.T) {
	_, s := open(t)
	u := mustUser(t, s, "frjeff@schoenstatt.us")

	live, dead := types.NewID(), types.NewID()

	if err := s.CreateToken(t.Context(), userbus.Token{
		ID: live, UserID: u.ID, Hash: []byte("h"),
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if err := s.CreateToken(t.Context(), userbus.Token{
		ID: dead, UserID: u.ID, Hash: []byte("h"),
		CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	liveSession, deadSession := types.NewID(), types.NewID()

	for id, expiry := range map[types.ID]time.Time{
		liveSession: now.Add(time.Hour),
		deadSession: now.Add(-time.Hour),
	} {
		if err := s.CreateSession(t.Context(), userbus.Session{
			ID: id, UserID: u.ID, Hash: []byte("h"),
			CreatedAt: now.Add(-3 * time.Hour), ExpiresAt: expiry, LastSeenAt: now,
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}

	if err := s.PruneExpired(t.Context(), now); err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}

	if _, err := s.TokenByID(t.Context(), live); err != nil {
		t.Errorf("a live link was pruned: %v", err)
	}
	if _, err := s.TokenByID(t.Context(), dead); !errors.Is(err, userbus.ErrNotFound) {
		t.Errorf("an expired link survived: %v", err)
	}
	if _, err := s.SessionByID(t.Context(), liveSession); err != nil {
		t.Errorf("a live session was pruned: %v", err)
	}
	if _, err := s.SessionByID(t.Context(), deadSession); !errors.Is(err, userbus.ErrNotFound) {
		t.Errorf("an expired session survived: %v", err)
	}
}

// A row a current parser refuses must not become an account that can sign in.
// Written by hand here, which is how it would arrive: an older binary, or
// somebody in a sqlite3 shell.
func TestAStoredRowThatCannotBeParsedIsRefused(t *testing.T) {
	db, s := open(t)

	id := types.NewID()

	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO users (id, email, name, enabled, created_at, updated_at) VALUES (?, ?, ?, 1, 0, 0)`,
		id.String(), "not an address", "Somebody"); err != nil {
		t.Fatalf("inserting by hand: %v", err)
	}

	_, err := s.UserByID(t.Context(), id)
	if err == nil {
		t.Fatal("an account with an unparseable address was returned")
	}
	if errors.Is(err, userbus.ErrNotFound) {
		t.Error("a corrupt row was reported as absent, which hides the problem")
	}
	if !strings.Contains(err.Error(), "bad address") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

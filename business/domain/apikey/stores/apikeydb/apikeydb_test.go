package apikeydb_test

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/apikey/apikeybus"
	"github.com/jroedel/dropin-forms/business/domain/apikey/stores/apikeydb"
	"github.com/jroedel/dropin-forms/business/domain/user/stores/userdb"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

var now = time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

// keys is the bus over the real stores, which is what is worth testing: the
// hash lookup, the account check and revocation all depend on both halves.
func keys(t *testing.T) (*apikeybus.Business, *userbus.Business, *userdb.Store) {
	t.Helper()

	db, err := sqldb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := userdb.Init(t.Context(), db); err != nil {
		t.Fatalf("userdb.Init: %v", err)
	}
	if err := apikeydb.Init(t.Context(), db); err != nil {
		t.Fatalf("Init: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	accounts := userdb.NewStore(db)
	users := userbus.NewBusiness(log, accounts)

	return apikeybus.NewBusiness(log, apikeydb.NewStore(db), users), users, accounts
}

func account(t *testing.T, users *userbus.Business, address string) userbus.User {
	t.Helper()

	email, err := types.ParseEmail(address)
	if err != nil {
		t.Fatalf("ParseEmail: %v", err)
	}

	u, err := users.Create(t.Context(), now, userbus.NewUser{Email: email, Name: address})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	return u
}

func TestAKeyIsItsAccountUntilItIsRevoked(t *testing.T) {
	b, users, _ := keys(t)
	me := account(t, users, "me@schoenstatt.test")

	k, secret, err := b.Create(t.Context(), now, me.ID, "  Claude on my laptop ")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(secret, "dfa_") || len(secret) < 40 {
		t.Fatalf("secret = %q", secret)
	}
	if k.Label != "Claude on my laptop" {
		t.Errorf("label = %q, want it trimmed", k.Label)
	}

	u, got, err := b.Authenticate(t.Context(), now.Add(time.Hour), secret)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if u.ID != me.ID || got.ID != k.ID {
		t.Errorf("authenticated as %v with %v, want %v with %v", u.ID, got.ID, me.ID, k.ID)
	}

	listed, err := b.ForUser(t.Context(), me.ID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ForUser = %v, %v", listed, err)
	}
	if !listed[0].LastUsedAt.Equal(now.Add(time.Hour)) {
		t.Errorf("last used = %v, want the time it was used", listed[0].LastUsedAt)
	}

	if err := b.Revoke(t.Context(), now, me.ID, k.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if _, _, err := b.Authenticate(t.Context(), now, secret); !errors.Is(err, apikeybus.ErrRefused) {
		t.Errorf("a revoked key = %v, want ErrRefused", err)
	}
}

func TestOnlyTheOwnerRevokesAKey(t *testing.T) {
	b, users, _ := keys(t)
	me := account(t, users, "me@schoenstatt.test")
	other := account(t, users, "other@schoenstatt.test")

	k, secret, err := b.Create(t.Context(), now, me.ID, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := b.Revoke(t.Context(), now, other.ID, k.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if _, _, err := b.Authenticate(t.Context(), now, secret); err != nil {
		t.Errorf("somebody else revoked my key: %v", err)
	}

	if listed, _ := b.ForUser(t.Context(), other.ID); len(listed) != 0 {
		t.Errorf("another account lists my key: %+v", listed)
	}
}

func TestADisabledAccountsKeyStopsWorking(t *testing.T) {
	b, users, accounts := keys(t)
	me := account(t, users, "me@schoenstatt.test")

	_, secret, err := b.Create(t.Context(), now, me.ID, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	me.Enabled = false
	if err := accounts.UpdateUser(t.Context(), me); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	if _, _, err := b.Authenticate(t.Context(), now, secret); !errors.Is(err, apikeybus.ErrRefused) {
		t.Errorf("a disabled account's key = %v, want ErrRefused", err)
	}
}

func TestAKeyThatIsNotOursIsRefused(t *testing.T) {
	b, users, _ := keys(t)
	me := account(t, users, "me@schoenstatt.test")

	if _, _, err := b.Create(t.Context(), now, me.ID, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, presented := range []string{"", "dfa_", "dfa_" + strings.Repeat("A", 43), "dfk_" + strings.Repeat("A", 43)} {
		if _, _, err := b.Authenticate(t.Context(), now, presented); !errors.Is(err, apikeybus.ErrRefused) {
			t.Errorf("Authenticate(%q) = %v, want ErrRefused", presented, err)
		}
	}
}

// RFC 7636, appendix B.
const (
	verifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"

	claude   = "https://claude.ai/oauth/test-client-metadata"
	callback = "https://claude.ai/api/mcp/auth_callback"
)

func TestACodeBecomesAKeyOnceAndOnlyWithItsVerifier(t *testing.T) {
	b, users, _ := keys(t)
	me := account(t, users, "me@schoenstatt.test")

	code, err := b.GrantAccess(t.Context(), now, me.ID, claude, "Claude (claude.ai)", callback, challenge)
	if err != nil {
		t.Fatalf("GrantAccess: %v", err)
	}

	// Each of these is refused, and none of them spends the code: a program
	// with a bug may try again, and a thief without the verifier gets nowhere.
	for name, tc := range map[string]struct{ client, redirect, verifier string }{
		"another program":    {"https://claude.ai/other", callback, verifier},
		"another redirect":   {claude, "https://claude.ai/elsewhere", verifier},
		"the wrong verifier": {claude, callback, strings.Repeat("a", 43)},
	} {
		if _, _, err := b.RedeemGrant(t.Context(), now, code, tc.client, tc.redirect, tc.verifier); !errors.Is(err, apikeybus.ErrRefused) {
			t.Errorf("%s: %v, want ErrRefused", name, err)
		}
	}

	k, secret, err := b.RedeemGrant(t.Context(), now.Add(time.Minute), code, claude, callback, verifier)
	if err != nil {
		t.Fatalf("RedeemGrant: %v", err)
	}

	if k.Label != "Claude (claude.ai)" || k.Client != claude || !k.ExpiresAt.Equal(now.Add(time.Minute).Add(apikeybus.OAuthKeyLife)) {
		t.Errorf("key = %+v", k)
	}

	if u, _, err := b.Authenticate(t.Context(), now.Add(time.Hour), secret); err != nil || u.ID != me.ID {
		t.Errorf("the key does not authenticate as its account: %v", err)
	}

	if _, _, err := b.RedeemGrant(t.Context(), now.Add(time.Minute), code, claude, callback, verifier); !errors.Is(err, apikeybus.ErrRefused) {
		t.Errorf("a code spent twice = %v, want ErrRefused", err)
	}

	// And it ends.
	if _, _, err := b.Authenticate(t.Context(), k.ExpiresAt, secret); !errors.Is(err, apikeybus.ErrRefused) {
		t.Errorf("a key past its end = %v, want ErrRefused", err)
	}
}

func TestACodeExpires(t *testing.T) {
	b, users, _ := keys(t)
	me := account(t, users, "me@schoenstatt.test")

	code, err := b.GrantAccess(t.Context(), now, me.ID, claude, "Claude", callback, challenge)
	if err != nil {
		t.Fatalf("GrantAccess: %v", err)
	}

	if _, _, err := b.RedeemGrant(t.Context(), now.Add(apikeybus.GrantLife), code, claude, callback, verifier); !errors.Is(err, apikeybus.ErrRefused) {
		t.Errorf("an expired code = %v, want ErrRefused", err)
	}
}

// Connecting Claude again replaces the key it had, rather than adding one;
// a key made on the page is left alone.
func TestConnectingAgainReplacesTheProgramsKey(t *testing.T) {
	b, users, _ := keys(t)
	me := account(t, users, "me@schoenstatt.test")

	_, mine, err := b.Create(t.Context(), now, me.ID, "laptop")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	connect := func() string {
		t.Helper()

		code, err := b.GrantAccess(t.Context(), now, me.ID, claude, "Claude", callback, challenge)
		if err != nil {
			t.Fatalf("GrantAccess: %v", err)
		}

		_, secret, err := b.RedeemGrant(t.Context(), now, code, claude, callback, verifier)
		if err != nil {
			t.Fatalf("RedeemGrant: %v", err)
		}

		return secret
	}

	first := connect()
	second := connect()

	if _, _, err := b.Authenticate(t.Context(), now, first); !errors.Is(err, apikeybus.ErrRefused) {
		t.Errorf("the first connection's key still works: %v", err)
	}

	for _, secret := range []string{second, mine} {
		if _, _, err := b.Authenticate(t.Context(), now, secret); err != nil {
			t.Errorf("a key that should still work: %v", err)
		}
	}
}

func TestAgreeingTooOftenIsRefused(t *testing.T) {
	b, users, _ := keys(t)
	me := account(t, users, "me@schoenstatt.test")

	for i := range 6 {
		_, err := b.GrantAccess(t.Context(), now, me.ID, claude, "Claude", callback, challenge)

		switch {
		case i < 5 && err != nil:
			t.Fatalf("grant %d: %v", i+1, err)
		case i == 5 && !errors.Is(err, apikeybus.ErrTooManyGrants):
			t.Fatalf("grant %d = %v, want ErrTooManyGrants", i+1, err)
		}
	}

	// Five minutes on, the old ones are dead and cleared.
	if _, err := b.GrantAccess(t.Context(), now.Add(apikeybus.GrantLife), me.ID, claude, "Claude", callback, challenge); err != nil {
		t.Errorf("after they expired: %v", err)
	}
}

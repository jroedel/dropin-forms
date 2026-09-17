package userbus_test

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// A fixed instant. A test that reads the real clock is a test that fails on
// the day a window closes.
var now = time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

func newBusiness(t *testing.T) (*userbus.Business, *memStore) {
	t.Helper()

	store := newMemStore()

	// Discarded rather than routed to t.Log. This package logs a line on every
	// failed attempt by design, and the tests provoke dozens of them.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	return userbus.NewBusiness(log, store), store
}

func mustEmail(t *testing.T, s string) types.Email {
	t.Helper()

	e, err := types.ParseEmail(s)
	if err != nil {
		t.Fatalf("ParseEmail(%q): %v", s, err)
	}

	return e
}

func mustCreate(t *testing.T, b *userbus.Business, addr string) userbus.User {
	t.Helper()

	u, err := b.Create(t.Context(), now, userbus.NewUser{
		Email: mustEmail(t, addr),
		Name:  "Somebody",
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", addr, err)
	}

	return u
}

func TestCreate(t *testing.T) {
	b, _ := newBusiness(t)

	u := mustCreate(t, b, "frjeff@schoenstatt.us")

	switch {
	case u.ID.Zero():
		t.Error("the new account has no identifier")
	case u.Email.String() != "frjeff@schoenstatt.us":
		t.Errorf("Email = %q", u.Email)
	case !u.Enabled:
		t.Error("a new account is not enabled, so nobody could ever sign in")
	case !u.CreatedAt.Equal(now):
		t.Errorf("CreatedAt = %s, want %s", u.CreatedAt, now)
	}

	// The collision is information an administrator is entitled to, unlike
	// anything on the sign-in path.
	_, err := b.Create(t.Context(), now, userbus.NewUser{Email: mustEmail(t, "frjeff@schoenstatt.us")})
	if !errors.Is(err, userbus.ErrEmailTaken) {
		t.Errorf("creating a duplicate returned %v, want ErrEmailTaken", err)
	}

	// And it is the *normalised* address that collides, which is the whole
	// point of folding case in types.Email.
	_, err = b.Create(t.Context(), now, userbus.NewUser{Email: mustEmail(t, "FrJeff@Schoenstatt.US")})
	if !errors.Is(err, userbus.ErrEmailTaken) {
		t.Errorf("creating the same address in different case returned %v, want ErrEmailTaken", err)
	}

	if _, err := b.Create(t.Context(), now, userbus.NewUser{}); err == nil {
		t.Error("Create accepted an account with no address")
	}
}

// The enumeration oracle this package is built to avoid. Asking for a link
// must look the same whether or not an account exists.
func TestRequestSignInSaysNothingAboutWhoExists(t *testing.T) {
	b, store := newBusiness(t)

	known := mustCreate(t, b, "frjeff@schoenstatt.us")

	disabled := mustCreate(t, b, "gone@schoenstatt.us")
	disabled.Enabled = false
	if err := store.UpdateUser(t.Context(), disabled); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	for _, tt := range []struct {
		name     string
		email    string
		sendable bool
	}{
		{"an account that exists", "frjeff@schoenstatt.us", true},
		{"an account that does not", "nobody@schoenstatt.us", false},
		{"a disabled account", "gone@schoenstatt.us", false},
		{"an address at another domain entirely", "someone@example.org", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, err := b.RequestSignIn(t.Context(), now, mustEmail(t, tt.email))

			// No error in any case. An error would tempt a handler into
			// saying something different, and saying something different is
			// the oracle.
			if err != nil {
				t.Fatalf("RequestSignIn returned an error for %s: %v", tt.name, err)
			}

			if req.Sendable() != tt.sendable {
				t.Errorf("Sendable() = %v, want %v", req.Sendable(), tt.sendable)
			}

			if !tt.sendable && req.Secret != "" {
				t.Error("there is a secret to send for an account that cannot receive one")
			}
			if tt.sendable && req.User.ID != known.ID {
				t.Errorf("the request names %q, want the known account", req.User.ID)
			}
		})
	}
}

func TestSignIn(t *testing.T) {
	b, store := newBusiness(t)
	u := mustCreate(t, b, "frjeff@schoenstatt.us")

	req, err := b.RequestSignIn(t.Context(), now, u.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}

	got, cookie, err := b.SignIn(t.Context(), now, req.Secret)
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}

	if got.ID != u.ID {
		t.Errorf("signed in as %q, want %q", got.ID, u.ID)
	}
	if cookie == "" {
		t.Fatal("SignIn returned no session")
	}

	// The session works.
	who, err := b.Authenticate(t.Context(), now, cookie)
	if err != nil {
		t.Fatalf("Authenticate with a fresh session: %v", err)
	}
	if who.ID != u.ID {
		t.Errorf("the session belongs to %q, want %q", who.ID, u.ID)
	}

	// Single use. This is the property the whole token table exists for.
	if _, _, err := b.SignIn(t.Context(), now, req.Secret); !errors.Is(err, userbus.ErrDenied) {
		t.Errorf("a sign-in link worked twice: %v", err)
	}

	// Neither the link nor the cookie is recoverable from storage.
	linkSecret := req.Secret[strings.Index(req.Secret, ".")+1:]
	cookieSecret := cookie[strings.Index(cookie, ".")+1:]

	for _, stored := range store.storedSecrets() {
		for _, secret := range []string{linkSecret, cookieSecret} {
			if strings.Contains(stored, secret) {
				t.Error("a secret was stored, not just its hash")
			}
		}
	}
}

func TestSignInRefuses(t *testing.T) {
	b, _ := newBusiness(t)
	u := mustCreate(t, b, "frjeff@schoenstatt.us")

	fresh := func(t *testing.T) string {
		t.Helper()

		req, err := b.RequestSignIn(t.Context(), now, u.Email)
		if err != nil {
			t.Fatalf("RequestSignIn: %v", err)
		}

		return req.Secret
	}

	valid := fresh(t)
	id, secret, _ := strings.Cut(valid, ".")

	tests := []struct {
		name string
		at   time.Time
		give func(t *testing.T) string
	}{
		{name: "blank", give: func(*testing.T) string { return "" }},
		{name: "no separator", give: func(*testing.T) string { return id + secret }},
		{name: "not an identifier", give: func(*testing.T) string { return "nonsense." + secret }},
		{name: "an identifier with no secret", give: func(*testing.T) string { return id + "." }},
		{name: "a secret too short to be one", give: func(*testing.T) string { return id + ".abc" }},

		{
			name: "a wrong secret against a real identifier",
			give: func(t *testing.T) string { return fresh(t)[:33] + strings.Repeat("A", 26) },
		},
		{
			name: "a real secret against a wrong identifier",
			give: func(t *testing.T) string {
				_, s, _ := strings.Cut(fresh(t), ".")

				return types.NewID().String() + "." + s
			},
		},
		{
			name: "two credentials joined together",
			give: func(t *testing.T) string { return fresh(t) + "." + fresh(t) },
		},

		{
			name: "expired by one second",
			at:   now.Add(15 * time.Minute),
			give: fresh,
		},
		{
			name: "expired by a day",
			at:   now.Add(24 * time.Hour),
			give: fresh,
		},
		{
			name: "already used",
			give: func(t *testing.T) string {
				s := fresh(t)
				if _, _, err := b.SignIn(t.Context(), now, s); err != nil {
					t.Fatalf("the first use failed: %v", err)
				}

				return s
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at := tt.at
			if at.IsZero() {
				at = now
			}

			user, cookie, err := b.SignIn(t.Context(), at, tt.give(t))

			// One error for every failure. The app layer cannot leak which
			// half was wrong because it is never told.
			if !errors.Is(err, userbus.ErrDenied) {
				t.Errorf("SignIn returned %v, want ErrDenied", err)
			}
			if cookie != "" {
				t.Error("a refused sign-in handed back a session")
			}
			if !user.ID.Zero() || user.Enabled {
				t.Errorf("a refused sign-in handed back an account: %+v", user)
			}
		})
	}
}

// A link used at the last moment before expiry must work, or the window is
// really one second shorter than it says.
func TestSignInAtTheEdgeOfTheWindow(t *testing.T) {
	b, _ := newBusiness(t)
	u := mustCreate(t, b, "frjeff@schoenstatt.us")

	req, err := b.RequestSignIn(t.Context(), now, u.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}

	justInside := now.Add(15*time.Minute - time.Nanosecond)
	if _, _, err := b.SignIn(t.Context(), justInside, req.Secret); err != nil {
		t.Errorf("a link used just inside its window was refused: %v", err)
	}
}

// Two requests holding the same link must not both succeed. This is what the
// atomic claim in Storer is for, and a SELECT-then-UPDATE would fail it.
func TestSignInIsSingleUseUnderConcurrency(t *testing.T) {
	b, _ := newBusiness(t)
	u := mustCreate(t, b, "frjeff@schoenstatt.us")

	req, err := b.RequestSignIn(t.Context(), now, u.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}

	const racers = 20

	var (
		start    sync.WaitGroup
		wg       sync.WaitGroup
		mu       sync.Mutex
		sessions []string
		denied   int
	)

	start.Add(1)

	for range racers {
		wg.Go(func() {
			start.Wait()

			_, cookie, err := b.SignIn(t.Context(), now, req.Secret)

			mu.Lock()
			defer mu.Unlock()

			switch {
			case err == nil:
				sessions = append(sessions, cookie)
			case errors.Is(err, userbus.ErrDenied):
				denied++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}

	start.Done()
	wg.Wait()

	if len(sessions) != 1 {
		t.Errorf("%d of %d racing requests each got a session; a sign-in link must be spendable once", len(sessions), racers)
	}
	if denied != racers-1 {
		t.Errorf("%d requests were denied, want %d", denied, racers-1)
	}
}

func TestBackupCodes(t *testing.T) {
	b, _ := newBusiness(t)
	u := mustCreate(t, b, "frjeff@schoenstatt.us")

	codes, err := b.IssueBackupCodes(t.Context(), now, u.ID)
	if err != nil {
		t.Fatalf("IssueBackupCodes: %v", err)
	}

	if len(codes) != 10 {
		t.Fatalf("got %d codes, want 10", len(codes))
	}

	seen := map[string]bool{}
	for _, c := range codes {
		if seen[c] {
			t.Errorf("the same code was issued twice: %q", c)
		}
		seen[c] = true

		// Grouped for typing: four groups of four.
		if got := strings.Split(c, "-"); len(got) != 4 {
			t.Errorf("%q is not grouped for typing", c)
		}
		if len(strings.ReplaceAll(c, "-", "")) != 16 {
			t.Errorf("%q is not sixteen characters", c)
		}
	}

	// One code signs in, once.
	if _, cookie, err := b.SignInWithBackupCode(t.Context(), now, u.Email, codes[0]); err != nil {
		t.Fatalf("SignInWithBackupCode: %v", err)
	} else if cookie == "" {
		t.Error("no session was issued")
	}

	if _, _, err := b.SignInWithBackupCode(t.Context(), now, u.Email, codes[0]); !errors.Is(err, userbus.ErrDenied) {
		t.Errorf("a backup code worked twice: %v", err)
	}

	// The others still work.
	if _, _, err := b.SignInWithBackupCode(t.Context(), now, u.Email, codes[1]); err != nil {
		t.Errorf("a second code was refused: %v", err)
	}

	// Reissuing replaces. Somebody who has lost track of which codes they
	// hold needs one honest list, and somebody who suspects a leak needs the
	// old ones dead.
	fresh, err := b.IssueBackupCodes(t.Context(), now, u.ID)
	if err != nil {
		t.Fatalf("IssueBackupCodes again: %v", err)
	}

	if _, _, err := b.SignInWithBackupCode(t.Context(), now, u.Email, codes[2]); !errors.Is(err, userbus.ErrDenied) {
		t.Errorf("a code from before reissuing still worked: %v", err)
	}
	if _, _, err := b.SignInWithBackupCode(t.Context(), now, u.Email, fresh[0]); err != nil {
		t.Errorf("a newly issued code was refused: %v", err)
	}
}

// Everything here is about a code read off a piece of paper and typed by hand
// at the moment mail has stopped working, which is the worst moment to be
// fussy about punctuation.
func TestBackupCodeNormalisation(t *testing.T) {
	b, _ := newBusiness(t)
	u := mustCreate(t, b, "frjeff@schoenstatt.us")

	bare := func(code string) string { return strings.ReplaceAll(code, "-", "") }

	variants := []struct {
		name  string
		type_ func(code string) string
	}{
		{"as printed", func(c string) string { return c }},
		{"without hyphens", bare},
		{"lower case", strings.ToLower},
		{"lower case without hyphens", func(c string) string { return strings.ToLower(bare(c)) }},
		{"with spaces instead of hyphens", func(c string) string { return strings.ReplaceAll(c, "-", " ") }},
		{"with spaces at the ends", func(c string) string { return "  " + c + "  " }},
		{"with extra hyphens", func(c string) string { return strings.ReplaceAll(c, "-", "--") }},

		// The base32 alphabet is A-Z and 2-7, so 0, 1 and 8 cannot appear in
		// a real code and can only be somebody reading O, I and B off paper.
		{"with zero for O", func(c string) string { return strings.ReplaceAll(c, "O", "0") }},
		{"with one for I", func(c string) string { return strings.ReplaceAll(c, "I", "1") }},
		{"with eight for B", func(c string) string { return strings.ReplaceAll(c, "B", "8") }},
	}

	for _, tt := range variants {
		t.Run(tt.name, func(t *testing.T) {
			// A fresh set per variant, because each success spends a code --
			// and reissuing invalidates the rest, so each subtest has to
			// derive its input from its own set.
			set, err := b.IssueBackupCodes(t.Context(), now, u.ID)
			if err != nil {
				t.Fatalf("IssueBackupCodes: %v", err)
			}

			typed := tt.type_(set[0])

			if _, _, err := b.SignInWithBackupCode(t.Context(), now, u.Email, typed); err != nil {
				t.Errorf("a code typed %s was refused: %q from %q: %v", tt.name, typed, set[0], err)
			}
		})
	}
}

func TestBackupCodeRefuses(t *testing.T) {
	b, _ := newBusiness(t)
	u := mustCreate(t, b, "frjeff@schoenstatt.us")
	other := mustCreate(t, b, "someone@schoenstatt.us")

	codes, err := b.IssueBackupCodes(t.Context(), now, u.ID)
	if err != nil {
		t.Fatalf("IssueBackupCodes: %v", err)
	}

	tests := []struct {
		name  string
		email types.Email
		typed string
	}{
		{name: "blank", email: u.Email},
		{name: "too short", email: u.Email, typed: "ABCD"},
		{name: "too long", email: u.Email, typed: strings.ReplaceAll(codes[0], "-", "") + "A"},
		{name: "invented", email: u.Email, typed: "ABCD-EFGH-JKLM-NPQR"},
		{name: "an address with no account", email: mustEmail(t, "nobody@schoenstatt.us"), typed: codes[0]},

		// Somebody else's code. Codes carry no identifier, so this is the
		// case that would pass if the lookup were by code alone.
		{name: "a valid code against the wrong account", email: other.Email, typed: codes[0]},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user, cookie, err := b.SignInWithBackupCode(t.Context(), now, tt.email, tt.typed)

			if !errors.Is(err, userbus.ErrDenied) {
				t.Errorf("returned %v, want ErrDenied", err)
			}
			if cookie != "" || !user.ID.Zero() {
				t.Error("a refused attempt handed back a session or an account")
			}
		})
	}

	// And the code that was tried against the wrong account still works for
	// the right one -- a failed attempt must not spend it.
	if _, _, err := b.SignInWithBackupCode(t.Context(), now, u.Email, codes[0]); err != nil {
		t.Errorf("a code tried against the wrong account was spent: %v", err)
	}
}

// The way in when nothing else works yet, which is why it exists at all.
func TestBootstrap(t *testing.T) {
	const secret = "a-long-one-time-secret-from-the-config-file"

	b, _ := newBusiness(t)
	email := mustEmail(t, "frjeff@schoenstatt.us")

	// There is no account at all. That is the situation this is for.
	u, cookie, err := b.Bootstrap(t.Context(), now, secret, secret, email)
	if err != nil {
		t.Fatalf("Bootstrap on an empty service: %v", err)
	}

	switch {
	case u.ID.Zero():
		t.Error("bootstrap did not create an account")
	case u.Email != email:
		t.Errorf("the account is %q, want %q", u.Email, email)
	case !u.Enabled:
		t.Error("the account it created cannot sign in")
	case cookie == "":
		t.Error("bootstrap issued no session")
	}

	if who, err := b.Authenticate(t.Context(), now, cookie); err != nil {
		t.Errorf("the bootstrap session does not work: %v", err)
	} else if who.ID != u.ID {
		t.Errorf("the session belongs to %q, want %q", who.ID, u.ID)
	}

	// Exactly once. Restarting the process does not restore it, because the
	// claim is in storage rather than in memory -- and rotating the secret in
	// the config does not revive it either, which is the next case.
	if _, _, err := b.Bootstrap(t.Context(), now, secret, secret, email); !errors.Is(err, userbus.ErrDenied) {
		t.Errorf("bootstrap worked twice: %v", err)
	}

	const rotated = "a-different-secret-written-into-the-config-later"
	if _, _, err := b.Bootstrap(t.Context(), now, rotated, rotated, email); !errors.Is(err, userbus.ErrDenied) {
		t.Errorf("rotating the configured secret revived the bootstrap: %v", err)
	}
}

func TestBootstrapRefuses(t *testing.T) {
	const secret = "a-long-one-time-secret-from-the-config-file"

	email := "frjeff@schoenstatt.us"

	tests := []struct {
		name       string
		configured string
		presented  string
	}{
		{name: "a wrong secret", configured: secret, presented: "not the secret at all now"},
		{name: "a blank presented secret", configured: secret, presented: ""},
		{name: "a prefix of the secret", configured: secret, presented: secret[:20]},
		{name: "the secret with something appended", configured: secret, presented: secret + "x"},

		// The case that matters most: bootstrap not configured at all. An
		// empty configured secret must never match, or a deployment that
		// forgot to set one is a deployment anybody can sign into.
		{name: "nothing configured, nothing presented", configured: "", presented: ""},
		{name: "nothing configured, something presented", configured: "", presented: "anything"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := newBusiness(t)

			u, cookie, err := b.Bootstrap(t.Context(), now, tt.configured, tt.presented, mustEmail(t, email))

			if !errors.Is(err, userbus.ErrDenied) {
				t.Errorf("returned %v, want ErrDenied", err)
			}
			if cookie != "" || !u.ID.Zero() {
				t.Error("a refused bootstrap handed back a session or an account")
			}

			// And it must not have been spent, or one wrong guess would
			// disable the only way back in.
			if _, _, err := b.Bootstrap(t.Context(), now, secret, secret, mustEmail(t, email)); err != nil {
				t.Errorf("a refused attempt spent the bootstrap: %v", err)
			}
		})
	}
}

func TestAuthenticate(t *testing.T) {
	b, store := newBusiness(t)
	u := mustCreate(t, b, "frjeff@schoenstatt.us")

	req, err := b.RequestSignIn(t.Context(), now, u.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}

	_, cookie, err := b.SignIn(t.Context(), now, req.Secret)
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}

	id, secret, _ := strings.Cut(cookie, ".")

	t.Run("a fresh session", func(t *testing.T) {
		if _, err := b.Authenticate(t.Context(), now, cookie); err != nil {
			t.Errorf("Authenticate: %v", err)
		}
	})

	t.Run("just inside the window", func(t *testing.T) {
		at := now.Add(14*24*time.Hour - time.Nanosecond)
		if _, err := b.Authenticate(t.Context(), at, cookie); err != nil {
			t.Errorf("a session just inside its window was refused: %v", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		at := now.Add(14 * 24 * time.Hour)
		if _, err := b.Authenticate(t.Context(), at, cookie); !errors.Is(err, userbus.ErrDenied) {
			t.Errorf("an expired session was accepted: %v", err)
		}
	})

	for _, tt := range []struct {
		name string
		give string
	}{
		{"blank", ""},
		{"no separator", id + secret},
		{"not an identifier", "nonsense." + secret},
		{"a wrong secret", id + "." + strings.Repeat("A", 26)},
		{"an unknown identifier", types.NewID().String() + "." + secret},
		{"a secret too short to be one", id + ".short"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := b.Authenticate(t.Context(), now, tt.give); !errors.Is(err, userbus.ErrDenied) {
				t.Errorf("Authenticate accepted %q: %v", tt.give, err)
			}
		})
	}

	// Disabling an account ends its sessions on the next request rather than
	// in two weeks, which is why Enabled is checked here and not only at
	// sign-in.
	t.Run("a disabled account", func(t *testing.T) {
		u.Enabled = false
		if err := store.UpdateUser(t.Context(), u); err != nil {
			t.Fatalf("UpdateUser: %v", err)
		}

		if _, err := b.Authenticate(t.Context(), now, cookie); !errors.Is(err, userbus.ErrDenied) {
			t.Errorf("a disabled account's session still worked: %v", err)
		}
	})
}

func TestSignOut(t *testing.T) {
	b, _ := newBusiness(t)
	u := mustCreate(t, b, "frjeff@schoenstatt.us")

	signIn := func(t *testing.T) string {
		t.Helper()

		req, err := b.RequestSignIn(t.Context(), now, u.Email)
		if err != nil {
			t.Fatalf("RequestSignIn: %v", err)
		}

		_, cookie, err := b.SignIn(t.Context(), now, req.Secret)
		if err != nil {
			t.Fatalf("SignIn: %v", err)
		}

		return cookie
	}

	one, two := signIn(t), signIn(t)

	if err := b.SignOut(t.Context(), one); err != nil {
		t.Fatalf("SignOut: %v", err)
	}

	if _, err := b.Authenticate(t.Context(), now, one); !errors.Is(err, userbus.ErrDenied) {
		t.Errorf("a signed-out session still worked: %v", err)
	}
	if _, err := b.Authenticate(t.Context(), now, two); err != nil {
		t.Errorf("signing out one session ended another: %v", err)
	}

	// A malformed or unknown cookie is not an error. The caller wanted no
	// session, and it has none.
	for _, junk := range []string{"", "nonsense", types.NewID().String() + ".whatever"} {
		if err := b.SignOut(t.Context(), junk); err != nil {
			t.Errorf("SignOut(%q) returned %v, want nothing", junk, err)
		}
	}

	if err := b.SignOutEverywhere(t.Context(), u.ID); err != nil {
		t.Fatalf("SignOutEverywhere: %v", err)
	}
	if _, err := b.Authenticate(t.Context(), now, two); !errors.Is(err, userbus.ErrDenied) {
		t.Errorf("SignOutEverywhere left a session working: %v", err)
	}
}

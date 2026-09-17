package formbus_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
)

const testSecret = "a-machine-generated-secret-long-enough"

func testKey(t *testing.T) formbus.GrantKey {
	t.Helper()

	k, err := formbus.ParseGrantKey(testSecret)
	if err != nil {
		t.Fatalf("ParseGrantKey: %v", err)
	}

	return k
}

func TestParseGrantKeyRefusesAShortSecret(t *testing.T) {
	for _, s := range []string{"", "short", strings.Repeat("x", 31)} {
		if _, err := formbus.ParseGrantKey(s); !errors.Is(err, formbus.ErrWeakGrantKey) {
			t.Errorf("ParseGrantKey(%d chars) = %v, want ErrWeakGrantKey", len(s), err)
		}
	}

	if _, err := formbus.ParseGrantKey(strings.Repeat("x", 32)); err != nil {
		t.Errorf("ParseGrantKey(32 chars): %v", err)
	}
}

func TestAMintedGrantRedeems(t *testing.T) {
	key := testKey(t)
	f := feast(t)

	token, err := formbus.Mint(key, f, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	g, err := formbus.Redeem(key, token, f, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}

	if g.Form != f.ID {
		t.Errorf("Form = %q, want %q", g.Form, f.ID)
	}
	if g.Version != f.Version {
		t.Errorf("Version = %q, want %q", g.Version, f.Version)
	}
	if g.Nonce == "" {
		t.Error("the grant carries no nonce")
	}
	if !g.ExpiresAt.After(now) {
		t.Errorf("ExpiresAt = %v, want after %v", g.ExpiresAt, now)
	}
}

func TestEveryMintIsADifferentGrant(t *testing.T) {
	key := testKey(t)
	f := feast(t)

	seen := map[string]bool{}
	for range 100 {
		token, err := formbus.Mint(key, f, now)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}

		g, err := formbus.Redeem(key, token, f, now)
		if err != nil {
			t.Fatalf("Redeem: %v", err)
		}

		if seen[g.Nonce] {
			t.Fatalf("nonce %q was minted twice", g.Nonce)
		}

		seen[g.Nonce] = true
	}
}

func TestMintRefusesAnUnstampedForm(t *testing.T) {
	key := testKey(t)

	f := feast(t)
	f.Version = ""

	if _, err := formbus.Mint(key, f, now); err == nil {
		t.Error("minted a grant against a form with no version")
	}
}

// The property the whole design rests on: editing a price invalidates every
// grant already sitting in a browser, with nobody having to remember anything.
func TestChangingThePriceRefusesAnOutstandingGrant(t *testing.T) {
	key := testKey(t)
	f := feast(t)

	token, err := formbus.Mint(key, f, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// The edit somebody makes the night before: twelve dollars becomes
	// fifteen. Stamp recomputes the fingerprint, which is what the grant
	// pinned.
	raised := f
	raised.Items = append([]formbus.Item(nil), f.Items...)
	raised.Items[0].Price = types.Money(1500)
	raised.Stamp()

	if raised.Version == f.Version {
		t.Fatal("raising a price did not change the version")
	}

	_, err = formbus.Redeem(key, token, raised, now.Add(time.Minute))
	if !errors.Is(err, formbus.ErrGrantRefused) {
		t.Fatalf("Redeem against the new price = %v, want ErrGrantRefused", err)
	}

	// And the old definition still accepts it, so the refusal is about the
	// version rather than about the token having been damaged.
	if _, err := formbus.Redeem(key, token, f, now.Add(time.Minute)); err != nil {
		t.Errorf("the grant no longer redeems against the form it was minted for: %v", err)
	}
}

// The ambiguity the length-prefixed encoding exists for. Concatenating
// ("fall-retreat", "2026abc") and ("fall-retreat2", "026abc") gives the same
// bytes, so with a naive encoding a grant for one form would MAC identically
// to a grant for the other.
func TestAGrantForOneFormCannotSubmitAnother(t *testing.T) {
	key := testKey(t)

	a := feast(t)
	a.ID = mustSlug(t, "fall-retreat")
	a.Version = "2026abc"

	b := feast(t)
	b.ID = mustSlug(t, "fall-retreat2")
	b.Version = "026abc"

	token, err := formbus.Mint(key, a, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if _, err := formbus.Redeem(key, token, b, now); !errors.Is(err, formbus.ErrGrantRefused) {
		t.Errorf("a grant for %q was accepted by %q: %v", a.ID, b.ID, err)
	}

	// And the other way round.
	token, err = formbus.Mint(key, b, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if _, err := formbus.Redeem(key, token, a, now); !errors.Is(err, formbus.ErrGrantRefused) {
		t.Errorf("a grant for %q was accepted by %q: %v", b.ID, a.ID, err)
	}
}

func TestAnExpiredGrantIsRefused(t *testing.T) {
	key := testKey(t)
	f := feast(t)

	token, err := formbus.Mint(key, f, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Still good just before, refused at the instant it expires -- the bound
	// is exclusive, so a grant is not usable at its own expiry.
	g, err := formbus.Redeem(key, token, f, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("Redeem an hour in: %v", err)
	}

	if _, err := formbus.Redeem(key, token, f, g.ExpiresAt); !errors.Is(err, formbus.ErrGrantRefused) {
		t.Errorf("Redeem at the expiry = %v, want ErrGrantRefused", err)
	}
	if _, err := formbus.Redeem(key, token, f, g.ExpiresAt.Add(time.Second)); !errors.Is(err, formbus.ErrGrantRefused) {
		t.Errorf("Redeem after the expiry = %v, want ErrGrantRefused", err)
	}
}

func TestAnotherKeyCannotMintAGrantWeAccept(t *testing.T) {
	ours := testKey(t)
	f := feast(t)

	theirs, err := formbus.ParseGrantKey("a-completely-different-secret-value-here")
	if err != nil {
		t.Fatalf("ParseGrantKey: %v", err)
	}

	token, err := formbus.Mint(theirs, f, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if _, err := formbus.Redeem(ours, token, f, now); !errors.Is(err, formbus.ErrGrantRefused) {
		t.Errorf("a grant signed with another key was accepted: %v", err)
	}
}

// Everything a browser could hand back that is not a grant we minted. The
// point is that each one is one error and none of them is a panic, a partial
// grant, or an index out of range in the decoder.
func TestMalformedGrantsAreRefusedAndNotDecoded(t *testing.T) {
	key := testKey(t)
	f := feast(t)

	good, err := formbus.Mint(key, f, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	payload, mac, _ := strings.Cut(good, ".")
	enc := base64.RawURLEncoding

	raw, err := enc.DecodeString(payload)
	if err != nil {
		t.Fatalf("decoding our own payload: %v", err)
	}

	// A payload truncated at every length, each still carrying a MAC that is
	// the right size but wrong. The decoder must refuse rather than read past
	// the end of a field.
	truncated := make([]string, 0, len(raw))
	for i := range len(raw) {
		truncated = append(truncated, enc.EncodeToString(raw[:i])+"."+mac)
	}

	cases := map[string][]string{
		"empty":                {""},
		"no separator":         {payload, payload + mac},
		"too many separators":  {payload + "." + mac + "." + mac, "." + payload + "." + mac},
		"separator at an edge": {"." + payload, payload + "."},
		"empty halves":         {".", ".."},
		"not base64":           {"!!!." + mac, payload + ".!!!"},
		"mac of the wrong size": {
			payload + "." + enc.EncodeToString([]byte("short")),
			payload + "." + enc.EncodeToString(append(raw, raw...)),
		},
		"payload swapped for the mac": {mac + "." + payload},
		"truncated payload":           truncated,

		// The MAC is checked before the payload is decoded, so a payload with
		// trailing bytes or a bogus format byte cannot even be reached without
		// the key -- these assert it is refused, not how.
		"trailing bytes": {enc.EncodeToString(append(raw, 0)) + "." + mac},
	}

	for name, tokens := range cases {
		for _, token := range tokens {
			g, err := formbus.Redeem(key, token, f, now)
			if !errors.Is(err, formbus.ErrGrantRefused) {
				t.Errorf("%s: Redeem(%.24q) = %v, want ErrGrantRefused", name, token, err)
			}
			if g != (formbus.Grant{}) {
				t.Errorf("%s: Redeem returned %+v alongside an error", name, g)
			}
		}
	}
}

// Flipping any single bit of the token must refuse it. This is the MAC doing
// its job, and it is cheap to assert exhaustively over the payload.
func TestFlippingAnyBitRefusesTheGrant(t *testing.T) {
	key := testKey(t)
	f := feast(t)

	good, err := formbus.Mint(key, f, now)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	enc := base64.RawURLEncoding
	payload, mac, _ := strings.Cut(good, ".")

	raw, err := enc.DecodeString(payload)
	if err != nil {
		t.Fatalf("decoding our own payload: %v", err)
	}

	for i := range raw {
		for bit := range 8 {
			flipped := append([]byte(nil), raw...)
			flipped[i] ^= 1 << bit

			token := enc.EncodeToString(flipped) + "." + mac

			if _, err := formbus.Redeem(key, token, f, now); !errors.Is(err, formbus.ErrGrantRefused) {
				t.Fatalf("byte %d bit %d: Redeem = %v, want ErrGrantRefused", i, bit, err)
			}
		}
	}
}

func TestAGrantHasNoKeyToRedeemWith(t *testing.T) {
	f := feast(t)

	if _, err := formbus.Mint(formbus.GrantKey{}, f, now); err == nil {
		t.Error("minted a grant with no key")
	}
	if _, err := formbus.Redeem(formbus.GrantKey{}, "whatever", f, now); err == nil {
		t.Error("redeemed a grant with no key")
	}
}

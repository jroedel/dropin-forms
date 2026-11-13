package seal_test

import (
	"errors"
	"testing"

	"github.com/jroedel/dropin-forms/foundation/seal"
)

const aKey = "q83vEjRWeJCrze8SNFZ4kKvN7xI0VniQq83vEjRWeJA="

func TestASealedValueOpensOnlyWithItsKeyAndPurpose(t *testing.T) {
	k, err := seal.ParseKey(aKey)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}

	sealed, err := k.Seal("1//refresh-token", "connection-a")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if got, err := k.Open(sealed, "connection-a"); err != nil || got != "1//refresh-token" {
		t.Fatalf("Open = %q, %v", got, err)
	}

	// Copied onto another row, it does not open.
	if _, err := k.Open(sealed, "connection-b"); !errors.Is(err, seal.ErrOpen) {
		t.Errorf("opened for another purpose: %v", err)
	}

	// Altered, it does not open.
	sealed[len(sealed)-1] ^= 1
	if _, err := k.Open(sealed, "connection-a"); !errors.Is(err, seal.ErrOpen) {
		t.Errorf("an altered value opened: %v", err)
	}

	// Another key does not open it.
	other, _ := seal.FromBytes(seal.Derive([]byte("x"), "y"))
	again, _ := k.Seal("1//refresh-token", "connection-a")
	if _, err := other.Open(again, "connection-a"); !errors.Is(err, seal.ErrOpen) {
		t.Errorf("another key opened it: %v", err)
	}

	// Two sealings of the same value differ.
	if a, b := mustSeal(t, k), mustSeal(t, k); string(a) == string(b) {
		t.Error("the same value sealed twice is the same bytes; the nonce is not fresh")
	}
}

func TestAKeyMustBeThirtyTwoBytes(t *testing.T) {
	for _, bad := range []string{"", "short", "c2hvcnQ="} {
		if _, err := seal.ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) accepted it", bad)
		}
	}
}

func mustSeal(t *testing.T, k seal.Key) []byte {
	t.Helper()

	b, err := k.Seal("v", "w")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	return b
}

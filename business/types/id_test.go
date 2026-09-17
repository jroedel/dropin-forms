package types_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/business/types"
)

func TestNewIDIsRandomAndParseable(t *testing.T) {
	const n = 1000

	seen := make(map[types.ID]bool, n)

	for range n {
		id := types.NewID()

		if id.Zero() {
			t.Fatal("NewID returned the zero identifier")
		}
		if len(id.String()) != 32 {
			t.Fatalf("NewID returned %q, which is %d characters", id, len(id.String()))
		}
		if seen[id] {
			t.Fatalf("NewID returned %q twice in %d draws", id, n)
		}
		seen[id] = true

		// An ID must survive the round trip it makes on every request: into a
		// URL or a database column, and back.
		back, err := types.ParseID(id.String())
		if err != nil {
			t.Fatalf("ParseID(%q): %v", id, err)
		}
		if back != id {
			t.Fatalf("ParseID(%q) = %q", id, back)
		}
	}
}

func TestParseIDRejects(t *testing.T) {
	valid := types.NewID().String()

	tests := []struct {
		name string
		in   string
	}{
		{"blank", ""},
		{"one character short", valid[:31]},
		{"one character long", valid + "0"},
		{"a whole extra id", valid + valid},

		// Upper case is refused rather than folded. hex.DecodeString accepts
		// it, so these would decode to the same bytes but compare unequal as
		// strings -- and a value used as both a map key and a database key
		// must have exactly one spelling.
		{"upper case", strings.ToUpper(valid)},
		{"mixed case", "ABCDEF01" + valid[8:]},

		{"not hex", "g" + valid[1:]},
		{"a slug", "feast-lunch-2026"},
		{"path traversal", strings.Repeat("a", 29) + "/.."},
		{"a null byte", valid[:31] + "\x00"},
		{"whitespace", " " + valid[1:]},
		{"leading 0x", "0x" + valid[2:]},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := types.ParseID(tt.in)
			if err == nil {
				t.Fatalf("ParseID(%q) = %q, want an error", tt.in, got)
			}
			if !errors.Is(err, types.ErrNotAnID) {
				t.Errorf("ParseID(%q) error does not wrap ErrNotAnID: %v", tt.in, err)
			}
			if !got.Zero() {
				t.Errorf("ParseID(%q) returned %q alongside its error", tt.in, got)
			}
		})
	}
}

// The boundaries of the alphabet, asserted from both ends. Digits are hex, so
// an ID of nothing but digits is legal however unlikely it looks -- and 'f' is
// in while 'g' is out.
func TestParseIDAcceptsTheWholeAlphabet(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("0", 32),
		strings.Repeat("f", 32),
		strings.Repeat("9", 32),
		"0123456789abcdef0123456789abcdef",
	} {
		if _, err := types.ParseID(in); err != nil {
			t.Errorf("ParseID(%q): %v", in, err)
		}
	}

	if _, err := types.ParseID(strings.Repeat("g", 32)); err == nil {
		t.Error("ParseID accepted 'g', which is not a hex digit")
	}
}

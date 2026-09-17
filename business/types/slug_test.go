package types_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/business/types"
)

func TestParseSlugAccepts(t *testing.T) {
	for _, in := range []string{
		"feast-lunch-2026",
		"ab",
		"a1",
		"2026",
		"a-b-c-d-e",
		strings.Repeat("a", 64),
	} {
		t.Run(in, func(t *testing.T) {
			got, err := types.ParseSlug(in)
			if err != nil {
				t.Fatalf("ParseSlug(%q): %v", in, err)
			}
			if got.String() != in {
				t.Errorf("ParseSlug(%q).String() = %q; a slug must survive unchanged", in, got.String())
			}
			if got.Zero() {
				t.Errorf("ParseSlug(%q) reports itself as the zero slug", in)
			}
		})
	}
}

func TestParseSlugRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"blank", ""},
		{"one character", "a"},
		{"too long", strings.Repeat("a", 65)},

		// Anything that changes what a path means.
		{"slash", "a/b"},
		{"parent directory", ".."},
		{"dot segment", "a/../b"},
		{"dot", "a.b"},
		{"trailing extension", "form.toml"},
		{"percent encoding", "a%2fb"},
		{"backslash", "a\\b"},
		{"null byte", "a\x00b"},
		{"newline", "a\nb"},
		{"space", "a b"},
		{"leading slash", "/ab"},

		// Anything that changes what HTML means.
		{"angle brackets", "<b>"},
		{"double quote", "a\"b"},
		{"single quote", "a'b"},
		{"ampersand", "a&b"},

		// Case, which is refused rather than folded.
		{"capital", "Feast"},
		{"all capitals", "FEAST"},
		{"mixed", "feastLunch"},

		// Hyphen placement.
		{"leading hyphen", "-ab"},
		{"trailing hyphen", "ab-"},
		{"doubled hyphen", "a--b"},
		{"only hyphens", "--"},

		// Other separators a person might reach for.
		{"underscore", "a_b"},
		{"colon", "a:b"},
		{"question mark", "a?b"},
		{"hash", "a#b"},

		// Not ASCII.
		{"accented", "café"},
		{"cyrillic homoglyphs", "аб"},
		{"emoji", "a\U0001f600b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := types.ParseSlug(tt.in)
			if err == nil {
				t.Fatalf("ParseSlug(%q) = %q, want an error", tt.in, got)
			}
			if !errors.Is(err, types.ErrNotASlug) {
				t.Errorf("ParseSlug(%q) error does not wrap ErrNotASlug: %v", tt.in, err)
			}
			if !got.Zero() {
				t.Errorf("ParseSlug(%q) returned %q alongside its error", tt.in, got)
			}
		})
	}
}

// The suggestion in the capital-letter message must itself be a valid slug, or
// it is advice that does not work.
func TestParseSlugCapitalMessageSuggestsSomethingValid(t *testing.T) {
	_, err := types.ParseSlug("Feast-Lunch-2026")
	if err == nil {
		t.Fatal("ParseSlug unexpectedly accepted a capital letter")
	}

	if !strings.Contains(err.Error(), "feast-lunch-2026") {
		t.Errorf("the message does not suggest the lowercase form: %v", err)
	}

	if _, err := types.ParseSlug("feast-lunch-2026"); err != nil {
		t.Errorf("the suggested slug is not itself valid: %v", err)
	}
}

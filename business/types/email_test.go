package types_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/business/types"
)

func TestParseEmailAccepts(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"frjeff@schoenstatt.us", "frjeff@schoenstatt.us"},
		{"a@b.co", "a@b.co"},
		{"first.last+tag@sub.example.org", "first.last+tag@sub.example.org"},

		// A real surname. Refusing this turns away a person rather than an
		// attack.
		{"o'neill@example.org", "o'neill@example.org"},

		// Normalised to lower case, so that one person cannot end up with two
		// accounts and two sets of per-form roles.
		{"FrJeff@Schoenstatt.US", "frjeff@schoenstatt.us"},
		{"UPPER@EXAMPLE.ORG", "upper@example.org"},

		// Trimmed, because an address pasted out of a mail client or a
		// spreadsheet arrives with a space on it constantly and there is
		// nothing to be gained from refusing one.
		{"  frjeff@schoenstatt.us  ", "frjeff@schoenstatt.us"},
		{"\tfrjeff@schoenstatt.us\n", "frjeff@schoenstatt.us"},

		{"a-b@c-d.example.org", "a-b@c-d.example.org"},
		{"x@a.b.c.d.example.org", "x@a.b.c.d.example.org"},

		// Punycode, which is how an internationalised domain has to arrive.
		{"jose@xn--caf-dma.example", "jose@xn--caf-dma.example"},

		{strings.Repeat("a", 64) + "@example.org", strings.Repeat("a", 64) + "@example.org"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := types.ParseEmail(tt.in)
			if err != nil {
				t.Fatalf("ParseEmail(%q): %v", tt.in, err)
			}
			if got.String() != tt.want {
				t.Errorf("ParseEmail(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if got.Zero() {
				t.Errorf("ParseEmail(%q) reports itself as the zero address", tt.in)
			}
		})
	}
}

func TestParseEmailRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"blank", ""},
		{"only spaces", "   "},
		{"no at sign", "frjeff"},
		{"nothing after the at", "frjeff@"},
		{"nothing before the at", "@schoenstatt.us"},
		{"only an at", "@"},
		{"two at signs", "a@b@c.org"},
		{"doubled at", "frjeff@@schoenstatt.us"},

		// A domain with no dot. Legal, never a real recipient here, and
		// almost always a dropped ".com".
		{"no dot in the domain", "frjeff@localhost"},
		{"no dot at all", "frjeff@schoenstattus"},

		{"leading dot in the domain", "frjeff@.example.org"},
		{"trailing dot in the domain", "frjeff@example.org."},
		{"doubled dot in the domain", "frjeff@example..org"},
		{"domain is only a dot", "frjeff@."},
		{"leading hyphen in the domain", "frjeff@-example.org"},
		{"trailing hyphen in the domain", "frjeff@example.org-"},

		// Whitespace inside.
		{"inner space", "fr jeff@schoenstatt.us"},
		{"space before the at", "frjeff @schoenstatt.us"},
		{"space in the domain", "frjeff@schoen statt.us"},
		{"inner newline", "frjeff@schoenstatt\n.us"},
		{"carriage return", "frjeff@schoenstatt.us\rBcc: someone@else.org"},

		// What net/mail.ParseAddress accepts and an "Email" field must not.
		{"a display name", "Jeff Roedel <frjeff@schoenstatt.us>"},
		{"a bracketed address", "<frjeff@schoenstatt.us>"},
		{"a quoted display name", `"Jeff Roedel" frjeff@schoenstatt.us`},
		{"two addresses, comma", "a@b.org, c@d.org"},
		{"two addresses, semicolon", "a@b.org; c@d.org"},
		{"a group syntax", "undisclosed:;"},

		// Identity, not syntax. Two addresses that look the same in a list
		// must not be two accounts.
		{"cyrillic homoglyph domain", "frjeff@schоnstatt.us"},
		{"accented domain", "jose@café.example"},
		{"emoji domain", "a@\U0001f600.example"},

		{"too long overall", strings.Repeat("a", 250) + "@example.org"},
		{"local part too long", strings.Repeat("a", 65) + "@example.org"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := types.ParseEmail(tt.in)
			if err == nil {
				t.Fatalf("ParseEmail(%q) = %q, want an error", tt.in, got)
			}
			if !errors.Is(err, types.ErrNotAnEmail) {
				t.Errorf("ParseEmail(%q) error does not wrap ErrNotAnEmail: %v", tt.in, err)
			}
			if !got.Zero() {
				t.Errorf("ParseEmail(%q) returned %q alongside its error", tt.in, got)
			}
		})
	}
}

// The property the whole type exists for: an address is a usable unique key.
// Two spellings of one person's address must parse to the same value, or one
// person ends up with two accounts and two sets of per-form roles, discovered
// as "my permissions disappeared".
func TestParseEmailIsAUsableKey(t *testing.T) {
	spellings := []string{
		"frjeff@schoenstatt.us",
		"FRJEFF@SCHOENSTATT.US",
		"FrJeff@Schoenstatt.Us",
		"  frjeff@SCHOENSTATT.us ",
	}

	var first types.Email

	for i, in := range spellings {
		got, err := types.ParseEmail(in)
		if err != nil {
			t.Fatalf("ParseEmail(%q): %v", in, err)
		}

		if i == 0 {
			first = got

			continue
		}

		if got != first {
			t.Errorf("ParseEmail(%q) = %q, but ParseEmail(%q) = %q; these must be one account",
				in, got, spellings[0], first)
		}
	}

	// Comparable, so it works as a map key without a String() call at every
	// use site.
	seen := map[types.Email]int{}
	for _, in := range spellings {
		e, _ := types.ParseEmail(in)
		seen[e]++
	}

	if len(seen) != 1 {
		t.Errorf("four spellings of one address made %d keys, want 1", len(seen))
	}
}

func TestEmailDomain(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want string
	}{
		{"frjeff@schoenstatt.us", "schoenstatt.us"},
		{"a@sub.example.ORG", "sub.example.org"},
		{"first.last+tag@example.org", "example.org"},
	} {
		t.Run(tt.in, func(t *testing.T) {
			e, err := types.ParseEmail(tt.in)
			if err != nil {
				t.Fatalf("ParseEmail: %v", err)
			}
			if got := e.Domain(); got != tt.want {
				t.Errorf("Domain() = %q, want %q", got, tt.want)
			}
		})
	}

	// The zero address has no domain, and asking must not panic -- a rate
	// limiter keyed on the domain will ask before it knows the address parsed.
	var zero types.Email
	if got := zero.Domain(); got != "" {
		t.Errorf("the zero address has domain %q, want nothing", got)
	}
}

// A header injection attempt must not survive parsing, because this address is
// interpolated into an SMTP envelope to send a sign-in link.
func TestParseEmailRefusesHeaderInjection(t *testing.T) {
	for _, in := range []string{
		"frjeff@schoenstatt.us\r\nBcc: someone@else.org",
		"frjeff@schoenstatt.us\nBcc: someone@else.org",
		"frjeff@schoenstatt.us\rSubject: Hello",
		"frjeff@schoenstatt.us%0d%0aBcc:someone@else.org",
		"\"frjeff@schoenstatt.us\"",
	} {
		t.Run(in, func(t *testing.T) {
			got, err := types.ParseEmail(in)
			if err != nil {
				return
			}

			// If something like this is ever accepted, it must at least carry
			// nothing that can break out of a header line.
			if strings.ContainsAny(got.String(), "\r\n\"") {
				t.Errorf("ParseEmail(%q) = %q, which can break an SMTP header", in, got)
			}
		})
	}
}

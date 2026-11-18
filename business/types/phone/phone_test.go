package phone_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/business/types/phone"
)

func TestTheSameNumberWrittenSixWaysIsOneNumber(t *testing.T) {
	// The property the whole type exists for: a number typed into a form, an
	// account page and the API must not be several recipients where there is
	// one person, because opting out is per number.
	written := []string{
		"+15550100123",
		"15550100123",
		"5550100123",
		"(555) 010-0123",
		"1 (555) 010-0123",
		"+1 555.010.0123",
		"  555 010 0123  ",
		"tel:+1-555-010-0123",
		"0015550100123",
	}

	const want = "+15550100123"

	for _, s := range written {
		got, err := phone.Parse(s)
		if err != nil {
			t.Errorf("Parse(%q): %v", s, err)

			continue
		}

		if got.String() != want {
			t.Errorf("Parse(%q) = %q, want %q", s, got.String(), want)
		}
	}
}

func TestRefusesWhatItCannotNormaliseHonestly(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"blank", "   "},
		{"no digits", "call me"},
		{"too short with a country code", "+1234"},
		{"too long", "+1234567890123456"},
		{"a UK number typed the way it is said", "07700 900123"},
		{"nine digits, so not North American", "555010012"},
		{"eleven digits not starting with 1", "25550100123"},
		{"a country code of zero", "+05550100123"},
		{"an extension nobody can dial from a message", "555-010-0123 x22"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := phone.Parse(c.in)
			if err == nil {
				t.Fatalf("Parse(%q) = %q, want an error", c.in, got)
			}

			if !errors.Is(err, phone.ErrInvalid) {
				t.Errorf("Parse(%q) error is %v, want it to wrap ErrInvalid", c.in, err)
			}
		})
	}
}

// An extension is the case worth its own note. "555-010-0123 x22" strips to
// twelve digits, which is neither ten nor eleven-starting-with-1, so it is
// refused rather than silently becoming a different subscriber's number.
func TestAnExtensionIsNotQuietlyAbsorbed(t *testing.T) {
	if _, err := phone.Parse("555-010-0123 x22"); err == nil {
		t.Fatal("an extension was accepted; it would have become another number")
	}
}

func TestInternationalNumbersSurviveWithTheirCountryCode(t *testing.T) {
	cases := map[string]string{
		"+44 7700 900123":  "+447700900123",
		"+49 30 901820":    "+4930901820",
		"0044 7700 900123": "+447700900123",
	}

	for in, want := range cases {
		got, err := phone.Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)

			continue
		}

		if got.String() != want {
			t.Errorf("Parse(%q) = %q, want %q", in, got.String(), want)
		}
	}
}

func TestNonAsciiDigitsAreRefusedRatherThanConverted(t *testing.T) {
	// Fullwidth digits. Accepting these would mean a number nobody typed.
	if _, err := phone.Parse("５５５０１００１２３"); err == nil {
		t.Fatal("fullwidth digits were accepted")
	}
}

func TestTheZeroValueIsInvalidAndSaysSo(t *testing.T) {
	var n phone.Number

	if !n.IsZero() {
		t.Error("the zero value does not report itself as zero")
	}

	if n.String() != "" {
		t.Errorf("zero String() = %q, want empty", n.String())
	}

	if n.National() != "" {
		t.Errorf("zero National() = %q, want empty", n.National())
	}

	if n.Redacted() != "(none)" {
		t.Errorf("zero Redacted() = %q, want %q", n.Redacted(), "(none)")
	}
}

func TestRedactedKeepsFourDigitsAndLosesTheRest(t *testing.T) {
	n := phone.MustParse("+15550100123")

	got := n.Redacted()

	if !strings.HasSuffix(got, "0123") {
		t.Errorf("Redacted() = %q, want it to end in the last four digits", got)
	}

	// The part that matters: what is left must not be enough to dial. Every
	// digit but the last four has to be gone, not merely styled.
	for _, digit := range []string{"555", "5550", "01001", "1555"} {
		if strings.Contains(got, digit) {
			t.Errorf("Redacted() = %q, but it still contains %q", got, digit)
		}
	}

	if strings.Contains(got, n.String()) {
		t.Errorf("Redacted() = %q, which contains the whole number", got)
	}
}

func TestNationalReadsLikeSomebodySayingIt(t *testing.T) {
	if got := phone.MustParse("+15550100123").National(); got != "(555) 010-0123" {
		t.Errorf("National() = %q, want %q", got, "(555) 010-0123")
	}
}

func TestNationalLeavesANumberItCannotGroupAlone(t *testing.T) {
	// Inventing a grouping for a country whose conventions this package does
	// not know would be harder to check against a contacts app than the digits.
	n := phone.MustParse("+447700900123")

	if got := n.National(); got != n.String() {
		t.Errorf("National() = %q, want the E.164 form %q", got, n.String())
	}
}

func TestMustParsePanicsRatherThanReturningNothing(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustParse did not panic on an invalid number")
		}
	}()

	phone.MustParse("not a number")
}

func TestNumbersAreComparable(t *testing.T) {
	// Used as a map key throughout the comms domain, which needs the type to
	// be comparable and equal for equal numbers however they were written.
	a := phone.MustParse("(555) 010-0123")
	b := phone.MustParse("+1 555 010 0123")

	if a != b {
		t.Errorf("%v and %v are the same number but compare unequal", a, b)
	}

	seen := map[phone.Number]int{a: 1}
	if seen[b] != 1 {
		t.Error("the same number written two ways missed in a map lookup")
	}
}

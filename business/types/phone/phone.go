// Package phone is the strong type for a mobile number a text message can be
// sent to, held in E.164 form (for example "+15550100123").
//
// Copied from eumaeus, which sends text messages through the same Twilio
// account; the rules below are its, and they are kept identical on purpose so
// that one person's number normalises the same way in both.
//
// One canonical form, decided at the door. A number arrives from a form, an
// account page, the API and an agent, and every one of those spells it
// differently: "(555) 010-0123", "555-010-0123", "1 555 010 0123",
// "+1 555.010.0123". Left as typed they are several recipients rather than
// one, and the consequences are not cosmetic:
//
//   - Opting out is per number. Somebody who replies STOP from
//     "+15550100123" has opted out as that number, and the provider's refusal
//     only matches if this service asks it about the same spelling.
//   - The provider is addressed in E.164 and nothing else, so a number that is
//     not normalised before it is used is normalised at every use instead --
//     which is the same conversion written in more places, each free to differ.
//   - A duplicate recipient is a duplicate message, and the person receiving
//     two copies has no way to tell you which one to stop.
//
// # The one assumption, stated plainly
//
// A number with no country code is read as North American (+1). That is a
// policy this type should not really hold, and it holds it because the
// alternative -- refusing every number a person types the way they say it out
// loud -- makes the form refuse most of the people filling it in.
//
// It is deliberately narrow. Ten digits, or eleven beginning with 1, become
// +1; anything else must arrive with a leading "+" and its country code. So a
// UK number typed as "07700 900123" is REFUSED rather than mangled into
// "+10770090012", because a wrong number that looks right is worse than an
// error message. When this installation gains a second country, the default
// belongs in configuration and [Parse] should take it as an argument; until
// then a constant that is wrong for nobody here is better than a setting
// nobody sets.
//
// # Numbers are identifying, and this type says so
//
// A phone number is the person. It goes in logs, in error messages and in
// support conversations, and once it is in a log file it outlives the request
// that carried it. [Number.Redacted] is what belongs in all three, and
// [Number.String] is for the provider, the database and the page the owner is
// looking at.
package phone

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalid reports anything that is not a phone number this program will
// send to.
//
// One error rather than several, and returned with a sentence rather than a
// classification, because every caller does the same thing with it: show it to
// whoever typed the number. The sentence is written for that person.
var ErrInvalid = errors.New("phone: not a phone number this can send to")

// E.164 allows at most fifteen digits including the country code, and no
// assignment is shorter than seven. Both bounds are the specification's, not a
// guess: they are what tells a typo from a number.
const (
	minDigits = 7
	maxDigits = 15
)

// defaultCountry is prefixed to a number typed without one. See the package
// comment for why this is a constant and when it should stop being one.
const defaultCountry = "1"

// nanpDigits is the length of a North American number without its country
// code.
const nanpDigits = 10

// Number is a validated phone number in E.164 form, including the leading "+".
//
// The zero value is invalid, so a recipient whose number was never parsed
// fails a check rather than being quietly sent to nowhere -- or, worse, to
// whichever number an empty string happens to mean to a provider.
type Number struct {
	value string
}

// Parse normalises a number as a person wrote it.
//
// Everything that is not a digit or a leading "+" is discarded first:
// brackets, spaces, hyphens, dots and the "tel:" a copied hyperlink brings
// with it. What is left has to be a plausible E.164 number, or this fails.
//
// A leading "00" is read as the international prefix and becomes "+", because
// that is how the rest of the world writes what North America writes as "011"
// -- and a number pasted from a European contacts app should not be refused
// for spelling the same thing differently.
func Parse(s string) (Number, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return Number{}, fmt.Errorf("%w: it is empty", ErrInvalid)
	}

	// Recorded before the digits are extracted, because extraction is what
	// destroys the distinction between "+15550100123" and "15550100123" -- and
	// that distinction is the whole of what says whether a country code is
	// present.
	plus := strings.HasPrefix(trimmed, "+")

	digits := onlyDigits(trimmed)

	if !plus && strings.HasPrefix(digits, "00") {
		plus, digits = true, digits[2:]
	}

	switch {
	case digits == "":
		return Number{}, fmt.Errorf("%w: %q has no digits in it", ErrInvalid, s)
	case len(digits) > maxDigits:
		return Number{}, fmt.Errorf(
			"%w: %q has %d digits and the most any number has is %d",
			ErrInvalid, s, len(digits), maxDigits)
	}

	if !plus {
		var err error

		digits, err = assumeDefaultCountry(s, digits)
		if err != nil {
			return Number{}, err
		}
	}

	if len(digits) < minDigits {
		return Number{}, fmt.Errorf(
			"%w: %q has %d digits and the shortest number has %d",
			ErrInvalid, s, len(digits), minDigits)
	}

	// A country code never begins with zero, so a "+0…" is a number that has
	// lost its country code rather than one that has an unusual one.
	if digits[0] == '0' {
		return Number{}, fmt.Errorf(
			"%w: %q begins with a zero, so its country code is missing", ErrInvalid, s)
	}

	return Number{value: "+" + digits}, nil
}

// assumeDefaultCountry applies the one assumption this package makes.
//
// Only the two lengths that can mean a North American number are accepted.
// Everything else is refused and told to say which country it is in, because
// the alternative is prefixing "+1" onto a number from somewhere else and
// producing a valid-looking number belonging to a stranger.
func assumeDefaultCountry(original, digits string) (string, error) {
	switch {
	case len(digits) == nanpDigits:
		return defaultCountry + digits, nil
	case len(digits) == nanpDigits+1 && strings.HasPrefix(digits, defaultCountry):
		return digits, nil
	default:
		return "", fmt.Errorf(
			"%w: %q has no country code, and only a %d-digit North American number "+
				"can be assumed to have one. Write it as +<country code><number>",
			ErrInvalid, original, nanpDigits)
	}
}

// MustParse parses s and panics on failure; for tests and known-good
// constants.
func MustParse(s string) Number {
	n, err := Parse(s)
	if err != nil {
		panic(err)
	}

	return n
}

// String returns the E.164 number, or "" for the zero value.
//
// This is what goes to the provider, into the database and onto a page the
// person it belongs to is reading. It is NOT what goes into a log -- see
// [Number.Redacted].
func (n Number) String() string { return n.value }

// IsZero reports whether n is the unset zero value.
func (n Number) IsZero() bool { return n.value == "" }

// Redacted renders the number for a log line, an error message, or any page
// shown to somebody who is not its owner: the country code, then dots, then
// the last four digits.
//
// The last four are kept because they are what lets a person confirm which of
// their numbers is meant without the line being enough to dial. That is the
// same trade every bank statement makes with a card number, and for the same
// reason.
//
// A zero Number renders as "(none)" rather than as an empty string, so a log
// line reporting one is legible instead of ending in a colon.
func (n Number) Redacted() string {
	if n.IsZero() {
		return "(none)"
	}

	digits := n.value[1:]

	const keep = 4

	if len(digits) <= keep {
		return "+" + strings.Repeat("•", len(digits))
	}

	// Everything but the last four, as dots. The country code is not separated
	// out: telling a one-digit code from a three-digit one needs a table this
	// package has no business carrying, and the dots read the same either way.
	return "+" + strings.Repeat("•", len(digits)-keep) + digits[len(digits)-keep:]
}

// National renders the number the way somebody here would read it aloud, for a
// page shown to the person whose number it is, or to an organiser who needs to dial it.
//
// North American numbers only; anything else is returned in E.164, which is
// how it was typed and how it will be dialled. Formatting a number whose
// grouping this package does not know would be inventing one, and an invented
// grouping is harder to check against a contacts app than the raw digits.
func (n Number) National() string {
	if n.IsZero() {
		return ""
	}

	digits := n.value[1:]

	if len(digits) != nanpDigits+1 || !strings.HasPrefix(digits, defaultCountry) {
		return n.value
	}

	d := digits[1:]

	return "(" + d[0:3] + ") " + d[3:6] + "-" + d[6:]
}

// onlyDigits keeps the ASCII digits and discards everything else.
//
// Deliberately not unicode.IsDigit: that accepts Arabic-Indic and fullwidth
// digits, which would be silently converted here into a number nobody typed.
// A number pasted with unusual digits should be refused, and refusing it is
// what happens when they are discarded and too few are left.
func onlyDigits(s string) string {
	var b strings.Builder

	b.Grow(len(s))

	for i := range len(s) {
		if c := s[i]; c >= '0' && c <= '9' {
			b.WriteByte(c)
		}
	}

	return b.String()
}

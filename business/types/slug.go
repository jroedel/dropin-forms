package types

import (
	"errors"
	"fmt"
	"strings"
)

// Slug names one form. It is the last segment of the embed URL, the value in
// the pasted snippet's data attribute, the stem of the definition's filename,
// and part of what a submission grant is signed over -- so it lands in a path,
// in HTML, in a filesystem and in a MAC, and the character set here is the
// intersection of what all four can take without escaping.
//
// The narrowness is the feature. A slug that cannot contain a dot, a slash or
// a percent sign cannot be a path traversal when it is joined to a directory
// of definitions, and one that cannot contain a quote or an angle bracket
// cannot escape the attribute it is rendered into even if somebody forgets
// that html/template already handles that.
type Slug struct {
	s string
}

// ErrNotASlug is what every parse failure wraps.
var ErrNotASlug = errors.New("not a form name")

// A slug is bounded at both ends. Two characters is enough to be meaningful;
// 64 keeps it inside every filesystem's per-component limit with room for the
// extension the definition store appends.
const (
	slugMinLen = 2
	slugMaxLen = 64
)

// ParseSlug reads a form name: lowercase ASCII letters, digits and single
// hyphens, beginning and ending with a letter or digit.
//
// Case is rejected rather than folded. A URL path is case-sensitive, so
// accepting "Feast-Lunch" and storing "feast-lunch" would mean the snippet
// somebody pasted and the form they created differ by a character nobody can
// see, and the failure arrives as an empty box on a live page.
func ParseSlug(s string) (Slug, error) {
	switch {
	case s == "":
		return Slug{}, fmt.Errorf("%w: it is blank", ErrNotASlug)
	case len(s) < slugMinLen:
		return Slug{}, fmt.Errorf("%w: %q is too short; use at least %d characters", ErrNotASlug, s, slugMinLen)
	case len(s) > slugMaxLen:
		return Slug{}, fmt.Errorf("%w: it is longer than %d characters", ErrNotASlug, slugMaxLen)
	}

	for i := range len(s) {
		c := s[i]

		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			continue
		case c == '-':
			// Not at either end, and not doubled. Both rules exist so that one
			// form cannot have two names that differ only in a way a person
			// reading a list would never notice.
			if i == 0 || i == len(s)-1 {
				return Slug{}, fmt.Errorf("%w: %q begins or ends with a hyphen", ErrNotASlug, s)
			}
			if s[i-1] == '-' {
				return Slug{}, fmt.Errorf("%w: %q has two hyphens in a row", ErrNotASlug, s)
			}
		case c >= 'A' && c <= 'Z':
			return Slug{}, fmt.Errorf("%w: %q has a capital letter; use %q", ErrNotASlug, s, strings.ToLower(s))
		default:
			return Slug{}, fmt.Errorf("%w: %q may only contain lowercase letters, digits and hyphens", ErrNotASlug, s)
		}
	}

	return Slug{s: s}, nil
}

// String is the slug as it appears in a path and in the snippet.
func (s Slug) String() string { return s.s }

// Zero reports whether this is the unset Slug. A Slug that came from
// ParseSlug is never zero, so this distinguishes "no form" from "some form".
func (s Slug) Zero() bool { return s.s == "" }

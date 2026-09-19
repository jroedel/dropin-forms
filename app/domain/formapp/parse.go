package formapp

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// This file is the whole of the app layer's job on the way in: strings out of
// a request, turned into the values the domain is written in, with a sentence
// for each way they can be wrong. Nothing here decides anything about a form
// -- [formbus.Form.Check] does that, over the assembled definition, once.

// value is one trimmed form value.
//
// Trimmed everywhere, without exception. A label with a trailing space is a
// label somebody cannot see is wrong, and an option value with one is refused
// by Check with a message about what a browser will not send back unchanged.
func value(r *http.Request, key string) string {
	return strings.TrimSpace(r.PostFormValue(key))
}

// checked reports whether a checkbox was ticked. An unticked box sends
// nothing, which is the whole of the encoding.
func checked(r *http.Request, key string) bool {
	return r.PostFormValue(key) != ""
}

// lines splits a textarea into its non-empty, trimmed lines.
//
// A textarea rather than a repeating group of inputs is this builder's answer
// to a list, and it is a consequence of having no JavaScript: adding a row to
// a repeating group means either a round trip per option or a script. One box,
// one thing per line, is the shape that needs neither and that somebody can
// paste into.
func lines(raw string) []string {
	var out []string

	for line := range strings.SplitSeq(raw, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}

	return out
}

// parseOptions reads a choice field's options, one per line, as either
//
//	value | what the person reads
//	both at once
//
// Value and label are separate on [formbus.Option] because renaming what
// somebody reads must not rewrite the answers already collected -- so a line
// with no bar gets the same string for both, which is the right default and is
// also what makes the simple case a list of words.
func parseOptions(raw string) []formbus.Option {
	var out []formbus.Option

	for _, line := range lines(raw) {
		v, label, split := strings.Cut(line, "|")

		v = strings.TrimSpace(v)
		label = strings.TrimSpace(label)

		if !split || label == "" {
			label = v
		}

		out = append(out, formbus.Option{Value: v, Label: label})
	}

	return out
}

// writeOptions is parseOptions backwards, for putting a field back into the
// box it was typed in. A pair that differs keeps the bar; one that does not
// loses it, so a list somebody typed as words comes back as words.
func writeOptions(options []formbus.Option) string {
	var b strings.Builder

	for _, o := range options {
		b.WriteString(o.Value)

		if o.Label != o.Value {
			b.WriteString(" | ")
			b.WriteString(o.Label)
		}

		b.WriteString("\n")
	}

	return b.String()
}

// parseCount reads a whole number where empty means none.
func parseCount(label, raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}

	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s has to be a whole number, or be left empty", label)
	}

	return n, nil
}

// parseMoney reads an amount where empty means none.
func parseMoney(label, raw string) (types.Money, error) {
	if raw == "" {
		return 0, nil
	}

	m, err := types.ParseMoney(raw)
	if err != nil {
		return 0, fmt.Errorf("%s has to be an amount like 12.00", label)
	}

	return m, nil
}

// parseBound reads one end of a field's Min/Max, where empty means unbounded.
//
// A pointer, because for a donation with a floor no minimum and a minimum of
// nothing are different rules -- [formbus.Field.Min] says so, and that
// distinction is the only reason this cannot return an int.
//
// money picks the unit, which follows the kind exactly as the domain type
// says: on an amount field the person types 5.00 and the bound is 500, and on
// a number field 5 is 5.
func parseBound(label, raw string, money bool) (*int64, error) {
	if raw == "" {
		return nil, nil
	}

	if money {
		m, err := parseMoney(label, raw)
		if err != nil {
			return nil, err
		}

		n := int64(m)

		return &n, nil
	}

	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s has to be a whole number, or be left empty", label)
	}

	return &n, nil
}

// writeBound is parseBound backwards.
func writeBound(b *int64, money bool) string {
	if b == nil {
		return ""
	}

	if money {
		return types.Money(*b).String()
	}

	return strconv.FormatInt(*b, 10)
}

// instantLayout is what a datetime-local input sends: a wall-clock time with
// no offset on it at all.
const instantLayout = "2006-01-02T15:04"

// parseWhen turns what the browser's date picker sends into an instant.
//
// This is the one place this app has to be careful, and formtoml's parseInstant
// explains why at length: "closes on the 17th at midnight" is ambiguous about
// which midnight, and resolving it in whatever zone the process happens to run
// in is the classic version of this bug. A file is made to write the offset out
// in full, which is right for a file and is a hostile thing to ask of somebody
// choosing a date in a browser.
//
// So the zone is named instead of guessed. The wall time is read in the zone
// this service runs in, which resolves the offset -- including across a
// daylight-saving change, which ParseInLocation gets right and a fixed offset
// would not -- and the page says which zone that is, right beside the input.
// The stored value is an instant, exactly as it is from a file.
func parseWhen(label, raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}

	// Some browsers include seconds when the value has them. Accepted rather
	// than refused, since the alternative is a date a person picked being
	// rejected by the site that offered the picker.
	for _, layout := range []string{instantLayout, instantLayout + ":05"} {
		if t, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("%s has to be a date and a time", label)
}

// writeWhen is parseWhen backwards, in the same zone.
func writeWhen(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return t.In(time.Local).Format(instantLayout)
}

// zoneName is what the settings page puts beside its two date inputs, so that
// nobody has to guess which midnight they just chose. Both halves: the IANA
// name, which is the unambiguous one, and today's abbreviation, which is the
// one somebody recognises.
func zoneName(now time.Time) string {
	abbr, _ := now.In(time.Local).Zone()

	if name := time.Local.String(); name != "" && name != abbr {
		return abbr + " (" + name + ")"
	}

	return abbr
}

// parseOrigins reads the list of sites permitted to frame a form.
//
// An empty list is honoured as written and means nobody may frame it, which
// [formbus.Form.Origins] is explicit is the fail-closed default and not an
// error. The page says so, because "leave it blank for the installation's
// list" and "leave it blank for nobody" are opposite behaviours and this one
// is the second.
func parseOrigins(raw string) ([]types.Origin, error) {
	var out []types.Origin

	for _, line := range lines(raw) {
		o, err := types.ParseOrigin(line)
		if err != nil {
			return nil, fmt.Errorf("%q is not a site this form could be embedded on: %w", line, err)
		}

		out = append(out, o)
	}

	return out, nil
}

// writeOrigins is parseOrigins backwards.
func writeOrigins(origins []types.Origin) string {
	var b strings.Builder

	for _, o := range origins {
		b.WriteString(o.String())
		b.WriteString("\n")
	}

	return b.String()
}

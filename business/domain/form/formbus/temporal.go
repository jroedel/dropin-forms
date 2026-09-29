package formbus

import (
	"fmt"
	"strings"
	"time"
)

// The three temporal kinds -- a date, a time of day, and the two together.
//
// # What is stored is a wall clock, not an instant
//
// A datetime answer is "17 October at half past three" as the person read it
// off the page, and it is stored exactly that way, with no offset. That is the
// opposite of what [Form.OpensAt] does, and deliberately so. An opening time is
// a rule this service enforces, so it has to be an instant. An answer is a
// fact somebody told us, and the zone it is in is the zone of whatever the
// question was about -- the parish hall, the pick-up, the Mass -- which the
// question's own label supplies and this service cannot know. Converting it
// with the zone this process happens to run in would be inventing an offset
// the person never gave, which is the bug the OpensAt comment describes, moved
// into somebody's answer.
//
// # One stored shape per kind
//
// Each kind is kept in one canonical form -- 2026-10-17, 15:30,
// 2026-10-17T15:30 -- whatever arrived. Those are what the browser's own date,
// time and datetime-local inputs send, so for nearly everybody normalising
// changes nothing. It matters for the rest: a browser that draws a plain text
// box instead, and a time sent with seconds. One shape is what lets a CSV of
// these sort, and what lets a condition on one of these fields compare by
// string equality the way every other condition does.
//
// A time typed as 3:30pm is accepted, because that is how people write a time
// in the text box an old browser shows them. A date typed as 10/17/2026 is not:
// it is 17 October in Austin and nothing at all in Aachen, and guessing which
// is a wrong answer stored with confidence. The sentence says what to type.
//
// # Earliest and latest are their own pair of fields
//
// These kinds are not [Kind.Bounded]. A Min and Max for them would be a date,
// not a number, and Field.Min is an int64 whose unit already changes by kind;
// a fourth unit in that one field is where the confusion starts. So the range
// is [Field.Earliest] and [Field.Latest], written in the field's own stored
// shape -- and because every stored shape is fixed-width and most significant
// first, a range check is a string comparison, with no parsing and no zone.
type temporal struct {
	// store is the canonical layout, and the first thing tried on the way in.
	store string

	// accept is every other layout taken on the way in.
	accept []string

	// example is what the sentence shows when nothing matched.
	example string

	// readable is how a person is shown it, on a page or in a message.
	readable string

	noun string
}

var temporals = map[Kind]temporal{
	KindDate: {
		store:    "2006-01-02",
		example:  "2026-10-17",
		readable: "Monday 2 January 2006",
		noun:     "a date",
	},
	KindTime: {
		store:    "15:04",
		accept:   []string{"15:04:05", "3:04pm", "3:04PM", "3:04 pm", "3:04 PM"},
		example:  "15:30, or 3:30pm",
		readable: "3:04pm",
		noun:     "a time",
	},
	KindDateTime: {
		store:    "2006-01-02T15:04",
		accept:   []string{"2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02 15:04:05"},
		example:  "2026-10-17 15:30",
		readable: "Monday 2 January 2006, 3:04pm",
		noun:     "a date and time",
	},
}

// Temporal reports whether the kind collects a date, a time, or both.
func (k Kind) Temporal() bool {
	_, ok := temporals[k]

	return ok
}

// parse reads a value in any layout this kind accepts. The time.Time it
// returns carries no zone that means anything -- see the comment above -- and
// is only ever formatted back into a layout that shows none.
func (t temporal) parse(s string) (time.Time, bool) {
	for _, layout := range append([]string{t.store}, t.accept...) {
		if at, err := time.Parse(layout, s); err == nil {
			return at, true
		}
	}

	return time.Time{}, false
}

// canonical is the stored shape of a value, and whether it could be read.
func (k Kind) canonical(s string) (string, bool) {
	t, ok := temporals[k]
	if !ok {
		return "", false
	}

	at, ok := t.parse(s)
	if !ok {
		return "", false
	}

	return at.Format(t.store), true
}

// takeTemporal checks and normalises a temporal answer, returning the stored
// value or why it cannot be read.
func (fld Field) takeTemporal(s string) (string, []Violation) {
	canon, ok := fld.Kind.canonical(s)
	if !ok {
		t := temporals[fld.Kind]

		return "", []Violation{{
			Field:   fld.Name,
			Message: fmt.Sprintf("%s needs %s, written like %s.", fld.Label, t.noun, t.example),
		}}
	}

	return canon, nil
}

// Readable is the answer the way a person reads it: every value joined, and a
// date or time written out in words rather than in the stored shape.
//
// For a page or a message, never for a CSV. A spreadsheet wants the stored
// shape, which sorts, and gets it from Values directly.
func (a Answer) Readable() string {
	t, ok := temporals[a.Kind]
	if !ok {
		return strings.Join(a.Values, ", ")
	}

	out := make([]string, 0, len(a.Values))

	for _, v := range a.Values {
		// A value stored before a release that changed the canonical shape,
		// or one written into the database by hand, is shown as it stands
		// rather than dropped.
		if at, ok := t.parse(v); ok {
			v = at.Format(t.readable)
		}

		out = append(out, v)
	}

	return strings.Join(out, ", ")
}

// dateOnly is the stored shape of a day, which is also the one other shape a
// datetime field's range may be written in.
const dateOnly = "2006-01-02"

// CanonicalBound is an earliest or latest value in the shape this kind stores
// its range in, and whether it could be read at all. It accepts what a person
// types -- 3:30pm, a space rather than a T -- so that the builder can hand it
// whatever was in the box.
//
// A datetime field's range may also be a day on its own, and that is kept as
// a day rather than turned into midnight, because "until the 13th" means the
// whole of the 13th and a latest of 2026-10-13T00:00 would mean none of it.
// Which end of the day it stands for is decided where it is compared.
func (k Kind) CanonicalBound(s string) (string, bool) {
	if k == KindDateTime {
		if day, err := time.Parse(dateOnly, s); err == nil {
			return day.Format(dateOnly), true
		}
	}

	return k.canonical(s)
}

// dayBound reports whether a datetime field's bound is a day on its own.
func (k Kind) dayBound(b string) bool {
	return k == KindDateTime && len(b) == len(dateOnly)
}

// span is a bound as an instant of the field's own stored shape, so that it
// can be compared with an answer as a string and handed to the browser as a
// min or max. A day standing for a datetime's latest is the last minute of
// it: answers have no seconds, so 23:59 is the whole day and nothing past it.
func (k Kind) span(b string, latest bool) string {
	switch {
	case b == "" || !k.dayBound(b):
		return b
	case latest:
		return b + "T23:59"
	default:
		return b + "T00:00"
	}
}

// Span is a field's range as the two values an input's min and max take: a
// day standing for a datetime's end is widened to the whole of that day, and
// an open end is "". For the app layer, which renders the picker and must not
// know that a day can stand for a datetime.
func (k Kind) Span(earliest, latest string) (string, string) {
	return k.span(earliest, false), k.span(latest, true)
}

// readableBound writes a bound out, a day as a day even on a datetime field.
func (k Kind) readableBound(b string) string {
	kind := k
	if k.dayBound(b) {
		kind = KindDate
	}

	return Answer{Kind: kind, Values: []string{b}}.Readable()
}

// RangeNote is the field's range as a sentence, for beside the field, or ""
// when it has none. Stated rather than left to the picker, because a browser
// that draws a text box shows no range at all, and a refusal that arrives
// only after the form is sent is one nobody could have avoided.
func (fld Field) RangeNote() string {
	r := fld.rangePhrase()
	if r == "" {
		return ""
	}

	return strings.ToUpper(r[:1]) + r[1:] + "."
}

// rangePhrase is the range in words, starting lower case so it can follow
// "has to be". On a day, and at a time: "on or after 9:00am" is not English.
func (fld Field) rangePhrase() string {
	k := fld.Kind
	on := func(b string) string {
		if k == KindDate || k.dayBound(b) {
			return "on"
		}

		return "at"
	}

	switch first, last := fld.Earliest, fld.Latest; {
	case first != "" && last != "":
		return fmt.Sprintf("between %s and %s", k.readableBound(first), k.readableBound(last))
	case first != "":
		return fmt.Sprintf("%s or after %s", on(first), k.readableBound(first))
	case last != "":
		return fmt.Sprintf("%s or before %s", on(last), k.readableBound(last))
	}

	return ""
}

// checkRange is the answer against the field's range. The answer is already
// in its stored shape, which is what makes this a comparison of strings.
func (fld Field) checkRange(canon string) []Violation {
	k := fld.Kind

	if (fld.Earliest != "" && canon < k.span(fld.Earliest, false)) ||
		(fld.Latest != "" && canon > k.span(fld.Latest, true)) {
		return []Violation{{
			Field:   fld.Name,
			Message: fmt.Sprintf("%s has to be %s.", fld.Label, fld.rangePhrase()),
		}}
	}

	return nil
}

// checkLimits is Check's part for a field's range: only on a kind that has
// one, only in its stored shape, and not the wrong way round.
func (fld *Field) checkLimits(where string) []string {
	if fld.Earliest == "" && fld.Latest == "" {
		return nil
	}

	var p []string
	add := func(format string, args ...any) {
		p = append(p, where+": "+fmt.Sprintf(format, args...))
	}

	if !fld.Kind.Temporal() {
		add("an earliest or latest applies only to a date or a time, and this is %s", fld.Kind)

		return p
	}

	example := temporals[fld.Kind].example
	if fld.Kind == KindDateTime {
		example += ", or a day on its own like 2026-10-17"
	}

	for _, end := range []struct{ label, b string }{{"earliest", fld.Earliest}, {"latest", fld.Latest}} {
		label, b := end.label, end.b
		if b == "" {
			continue
		}

		// The stored shape exactly, not merely readable: the comparison is a
		// string comparison, and 3:30pm compared with 09:00 as strings is a
		// range nobody meant.
		if canon, ok := fld.Kind.CanonicalBound(b); !ok || canon != b {
			add("its %s %q is not written the way this field stores it, like %s", label, b, example)
		}
	}

	if len(p) == 0 && fld.Earliest != "" && fld.Latest != "" &&
		fld.Kind.span(fld.Earliest, false) > fld.Kind.span(fld.Latest, true) {
		add("its earliest %s is after its latest %s, so nothing could be chosen", fld.Earliest, fld.Latest)
	}

	return p
}

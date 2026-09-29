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
// # No bounds, yet
//
// These kinds are not [Kind.Bounded]. A Min and Max for them would be a date,
// not a number, and Field.Min is an int64 whose unit already changes by kind;
// a fourth unit in that one field is where the confusion starts. When an
// earliest or latest date is needed it wants its own pair of fields.
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

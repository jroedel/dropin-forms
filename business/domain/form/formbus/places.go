package formbus

import (
	"maps"
	"slices"
)

// Taken is how many places each limited option has given out: field name, then
// option value, then the count.
//
// It is counted from the submissions that hold a place, and which those are is
// a rule about submissions rather than about forms, so this package only
// reads the numbers. A field or value absent from the map has given out none.
//
// The form's own places, on a form with a number of them, are counted under
// the empty field and the empty value. No question can be named "" -- Check
// refuses it -- so the whole form has the one key no option can collide with.
type Taken map[string]map[string]int

// Count is how many places one option has given out.
func (t Taken) Count(field, value string) int {
	return t[field][value]
}

// Full reports whether an option with this limit has no place left. An option
// with no limit is never full.
func (t Taken) Full(field, value string, limit int) bool {
	return limit > 0 && t.Count(field, value) >= limit
}

// Without is the count as one person sees it when they come back to change
// their answers: the places they already hold are given back to them, so that
// their own time slot is not shown as taken by somebody else -- and their own
// place on a full form does not make the form full to them.
//
// Only for a submission that holds its places; the caller knows which those
// are and this package does not.
//
// A copy. The receiver is left as it was, because the same count may be drawn
// for more than one page.
func (t Taken) Without(a Answers) Taken {
	out := make(Taken, len(t))
	for field, counts := range t {
		out[field] = maps.Clone(counts)
	}

	if whole := out[""]; whole[""] > 0 {
		whole[""]--
	}

	for _, ans := range a.Fields {
		counts, ok := out[ans.Name]
		if !ok {
			continue
		}

		for _, v := range ans.Values {
			if counts[v] > 0 {
				counts[v]--
			}
		}
	}

	return out
}

// Claim is one limited place that a submission takes: the option it chose,
// and how many may hold that option at once.
type Claim struct {
	Field string
	Value string
	Limit int
}

// Limited reports whether any option on the form has a limit, or the form a
// number of places -- and therefore whether drawing the form has to count
// anything. Every form written before limits existed answers no, and is drawn
// without a query.
func (f Form) Limited() bool {
	if f.Capacity > 0 {
		return true
	}

	for _, fld := range f.Fields {
		for _, opt := range fld.Options {
			if opt.Limit > 0 {
				return true
			}
		}
	}

	return false
}

// Claims is the limited places these answers take, in the definition's order.
//
// Read from accepted answers rather than from the request, so a field hidden
// by its condition -- which Answers leaves out -- claims nothing, and neither
// does an option with no limit. The store checks each one against what is
// already held in the same transaction that saves the submission.
//
// A form with a number of places claims one of them first, as the [Claim] with
// no field and no value -- the place the submission itself takes. First, so
// that a form both full and out of the chosen slot is reported as full: that is
// the answer that decides whether somebody goes on the waiting list.
func (f Form) Claims(a Answers) []Claim {
	var out []Claim

	if f.Capacity > 0 {
		out = append(out, Claim{Limit: f.Capacity})
	}

	for _, fld := range f.Fields {
		ans, ok := a.Field(fld.Name)
		if !ok {
			continue
		}

		for _, opt := range fld.Options {
			if opt.Limit > 0 && slices.Contains(ans.Values, opt.Value) {
				out = append(out, Claim{Field: fld.Name, Value: opt.Value, Limit: opt.Limit})
			}
		}
	}

	return out
}

// Full reports whether every one of the form's places is held, by the count a
// store keeps under the empty field and value. Always false on a form with no
// number of places.
func (f Form) Full(t Taken) bool {
	return f.Capacity > 0 && t.Full("", "", f.Capacity)
}

// Held is how many of the form's places are held, from the same count.
func (t Taken) Held() int { return t.Count("", "") }

// Exhausted reports whether nobody can submit this form any more because a
// question they must answer has nothing left to choose: a required radio or
// dropdown, always shown, every one of whose options has a limit and is full.
//
// Deliberately narrow. A question behind a condition, or in a section behind
// one, might not be asked of the next person, so its being full closes
// nothing; and a set of checkboxes asks for a number of boxes that its own
// bounds decide, which is not worth second-guessing here. In those cases the
// full options are shown taken and the form stays open, which is the honest
// state -- somebody may still be able to submit it.
func (f Form) Exhausted(t Taken) bool {
	for _, fld := range f.Fields {
		if !fld.Required || fld.ShowIf != nil {
			continue
		}

		if fld.Kind != KindRadio && fld.Kind != KindSelect {
			continue
		}

		if sec, ok := f.SectionOf(fld.Name); ok && sec.ShowIf != nil {
			continue
		}

		if len(fld.Options) > 0 && allFull(fld, t) {
			return true
		}
	}

	return false
}

func allFull(fld Field, t Taken) bool {
	for _, opt := range fld.Options {
		if !t.Full(fld.Name, opt.Value, opt.Limit) {
			return false
		}
	}

	return true
}

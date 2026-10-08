package formbus

import (
	"fmt"
	"slices"
	"strconv"
)

// A field's name and an item's id are generated rather than typed.
//
// Both are identifiers that nobody filling the form in ever sees, and that
// the builder cannot let anybody change afterwards: a field's name is the key
// every answer is stored under and the heading of its CSV column, and an
// item's id is recorded on every order. Asking somebody who has never written
// HTML to invent a lowercase-underscore identifier, and then telling them it
// can never be corrected, is asking them to make a permanent decision about a
// thing they have no reason to understand. So the builder asks what the
// question says and what sort of answer it takes, and the name is the kind and
// a number: text_1, checkbox_2, item_3.
//
// # A name is never handed out twice
//
// The one way generated names can go wrong is reuse. Remove text_2 from a form
// that has taken submissions, add another text question, and "the smallest
// number not in use" is 2 again -- so every answer given to the old question
// would be read back as an answer to the new one, under its label, in its
// column. The submissions are keyed by name and have no other way to tell the
// two apart.
//
// So a form remembers the names it has retired, in [Form.Retired], and a new
// name skips them as well as the names in use. Kept on the definition rather
// than worked out from the submissions, because formbus does not know
// submissions exist -- submissionbus depends on it, not the other way round.

// NextFieldName is the name a new field of this kind gets: its kind and the
// smallest number neither in use nor retired.
func (f Form) NextFieldName(k Kind) string {
	return f.next(string(k), func(name string) bool {
		_, taken := f.Field(name)

		return taken
	})
}

// NextItemID is the id a new thing for sale gets, on the same terms.
func (f Form) NextItemID() string {
	return f.next("item", func(id string) bool {
		_, taken := f.Item(id)

		return taken
	})
}

func (f Form) next(prefix string, taken func(string) bool) string {
	for n := 1; ; n++ {
		name := prefix + "_" + strconv.Itoa(n)

		if !taken(name) && !slices.Contains(f.Retired, name) {
			return name
		}
	}
}

// Retire records that a field's name or an item's id has been used, so that
// nothing new is ever given it. Called by whatever removes one.
//
// Unconditionally, including on a form that has never been published and so
// can have no answers under the name. Knowing that would mean knowing about
// submissions, and the cost of being wrong in the safe direction is one
// skipped number.
func (f *Form) Retire(name string) {
	if !slices.Contains(f.Retired, name) {
		f.Retired = append(f.Retired, name)
	}
}

// Revise returns next as the definition that replaces was, with the two
// guarantees above kept across a whole-document write.
//
// The builder changes a form one field at a time, and each of its handlers
// keeps the rules for that one change: a new field is named, a removed one is
// retired, and a kept one keeps its kind. A caller that hands over the whole
// definition at once -- the API, which is how a program builds a form -- has
// to have the same rules applied to the difference between two documents, and
// this is where that difference is read, so that the rules stay in one
// package rather than being re-derived by whoever wrote the next writer.
//
// So:
//   - a field or item next leaves unnamed is given the name the builder would
//     have given it, in the order they appear;
//   - every field name and item id was had that next does not is retired, so
//     that it is never handed out again;
//   - Retired is was's, whatever next carried. It is a record of what this
//     form has used, and a caller cannot un-use a name by leaving it out.
//
// And one refusal, as a [DefinitionError] so that it reaches whoever is
// writing in the same shape as Check's problems: a field that keeps its name
// and changes its kind. The builder's field page has no way to ask for that,
// for the reason it gives -- answers already stored were checked as that
// kind, and a question that has collected "yes" and "no" does not become a
// date. A caller that wants a different kind removes the field and adds a new
// one, which retires the old name.
//
// A name next gives explicitly that was had retired is not refused here. It is
// refused by Check, which holds that invariant for every writer, and on a
// draft it is one of the problems listed rather than a write that fails.
func Revise(was, next Form) (Form, error) {
	next.ID = was.ID
	next.Fields = slices.Clone(next.Fields)
	next.Items = slices.Clone(next.Items)
	next.Retired = slices.Clone(was.Retired)

	var problems []string

	for _, old := range was.Fields {
		now, kept := next.Field(old.Name)

		switch {
		case !kept:
			next.Retire(old.Name)
		case now.Kind != old.Kind:
			problems = append(problems, fmt.Sprintf(
				"the field %q is a %s and cannot become a %s; remove it and add a new field without a name instead",
				old.Name, old.Kind, now.Kind))
		}
	}

	for _, old := range was.Items {
		if _, kept := next.Item(old.ID); !kept {
			next.Retire(old.ID)
		}
	}

	if len(problems) > 0 {
		return Form{}, DefinitionError{FormID: was.ID.String(), Problems: problems}
	}

	// Named after the retirements, so that a name was used and next dropped
	// is skipped -- and one at a time, so that two new text fields are
	// text_1 and text_2 rather than both the smallest free number.
	for i := range next.Fields {
		if next.Fields[i].Name == "" && next.Fields[i].Kind.Known() {
			next.Fields[i].Name = next.NextFieldName(next.Fields[i].Kind)
		}
	}

	for i := range next.Items {
		if next.Items[i].ID == "" {
			next.Items[i].ID = next.NextItemID()
		}
	}

	return next, nil
}

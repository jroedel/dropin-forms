package formbus

import (
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

package formbus_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
)

// signup is a form whose list is the point of it: a potluck, where the next
// person to fill it in wants to know what is already coming.
func signup(t *testing.T) formbus.Form {
	t.Helper()

	f := formbus.Form{
		ID:        mustSlug(t, "potluck"),
		Title:     "Parish potluck",
		Currency:  "usd",
		ReturnURL: "https://www.example.org/potluck",
		Fields: []formbus.Field{
			{Name: "name", Label: "Your name", Kind: formbus.KindText, Required: true},
			{Name: "email", Label: "Email", Kind: formbus.KindEmail},
			{Name: "phone", Label: "Phone", Kind: formbus.KindTel},
			{Name: "dish", Label: "What you are bringing", Kind: formbus.KindSelect, Options: []formbus.Option{
				{Value: "main", Label: "A main dish"},
				{Value: "dessert", Label: "Dessert"},
			}},
			{Name: "extras", Label: "Also", Kind: formbus.KindChoices, Options: []formbus.Option{
				{Value: "plates", Label: "Plates"},
				{Value: "cups", Label: "Cups"},
			}},
			{Name: "gf", Label: "It is gluten free", Kind: formbus.KindCheckbox},
			{Name: "gift", Label: "Towards the hall", Kind: formbus.KindAmount},
			{Name: "note", Label: "A note", Kind: formbus.KindParagraph},
		},
		Listing: formbus.Listing{Heading: "Who is bringing what", Line: "{name} -- {dish}"},
	}
	f.Stamp()

	return f
}

func TestListingAcceptsALineNamingQuestions(t *testing.T) {
	f := signup(t)

	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func TestListingRefuses(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		limit int
		want  string
	}{
		{"a question that does not exist", "{nmae}", 0, "{nmae}, which is not one of this form's questions"},
		{"an email address", "{name} <{email}>", 0, "an email address"},
		{"a telephone number", "{name} {phone}", 0, "a telephone number"},
		{"an unclosed brace", "{name", 0, "a { with no }"},
		{"a stray closing brace", "name}", 0, "a } with no {"},
		{"a brace inside a placeholder", "{na{me}", 0, "a { inside a placeholder"},
		{"no question at all", "Somebody is coming", 0, "names no question"},
		{"a negative length", "{name}", -1, "negative"},
		{"a length past the ceiling", "{name}", 501, "the most it can show is 500"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := signup(t)
			f.Listing.Line = tt.line
			f.Listing.Limit = tt.limit
			f.Stamp()

			err := f.Check()

			de, ok := errors.AsType[formbus.DefinitionError](err)
			if !ok {
				t.Fatalf("Check = %v, want a DefinitionError", err)
			}
			if !strings.Contains(strings.Join(de.Problems, "\n"), tt.want) {
				t.Errorf("problems = %q, want one containing %q", de.Problems, tt.want)
			}
		})
	}
}

// Switching the list off by emptying the line should not also demand that the
// heading and the length be cleared.
func TestListingOffIgnoresItsOtherSettings(t *testing.T) {
	f := signup(t)
	f.Listing = formbus.Listing{Heading: "Who is coming", Limit: 20}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("Check with the list off: %v", err)
	}
	if got := f.Listed(); len(got) != 0 {
		t.Errorf("Listed with the list off = %v, want nothing", got)
	}
}

func TestListedNamesWhatWillBeShown(t *testing.T) {
	f := signup(t)
	f.Listing.Line = "{name} brings {dish} ({name})"

	got := f.Listed()

	if len(got) != 2 || !got["name"] || !got["dish"] {
		t.Errorf("Listed = %v, want name and dish", got)
	}
}

func TestListLine(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		fields []formbus.Answer
		want   string
	}{
		{
			name:   "an option by its label",
			line:   "{name} -- {dish}",
			fields: []formbus.Answer{{Name: "name", Values: []string{"Maria G."}}, {Name: "dish", Values: []string{"dessert"}}},
			want:   "Maria G. -- Dessert",
		},
		{
			name:   "an option since removed, by what was stored",
			line:   "{dish}",
			fields: []formbus.Answer{{Name: "dish", Values: []string{"salad"}}},
			want:   "salad",
		},
		{
			name:   "several choices",
			line:   "{extras}",
			fields: []formbus.Answer{{Name: "extras", Values: []string{"plates", "cups"}}},
			want:   "Plates, Cups",
		},
		{
			name:   "a ticked box",
			line:   "{name} gluten free: {gf}",
			fields: []formbus.Answer{{Name: "name", Values: []string{"Tom"}}, {Name: "gf", Values: []string{"on"}}},
			want:   "Tom gluten free: yes",
		},
		{
			name:   "an amount in the form's currency",
			line:   "{name} {gift}",
			fields: []formbus.Answer{{Name: "name", Values: []string{"Ann"}}, {Name: "gift", Values: []string{"12.5"}}},
			want:   "Ann $12.50",
		},
		{
			name:   "a paragraph folded onto one line",
			line:   "{note}",
			fields: []formbus.Answer{{Name: "note", Values: []string{"one\n\ntwo  three"}}},
			want:   "one two three",
		},
		{
			name:   "an optional answer left blank",
			line:   "{name} -- {dish}",
			fields: []formbus.Answer{{Name: "name", Values: []string{"Maria G."}}},
			want:   "Maria G. --",
		},
		{
			name:   "nothing the line names",
			line:   "{dish}",
			fields: []formbus.Answer{{Name: "name", Values: []string{"Maria G."}}},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := signup(t)
			f.Listing.Line = tt.line

			if got := f.ListLine(formbus.Answers{Fields: tt.fields}); got != tt.want {
				t.Errorf("ListLine = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestListingFingerprint(t *testing.T) {
	f := signup(t)
	f.Listing = formbus.Listing{}
	off := f.Fingerprint()

	f.Listing = formbus.Listing{Line: "{name}"}
	on := f.Fingerprint()

	if on == off {
		t.Error("switching the list on did not change the fingerprint, so a tab that never said the name would be shown can still submit")
	}

	f.Listing = formbus.Listing{Line: "{name} -- {dish}"}
	if f.Fingerprint() == on {
		t.Error("showing a further answer did not change the fingerprint")
	}

	f.Listing = formbus.Listing{Line: "Coming: {name}", Heading: "Who is coming", Limit: 20}
	if f.Fingerprint() != on {
		t.Error("rewording the line changed the fingerprint, which discards every half-filled form for a typo")
	}
}

func TestListingShownDefaults(t *testing.T) {
	if got := (formbus.Listing{}).Shown(); got != formbus.ListingDefault {
		t.Errorf("Shown with no limit = %d, want %d", got, formbus.ListingDefault)
	}
	if got := (formbus.Listing{Limit: 7}).Shown(); got != 7 {
		t.Errorf("Shown = %d, want 7", got)
	}
}

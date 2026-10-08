package formbus_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
)

func TestANewFieldIsNamedByItsKindAndTheNextFreeNumber(t *testing.T) {
	f := base(t)
	f.Fields = []formbus.Field{
		{Name: "text_1", Label: "A", Kind: formbus.KindText},
		{Name: "text_2", Label: "B", Kind: formbus.KindText},
		{Name: "checkbox_1", Label: "C", Kind: formbus.KindCheckbox},
	}

	for kind, want := range map[formbus.Kind]string{
		formbus.KindText:     "text_3",
		formbus.KindCheckbox: "checkbox_2",
		formbus.KindDateTime: "datetime_1",
	} {
		if got := f.NextFieldName(kind); got != want {
			t.Errorf("NextFieldName(%s) = %q, want %q", kind, got, want)
		}
	}
}

// The property the retired list exists for: remove a question that has
// answers, add another of the same kind, and it must not inherit them.
func TestARemovedNameIsNeverHandedOutAgain(t *testing.T) {
	f := base(t)
	f.Fields = []formbus.Field{{Name: "text_1", Label: "A", Kind: formbus.KindText}}
	f.Items = []formbus.Item{{ID: "item_1", Label: "Lunch"}}

	f.Fields = nil
	f.Retire("text_1")
	f.Items = nil
	f.Retire("item_1")
	f.Retire("item_1") // twice is once

	if got := f.NextFieldName(formbus.KindText); got != "text_2" {
		t.Errorf("NextFieldName after removing text_1 = %q, want text_2", got)
	}
	if got := f.NextItemID(); got != "item_2" {
		t.Errorf("NextItemID after removing item_1 = %q, want item_2", got)
	}
	if len(f.Retired) != 2 {
		t.Errorf("Retired = %v, want each name once", f.Retired)
	}
}

func TestCheckRefusesAFieldOrItemReusingARetiredName(t *testing.T) {
	f := base(t)
	f.Retired = []string{"recipient", "lunch"}
	f.Items = []formbus.Item{{ID: "lunch", Label: "Lunch", Price: 1200}}
	f.ReturnURL = "https://example.test/lunch"
	f.Stamp()

	err := f.Check()
	if err == nil {
		t.Fatal("Check accepted a field and an item with retired names")
	}

	for _, want := range []string{`field "recipient" has a name this form has already used`, `item "lunch" has an id this form has already used`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Check = %v, want it to contain %q", err, want)
		}
	}
}

// Which names the builder will give next is not what a submission means, so
// retiring one must not throw away every half-filled form.
func TestRetiringANameLeavesTheFingerprintAlone(t *testing.T) {
	f := base(t)
	before := f.Fingerprint()

	f.Retire("text_9")

	if f.Fingerprint() != before {
		t.Error("retiring a name changed the fingerprint")
	}
}

// A whole definition handed over at once keeps the builder's rules: what is
// new is named, what is gone is retired, and a caller cannot un-retire a name
// by leaving the list out.
func TestARevisedDefinitionIsNamedAndRetiredLikeTheBuildersEdits(t *testing.T) {
	was := base(t)
	was.Fields = []formbus.Field{
		{Name: "text_1", Label: "Name", Kind: formbus.KindText},
		{Name: "text_2", Label: "Parish", Kind: formbus.KindText},
	}
	was.Items = []formbus.Item{{ID: "item_1", Label: "Lunch", Price: 1200}}
	was.Retired = []string{"text_3"}

	next := formbus.Form{
		Title: "Renamed",
		Fields: []formbus.Field{
			{Name: "text_1", Label: "Your name", Kind: formbus.KindText},
			{Label: "Diocese", Kind: formbus.KindText},
			{Label: "Coming?", Kind: formbus.KindCheckbox},
		},
		Items: []formbus.Item{{Label: "Dinner", Price: 2000}},
	}

	got, err := formbus.Revise(was, next)
	if err != nil {
		t.Fatalf("Revise: %v", err)
	}

	if got.ID != was.ID || got.Title != "Renamed" {
		t.Errorf("ID, Title = %v, %q; want the form's own name and the new title", got.ID, got.Title)
	}

	var names []string
	for _, f := range got.Fields {
		names = append(names, f.Name)
	}

	// text_2 was dropped and text_3 was already retired, so the new text
	// question is text_4.
	if want := "text_1 text_4 checkbox_1"; strings.Join(names, " ") != want {
		t.Errorf("names = %v, want %s", names, want)
	}

	if got.Items[0].ID != "item_2" {
		t.Errorf("new item = %q, want item_2: item_1 was dropped and is retired", got.Items[0].ID)
	}

	for _, gone := range []string{"text_2", "text_3", "item_1"} {
		if !slices.Contains(got.Retired, gone) {
			t.Errorf("Retired = %v, want %s in it", got.Retired, gone)
		}
	}

	// And the caller's own slices are not written through.
	if next.Fields[1].Name != "" {
		t.Error("Revise named a field in the caller's slice")
	}
}

func TestARevisionCannotChangeAFieldsKind(t *testing.T) {
	was := base(t)
	was.Fields = []formbus.Field{{Name: "coming", Label: "Coming?", Kind: formbus.KindCheckbox}}

	_, err := formbus.Revise(was, formbus.Form{
		Fields: []formbus.Field{{Name: "coming", Label: "When?", Kind: formbus.KindDate}},
	})

	bad, ok := errors.AsType[formbus.DefinitionError](err)
	if !ok || len(bad.Problems) != 1 || !strings.Contains(bad.Problems[0], `"coming"`) {
		t.Fatalf("Revise = %v, want one problem naming the field", err)
	}
}

func TestARevisionCannotReuseARetiredName(t *testing.T) {
	was := base(t)
	was.Fields = []formbus.Field{{Name: "text_1", Label: "Name", Kind: formbus.KindText}}
	was.Retired = []string{"text_2"}

	got, err := formbus.Revise(was, formbus.Form{
		Title:    was.Title,
		Currency: was.Currency,
		Fields:   []formbus.Field{{Name: "text_2", Label: "Again", Kind: formbus.KindText}},
	})
	if err != nil {
		t.Fatalf("Revise: %v", err)
	}

	got.Stamp()

	if err := got.Check(); err == nil || !strings.Contains(err.Error(), "already used and removed") {
		t.Errorf("Check = %v, want the retired name refused", err)
	}
}

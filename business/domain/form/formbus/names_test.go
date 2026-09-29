package formbus_test

import (
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

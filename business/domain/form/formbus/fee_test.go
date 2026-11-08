package formbus_test

import (
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// advent is a registration: every submission is one person at one price,
// with a donation on top for whoever wants to give one.
func advent(t *testing.T) formbus.Form {
	t.Helper()

	f := formbus.Form{
		ID: mustSlug(t, "advent-retreat"), Title: "Advent retreat", Currency: "usd",
		ReturnURL: "https://example.test/retreat",
		Fields: []formbus.Field{
			{Name: "name", Label: "Your name", Kind: formbus.KindText, Required: true},
			{Name: "email", Label: "Your email", Kind: formbus.KindEmail, Required: true},
			{Name: "gift", Label: "A gift toward the house", Kind: formbus.KindAmount},
		},
		Fee:      15000,
		FeeLabel: "Retreat registration",
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("the retreat form does not pass Check: %v", err)
	}

	return f
}

func TestAFeeIsChargedOnceOnALineOfItsOwn(t *testing.T) {
	f := advent(t)

	if !f.Sells() {
		t.Fatal("a form with a fee must report that it sells something")
	}

	ans, err := f.Validate(now, formbus.Values{"name": {"Maria"}, "email": {"maria@example.test"}})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	want := formbus.Line{ItemID: formbus.FeeLineID, Label: "Retreat registration", Price: 15000, Qty: 1, Amount: 15000}

	if len(ans.Lines) != 1 || ans.Lines[0] != want {
		t.Fatalf("Lines = %+v, want just %+v", ans.Lines, want)
	}

	if ans.Total != 15000 {
		t.Errorf("Total = %s, want 150.00", ans.Total)
	}

	// And it does not come back as a quantity somebody could edit.
	if _, ok := ans.Values()[formbus.QuantityField(formbus.FeeLineID)]; ok {
		t.Error("the fee came back as a quantity in Values")
	}
}

// Nothing in a body moves it: not a quantity under the reserved id, not one
// typed as zero, and not one for five people.
func TestAFeeCannotBeChangedFromTheRequest(t *testing.T) {
	f := advent(t)

	for _, qty := range []string{"0", "5", "-1"} {
		ans, err := f.Validate(now, formbus.Values{
			"name": {"Maria"}, "email": {"maria@example.test"},
			formbus.QuantityField(formbus.FeeLineID): {qty},
		})
		if err != nil {
			t.Fatalf("Validate with %s under the fee's name: %v", qty, err)
		}

		if ans.Total != 15000 || len(ans.Lines) != 1 || ans.Lines[0].Qty != 1 {
			t.Errorf("sending %s moved the fee: total %s, lines %+v", qty, ans.Total, ans.Lines)
		}
	}
}

func TestADonationAndItemsAddToTheFee(t *testing.T) {
	f := advent(t)
	f.Items = []formbus.Item{{ID: "shirt", Label: "Retreat T-shirt", Price: 2000}}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	ans, err := f.Validate(now, formbus.Values{
		"name": {"Maria"}, "email": {"maria@example.test"},
		"gift": {"25"}, formbus.QuantityField("shirt"): {"2"},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if ans.Total != 15000+4000+2500 {
		t.Errorf("Total = %s, want 215.00", ans.Total)
	}

	// The fee leads, and the items follow in the definition's order.
	if len(ans.Lines) != 2 || ans.Lines[0].ItemID != formbus.FeeLineID || ans.Lines[1].ItemID != "shirt" {
		t.Errorf("Lines = %+v, want the fee then the shirts", ans.Lines)
	}
}

func TestAFeeWithNoLabelIsCalledARegistration(t *testing.T) {
	f := advent(t)
	f.FeeLabel = "  "

	if got := f.FeeName(); got != formbus.DefaultFeeLabel {
		t.Errorf("FeeName = %q, want %q", got, formbus.DefaultFeeLabel)
	}
}

// A fee is a price, so changing it re-renders every open copy. A form without
// one keeps the version it had before fees existed.
func TestAFeeIsInTheFingerprintOnlyWhenThereIsOne(t *testing.T) {
	f := advent(t)
	before := f.Version

	f.Fee = 16000
	f.Stamp()

	if f.Version == before {
		t.Error("changing the fee did not change the version")
	}

	f.FeeLabel = "Registration for the retreat"
	at := f.Version
	f.Stamp()

	if f.Version != at {
		t.Error("renaming the fee changed the version; it is words, not a price")
	}

	free := formbus.Form{ID: mustSlug(t, "free-form"), Title: "Free", Currency: "usd"}
	free.Stamp()
	unset := free.Version

	free.FeeLabel = "Registration"
	free.Stamp()

	if free.Version != unset {
		t.Error("a label with no fee changed the version of a form that charges nothing")
	}
}

func TestCheckRefusesAFeeItCannotCharge(t *testing.T) {
	for _, tt := range []struct {
		name  string
		fee   types.Money
		label string
		want  string
	}{
		{name: "negative", fee: -1, want: "negative"},
		{name: "too large", fee: types.MaxAmount() + 1, want: "larger than"},
		{name: "two lines", fee: 100, label: "Registration\nand lunch", want: "one line"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := advent(t)
			f.Fee, f.FeeLabel = tt.fee, tt.label
			f.Stamp()

			err := f.Check()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Check = %v, want a problem mentioning %q", err, tt.want)
			}
		})
	}
}

// A paid registration is a paid form, and the rules that hold for one hold
// for it: an order cannot be changed or declined after it is paid for.
func TestAFeeMakesAFormOneThatTakesMoney(t *testing.T) {
	f := advent(t)
	f.NeedsApproval = true
	f.Stamp()

	if err := f.Check(); err == nil || !strings.Contains(err.Error(), "takes money") {
		t.Errorf("Check = %v, want approval refused on a form with a fee", err)
	}
}

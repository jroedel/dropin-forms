package formbus_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
)

// retreat is a form that stops taking new answers on the 15th of November and
// takes changes to the ones it has until the 1st of December.
func retreat(t *testing.T) formbus.Form {
	t.Helper()

	f := base(t)
	f.ClosesAt = time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	f.ChangeableUntil = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	f.Fields = append(f.Fields,
		formbus.Field{Name: "arrival", Label: "Arriving", Kind: formbus.KindDateTime},
		formbus.Field{Name: "needs", Label: "Needs", Kind: formbus.KindChoices, Options: []formbus.Option{
			{Value: "room", Label: "A room"}, {Value: "ride", Label: "A ride"},
		}},
	)
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	return f
}

func TestChangesKeepTheirOwnWindow(t *testing.T) {
	f := retreat(t)
	in := formbus.Values{"recipient": {"Fr. Hector"}}

	cases := []struct {
		name          string
		now           time.Time
		newOK, change bool
	}{
		{"while both are open", time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), true, true},
		{"after new answers close", time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), false, true},
		{"at the change date", f.ChangeableUntil, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.Validate(tc.now, in)
			if got := err == nil; got != tc.newOK {
				t.Errorf("Validate: %v", err)
			}

			_, err = f.ValidateChange(tc.now, in)
			if got := err == nil; got != tc.change {
				t.Errorf("ValidateChange: %v", err)
			}

			if !tc.change {
				if _, ok := errors.AsType[formbus.Unchangeable](err); !ok {
					t.Errorf("ValidateChange refused with %T, want Unchangeable", err)
				}
			}
		})
	}
}

func TestAFormWithoutADateTakesNoChanges(t *testing.T) {
	f := base(t)
	f.Stamp()

	_, err := f.ValidateChange(time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), formbus.Values{"recipient": {"x"}})
	if _, ok := errors.AsType[formbus.Unchangeable](err); !ok {
		t.Fatalf("ValidateChange: %v, want Unchangeable", err)
	}
}

func TestAChangeIsHeldToEveryRuleANewAnswerIs(t *testing.T) {
	f := retreat(t)

	_, err := f.ValidateChange(time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), formbus.Values{"recipient": {""}})
	if _, ok := errors.AsType[formbus.Invalid](err); !ok {
		t.Fatalf("ValidateChange with a required field blank: %v, want Invalid", err)
	}
}

func TestAnswersGoBackIntoTheBoxesTheyCameFrom(t *testing.T) {
	f := retreat(t)
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)

	in := formbus.Values{
		"recipient": {"  Fr. Hector  "},
		"arrival":   {"2027-02-05T14:20"},
		"needs":     {"room", "ride"},
	}

	first, err := f.Validate(now, in)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	again, err := f.ValidateChange(now, first.Values())
	if err != nil {
		t.Fatalf("ValidateChange of the answers' own values: %v", err)
	}

	for _, name := range []string{"recipient", "arrival", "needs"} {
		a, _ := first.Field(name)
		b, _ := again.Field(name)

		if len(a.Values) == 0 || len(a.Values) != len(b.Values) {
			t.Fatalf("%s: %q then %q", name, a.Values, b.Values)
		}

		for i := range a.Values {
			if a.Values[i] != b.Values[i] {
				t.Errorf("%s: %q then %q", name, a.Values, b.Values)
			}
		}
	}
}

func TestTheChangeDateIsNotPartOfTheVersion(t *testing.T) {
	f := retreat(t)
	was := f.Version

	f.ChangeableUntil = f.ChangeableUntil.Add(30 * 24 * time.Hour)
	f.Stamp()

	if f.Version != was {
		t.Errorf("moving the change date moved the version from %s to %s; every open tab would be thrown away", was, f.Version)
	}
}

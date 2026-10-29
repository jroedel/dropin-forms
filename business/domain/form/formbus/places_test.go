package formbus_test

import (
	"slices"
	"testing"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
)

// visits is the sign-up #73 was written for: a priest's home visits in
// one-hour blocks, each of which one household may take, plus a block that
// anybody may choose because it means "whenever suits you".
func visits(t *testing.T) formbus.Form {
	t.Helper()

	f := base(t)
	f.Fields = append(f.Fields,
		formbus.Field{
			Name: "slot", Label: "Time slot", Kind: formbus.KindRadio, Required: true,
			Options: []formbus.Option{
				{Value: "a", Label: "Monday 9 am", Limit: 1},
				{Value: "b", Label: "Monday 10 am", Limit: 1},
				{Value: "c", Label: "Monday 11 am", Limit: 2},
			},
		},
		formbus.Field{
			Name: "also", Label: "Also bless", Kind: formbus.KindChoices,
			Options: []formbus.Option{
				{Value: "car", Label: "The car", Limit: 1},
				{Value: "garden", Label: "The garden"},
			},
		},
	)
	f.Stamp()

	return f
}

func TestALimitIsAcceptedAndChangesNoFingerprint(t *testing.T) {
	f := visits(t)
	if err := f.Check(); err != nil {
		t.Fatalf("Check on a form with limits: %v", err)
	}

	before := f.Fingerprint()
	f.Fields[1].Options[0].Limit = 5

	if f.Fingerprint() != before {
		t.Error("raising a limit changed the fingerprint, which would send everybody halfway through the form back to the start")
	}
}

func TestLimitedIsTrueOnlyWhenSomeOptionHasALimit(t *testing.T) {
	if !visits(t).Limited() {
		t.Error("a form with limited options says it has none")
	}

	if base(t).Limited() {
		t.Error("a form with no options says it has limits")
	}
}

func TestClaimsAreTheLimitedOptionsChosen(t *testing.T) {
	f := visits(t)
	a := formbus.Answers{Fields: []formbus.Answer{
		{Name: "recipient", Values: []string{"Ana"}},
		{Name: "slot", Values: []string{"b"}},
		{Name: "also", Values: []string{"car", "garden"}},
	}}

	got := f.Claims(a)
	want := []formbus.Claim{
		{Field: "slot", Value: "b", Limit: 1},
		{Field: "also", Value: "car", Limit: 1},
	}

	if !slices.Equal(got, want) {
		t.Errorf("Claims = %+v, want %+v (the unlimited garden claims nothing)", got, want)
	}

	if got := f.Claims(formbus.Answers{}); len(got) != 0 {
		t.Errorf("Claims of no answers = %+v, want none", got)
	}
}

func TestFullAndWithout(t *testing.T) {
	taken := formbus.Taken{"slot": {"a": 1, "c": 1}}

	if !taken.Full("slot", "a", 1) {
		t.Error("a slot of one that one person holds is not full")
	}

	if taken.Full("slot", "c", 2) {
		t.Error("a slot of two that one person holds is full")
	}

	if taken.Full("slot", "a", 0) {
		t.Error("an option with no limit is full")
	}

	mine := formbus.Answers{Fields: []formbus.Answer{{Name: "slot", Values: []string{"a"}}}}

	if taken.Without(mine).Full("slot", "a", 1) {
		t.Error("the person who holds a slot sees it as taken when they come back to change their answers")
	}

	if !taken.Full("slot", "a", 1) {
		t.Error("Without changed the count it was called on")
	}
}

func TestExhausted(t *testing.T) {
	full := formbus.Taken{"slot": {"a": 1, "b": 1, "c": 2}}

	cases := []struct {
		name  string
		edit  func(f *formbus.Form)
		taken formbus.Taken
		want  bool
	}{
		{"every slot of a required question is full", nil, full, true},
		{"one slot is still free", nil, formbus.Taken{"slot": {"a": 1, "b": 1, "c": 1}}, false},
		{"nothing is taken", nil, nil, false},
		{"the question is optional", func(f *formbus.Form) { f.Fields[1].Required = false }, full, false},
		{"one option has no limit", func(f *formbus.Form) { f.Fields[1].Options[2].Limit = 0 }, full, false},
		{"the question is asked only sometimes", func(f *formbus.Form) {
			f.Fields[1].ShowIf = &formbus.Condition{Field: "recipient", Is: []string{"x"}}
		}, full, false},
		{"its section is shown only sometimes", func(f *formbus.Form) {
			sec := formbus.Field{
				Name: "visit", Label: "Visit", Kind: formbus.KindSection,
				ShowIf: &formbus.Condition{Field: "recipient", Is: []string{"x"}},
			}
			f.Fields = slices.Insert(f.Fields, 1, sec)
		}, full, false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := visits(t)
			if tt.edit != nil {
				tt.edit(&f)
			}

			if got := f.Exhausted(tt.taken); got != tt.want {
				t.Errorf("Exhausted = %v, want %v", got, tt.want)
			}
		})
	}
}

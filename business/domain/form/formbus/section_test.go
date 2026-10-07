package formbus_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
)

// travelForm has a question that decides, a Travel section shown only to
// somebody coming, and a section after it that is always shown.
func travelForm(t *testing.T) formbus.Form {
	t.Helper()

	f := base(t)
	f.Fields = []formbus.Field{
		{Name: "plans", Label: "Plans", Kind: formbus.KindRadio, Required: true, Options: []formbus.Option{
			{Value: "thinking", Label: "Thinking about it"},
			{Value: "coming", Label: "Coming"},
		}},
		{Name: "section_1", Label: "Travel", Kind: formbus.KindSection, Help: "Once you have tickets.",
			ShowIf: &formbus.Condition{Field: "plans", Is: []string{"coming"}}},
		{Name: "flight", Label: "Flight", Kind: formbus.KindText, Required: true},
		{Name: "lodging", Label: "Lodging", Kind: formbus.KindRadio, Options: []formbus.Option{
			{Value: "house", Label: "Please house me"}, {Value: "own", Label: "My own"},
		}},
		{Name: "nights", Label: "Nights", Kind: formbus.KindNumber,
			ShowIf: &formbus.Condition{Field: "lodging", Is: []string{"house"}}},
		{Name: "section_2", Label: "Anything else", Kind: formbus.KindSection},
		{Name: "notes", Label: "Notes", Kind: formbus.KindParagraph},
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	return f
}

var sectionNow = time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)

func TestAHiddenSectionHidesEveryQuestionInIt(t *testing.T) {
	f := travelForm(t)

	// Thinking about it: the Travel section is hidden, so its required
	// question is not required and what arrived for it is ignored -- and the
	// section after it is shown.
	ans, err := f.Validate(sectionNow, formbus.Values{
		"plans":   {"thinking"},
		"flight":  {"UA 1"},
		"lodging": {"house"},
		"nights":  {"3"},
		"notes":   {"Looking forward"},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	for _, gone := range []string{"flight", "lodging", "nights", "section_1", "section_2"} {
		if _, ok := ans.Field(gone); ok {
			t.Errorf("%s was kept although its section is hidden or it is a heading", gone)
		}
	}
	if a, _ := ans.Field("notes"); a.Value() != "Looking forward" {
		t.Errorf("the next section's question was not taken: %q", a.Value())
	}
}

func TestAShownSectionAsksItsQuestions(t *testing.T) {
	f := travelForm(t)

	_, err := f.Validate(sectionNow, formbus.Values{"plans": {"coming"}})
	inv, ok := errors.AsType[formbus.Invalid](err)
	if !ok || len(inv.For("flight")) == 0 {
		t.Fatalf("Validate: %v, want the section's required question enforced", err)
	}

	ans, err := f.Validate(sectionNow, formbus.Values{
		"plans": {"coming"}, "flight": {"UA 1"}, "lodging": {"house"}, "nights": {"3"},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if a, _ := ans.Field("nights"); a.Value() != "3" {
		t.Errorf("a question with its own condition inside a shown section was not taken")
	}
}

func TestQuestionsLeaveOutTheHeadings(t *testing.T) {
	f := travelForm(t)

	var names []string
	for _, q := range f.Questions() {
		names = append(names, q.Name)
	}

	if got := strings.Join(names, ","); got != "plans,flight,lodging,nights,notes" {
		t.Errorf("Questions = %s", got)
	}

	if s, ok := f.SectionOf("nights"); !ok || s.Name != "section_1" {
		t.Errorf("SectionOf(nights) = %s, %v", s.Name, ok)
	}
	if _, ok := f.SectionOf("plans"); ok {
		t.Error("a question above every heading is in a section")
	}
	if _, ok := f.SectionOf("section_2"); ok {
		t.Error("a heading is in a section")
	}
}

func TestCheckRefusesWhatASectionCannotBe(t *testing.T) {
	cases := map[string]struct {
		change func(f *formbus.Form)
		want   string
	}{
		"a required heading": {
			func(f *formbus.Form) { f.Fields[1].Required = true },
			"cannot have an answer that is required",
		},
		"a heading with choices": {
			func(f *formbus.Form) { f.Fields[1].Options = []formbus.Option{{Value: "a", Label: "A"}} },
			"cannot have choices",
		},
		"a condition on a heading": {
			func(f *formbus.Form) { f.Fields[6].ShowIf = &formbus.Condition{Field: "section_1", Is: []string{"x"}} },
			"which is a section heading and has no answer",
		},
		"nothing but headings": {
			func(f *formbus.Form) { f.Fields = []formbus.Field{f.Fields[1]}; f.Fields[0].ShowIf = nil },
			"only section headings",
		},
		"a heading in the list beneath the form": {
			func(f *formbus.Form) { f.Listing.Line = "{section_1}" },
			"a section heading, which has no answer",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := travelForm(t)
			tc.change(&f)
			f.Stamp()

			err := f.Check()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check: %v, want %q", err, tc.want)
			}
		})
	}
}

func TestASectionsConditionIsPartOfTheVersion(t *testing.T) {
	f := travelForm(t)
	was := f.Version

	f.Fields[1].ShowIf.Is = []string{"thinking", "coming"}
	f.Stamp()

	if f.Version == was {
		t.Error("changing which answers show a section did not change the version; an open tab would be held to rules it was not shown")
	}
}

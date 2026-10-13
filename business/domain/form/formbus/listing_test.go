package formbus_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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
			{Name: "day", Label: "Which day", Kind: formbus.KindDate},
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
			name:   "a date written out",
			line:   "{name} on {day}",
			fields: []formbus.Answer{{Name: "name", Values: []string{"Ann"}}, {Name: "day", Values: []string{"2026-10-17"}}},
			want:   "Ann on Saturday 17 October 2026",
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

	f.Listing = formbus.Listing{Line: "Coming: {name}", Heading: "Who is coming", Limit: 20, OldestFirst: true,
		Sort: []formbus.SortKey{{By: "name", Order: formbus.SortDesc}}}
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

func TestListingSortAccepts(t *testing.T) {
	f := signup(t)
	f.Listing.Sort = []formbus.SortKey{
		{By: "dish"},
		{By: "name", Order: formbus.SortDesc},
		{By: formbus.SortSubmitted, Order: formbus.SortAsc},
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func TestListingSortRefuses(t *testing.T) {
	tests := []struct {
		name   string
		fields []formbus.Field
		sort   []formbus.SortKey
		want   string
	}{
		{"a question that does not exist", nil, []formbus.SortKey{{By: "nmae"}}, "{nmae}, which is not one of this form's questions"},
		{"a question the line does not show", nil, []formbus.SortKey{{By: "day"}}, "{day}, which the line does not show"},
		{"an email address", nil, []formbus.SortKey{{By: "email"}}, "{email}, which cannot be shown on a public page"},
		{"a telephone number", nil, []formbus.SortKey{{By: "phone"}}, "{phone}, which cannot be shown"},
		{"a direction that is neither", nil, []formbus.SortKey{{By: "name", Order: "up"}}, `sorted "up"`},
		{"the same key twice", nil, []formbus.SortKey{{By: "name"}, {By: "name", Order: formbus.SortDesc}}, "{name} twice"},
		{"a step naming nothing", nil, []formbus.SortKey{{}}, "names nothing"},
		{
			"the time on a form with a question of that name",
			[]formbus.Field{{Name: formbus.SortSubmitted, Label: "When", Kind: formbus.KindText}},
			[]formbus.SortKey{{By: formbus.SortSubmitted}},
			"both a question here and the time",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := signup(t)
			f.Fields = append(f.Fields, tt.fields...)
			f.Listing.Sort = tt.sort
			f.Stamp()

			de, ok := errors.AsType[formbus.DefinitionError](f.Check())
			if !ok {
				t.Fatalf("Check = %v, want a DefinitionError", f.Check())
			}
			if !strings.Contains(strings.Join(de.Problems, "\n"), tt.want) {
				t.Errorf("problems = %q, want one containing %q", de.Problems, tt.want)
			}
		})
	}
}

// An order with the list off is left alone, like the heading and the limit.
func TestListingSortOffIsIgnored(t *testing.T) {
	f := signup(t)
	f.Listing = formbus.Listing{Sort: []formbus.SortKey{{By: "nmae"}}}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("Check with the list off: %v", err)
	}
}

// listed is a stand-in for a stored submission: what Arrange is handed.
type listed struct {
	who     string
	answers []formbus.Answer
}

// arrange runs Arrange over submissions given oldest first, a minute apart,
// and returns who ends up where. It hands them over newest first, as the
// store reads them.
func arrange(f formbus.Form, oldestFirst ...listed) []string {
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	type sub struct {
		listed
		at time.Time
	}

	subs := make([]sub, len(oldestFirst))
	for i, l := range oldestFirst {
		subs[i] = sub{l, start.Add(time.Duration(i) * time.Minute)}
	}
	slices.Reverse(subs)

	formbus.Arrange(f, subs, func(s sub) (formbus.Answers, time.Time) {
		return formbus.Answers{Fields: s.answers}, s.at
	})

	out := make([]string, len(subs))
	for i, s := range subs {
		out[i] = s.who
	}

	return out
}

func answered(who string, more ...formbus.Answer) listed {
	return listed{who: who, answers: append([]formbus.Answer{{Name: "name", Values: []string{who}}}, more...)}
}

func ans(name string, values ...string) formbus.Answer {
	return formbus.Answer{Name: name, Values: values}
}

// The example from the issue that asked for this: a time-slot sign-up, read
// in slot order rather than the order people signed up in.
func TestArrangeBySlotThenArrival(t *testing.T) {
	f := signup(t)
	f.Fields = append(f.Fields, formbus.Field{Name: "slot", Label: "Slot", Kind: formbus.KindSelect, Options: []formbus.Option{
		{Value: "09:00", Label: "9:00 am"},
		{Value: "09:30", Label: "9:30 am"},
		{Value: "10:00", Label: "10:00 am"},
	}})
	f.Listing.Line = "{slot} -- {name}"
	f.Listing.Sort = []formbus.SortKey{{By: "slot"}, {By: formbus.SortSubmitted}}

	got := arrange(f,
		answered("C", ans("slot", "10:00")),
		answered("A", ans("slot", "09:00")),
		answered("B", ans("slot", "09:30")),
		answered("D", ans("slot", "09:00")),
	)

	if want := []string{"A", "D", "B", "C"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestArrange(t *testing.T) {
	tests := []struct {
		name   string
		sort   []formbus.SortKey
		oldest bool
		subs   []listed
		want   []string
	}{
		{
			name: "no sort is newest first, as before",
			subs: []listed{answered("a"), answered("b"), answered("c")},
			want: []string{"c", "b", "a"},
		},
		{
			name:   "no sort, oldest first, as before",
			oldest: true,
			subs:   []listed{answered("a"), answered("b"), answered("c")},
			want:   []string{"a", "b", "c"},
		},
		{
			name: "options in the definition's order, not their labels'",
			sort: []formbus.SortKey{{By: "dish"}},
			subs: []listed{answered("d", ans("dish", "dessert")), answered("m", ans("dish", "main"))},
			want: []string{"m", "d"},
		},
		{
			name: "an option since removed after every option still there",
			sort: []formbus.SortKey{{By: "dish"}},
			subs: []listed{answered("s", ans("dish", "salad")), answered("d", ans("dish", "dessert"))},
			want: []string{"d", "s"},
		},
		{
			name: "several boxes by the earliest ticked",
			sort: []formbus.SortKey{{By: "extras"}},
			subs: []listed{answered("cups", ans("extras", "cups")), answered("both", ans("extras", "cups", "plates"))},
			want: []string{"both", "cups"},
		},
		{
			name: "unanswered last when ascending",
			sort: []formbus.SortKey{{By: "dish"}},
			subs: []listed{answered("none"), answered("d", ans("dish", "dessert"))},
			want: []string{"d", "none"},
		},
		{
			name: "unanswered last when descending too",
			sort: []formbus.SortKey{{By: "dish", Order: formbus.SortDesc}},
			subs: []listed{answered("none"), answered("m", ans("dish", "main")), answered("d", ans("dish", "dessert"))},
			want: []string{"d", "m", "none"},
		},
		{
			name: "text without regard to case",
			sort: []formbus.SortKey{{By: "name"}},
			subs: []listed{answered("bob"), answered("Carla"), answered("ann")},
			want: []string{"ann", "bob", "Carla"},
		},
		{
			name: "amounts by value, not as text",
			sort: []formbus.SortKey{{By: "gift", Order: formbus.SortDesc}},
			subs: []listed{answered("ten", ans("gift", "10.00")), answered("nine", ans("gift", "9.00")), answered("hundred", ans("gift", "100.00"))},
			want: []string{"hundred", "ten", "nine"},
		},
		{
			name: "dates by day",
			sort: []formbus.SortKey{{By: "day"}},
			subs: []listed{answered("late", ans("day", "2026-11-02")), answered("early", ans("day", "2026-10-30"))},
			want: []string{"early", "late"},
		},
		{
			name: "ties newest first when the list is",
			sort: []formbus.SortKey{{By: "dish"}},
			subs: []listed{answered("first", ans("dish", "main")), answered("second", ans("dish", "main"))},
			want: []string{"second", "first"},
		},
		{
			name:   "ties oldest first when the list is",
			sort:   []formbus.SortKey{{By: "dish"}},
			oldest: true,
			subs:   []listed{answered("first", ans("dish", "main")), answered("second", ans("dish", "main"))},
			want:   []string{"first", "second"},
		},
		{
			name: "a later key settles what an earlier one left level",
			sort: []formbus.SortKey{{By: "dish"}, {By: "name", Order: formbus.SortDesc}},
			subs: []listed{answered("a", ans("dish", "main")), answered("z", ans("dish", "main")), answered("d", ans("dish", "dessert"))},
			want: []string{"z", "a", "d"},
		},
		{
			name: "by the time it was sent",
			sort: []formbus.SortKey{{By: formbus.SortSubmitted, Order: formbus.SortAsc}},
			subs: []listed{answered("a"), answered("b")},
			want: []string{"a", "b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := signup(t)
			f.Listing.Line = "{name} {dish} {extras} {gift} {day}"
			f.Listing.Sort = tt.sort
			f.Listing.OldestFirst = tt.oldest

			if got := arrange(f, tt.subs...); !slices.Equal(got, tt.want) {
				t.Errorf("order = %v, want %v", got, tt.want)
			}
		})
	}
}

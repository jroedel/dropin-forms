package formbus_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
)

// appointment asks for each temporal kind once, all optional, so a test fills
// in only the one it is about.
func appointment(t *testing.T) formbus.Form {
	t.Helper()

	f := formbus.Form{
		ID:       mustSlug(t, "appointment"),
		Title:    "Book a visit",
		Currency: "usd",
		Fields: []formbus.Field{
			{Name: "day", Label: "Which day", Kind: formbus.KindDate},
			{Name: "at", Label: "What time", Kind: formbus.KindTime},
			{Name: "when", Label: "Pick-up", Kind: formbus.KindDateTime},
		},
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("the appointment form does not pass Check: %v", err)
	}

	return f
}

var sometime = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// Whatever arrived, one stored shape: what the browser's pickers send is kept
// as it is, and the other readable spellings are rewritten into it.
func TestATemporalAnswerIsStoredInOneShape(t *testing.T) {
	tests := []struct {
		field, in, want string
	}{
		{"day", "2026-10-17", "2026-10-17"},
		{"at", "15:30", "15:30"},
		{"at", "15:30:00", "15:30"},
		{"at", "3:30pm", "15:30"},
		{"at", "3:30 PM", "15:30"},
		{"at", "9:05am", "09:05"},
		{"when", "2026-10-17T15:30", "2026-10-17T15:30"},
		{"when", "2026-10-17T15:30:00", "2026-10-17T15:30"},
		{"when", "2026-10-17 15:30", "2026-10-17T15:30"},
	}

	for _, tt := range tests {
		t.Run(tt.field+" "+tt.in, func(t *testing.T) {
			f := appointment(t)

			a, err := f.Validate(sometime, formbus.Values{tt.field: {tt.in}})
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}

			got, ok := a.Field(tt.field)
			if !ok || got.Value() != tt.want {
				t.Errorf("stored %q, want %q", got.Value(), tt.want)
			}
		})
	}
}

// Refused with a sentence naming the shape to type -- the case being a browser
// that drew a plain text box. 10/17/2026 is in the list on purpose: it is a
// different day in Austin and in Aachen, so it is not guessed at.
func TestATemporalAnswerThatCannotBeReadIsRefused(t *testing.T) {
	tests := []struct {
		field, in, want string
	}{
		{"day", "10/17/2026", "Which day needs a date, written like 2026-10-17."},
		{"day", "2026-02-30", "Which day needs a date"},
		{"day", "next Saturday", "Which day needs a date"},
		{"at", "25:00", "What time needs a time, written like 15:30, or 3:30pm."},
		{"when", "2026-10-17", "Pick-up needs a date and time, written like 2026-10-17 15:30."},
		{"when", "2026-10-17T15:30Z", "Pick-up needs a date and time"},
	}

	for _, tt := range tests {
		t.Run(tt.field+" "+tt.in, func(t *testing.T) {
			f := appointment(t)

			_, err := f.Validate(sometime, formbus.Values{tt.field: {tt.in}})

			inv, ok := errors.AsType[formbus.Invalid](err)
			if !ok {
				t.Fatalf("Validate = %v, want Invalid", err)
			}

			vs := inv.For(tt.field)
			if len(vs) != 1 || !strings.HasPrefix(vs[0].Message, tt.want) {
				t.Errorf("violations = %+v, want one beginning %q", vs, tt.want)
			}
		})
	}
}

func TestARequiredDateLeftBlankIsRefused(t *testing.T) {
	f := appointment(t)
	f.Fields[0].Required = true
	f.Stamp()

	_, err := f.Validate(sometime, formbus.Values{"day": {""}})

	inv, ok := errors.AsType[formbus.Invalid](err)
	if !ok || len(inv.For("day")) != 1 {
		t.Fatalf("Validate = %v, want the blank date refused", err)
	}
}

// A condition is compared as a string against the stored shape, so one
// written in any other shape can never be met and is refused at load time.
func TestAConditionOnADateMustBeWrittenInItsStoredShape(t *testing.T) {
	for is, ok := range map[string]bool{
		"2026-10-17":       true,
		"17/10/2026":       false,
		"2026-10-17T00:00": false,
	} {
		t.Run(is, func(t *testing.T) {
			f := appointment(t)
			f.Fields = append(f.Fields, formbus.Field{
				Name: "lunch", Label: "Staying for lunch", Kind: formbus.KindCheckbox,
				ShowIf: &formbus.Condition{Field: "day", Is: []string{is}},
			})
			f.Stamp()

			err := f.Check()

			switch {
			case ok && err != nil:
				t.Errorf("Check refused %q: %v", is, err)
			case !ok && (err == nil || !strings.Contains(err.Error(), "not written the way that field stores it")):
				t.Errorf("Check = %v, want %q refused with the shape to use", err, is)
			}
		})
	}
}

// Not bounded and not textual, so a bound, a length or a pattern on one of
// these is a rule that would silently not apply -- refused like any other.
func TestATemporalFieldTakesNoBoundLengthOrPattern(t *testing.T) {
	one := int64(1)

	for name, breakIt := range map[string]func(*formbus.Field){
		"a minimum": func(fld *formbus.Field) { fld.Min = &one },
		"a length":  func(fld *formbus.Field) { fld.MaxLen = 10 },
		"a pattern": func(fld *formbus.Field) { fld.Pattern = "2026-.*"; fld.PatternNote = "This year." },
	} {
		t.Run(name, func(t *testing.T) {
			f := appointment(t)
			breakIt(&f.Fields[0])
			f.Stamp()

			if err := f.Check(); err == nil {
				t.Errorf("Check accepted a date field with %s", name)
			}
		})
	}
}

func TestReadable(t *testing.T) {
	tests := []struct {
		kind formbus.Kind
		in   []string
		want string
	}{
		{formbus.KindDate, []string{"2026-10-17"}, "Saturday 17 October 2026"},
		{formbus.KindTime, []string{"15:30"}, "3:30pm"},
		{formbus.KindDateTime, []string{"2026-10-17T09:05"}, "Saturday 17 October 2026, 9:05am"},
		{formbus.KindDate, []string{"not a date"}, "not a date"},
		{formbus.KindChoices, []string{"plates", "cups"}, "plates, cups"},
	}

	for _, tt := range tests {
		if got := (formbus.Answer{Kind: tt.kind, Values: tt.in}).Readable(); got != tt.want {
			t.Errorf("Readable(%s %q) = %q, want %q", tt.kind, tt.in, got, tt.want)
		}
	}
}

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

// The case this was written for: a pick-up between Wednesday 30 September and
// Tuesday 13 October, both whole days, on a datetime field.
func TestADatetimeRangeWrittenAsDaysTakesTheWholeOfBoth(t *testing.T) {
	f := appointment(t)
	f.Fields[2].Earliest = "2026-09-30"
	f.Fields[2].Latest = "2026-10-13"
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	for in, ok := range map[string]bool{
		"2026-09-30T00:00": true,
		"2026-10-13T23:59": true,
		"2026-10-05T12:00": true,
		"2026-09-29T23:59": false,
		"2026-10-14T00:00": false,
	} {
		_, err := f.Validate(sometime, formbus.Values{"when": {in}})

		switch inv, refused := errors.AsType[formbus.Invalid](err); {
		case ok && err != nil:
			t.Errorf("%s refused: %v", in, err)
		case !ok && !refused:
			t.Errorf("%s accepted, outside the range", in)
		case !ok && inv.For("when")[0].Message != "Pick-up has to be between Wednesday 30 September 2026 and Tuesday 13 October 2026.":
			t.Errorf("%s refused with %q", in, inv.For("when")[0].Message)
		}
	}

	if got := f.Fields[2].RangeNote(); got != "Between Wednesday 30 September 2026 and Tuesday 13 October 2026." {
		t.Errorf("RangeNote = %q", got)
	}
}

func TestARangeOnEachKind(t *testing.T) {
	tests := []struct {
		field, earliest, latest, in string
		want                        string // "" for accepted
	}{
		{"day", "2026-09-30", "", "2026-09-30", ""},
		{"day", "2026-09-30", "", "2026-09-29", "Which day has to be on or after Wednesday 30 September 2026."},
		{"at", "09:00", "17:00", "5:00pm", ""},
		{"at", "09:00", "17:00", "8:59am", "What time has to be between 9:00am and 5:00pm."},
		{"at", "", "17:00", "17:01", "What time has to be at or before 5:00pm."},
		{"when", "2026-09-30T09:00", "", "2026-09-30T08:30", "Pick-up has to be at or after Wednesday 30 September 2026, 9:00am."},
		{"when", "", "2026-10-13", "2026-10-14T08:00", "Pick-up has to be on or before Tuesday 13 October 2026."},
	}

	for _, tt := range tests {
		t.Run(tt.field+" "+tt.in, func(t *testing.T) {
			f := appointment(t)
			for i := range f.Fields {
				if f.Fields[i].Name == tt.field {
					f.Fields[i].Earliest, f.Fields[i].Latest = tt.earliest, tt.latest
				}
			}
			f.Stamp()

			if err := f.Check(); err != nil {
				t.Fatalf("Check: %v", err)
			}

			_, err := f.Validate(sometime, formbus.Values{tt.field: {tt.in}})

			if tt.want == "" {
				if err != nil {
					t.Errorf("refused: %v", err)
				}

				return
			}

			inv, ok := errors.AsType[formbus.Invalid](err)
			if !ok || len(inv.For(tt.field)) != 1 || inv.For(tt.field)[0].Message != tt.want {
				t.Errorf("Validate = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCheckRefusesABadRange(t *testing.T) {
	tests := []struct {
		name             string
		at               int
		earliest, latest string
		want             string
	}{
		{"on a text field", -1, "2026-09-30", "", "applies only to a date or a time"},
		{"written as the day comes on paper", 0, "30/09/2026", "", `earliest "30/09/2026" is not written the way this field stores it`},
		{"a time in words", 1, "9:00am", "", `earliest "9:00am" is not written the way`},
		{"a day on a time field", 1, "2026-09-30", "", "not written the way"},
		{"the wrong way round", 0, "2026-10-13", "2026-09-30", "is after its latest"},
		{"a datetime ending before it starts", 2, "2026-10-14", "2026-10-13T09:00", "is after its latest"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := appointment(t)

			if tt.at < 0 {
				f.Fields = append(f.Fields, formbus.Field{Name: "who", Label: "Name", Kind: formbus.KindText})
				tt.at = len(f.Fields) - 1
			}

			f.Fields[tt.at].Earliest, f.Fields[tt.at].Latest = tt.earliest, tt.latest
			f.Stamp()

			err := f.Check()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Check = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestARangeIsInTheFingerprint(t *testing.T) {
	f := appointment(t)
	before := f.Fingerprint()

	f.Fields[2].Latest = "2026-10-13"

	if f.Fingerprint() == before {
		t.Error("setting a latest date did not change the fingerprint, so a tab opened before it could still submit the 14th")
	}
}

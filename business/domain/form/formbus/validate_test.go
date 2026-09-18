package formbus_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
)

func mustOrigin(t *testing.T, s string) types.Origin {
	t.Helper()

	o, err := types.ParseOrigin(s)
	if err != nil {
		t.Fatalf("ParseOrigin(%q): %v", s, err)
	}

	return o
}

// feast is the real form this service was built for: the Feast of Our Lady of
// Schoenstatt on 17 October 2026. It sells optional lunch tickets and takes
// donations toward maintaining the Shrine, and it is the fixture for the money
// tests because it is the definition that will actually take somebody's card.
//
// One price, no schedule. Changing it is an edit to the definition, which
// changes the fingerprint, which invalidates every grant already in a browser
// -- so a tab opened at the old price is re-rendered rather than charged.
func feast(t *testing.T) formbus.Form {
	t.Helper()

	f := formbus.Form{
		ID:       mustSlug(t, "feast-lunch-2026"),
		Title:    "Feast of Our Lady of Schoenstatt",
		Currency: "usd",

		// Where Stripe sends somebody back to. Required because this form
		// sells, and unconstrained here because the fixture names no origins.
		ReturnURL: "https://schoenstatt-austin.us/lunch",

		// End of the day of the feast itself, because the page takes donations
		// as well as tickets. Central Daylight Time: US daylight saving does
		// not end until 1 November 2026, so the offset on this date is -05:00.
		ClosesAt: feastCloses,

		Fields: []formbus.Field{
			{
				// One name serving both purposes. A ticket recipient and a
				// donor are not the same person conceptually, but a condition
				// can only depend on another field and not on how many
				// tickets are in the order -- so a separate "name on the
				// ticket" field would be required of a donor who is not
				// buying one.
				Name: "name", Label: "Your name",
				Help: "Whose name should go on the ticket, or on your donation receipt?",
				Kind: formbus.KindText, Required: true, MaxLen: 100,
				Autocomplete: "name",
			},
			{
				Name: "email", Label: "Email for your receipt",
				Kind: formbus.KindEmail, Required: true, MaxLen: 254,
				Autocomplete: "email",
			},
			{
				Name: "donation", Label: "Donation for the Shrine",
				Help: "Toward maintaining the Shrine of Our Lady of Schoenstatt.",
				Kind: formbus.KindAmount,
				// A five dollar floor, which is also an abuse control: a form
				// whose smallest charge is $5 is a poor card checker.
				Min: ptr(int64(500)),
				Max: ptr(int64(500000)),
			},
			{
				Name: "notes", Label: "Anything we should know",
				Kind: formbus.KindParagraph, MaxLen: 500,
			},
		},

		Items: []formbus.Item{
			{ID: "ticket", Label: "Lunch ticket", Price: 1200, Max: 20},
		},

		// No per-order minimum: the tickets are optional. What stops an empty
		// submission is PaymentRequired, which asks only that it be worth
		// something.
		MaxPerOrder: 20,
		MaxTotal:    500000,

		PaymentRequired: true,
		PaymentNote:     "Choose at least one lunch ticket, or enter a donation for the Shrine.",

		Origins: []types.Origin{mustOrigin(t, "https://schoenstatt-austin.us")},
	}

	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("the feast form does not pass Check: %v", err)
	}

	return f
}

// feastCloses is the end of the day of the feast: Saturday 17 October 2026,
// 23:59:59 Central Daylight Time.
var feastCloses = time.Date(2026, 10, 17, 23, 59, 59, 0, time.FixedZone("CDT", -5*60*60))

// now is a fixed instant inside every window the tests use. A test that reads
// the real clock is a test that fails on the day a window closes.
var now = time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

func TestValidateAcceptsAFeastOrder(t *testing.T) {
	f := feast(t)

	ans, err := f.Validate(now, formbus.Values{
		"name":       {"Jeff Roedel"},
		"email":      {"frjeff@example.org"},
		"notes":      {"Two of us are coming with a toddler."},
		"qty_ticket": {"3"},
	})
	if err != nil {
		t.Fatalf("Validate refused a good order: %v", err)
	}

	if ans.FormID != f.ID {
		t.Errorf("FormID = %q, want %q", ans.FormID, f.ID)
	}
	// The answers carry the fingerprint they were checked against, so a row
	// read back later can be read against the rules that applied to it.
	if ans.Version != f.Version {
		t.Errorf("Version = %q, want the form's own %q", ans.Version, f.Version)
	}
	if ans.Currency != "usd" {
		t.Errorf("Currency = %q, want usd", ans.Currency)
	}

	if want := types.Money(3600); ans.Total != want {
		t.Errorf("Total = %s, want %s: three tickets at $12.00", ans.Total, want)
	}

	if len(ans.Lines) != 1 {
		t.Fatalf("got %d lines, want 1: %+v", len(ans.Lines), ans.Lines)
	}

	line := ans.Lines[0]
	switch {
	case line.ItemID != "ticket":
		t.Errorf("line item = %q, want ticket", line.ItemID)
	case line.Qty != 3:
		t.Errorf("line quantity = %d, want 3", line.Qty)
	case line.Price != 1200:
		t.Errorf("line price = %s, want 12.00", line.Price)
	case line.Amount != 3600:
		t.Errorf("line amount = %s, want 36.00", line.Amount)
	}

	// The order of Answers.Fields is the definition's order, which is what the
	// CSV export's columns and the notification email both rely on.
	var names []string
	for _, a := range ans.Fields {
		names = append(names, a.Name)
	}

	if got, want := strings.Join(names, ","), "name,email,notes"; got != want {
		t.Errorf("answers came back as %q, want %q in the definition's order", got, want)
	}

	if a, ok := ans.Field("email"); !ok || a.Value() != "frjeff@example.org" {
		t.Errorf("Field(email) = %+v, %v", a, ok)
	}
	if a, _ := ans.Field("name"); a.Label != "Your name" {
		t.Errorf("the answer did not carry the label: %+v", a)
	}
}

// The rule the whole package exists to hold: a price submitted by a browser is
// not wrong, it is simply not read.
func TestValidateNeverReadsAPriceFromTheRequest(t *testing.T) {
	f := feast(t)

	good := formbus.Values{
		"name":       {"Jeff Roedel"},
		"email":      {"frjeff@example.org"},
		"qty_ticket": {"2"},
	}

	want, err := f.Validate(now, good)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if want.Total != 2400 {
		t.Fatalf("Total = %s, want 24.00", want.Total)
	}

	// Every name somebody would reach for, plus the ones our own rendering
	// might plausibly use.
	for _, name := range []string{
		"price", "amount", "total", "cost", "ticket_price", "price_cents",
		"ticket", "line_total", "Price", "PRICE", "qty_ticket_price",
		"items[0][price]", "price[ticket]",
	} {
		t.Run(name, func(t *testing.T) {
			in := formbus.Values{}
			for k, v := range good {
				in[k] = v
			}
			in[name] = []string{"1"}

			got, err := f.Validate(now, in)
			if err != nil {
				t.Fatalf("Validate refused a submission carrying %q: %v", name, err)
			}

			if got.Total != want.Total {
				t.Errorf("submitting %s=1 changed the total from %s to %s", name, want.Total, got.Total)
			}
			if len(got.Fields) != len(want.Fields) {
				t.Errorf("submitting %s=1 added an answer: %+v", name, got.Fields)
			}
		})
	}
}

func TestValidateQuantity(t *testing.T) {
	f := feast(t)

	tests := []struct {
		name  string
		qty   []string
		total types.Money
		want  string // a phrase the violation must contain; "" means accept
	}{
		{name: "one", qty: []string{"1"}, total: 1200},
		{name: "twenty, the cap", qty: []string{"20"}, total: 24000},
		{name: "leading zeros", qty: []string{"007"}, total: 8400},

		{name: "twenty-one", qty: []string{"21"}, want: "at most 20"},
		{name: "zero", qty: []string{"0"}, want: "or enter a donation"},
		{name: "absent", qty: nil, want: "or enter a donation"},
		{name: "blank", qty: []string{""}, want: "or enter a donation"},
		{name: "blank with spaces", qty: []string{"   "}, want: "or enter a donation"},

		{name: "negative", qty: []string{"-1"}, want: "whole number"},
		{name: "a decimal", qty: []string{"1.5"}, want: "whole number"},
		{name: "a leading plus", qty: []string{"+1"}, want: "whole number"},
		{name: "scientific notation", qty: []string{"1e3"}, want: "whole number"},
		{name: "hexadecimal", qty: []string{"0x2"}, want: "whole number"},
		{name: "a word", qty: []string{"two"}, want: "whole number"},
		{name: "not ASCII digits", qty: []string{"٢"}, want: "whole number"},

		// The values that would overflow rather than merely exceed a cap. A
		// naive price*qty on the first of these wraps to a small positive
		// number, which then passes a maximum check.
		{name: "int64 maximum", qty: []string{"9223372036854775807"}, want: "at most 20"},
		{name: "past int64", qty: []string{"99999999999999999999"}, want: "whole number"},

		// Two values for one quantity did not come from our page.
		{name: "sent twice", qty: []string{"1", "20"}, want: "reload the form"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := formbus.Values{
				"name":  {"Jeff Roedel"},
				"email": {"frjeff@example.org"},
			}
			if tt.qty != nil {
				in["qty_ticket"] = tt.qty
			}

			ans, err := f.Validate(now, in)

			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate refused quantity %v: %v", tt.qty, err)
				}
				if ans.Total != tt.total {
					t.Errorf("Total = %s, want %s", ans.Total, tt.total)
				}

				return
			}

			if err == nil {
				t.Fatalf("Validate accepted quantity %v, total %s", tt.qty, ans.Total)
			}

			invalid, ok := errors.AsType[formbus.Invalid](err)
			if !ok {
				t.Fatalf("Validate returned %T, want Invalid: %v", err, err)
			}
			if !strings.Contains(invalid.Error(), tt.want) {
				t.Errorf("violations do not mention %q: %v", tt.want, invalid)
			}
			if ans.Total != 0 || len(ans.Fields) != 0 || len(ans.Lines) != 0 {
				t.Errorf("a refused submission came back with answers: %+v", ans)
			}
		})
	}
}

func TestValidateRequiredFields(t *testing.T) {
	f := feast(t)

	tests := []struct {
		name string
		in   formbus.Values
		want []string
	}{
		{
			name: "nothing at all",
			in:   formbus.Values{},
			want: []string{"Please fill in Your name", "Please fill in Email for your receipt", "or enter a donation"},
		},
		{
			name: "blank strings count as missing",
			in: formbus.Values{
				"name":       {"   "},
				"email":      {"\t\n"},
				"qty_ticket": {"1"},
			},
			want: []string{"Please fill in Your name", "Please fill in Email for your receipt"},
		},
		{
			name: "an optional field left out is fine",
			in: formbus.Values{
				"name":       {"Jeff Roedel"},
				"email":      {"frjeff@example.org"},
				"qty_ticket": {"1"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ans, err := f.Validate(now, tt.in)

			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}

				// An optional field left blank is absent from the answers
				// rather than present as an empty string, so the record says
				// "not answered" instead of "answered with nothing".
				if _, ok := ans.Field("notes"); ok {
					t.Error("a blank optional field became an answer")
				}

				return
			}

			if err == nil {
				t.Fatal("Validate accepted a submission with required fields missing")
			}

			// Every violation at once. Sending somebody back one field at a
			// time is the worst version of this.
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("violations do not mention %q: %v", want, err)
				}
			}
		})
	}
}

func TestValidateEmail(t *testing.T) {
	f := feast(t)

	accept := []string{
		"frjeff@schoenstatt.us",
		"a@b.co",
		"first.last+tag@sub.example.org",
		"UPPER@EXAMPLE.ORG",
		"o'neill@example.org",
	}

	reject := []string{
		"frjeff",
		"frjeff@",
		"@schoenstatt.us",
		"frjeff@localhost",
		"frjeff@@schoenstatt.us",
		"frjeff@a@b.com",
		"frjeff @schoenstatt.us",
		"frjeff@schoen statt.us",
		"frjeff@.us",
		"frjeff@schoenstatt.",
		"frjeff@schoenstatt..us",
		".@.",

		// What net/mail.ParseAddress accepts and a field labelled "Email"
		// must not: a display name, and a bracketed address.
		"Jeff Roedel <frjeff@schoenstatt.us>",
		"<frjeff@schoenstatt.us>",
		`"Jeff Roedel" frjeff@schoenstatt.us`,
		"frjeff@schoenstatt.us, someone@else.org",
		"frjeff@schoenstatt.us; someone@else.org",

		strings.Repeat("a", 250) + "@example.org",
		strings.Repeat("a", 65) + "@example.org",
	}

	for _, addr := range accept {
		t.Run("accept "+addr, func(t *testing.T) {
			if err := submitEmail(t, f, addr); err != nil {
				t.Errorf("Validate refused %q: %v", addr, err)
			}
		})
	}

	for _, addr := range reject {
		t.Run("reject "+addr, func(t *testing.T) {
			err := submitEmail(t, f, addr)
			if err == nil {
				t.Fatalf("Validate accepted %q as an email address", addr)
			}
			if !strings.Contains(err.Error(), "Check for a typo") {
				t.Errorf("the message is not about a typo: %v", err)
			}
		})
	}
}

func submitEmail(t *testing.T, f formbus.Form, addr string) error {
	t.Helper()

	_, err := f.Validate(now, formbus.Values{
		"name":       {"Jeff Roedel"},
		"email":      {addr},
		"qty_ticket": {"1"},
	})

	return err
}

func TestValidateRefusesAClosedForm(t *testing.T) {
	opens := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	closes := time.Date(2026, 10, 16, 5, 0, 0, 0, time.UTC)

	f := feast(t)
	f.OpensAt, f.ClosesAt = opens, closes
	f.ClosedNote = "Lunch tickets are sold out. Please write to us at the shrine."

	good := formbus.Values{
		"name":       {"Jeff Roedel"},
		"email":      {"frjeff@example.org"},
		"qty_ticket": {"1"},
	}

	for _, tt := range []struct {
		name       string
		at         time.Time
		open       bool
		notYetOpen bool
	}{
		{name: "before it opens", at: opens.Add(-time.Hour), notYetOpen: true},
		{name: "once open", at: opens, open: true},
		{name: "the instant it closes", at: closes},
		{name: "after it closes", at: closes.Add(time.Hour)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ans, err := f.Validate(tt.at, good)

			if tt.open {
				if err != nil {
					t.Fatalf("Validate refused a submission while open: %v", err)
				}

				return
			}

			// Closed is its own type, because there is nothing for the person
			// to correct: the page shows the note instead of the form with
			// errors on it.
			closed, ok := errors.AsType[formbus.Closed](err)
			if !ok {
				t.Fatalf("Validate returned %T, want Closed: %v", err, err)
			}
			if closed.FormID != f.ID {
				t.Errorf("Closed names %q, want %q", closed.FormID, f.ID)
			}
			if closed.Error() != f.ClosedNote {
				t.Errorf("Closed reads %q, want the form's own note", closed.Error())
			}
			if got := closed.NotYetOpen(tt.at); got != tt.notYetOpen {
				t.Errorf("NotYetOpen = %v, want %v", got, tt.notYetOpen)
			}
			if ans.Total != 0 || len(ans.Fields) != 0 {
				t.Errorf("a closed form came back with answers: %+v", ans)
			}
		})
	}

	// A form with no note still says something, rather than rendering a blank
	// box where the form was.
	bare := feast(t)
	bare.ClosesAt = closes

	_, err := bare.Validate(closes, good)
	if err == nil || err.Error() == "" {
		t.Errorf("a closed form with no note must still say something: %v", err)
	}
}

// Conditional fields, both ways: a hidden field's value is ignored and its
// requiredness is suspended, and a visible one is enforced.
func TestValidateConditionalFields(t *testing.T) {
	newForm := func() formbus.Form {
		f := formbus.Form{
			ID:       mustSlug(t, "retreat-signup"),
			Title:    "Retreat sign-up",
			Currency: "usd",
			Fields: []formbus.Field{
				{
					Name: "meal", Label: "Meal", Kind: formbus.KindSelect, Required: true,
					Options: []formbus.Option{
						{Value: "chicken", Label: "Chicken"},
						{Value: "vegetarian", Label: "Vegetarian"},
						{Value: "none", Label: "No lunch, thank you"},
					},
				},
				{
					Name: "allergies", Label: "Allergies", Kind: formbus.KindText, Required: true,
					MaxLen: 200,
					ShowIf: &formbus.Condition{Field: "meal", Is: []string{"chicken", "vegetarian"}},
				},
				{
					// Chained: visible only when allergies is visible *and*
					// answered, which is what makes the forward pass matter.
					Name: "epipen", Label: "Do you carry an EpiPen", Kind: formbus.KindCheckbox,
					ShowIf: &formbus.Condition{Field: "allergies", Is: []string{"peanuts"}},
				},
			},
		}

		f.Stamp()

		if err := f.Check(); err != nil {
			t.Fatalf("Check: %v", err)
		}

		return f
	}

	f := newForm()

	t.Run("hidden field suspends its own requiredness", func(t *testing.T) {
		ans, err := f.Validate(now, formbus.Values{"meal": {"none"}})
		if err != nil {
			t.Fatalf("a required field hidden by its condition was still enforced: %v", err)
		}
		if _, ok := ans.Field("allergies"); ok {
			t.Error("a hidden field appeared in the answers")
		}
	})

	t.Run("hidden field ignores a submitted value", func(t *testing.T) {
		// What happens when somebody fills the field in and then changes the
		// dropdown above it. Ignored, not refused: treating it as tampering
		// would refuse ordinary use of the form.
		ans, err := f.Validate(now, formbus.Values{
			"meal":      {"none"},
			"allergies": {"peanuts"},
			"epipen":    {"on"},
		})
		if err != nil {
			t.Fatalf("Validate refused a leftover value for a hidden field: %v", err)
		}
		if _, ok := ans.Field("allergies"); ok {
			t.Error("a hidden field's leftover value became an answer")
		}
		if _, ok := ans.Field("epipen"); ok {
			t.Error("a field hidden behind a hidden field became an answer")
		}
	})

	t.Run("visible field is enforced", func(t *testing.T) {
		_, err := f.Validate(now, formbus.Values{"meal": {"chicken"}})
		if err == nil {
			t.Fatal("a required field made visible by its condition was not enforced")
		}
		if !strings.Contains(err.Error(), "Please fill in Allergies") {
			t.Errorf("violations do not ask for the visible field: %v", err)
		}
	})

	t.Run("a chained condition can reach the third field", func(t *testing.T) {
		ans, err := f.Validate(now, formbus.Values{
			"meal":      {"vegetarian"},
			"allergies": {"peanuts"},
			"epipen":    {"on"},
		})
		if err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if a, ok := ans.Field("epipen"); !ok {
			t.Error("the chained field was hidden when its condition was met")
		} else if a.Value() != "on" {
			t.Errorf("epipen = %q, want on", a.Value())
		}
	})

	t.Run("a chained condition stays closed when the middle value differs", func(t *testing.T) {
		ans, err := f.Validate(now, formbus.Values{
			"meal":      {"vegetarian"},
			"allergies": {"shellfish"},
			"epipen":    {"on"},
		})
		if err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if _, ok := ans.Field("epipen"); ok {
			t.Error("the chained field appeared although its condition was not met")
		}
	})
}

// A second value for a single-value field did not come from the page we
// rendered, and which one a caller would have taken is exactly what an
// attacker would be choosing between.
func TestValidateRefusesRepeatedValues(t *testing.T) {
	f := feast(t)

	_, err := f.Validate(now, formbus.Values{
		"name":       {"Jeff Roedel", "Somebody Else"},
		"email":      {"frjeff@example.org"},
		"qty_ticket": {"1"},
	})
	if err == nil {
		t.Fatal("Validate accepted two values for a single-value field")
	}
	if !strings.Contains(err.Error(), "reload the form") {
		t.Errorf("the message does not tell them what to do: %v", err)
	}

	// A choices field is the one kind where repetition is how the browser
	// sends the answer.
	multi := formbus.Form{
		ID: mustSlug(t, "helpers"), Title: "Helpers", Currency: "usd",
		Fields: []formbus.Field{{
			Name: "jobs", Label: "What can you help with", Kind: formbus.KindChoices,
			Options: []formbus.Option{
				{Value: "setup", Label: "Setting up"},
				{Value: "serving", Label: "Serving"},
				{Value: "cleanup", Label: "Clearing away"},
			},
		}},
	}
	multi.Stamp()

	if err := multi.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	ans, err := multi.Validate(now, formbus.Values{"jobs": {"setup", "cleanup"}})
	if err != nil {
		t.Fatalf("Validate refused two choices on a choices field: %v", err)
	}
	if a, _ := ans.Field("jobs"); len(a.Values) != 2 {
		t.Errorf("jobs = %v, want both choices", a.Values)
	}
}

func TestValidateChoices(t *testing.T) {
	newForm := func(min, max *int64) formbus.Form {
		f := formbus.Form{
			ID: mustSlug(t, "helpers"), Title: "Helpers", Currency: "usd",
			Fields: []formbus.Field{{
				Name: "jobs", Label: "What can you help with", Kind: formbus.KindChoices,
				Min: min, Max: max,
				Options: []formbus.Option{
					{Value: "setup", Label: "Setting up"},
					{Value: "serving", Label: "Serving"},
					{Value: "cleanup", Label: "Clearing away"},
				},
			}},
		}
		f.Stamp()

		if err := f.Check(); err != nil {
			t.Fatalf("Check: %v", err)
		}

		return f
	}

	tests := []struct {
		name     string
		min, max *int64
		values   []string
		want     string
	}{
		{name: "one of the list", values: []string{"setup"}},
		{name: "all of the list", values: []string{"setup", "serving", "cleanup"}},
		{name: "none, with no minimum", values: nil},

		{name: "not on the list", values: []string{"keynote"}, want: "Please choose from the listed options"},
		{name: "one good one invented", values: []string{"setup", "keynote"}, want: "Please choose from the listed options"},
		{name: "the same choice twice", values: []string{"setup", "setup"}, want: "reload the form"},

		{name: "below the minimum", min: ptr(int64(2)), values: []string{"setup"}, want: "at least 2"},
		{name: "at the minimum", min: ptr(int64(2)), values: []string{"setup", "serving"}},
		{name: "above the maximum", max: ptr(int64(1)), values: []string{"setup", "serving"}, want: "at most 1"},
		{name: "at the maximum", max: ptr(int64(2)), values: []string{"setup", "serving"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newForm(tt.min, tt.max)

			in := formbus.Values{}
			if tt.values != nil {
				in["jobs"] = tt.values
			}

			_, err := f.Validate(now, in)

			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("Validate refused %v: %v", tt.values, err)
			case tt.want == "":
				return
			case err == nil:
				t.Fatalf("Validate accepted %v", tt.values)
			case !strings.Contains(err.Error(), tt.want):
				t.Errorf("violations do not mention %q: %v", tt.want, err)
			}
		})
	}
}

func TestValidateSelectAndRadio(t *testing.T) {
	for _, kind := range []formbus.Kind{formbus.KindSelect, formbus.KindRadio} {
		t.Run(string(kind), func(t *testing.T) {
			f := formbus.Form{
				ID: mustSlug(t, "seating"), Title: "Seating", Currency: "usd",
				Fields: []formbus.Field{{
					Name: "table", Label: "Where would you like to sit", Kind: kind, Required: true,
					Options: []formbus.Option{
						{Value: "front", Label: "Near the front"},
						{Value: "back", Label: "Near the back"},
					},
				}},
			}
			f.Stamp()

			if err := f.Check(); err != nil {
				t.Fatalf("Check: %v", err)
			}

			if _, err := f.Validate(now, formbus.Values{"table": {"front"}}); err != nil {
				t.Errorf("Validate refused a listed option: %v", err)
			}

			// The submitted value is not echoed back. It came from outside the
			// list we rendered, so it is tampering or a stale page, and neither
			// is improved by quoting it into a page.
			_, err := f.Validate(now, formbus.Values{"table": {"<script>alert(1)</script>"}})
			if err == nil {
				t.Fatal("Validate accepted a value that is not an option")
			}
			if strings.Contains(err.Error(), "script") {
				t.Errorf("the violation echoes the submitted value back: %v", err)
			}
			if !strings.Contains(err.Error(), "Please choose one of the listed options") {
				t.Errorf("unexpected message: %v", err)
			}
		})
	}
}

func TestValidateNumber(t *testing.T) {
	f := formbus.Form{
		ID: mustSlug(t, "count"), Title: "Count", Currency: "usd",
		Fields: []formbus.Field{{
			Name: "guests", Label: "How many in your party", Kind: formbus.KindNumber,
			Required: true, Min: ptr(int64(1)), Max: ptr(int64(12)),
		}},
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	for _, tt := range []struct {
		in   string
		want string
	}{
		{in: "1"},
		{in: "12"},
		{in: "0", want: "at least 1"},
		{in: "13", want: "at most 12"},
		{in: "-1", want: "whole number"},
		{in: "1.0", want: "whole number"},
		{in: "1e1", want: "whole number"},
		{in: " 5 "}, // trimmed, then a plain 5
	} {
		t.Run(tt.in, func(t *testing.T) {
			_, err := f.Validate(now, formbus.Values{"guests": {tt.in}})

			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("Validate refused %q: %v", tt.in, err)
			case tt.want == "":
				return
			case err == nil:
				t.Fatalf("Validate accepted %q", tt.in)
			case !strings.Contains(err.Error(), tt.want):
				t.Errorf("violations do not mention %q: %v", tt.want, err)
			}
		})
	}
}

func TestValidateAmountField(t *testing.T) {
	f := formbus.Form{
		ID: mustSlug(t, "offering"), Title: "Offering", Currency: "usd",
		ReturnURL: "https://example.test/offering",
		Fields: []formbus.Field{{
			Name: "gift", Label: "Your gift", Kind: formbus.KindAmount, Required: true,
			Min: ptr(int64(500)), Max: ptr(int64(500000)),
		}},
		MaxTotal: 500000,
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	if !f.Sells() {
		t.Fatal("a form with an amount field must report that it sells something")
	}

	for _, tt := range []struct {
		in    string
		total types.Money
		want  string
	}{
		{in: "5", total: 500},
		{in: "5.00", total: 500},
		{in: "25.50", total: 2550},
		{in: "5000", total: 500000},

		{in: "4.99", want: "at least $5.00"},
		{in: "5000.01", want: "at most $5000.00"},

		{in: "1e3", want: "like 25 or 25.50"},
		{in: "$25", want: "like 25 or 25.50"},
		{in: "25.001", want: "like 25 or 25.50"},
		{in: "-25", want: "like 25 or 25.50"},
		{in: "twenty", want: "like 25 or 25.50"},
	} {
		t.Run(tt.in, func(t *testing.T) {
			ans, err := f.Validate(now, formbus.Values{"gift": {tt.in}})

			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("Validate refused %q: %v", tt.in, err)
			case tt.want == "":
				if ans.Total != tt.total {
					t.Errorf("Total = %s, want %s", ans.Total, tt.total)
				}

				return
			case err == nil:
				t.Fatalf("Validate accepted %q, total %s", tt.in, ans.Total)
			case !strings.Contains(err.Error(), tt.want):
				t.Errorf("violations do not mention %q: %v", tt.want, err)
			}
		})
	}
}

// A typed amount and priced items add up, and the sum is bounded.
// A donation and priced tickets add up, and the ceiling applies to the sum
// rather than to either part.
func TestValidateTotalBounds(t *testing.T) {
	f := feast(t)

	in := func(qty, donation string) formbus.Values {
		v := formbus.Values{
			"name":       {"Jeff Roedel"},
			"email":      {"frjeff@example.org"},
			"qty_ticket": {qty},
		}
		if donation != "" {
			v["donation"] = []string{donation}
		}

		return v
	}

	ans, err := f.Validate(now, in("2", "50"))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if want := types.Money(2400 + 5000); ans.Total != want {
		t.Errorf("Total = %s, want %s: two tickets plus a $50 donation", ans.Total, want)
	}

	// Twenty tickets is within the per-item cap and $4,900 is within the
	// donation field's own maximum, so neither part is out of bounds on its
	// own. The sum is what exceeds the form's ceiling.
	_, err = f.Validate(now, in("20", "4900"))
	if err == nil {
		t.Fatal("Validate accepted a total above the form's maximum")
	}
	if !strings.Contains(err.Error(), "largest order this form takes is $5000.00") {
		t.Errorf("the message does not name the ceiling in dollars: %v", err)
	}
}

// Stripe refuses a charge below its own per-currency floor, so catching it
// here is the difference between a sentence and a failed payment.
func TestValidateRefusesBelowThePaymentFloor(t *testing.T) {
	f := formbus.Form{
		ID: mustSlug(t, "candle"), Title: "Light a candle", Currency: "usd",
		ReturnURL: "https://example.test/candle",
		Fields: []formbus.Field{{
			Name: "gift", Label: "Amount", Kind: formbus.KindAmount, Required: true,
		}},
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	_, err := f.Validate(now, formbus.Values{"gift": {"0.25"}})
	if err == nil {
		t.Fatal("Validate accepted a payment below the floor")
	}
	if !strings.Contains(err.Error(), "smallest payment we can take is $0.50") {
		t.Errorf("unexpected message: %v", err)
	}

	if _, err := f.Validate(now, formbus.Values{"gift": {"0.50"}}); err != nil {
		t.Errorf("Validate refused the floor itself: %v", err)
	}

	// A total of nothing is not a payment, so neither floor applies.
	free := f
	free.Fields[0].Required = false

	if _, err := free.Validate(now, formbus.Values{}); err != nil {
		t.Errorf("Validate refused a form submitted with nothing to charge: %v", err)
	}
}

// A textarea submits CRLF per the HTML spec. Normalising is reading the format
// the browser actually sends, not defensiveness.
func TestValidateNormalisesNewlines(t *testing.T) {
	f := feast(t)

	ans, err := f.Validate(now, formbus.Values{
		"name":       {"Jeff Roedel"},
		"email":      {"frjeff@example.org"},
		"notes":      {"Two adults.\r\nOne toddler.\r\n"},
		"qty_ticket": {"3"},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	notes, _ := ans.Field("notes")
	if want := "Two adults.\nOne toddler."; notes.Value() != want {
		t.Errorf("notes = %q, want %q: CRLF normalised and the trailing break trimmed", notes.Value(), want)
	}

	// A newline in a single-line field is not a newline the browser sent.
	_, err = f.Validate(now, formbus.Values{
		"name":       {"Jeff\nRoedel"},
		"email":      {"frjeff@example.org"},
		"qty_ticket": {"1"},
	})
	if err == nil {
		t.Fatal("Validate accepted a newline inside a single-line field")
	}
	if !strings.Contains(err.Error(), "cannot contain that character") {
		t.Errorf("unexpected message: %v", err)
	}
}

func TestValidateRefusesControlCharactersAndBrokenText(t *testing.T) {
	f := feast(t)

	submit := func(recipient string) error {
		_, err := f.Validate(now, formbus.Values{
			"name":       {recipient},
			"email":      {"frjeff@example.org"},
			"qty_ticket": {"1"},
		})

		return err
	}

	for _, tt := range []struct {
		name string
		in   string
	}{
		{"a null byte", "Jeff\x00Roedel"},
		{"an escape", "Jeff\x1bRoedel"},
		{"a bell", "Jeff\aRoedel"},
		{"a delete", "Jeff\x7fRoedel"},
		{"a tab in the middle", "Jeff\tRoedel"},
		{"a vertical tab", "Jeff\x0bRoedel"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := submit(tt.in); err == nil {
				t.Errorf("Validate accepted %q", tt.in)
			}
		})
	}

	t.Run("not valid text at all", func(t *testing.T) {
		err := submit("Jeff\xff\xfeRoedel")
		if err == nil {
			t.Fatal("Validate accepted a value that is not valid text")
		}
		if !strings.Contains(err.Error(), "Please retype it") {
			t.Errorf("unexpected message: %v", err)
		}
	})

	t.Run("accented letters are fine", func(t *testing.T) {
		if err := submit("José María"); err != nil {
			t.Errorf("Validate refused an accented name: %v", err)
		}
	})

	t.Run("a newline is allowed only in a paragraph", func(t *testing.T) {
		_, err := f.Validate(now, formbus.Values{
			"name":       {"Jeff Roedel"},
			"email":      {"frjeff@example.org"},
			"notes":      {"First line.\nSecond line."},
			"qty_ticket": {"1"},
		})
		if err != nil {
			t.Errorf("Validate refused a newline in a paragraph field: %v", err)
		}
	})
}

// Length is counted in runes, not bytes. A name limited to 40 bytes is a name
// limited to 40 Latin letters or 13 Chinese characters, which is not one limit.
func TestValidateLengthCountsRunes(t *testing.T) {
	f := formbus.Form{
		ID: mustSlug(t, "names"), Title: "Names", Currency: "usd",
		Fields: []formbus.Field{{
			Name: "name", Label: "Name", Kind: formbus.KindText, Required: true,
			MinLen: 2, MaxLen: 10,
		}},
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	for _, tt := range []struct {
		name string
		in   string
		want string
	}{
		{name: "at the minimum", in: "Jo"},
		{name: "below the minimum", in: "J", want: "at least 2 characters"},
		{name: "at the maximum", in: strings.Repeat("a", 10)},
		{name: "above the maximum", in: strings.Repeat("a", 11), want: "at most 10 characters, and you typed 11"},

		// Ten accented letters are twenty bytes and ten characters.
		{name: "ten accented letters", in: strings.Repeat("é", 10)},
		{name: "eleven accented letters", in: strings.Repeat("é", 11), want: "you typed 11"},

		// Ten Chinese characters are thirty bytes and ten characters.
		{name: "ten wide characters", in: strings.Repeat("安", 10)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.Validate(now, formbus.Values{"name": {tt.in}})

			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("Validate refused a %d-rune value: %v", len([]rune(tt.in)), err)
			case tt.want == "":
				return
			case err == nil:
				t.Fatalf("Validate accepted a %d-rune value", len([]rune(tt.in)))
			case !strings.Contains(err.Error(), tt.want):
				t.Errorf("violations do not mention %q: %v", tt.want, err)
			}
		})
	}
}

func TestInvalidForGroupsViolationsByField(t *testing.T) {
	f := feast(t)

	_, err := f.Validate(now, formbus.Values{"email": {"nope"}})

	invalid, ok := errors.AsType[formbus.Invalid](err)
	if !ok {
		t.Fatalf("Validate returned %T, want Invalid: %v", err, err)
	}

	if len(invalid.For("email")) != 1 {
		t.Errorf("For(email) = %+v, want the one address violation", invalid.For("email"))
	}
	if len(invalid.For("name")) != 1 {
		t.Errorf("For(name) = %+v, want the one missing-field violation", invalid.For("name"))
	}
	if got := invalid.For("notes"); len(got) != 0 {
		t.Errorf("For(notes) = %+v, want nothing", got)
	}

	// A form-level violation has no field, so it belongs in the summary rather
	// than beside an input.
	var formLevel int
	for _, v := range invalid.Violations {
		if v.Field == "" {
			formLevel++
		}
	}
	if formLevel != 1 {
		t.Errorf("got %d form-level violations, want the one about ordering nothing", formLevel)
	}
}

// Every sentence a person reads must be about what they should do, and must
// never name a Go package or type. House style, asserted once rather than
// remembered at fifty call sites.
func TestViolationMessagesAreForPeople(t *testing.T) {
	f := feast(t)
	f.Fields = append(f.Fields, formbus.Field{
		Name: "gift", Label: "An extra gift", Kind: formbus.KindAmount,
		Min: ptr(int64(500)),
	})
	f.MaxTotal = 30000
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	bodies := []formbus.Values{
		{},
		{"email": {"nope"}, "qty_ticket": {"1"}},
		{"name": {"A"}, "email": {"a@b.co"}, "qty_ticket": {"999"}},
		{"name": {"A"}, "email": {"a@b.co"}, "qty_ticket": {"two"}},
		{"name": {"A"}, "email": {"a@b.co"}, "qty_ticket": {"1"}, "gift": {"1"}},
		{"name": {"A"}, "email": {"a@b.co"}, "qty_ticket": {"20"}, "gift": {"5000"}},
		{"name": {"A\x00B"}, "email": {"a@b.co"}, "qty_ticket": {"1"}},
		{"name": {"A", "B"}, "email": {"a@b.co"}, "qty_ticket": {"1"}},
	}

	forbidden := []string{
		"formbus", "types.", "Money", "int64", "strconv", "utf8",
		"Violation", "nil", "error", "err ", "panic", "regexp",
	}

	for _, in := range bodies {
		_, err := f.Validate(now, in)
		if err == nil {
			continue
		}

		invalid, ok := errors.AsType[formbus.Invalid](err)
		if !ok {
			t.Fatalf("Validate returned %T, want Invalid: %v", err, err)
		}

		for _, v := range invalid.Violations {
			for _, word := range forbidden {
				if strings.Contains(v.Message, word) {
					t.Errorf("a violation names %q, which means nothing to a person: %q", word, v.Message)
				}
			}

			if v.Message == "" {
				t.Error("a violation has no message at all")
			}
			if !strings.HasSuffix(v.Message, ".") && !strings.HasSuffix(v.Message, "?") {
				t.Errorf("a violation is not a sentence: %q", v.Message)
			}
		}
	}
}

// Nothing half-accepted. On any error the answers are the zero value, so a
// caller that forgets to check the error cannot charge a card from a partial
// total.
func TestValidateReturnsNothingOnFailure(t *testing.T) {
	f := feast(t)

	for _, in := range []formbus.Values{
		{},
		{"name": {"A"}, "email": {"nope"}, "qty_ticket": {"1"}},
		{"name": {"A"}, "email": {"a@b.co"}, "qty_ticket": {"999"}},
	} {
		ans, err := f.Validate(now, in)
		if err == nil {
			t.Fatalf("Validate accepted %v", in)
		}

		if ans.Total != 0 || len(ans.Fields) != 0 || len(ans.Lines) != 0 || !ans.FormID.Zero() {
			t.Errorf("a refused submission came back with answers: %+v", ans)
		}
	}
}

// A form with no items and no amount field derives a total of nothing, and a
// submission to it is a plain record with no payment step.
func TestValidateAFreeForm(t *testing.T) {
	f := formbus.Form{
		ID: mustSlug(t, "prayer-request"), Title: "Prayer request", Currency: "usd",
		Fields: []formbus.Field{
			{Name: "intention", Label: "Your intention", Kind: formbus.KindParagraph, Required: true, MaxLen: 1000},
		},
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	if f.Sells() {
		t.Error("a form with nothing for sale reported that it sells something")
	}

	ans, err := f.Validate(now, formbus.Values{"intention": {"For my grandmother."}})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if ans.Total != 0 {
		t.Errorf("Total = %s, want nothing", ans.Total)
	}
	if len(ans.Lines) != 0 {
		t.Errorf("got %d lines on a form that sells nothing", len(ans.Lines))
	}
	if ans.Version != f.Version || ans.Version == "" {
		t.Errorf("Version = %q, want the form's own %q", ans.Version, f.Version)
	}
}

// Optional tickets plus donations: the page has two ways to be worth
// something and neither is individually required, so the only rule left is
// that a submission must be worth something at all.
func TestValidateRequiresTheSubmissionToBeWorthSomething(t *testing.T) {
	f := feast(t)

	who := formbus.Values{
		"name":  {"Jeff Roedel"},
		"email": {"frjeff@example.org"},
	}

	with := func(extra formbus.Values) formbus.Values {
		in := formbus.Values{}
		for k, v := range who {
			in[k] = v
		}
		for k, v := range extra {
			in[k] = v
		}

		return in
	}

	t.Run("tickets alone", func(t *testing.T) {
		ans, err := f.Validate(now, with(formbus.Values{"qty_ticket": {"2"}}))
		if err != nil {
			t.Fatalf("Validate refused an order of tickets with no donation: %v", err)
		}
		if ans.Total != 2400 {
			t.Errorf("Total = %s, want 24.00", ans.Total)
		}
		if _, ok := ans.Field("donation"); ok {
			t.Error("a blank donation became an answer")
		}
	})

	t.Run("a donation alone", func(t *testing.T) {
		ans, err := f.Validate(now, with(formbus.Values{"donation": {"25"}}))
		if err != nil {
			t.Fatalf("Validate refused a donation with no tickets: %v", err)
		}
		if ans.Total != 2500 {
			t.Errorf("Total = %s, want 25.00", ans.Total)
		}
		if len(ans.Lines) != 0 {
			t.Errorf("got %d lines on an order with no tickets", len(ans.Lines))
		}
	})

	t.Run("both", func(t *testing.T) {
		ans, err := f.Validate(now, with(formbus.Values{
			"qty_ticket": {"2"},
			"donation":   {"25"},
		}))
		if err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if want := types.Money(2400 + 2500); ans.Total != want {
			t.Errorf("Total = %s, want %s: two tickets and a $25 donation", ans.Total, want)
		}
	})

	t.Run("neither", func(t *testing.T) {
		_, err := f.Validate(now, who)
		if err == nil {
			t.Fatal("Validate accepted a submission worth nothing")
		}

		invalid, ok := errors.AsType[formbus.Invalid](err)
		if !ok {
			t.Fatalf("Validate returned %T, want Invalid: %v", err, err)
		}

		// The author's own sentence, because only the author knows what is on
		// the page. A generic "this form requires a payment" would not tell
		// anybody which of the two things in front of them to fill in.
		if len(invalid.Violations) != 1 || invalid.Violations[0].Message != f.PaymentNote {
			t.Errorf("violations = %+v, want just the form's own note", invalid.Violations)
		}
		if invalid.Violations[0].Field != "" {
			t.Error("the note belongs in the summary, not beside one input")
		}
	})

	t.Run("a donation below the floor is refused rather than counted", func(t *testing.T) {
		_, err := f.Validate(now, with(formbus.Values{"donation": {"1"}}))
		if err == nil {
			t.Fatal("Validate accepted a donation below the form's floor")
		}
		if !strings.Contains(err.Error(), "at least $5.00") {
			t.Errorf("unexpected message: %v", err)
		}
	})

	t.Run("a form that may be empty still accepts an empty submission", func(t *testing.T) {
		open := feast(t)
		open.PaymentRequired = false
		open.PaymentNote = ""
		open.Stamp()

		if err := open.Check(); err != nil {
			t.Fatalf("Check: %v", err)
		}

		ans, err := open.Validate(now, who)
		if err != nil {
			t.Fatalf("Validate refused an empty submission to a form that allows one: %v", err)
		}
		if ans.Total != 0 {
			t.Errorf("Total = %s, want nothing", ans.Total)
		}
	})
}

// The form closes at the end of the day of the feast, because the page takes
// donations as well as tickets.
func TestTheFeastFormClosesAtTheEndOfTheFeastDay(t *testing.T) {
	f := feast(t)

	good := formbus.Values{
		"name":       {"Jeff Roedel"},
		"email":      {"frjeff@example.org"},
		"qty_ticket": {"1"},
	}

	// 23:59:59 CDT on Saturday 17 October 2026 is 04:59:59 UTC on the 18th.
	// Asserting the UTC instant is the point: a naive local time would be an
	// hour out whenever somebody reads it in the wrong offset, and five hours
	// out if it were read as UTC.
	if got, want := f.ClosesAt.UTC().Format(time.RFC3339), "2026-10-18T04:59:59Z"; got != want {
		t.Errorf("the form closes at %s, want %s", got, want)
	}

	if _, err := f.Validate(f.ClosesAt.Add(-time.Second), good); err != nil {
		t.Errorf("Validate refused a submission a second before closing: %v", err)
	}

	if _, err := f.Validate(f.ClosesAt, good); err == nil {
		t.Error("Validate accepted a submission at the closing instant")
	}
}

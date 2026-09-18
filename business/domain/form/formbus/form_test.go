package formbus_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
)

func ptr[T any](v T) *T { return &v }

func mustSlug(t *testing.T, s string) types.Slug {
	t.Helper()

	slug, err := types.ParseSlug(s)
	if err != nil {
		t.Fatalf("ParseSlug(%q): %v", s, err)
	}

	return slug
}

// base is the smallest definition that passes Check. Every Check test starts
// here and breaks exactly one thing, so a failure names the rule it broke
// rather than whatever else the fixture happened to be missing.
// base is not stamped. Every caller mutates it first and stamps afterwards,
// which is the order a store uses too: build the definition, then fingerprint
// what you built.
func base(t *testing.T) formbus.Form {
	t.Helper()

	return formbus.Form{
		ID:       mustSlug(t, "feast-lunch-2026"),
		Title:    "Feast of Our Lady of Schoenstatt: lunch",
		Currency: "usd",
		Fields: []formbus.Field{
			{Name: "recipient", Label: "Name on the ticket", Kind: formbus.KindText, Required: true, MaxLen: 100},
		},
	}
}

func TestCheckAcceptsTheSmallestUsefulForm(t *testing.T) {
	f := base(t)
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("Check on the base form: %v", err)
	}
}

func TestCheckAcceptsTheFeastForm(t *testing.T) {
	f := feast(t)

	if err := f.Check(); err != nil {
		t.Fatalf("Check on the feast form: %v", err)
	}
}

// Each case breaks one thing and names a phrase the report must contain. The
// phrase is what somebody reads at startup, so asserting on it is asserting
// that the message is about the thing that is wrong.
func TestCheckRefuses(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(f *formbus.Form)
		want   string
	}{
		{
			name:   "no name",
			break_: func(f *formbus.Form) { f.ID = types.Slug{} },
			want:   "has no name",
		},
		{
			name:   "no title",
			break_: func(f *formbus.Form) { f.Title = "   " },
			want:   "has no title",
		},
		{
			name:   "unknown currency",
			break_: func(f *formbus.Form) { f.Currency = "eur" },
			want:   "this service handles usd",
		},
		{
			name:   "blank currency",
			break_: func(f *formbus.Form) { f.Currency = "" },
			want:   "this service handles usd",
		},
		{
			name:   "no fields",
			break_: func(f *formbus.Form) { f.Fields = nil },
			want:   "has no fields",
		},
		{
			name: "closes before it opens",
			break_: func(f *formbus.Form) {
				f.OpensAt = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
				f.ClosesAt = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			},
			want: "which is not after it opens",
		},
		{
			name: "closes exactly when it opens",
			break_: func(f *formbus.Form) {
				at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
				f.OpensAt, f.ClosesAt = at, at
			},
			want: "which is not after it opens",
		},

		// Field names.
		{
			name:   "field with no name",
			break_: func(f *formbus.Form) { f.Fields[0].Name = "" },
			want:   "has no name",
		},
		{
			name:   "field name with a hyphen",
			break_: func(f *formbus.Form) { f.Fields[0].Name = "first-name" },
			want:   "lowercase letters, digits and underscores",
		},
		{
			name:   "field name with a capital",
			break_: func(f *formbus.Form) { f.Fields[0].Name = "firstName" },
			want:   "lowercase letters, digits and underscores",
		},
		{
			name:   "field name starting with a digit",
			break_: func(f *formbus.Form) { f.Fields[0].Name = "1name" },
			want:   "must begin with a lowercase letter",
		},
		{
			name:   "field name starting with an underscore",
			break_: func(f *formbus.Form) { f.Fields[0].Name = "_name" },
			want:   "must begin with a lowercase letter",
		},
		{
			name:   "field name too long",
			break_: func(f *formbus.Form) { f.Fields[0].Name = strings.Repeat("a", 41) },
			want:   "longer than 40 characters",
		},
		{
			// The collision that would otherwise be discovered as a wrong
			// total: a field called qty_tickets shadows the quantity input for
			// an item called tickets.
			name:   "field name takes the quantity prefix",
			break_: func(f *formbus.Form) { f.Fields[0].Name = "qty_tickets" },
			want:   `begins with "qty_"`,
		},
		{
			name:   "field name takes our own prefix",
			break_: func(f *formbus.Form) { f.Fields[0].Name = "dropin_grant" },
			want:   `begins with "dropin_"`,
		},
		{
			name: "two fields with the same name",
			break_: func(f *formbus.Form) {
				f.Fields = append(f.Fields, formbus.Field{
					Name: "recipient", Label: "Again", Kind: formbus.KindText,
				})
			},
			want: "there is already a field with that name, at position 1",
		},
		{
			name:   "field with no label",
			break_: func(f *formbus.Form) { f.Fields[0].Label = " " },
			want:   "it has no label",
		},
		{
			name:   "unknown kind",
			break_: func(f *formbus.Form) { f.Fields[0].Kind = "signature" },
			want:   `"signature" is not a kind of field`,
		},
		{
			name:   "empty kind",
			break_: func(f *formbus.Form) { f.Fields[0].Kind = "" },
			want:   "is not a kind of field",
		},

		// Options.
		{
			name:   "choice field with no options",
			break_: func(f *formbus.Form) { f.Fields[0].Kind = formbus.KindSelect },
			want:   "needs at least one option",
		},
		{
			name: "text field carrying options",
			break_: func(f *formbus.Form) {
				f.Fields[0].Options = []formbus.Option{{Value: "a", Label: "A"}}
			},
			want: "would never be shown",
		},
		{
			name: "option with no value",
			break_: func(f *formbus.Form) {
				f.Fields[0].Kind = formbus.KindRadio
				f.Fields[0].Options = []formbus.Option{{Value: "", Label: "Blank"}}
			},
			want: "option 1 has no value",
		},
		{
			name: "option value with a trailing space",
			break_: func(f *formbus.Form) {
				f.Fields[0].Kind = formbus.KindRadio
				f.Fields[0].Options = []formbus.Option{{Value: "yes ", Label: "Yes"}}
			},
			want: "a space at one end",
		},
		{
			name: "two options with the same value",
			break_: func(f *formbus.Form) {
				f.Fields[0].Kind = formbus.KindRadio
				f.Fields[0].Options = []formbus.Option{
					{Value: "yes", Label: "Yes"},
					{Value: "yes", Label: "Also yes"},
				}
			},
			want: "more than one option with the value",
		},
		{
			name: "option with no label",
			break_: func(f *formbus.Form) {
				f.Fields[0].Kind = formbus.KindRadio
				f.Fields[0].Options = []formbus.Option{{Value: "yes", Label: ""}}
			},
			want: "has no label",
		},

		// Bounds.
		{
			name:   "bounds on a field with no number",
			break_: func(f *formbus.Form) { f.Fields[0].Min = ptr(int64(1)) },
			want:   "use a length instead",
		},
		{
			name: "negative minimum",
			break_: func(f *formbus.Form) {
				f.Fields[0].Kind = formbus.KindNumber
				f.Fields[0].MaxLen = 0
				f.Fields[0].Min = ptr(int64(-1))
			},
			want: "is negative",
		},
		{
			name: "minimum above maximum",
			break_: func(f *formbus.Form) {
				f.Fields[0].Kind = formbus.KindNumber
				f.Fields[0].MaxLen = 0
				f.Fields[0].Min, f.Fields[0].Max = ptr(int64(10)), ptr(int64(2))
			},
			want: "is above its maximum",
		},
		{
			name: "amount maximum above the ceiling",
			break_: func(f *formbus.Form) {
				f.Fields[0].Kind = formbus.KindAmount
				f.Fields[0].MaxLen = 0
				f.Fields[0].Max = ptr(int64(types.MaxAmount()) + 1)
			},
			want: "larger than this service will take",
		},
		{
			name: "length on a field with no text",
			break_: func(f *formbus.Form) {
				f.Fields[0].Kind = formbus.KindCheckbox
			},
			want: "has no text to measure",
		},
		{
			name: "minimum length above maximum length",
			break_: func(f *formbus.Form) {
				f.Fields[0].MinLen, f.Fields[0].MaxLen = 50, 10
			},
			want: "is above its maximum length",
		},
		{
			name:   "negative length",
			break_: func(f *formbus.Form) { f.Fields[0].MinLen = -1 },
			want:   "cannot be negative",
		},

		// Patterns.
		{
			name: "pattern that does not compile",
			break_: func(f *formbus.Form) {
				f.Fields[0].Pattern = "[0-9"
				f.Fields[0].PatternNote = "use digits"
			},
			want: "not one this service can read",
		},
		{
			name:   "pattern with nothing to tell somebody",
			break_: func(f *formbus.Form) { f.Fields[0].Pattern = `\d{5}` },
			want:   "nothing to tell somebody",
		},
		{
			name:   "an explanation for a pattern that does not exist",
			break_: func(f *formbus.Form) { f.Fields[0].PatternNote = "use digits" },
			want:   "explains a pattern it does not have",
		},
		{
			name: "pattern on a field with no text",
			break_: func(f *formbus.Form) {
				f.Fields[0].Kind = formbus.KindNumber
				f.Fields[0].MaxLen = 0
				f.Fields[0].Pattern = `\d+`
				f.Fields[0].PatternNote = "use digits"
			},
			want: "has no text for a pattern",
		},

		// Conditions. These are the ones whose failure at runtime is silent --
		// the field simply never appears -- which is why they are refused here.
		{
			name: "condition on a field that does not exist",
			break_: func(f *formbus.Form) {
				f.Fields[0].ShowIf = &formbus.Condition{Field: "nope", Is: []string{"yes"}}
			},
			want: "there is no such field",
		},
		{
			name: "condition on a later field",
			break_: func(f *formbus.Form) {
				f.Fields[0].ShowIf = &formbus.Condition{Field: "later", Is: []string{"yes"}}
				f.Fields = append(f.Fields, formbus.Field{
					Name: "later", Label: "Later", Kind: formbus.KindText,
				})
			},
			want: "comes after it",
		},
		{
			name: "condition on itself",
			break_: func(f *formbus.Form) {
				f.Fields[0].ShowIf = &formbus.Condition{Field: "recipient", Is: []string{"yes"}}
			},
			want: "can never do",
		},
		{
			name: "condition with no value",
			break_: func(f *formbus.Form) {
				f.Fields = append(f.Fields, formbus.Field{
					Name: "extra", Label: "Extra", Kind: formbus.KindText,
					ShowIf: &formbus.Condition{Field: "recipient"},
				})
			},
			want: "no value is named",
		},
		{
			name: "condition naming a value the field does not offer",
			break_: func(f *formbus.Form) {
				f.Fields = []formbus.Field{
					{
						Name: "meal", Label: "Meal", Kind: formbus.KindSelect,
						Options: []formbus.Option{{Value: "chicken", Label: "Chicken"}},
					},
					{
						Name: "allergies", Label: "Allergies", Kind: formbus.KindText,
						ShowIf: &formbus.Condition{Field: "meal", Is: []string{"fish"}},
					},
				}
			},
			want: "not one of that field's options",
		},

		// Items.
		{
			name: "item with no name",
			break_: func(f *formbus.Form) {
				f.Items = []formbus.Item{{Label: "Ticket", Price: 1200}}
			},
			want: "has no name",
		},
		{
			name: "item name with a hyphen",
			break_: func(f *formbus.Form) {
				f.Items = []formbus.Item{{ID: "lunch-ticket", Label: "Ticket", Price: 1200}}
			},
			want: "lowercase letters, digits and underscores",
		},
		{
			name: "two items with the same name",
			break_: func(f *formbus.Form) {
				f.Items = []formbus.Item{
					{ID: "ticket", Label: "Ticket", Price: 1200},
					{ID: "ticket", Label: "Another", Price: 1500},
				}
			},
			want: "already an item with that name",
		},
		{
			name: "item with no label",
			break_: func(f *formbus.Form) {
				f.Items = []formbus.Item{{ID: "ticket", Label: "", Price: 1200}}
			},
			want: "has no label",
		},
		{
			name: "item with a negative price",
			break_: func(f *formbus.Form) {
				f.Items = []formbus.Item{{ID: "ticket", Label: "Ticket", Price: -1}}
			},
			want: "its price is negative",
		},
		{
			name: "item priced above the ceiling",
			break_: func(f *formbus.Form) {
				f.Items = []formbus.Item{{ID: "ticket", Label: "Ticket", Price: types.MaxAmount() + 1}}
			},
			want: "larger than this service will take",
		},
		{
			name: "item with a negative maximum",
			break_: func(f *formbus.Form) {
				f.Items = []formbus.Item{{ID: "ticket", Label: "Ticket", Price: 1200, Max: -1}}
			},
			want: "per-order maximum is negative",
		},

		// Order and total bounds.
		{
			name:   "negative per-order maximum",
			break_: func(f *formbus.Form) { f.MaxPerOrder = -1 },
			want:   "per-order maximum is negative",
		},
		{
			name: "per-order minimum above the maximum",
			break_: func(f *formbus.Form) {
				f.Items = []formbus.Item{{ID: "ticket", Label: "Ticket", Price: 1200}}
				f.MinPerOrder, f.MaxPerOrder = 5, 2
			},
			want: "is above its per-order maximum",
		},
		{
			name:   "a per-order minimum with nothing for sale",
			break_: func(f *formbus.Form) { f.MinPerOrder = 1 },
			want:   "nothing for sale",
		},
		{
			name:   "requires a payment with nothing to pay for",
			break_: func(f *formbus.Form) { f.PaymentRequired = true; f.PaymentNote = "Give us money." },
			want:   "nothing to pay for",
		},
		{
			name: "requires a payment with nothing to say about it",
			break_: func(f *formbus.Form) {
				f.Items = []formbus.Item{{ID: "ticket", Label: "Ticket", Price: 1200}}
				f.PaymentRequired = true
			},
			want: "nothing to tell somebody who submits it empty",
		},
		{
			name:   "explains a payment it does not require",
			break_: func(f *formbus.Form) { f.PaymentNote = "Give us money." },
			want:   "explains a payment it does not require",
		},
		{
			name:   "negative minimum total",
			break_: func(f *formbus.Form) { f.MinTotal = -1 },
			want:   "minimum total is negative",
		},
		{
			name: "minimum total above maximum total",
			break_: func(f *formbus.Form) {
				f.MinTotal, f.MaxTotal = 5000, 1000
			},
			want: "is above its maximum total",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := base(t)
			tt.break_(&f)
			f.Stamp()

			err := f.Check()
			if err == nil {
				t.Fatalf("Check accepted a form with %s", tt.name)
			}

			var de formbus.DefinitionError
			if !errors.As(err, &de) {
				t.Fatalf("Check returned %T, want a DefinitionError: %v", err, err)
			}

			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Check did not report %q.\ngot:\n%s", tt.want, err)
			}
		})
	}
}

// A definition error names every problem at once, because it is fixed in a
// file by a person who should not have to restart once per mistake.
func TestCheckReportsEveryProblemAtOnce(t *testing.T) {
	f := formbus.Form{
		Currency: "gbp",
		Fields: []formbus.Field{
			{Name: "Bad-Name", Kind: "nonsense"},
		},
	}

	err := f.Check()
	if err == nil {
		t.Fatal("Check accepted a form with nothing right about it")
	}

	de, ok := errors.AsType[formbus.DefinitionError](err)
	if !ok {
		t.Fatalf("Check returned %T, want a DefinitionError", err)
	}

	// name, version, title, currency, field name, field label: six at least.
	if len(de.Problems) < 6 {
		t.Errorf("Check reported %d problems, want every one of them:\n%s", len(de.Problems), err)
	}

	if !strings.Contains(err.Error(), "(unnamed)") {
		t.Errorf("a form with no name should say so in its report:\n%s", err)
	}
}

// Check compiles the patterns, so a form that has passed Check never pays for
// a compile per request and never has to handle a compile failure in a
// handler. The observable consequence is that a pattern is enforced only after
// Check has run, which this asserts in both directions.
func TestCheckCompilesPatternsAndAnchorsThem(t *testing.T) {
	newForm := func() formbus.Form {
		f := base(t)
		f.Fields[0].Kind = formbus.KindText
		f.Fields[0].MaxLen = 0
		f.Fields[0].Pattern = `\d{5}`
		f.Fields[0].PatternNote = "use five digits, like 78745"

		return f
	}

	unchecked := newForm()
	unchecked.Stamp()
	if _, err := unchecked.Validate(time.Now(), formbus.Values{"recipient": {"not digits"}}); err != nil {
		t.Errorf("before Check a pattern cannot be enforced, and the submission should pass: %v", err)
	}

	f := newForm()
	f.Stamp()
	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	if _, err := f.Validate(time.Now(), formbus.Values{"recipient": {"78745"}}); err != nil {
		t.Errorf("a matching value was refused: %v", err)
	}

	// Anchored at both ends. An unanchored \d{5} matches inside this value,
	// which is how a "validated" field accepts anything with a valid prefix.
	for _, bad := range []string{"78745x", "x78745", "1234", "787451", "78745\n78745"} {
		if _, err := f.Validate(time.Now(), formbus.Values{"recipient": {bad}}); err == nil {
			t.Errorf("Validate accepted %q against the pattern \\d{5}", bad)
		}
	}
}

func TestKindPredicates(t *testing.T) {
	// Every kind Kinds() offers must be Known, or a builder UI can offer a
	// field this service refuses to load.
	for _, k := range formbus.Kinds() {
		if !k.Known() {
			t.Errorf("Kinds() offers %q, which Known() rejects", k)
		}
	}

	if formbus.Kind("signature").Known() {
		t.Error("an invented kind reported itself as known")
	}

	// Kinds() must hand back a copy, or a caller reordering it reorders the
	// builder's palette for every later caller.
	first := formbus.Kinds()
	first[0] = "tampered"

	if formbus.Kinds()[0] == "tampered" {
		t.Error("Kinds() exposes its own slice")
	}

	for _, tt := range []struct {
		kind                             formbus.Kind
		options, multi, textual, bounded bool
	}{
		{formbus.KindText, false, false, true, false},
		{formbus.KindParagraph, false, false, true, false},
		{formbus.KindEmail, false, false, true, false},
		{formbus.KindTel, false, false, true, false},
		{formbus.KindNumber, false, false, false, true},
		{formbus.KindSelect, true, false, false, false},
		{formbus.KindRadio, true, false, false, false},
		{formbus.KindCheckbox, false, false, false, false},
		{formbus.KindChoices, true, true, false, true},
		{formbus.KindAmount, false, false, false, true},
	} {
		t.Run(string(tt.kind), func(t *testing.T) {
			if got := tt.kind.HasOptions(); got != tt.options {
				t.Errorf("HasOptions() = %v, want %v", got, tt.options)
			}
			if got := tt.kind.MultiValue(); got != tt.multi {
				t.Errorf("MultiValue() = %v, want %v", got, tt.multi)
			}
			if got := tt.kind.Textual(); got != tt.textual {
				t.Errorf("Textual() = %v, want %v", got, tt.textual)
			}
			if got := tt.kind.Bounded(); got != tt.bounded {
				t.Errorf("Bounded() = %v, want %v", got, tt.bounded)
			}
		})
	}
}

func TestSellsAndOpen(t *testing.T) {
	plain := base(t)
	if plain.Sells() {
		t.Error("a form with no items and no amount field reported that it sells something")
	}

	withItem := base(t)
	withItem.Items = []formbus.Item{{ID: "ticket", Label: "Ticket", Price: 1200}}
	if !withItem.Sells() {
		t.Error("a form with an item reported that it sells nothing")
	}

	withAmount := base(t)
	withAmount.Fields[0].Kind = formbus.KindAmount
	withAmount.Fields[0].MaxLen = 0
	if !withAmount.Sells() {
		t.Error("a form with an amount field reported that it sells nothing")
	}

	opens := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	closes := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	f := base(t)
	f.OpensAt, f.ClosesAt = opens, closes

	for _, tt := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"before it opens", opens.Add(-time.Second), false},
		{"the instant it opens", opens, true},
		{"while open", opens.Add(time.Hour), true},
		{"a second before it closes", closes.Add(-time.Second), true},
		// Closing is exclusive at the far end: "closes at 23:59:59" means
		// submissions at 23:59:59 are already too late, which is the reading
		// that never leaves a one-second window open.
		{"the instant it closes", closes, false},
		{"after it closes", closes.Add(time.Second), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := f.Open(tt.now); got != tt.want {
				t.Errorf("Open(%s) = %v, want %v", tt.now.Format(time.RFC3339), got, tt.want)
			}
		})
	}

	always := base(t)
	if !always.Open(time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("a form with no window should always be open")
	}
}

func TestFieldAndItemLookup(t *testing.T) {
	f := feast(t)

	if _, ok := f.Field("name"); !ok {
		t.Error("Field did not find a field that is there")
	}
	if _, ok := f.Field("nope"); ok {
		t.Error("Field found a field that is not there")
	}
	if it, ok := f.Item("ticket"); !ok || it.Price != 1200 {
		t.Errorf("Item(ticket) = %+v, %v; want the $12.00 ticket", it, ok)
	}
	if _, ok := f.Item("nope"); ok {
		t.Error("Item found an item that is not there")
	}
}

// The version is an invariant rather than advice: no store can serve a form
// whose version has stopped tracking its content.
func TestCheckRefusesAnUnstampedOrStaleVersion(t *testing.T) {
	t.Run("never stamped", func(t *testing.T) {
		f := base(t)

		err := f.Check()
		if err == nil {
			t.Fatal("Check accepted a form with no version")
		}
		if !strings.Contains(err.Error(), "must call Stamp") {
			t.Errorf("unexpected message: %v", err)
		}
	})

	t.Run("stamped, then edited", func(t *testing.T) {
		f := base(t)
		f.Stamp()

		if err := f.Check(); err != nil {
			t.Fatalf("a stamped form was refused: %v", err)
		}

		// The edit somebody makes at 11:59pm on the night the price changes.
		f.Items = []formbus.Item{{ID: "ticket", Label: "Lunch ticket", Price: 1500}}

		err := f.Check()
		if err == nil {
			t.Fatal("Check accepted a form whose price no longer matches its version")
		}
		if !strings.Contains(err.Error(), "no longer matches its content") {
			t.Errorf("unexpected message: %v", err)
		}
	})
}

// What the fingerprint covers is the whole point of it: everything that
// changes what a submission means, and nothing that does not.
func TestFingerprintTracksMeaningAndIgnoresPresentation(t *testing.T) {
	changes := []struct {
		name string
		edit func(f *formbus.Form)
	}{
		{"the price", func(f *formbus.Form) { f.Items[0].Price = 1500 }},
		{"an item's per-order cap", func(f *formbus.Form) { f.Items[0].Max = 4 }},
		{"a new item", func(f *formbus.Form) {
			f.Items = append(f.Items, formbus.Item{ID: "dessert", Label: "Dessert", Price: 300})
		}},
		{"the closing time", func(f *formbus.Form) {
			f.ClosesAt = time.Date(2026, 10, 14, 5, 0, 0, 0, time.UTC)
		}},
		{"the currency", func(f *formbus.Form) { f.Currency = "eur" }},
		{"the per-order minimum", func(f *formbus.Form) { f.MinPerOrder = 2 }},
		{"the total ceiling", func(f *formbus.Form) { f.MaxTotal = 1000 }},
		{"whether it may be empty", func(f *formbus.Form) { f.PaymentRequired = false }},
		{"a field becoming required", func(f *formbus.Form) { f.Fields[2].Required = true }},
		{"a maximum length", func(f *formbus.Form) { f.Fields[0].MaxLen = 40 }},
		{"a new field", func(f *formbus.Form) {
			f.Fields = append(f.Fields, formbus.Field{
				Name: "table", Label: "Table", Kind: formbus.KindText,
			})
		}},
		{"the field order", func(f *formbus.Form) {
			f.Fields[0], f.Fields[1] = f.Fields[1], f.Fields[0]
		}},
		{"a field's name", func(f *formbus.Form) { f.Fields[0].Name = "guest" }},
		{"a field's kind", func(f *formbus.Form) { f.Fields[0].Kind = formbus.KindParagraph }},
		{"who may embed it", func(f *formbus.Form) { f.Origins = nil }},
	}

	for _, tt := range changes {
		t.Run("changes with "+tt.name, func(t *testing.T) {
			f := feast(t)
			before := f.Fingerprint()

			tt.edit(&f)

			if f.Fingerprint() == before {
				t.Errorf("changing %s did not change the fingerprint, so a grant minted before it would still be honoured", tt.name)
			}
		})
	}

	// Presentation. Changing any of these throws away every half-filled form
	// on the site if it is hashed, and changes nothing about what an answer
	// means if it is not.
	same := []struct {
		name string
		edit func(f *formbus.Form)
	}{
		{"the title", func(f *formbus.Form) { f.Title = "Lunch after Mass" }},
		{"the introduction", func(f *formbus.Form) { f.Intro = "Please join us." }},
		{"a field's label", func(f *formbus.Form) { f.Fields[0].Label = "Who is this ticket for" }},
		{"help text", func(f *formbus.Form) { f.Fields[0].Help = "As it should appear on the ticket." }},
		{"a placeholder", func(f *formbus.Form) { f.Fields[0].Placeholder = "Jane Doe" }},
		{"an item's label", func(f *formbus.Form) { f.Items[0].Label = "Lunch" }},
		{"the confirmation message", func(f *formbus.Form) { f.Confirmation = "Thank you!" }},
		{"the closed note", func(f *formbus.Form) { f.ClosedNote = "Sold out." }},
		{"the wording of the payment note", func(f *formbus.Form) { f.PaymentNote = "Buy a ticket or give something." }},
		{"who is notified", func(f *formbus.Form) { f.Notify = []string{"kitchen@example.org"} }},
		{"the daily cap", func(f *formbus.Form) { f.DailyCap = 500 }},
		{"the same instant in another offset", func(f *formbus.Form) {
			f.ClosesAt = f.ClosesAt.In(time.FixedZone("CDT", -5*60*60))
		}},
	}

	for _, tt := range same {
		t.Run("ignores "+tt.name, func(t *testing.T) {
			f := feast(t)
			before := f.Fingerprint()

			tt.edit(&f)

			if f.Fingerprint() != before {
				t.Errorf("changing %s changed the fingerprint, which discards every form somebody has half filled in", tt.name)
			}
		})
	}
}

// Length-prefixed, so that two definitions differing only in where one string
// ends and the next begins cannot fingerprint the same. Without the prefix,
// an item called "ab" priced with a cap of 1 and one called "a" with the
// leftover shifted along would collide.
func TestFingerprintIsUnambiguous(t *testing.T) {
	one := base(t)
	one.Fields = []formbus.Field{
		{Name: "ab", Label: "A", Kind: formbus.KindText},
		{Name: "cd", Label: "B", Kind: formbus.KindText},
	}

	two := base(t)
	two.Fields = []formbus.Field{
		{Name: "a", Label: "A", Kind: formbus.KindText},
		{Name: "bcd", Label: "B", Kind: formbus.KindText},
	}

	if one.Fingerprint() == two.Fingerprint() {
		t.Error("two different definitions fingerprinted the same")
	}

	// And it is stable: the same definition, fingerprinted twice, and again
	// after a round trip through a copy.
	f := feast(t)

	first, second := f.Fingerprint(), f.Fingerprint()
	if first != second {
		t.Errorf("the fingerprint is not stable: %q then %q", first, second)
	}

	copied := f
	if copied.Fingerprint() != f.Fingerprint() {
		t.Error("copying a form changed its fingerprint")
	}

	if len(f.Fingerprint()) != 12 {
		t.Errorf("the fingerprint is %d characters, want 12", len(f.Fingerprint()))
	}
}

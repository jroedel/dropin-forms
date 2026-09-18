package formtoml_test

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formtoml"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/forms"
)

// The definitions that will actually be served. This test is the reason the
// real files are in the repository rather than uploaded to a server: a typo in
// a price or a missing offset on a closing time fails CI instead of failing on
// the day of the feast.
func TestTheRealDefinitionsLoad(t *testing.T) {
	store, err := formtoml.Load(forms.FS)
	if err != nil {
		t.Fatalf("the forms that ship in this binary do not load: %v", err)
	}

	all := store.All()
	if len(all) == 0 {
		t.Fatal("no definitions were loaded")
	}

	for _, f := range all {
		t.Logf("%s version %s: %d fields, %d items", f.ID, f.Version, len(f.Fields), len(f.Items))
	}
}

func TestTheFeastForm(t *testing.T) {
	store, err := formtoml.Load(forms.FS)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	slug, err := types.ParseSlug("feast-lunch-2026")
	if err != nil {
		t.Fatalf("ParseSlug: %v", err)
	}

	f, err := store.ByID(slug)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	// The price. Asserted here because it is the number that leaves somebody's
	// bank account, and because "12.00" in a TOML file becoming 1200 cents is
	// exactly the conversion worth pinning.
	item, ok := f.Item("ticket")
	if !ok {
		t.Fatal("there is no ticket to buy")
	}
	if item.Price != 1200 {
		t.Errorf("a lunch ticket is %s, want 12.00", item.Price)
	}
	if item.Max != 20 {
		t.Errorf("the per-item cap is %d, want 20", item.Max)
	}

	// The closing instant, in UTC. A local date-time silently read as UTC
	// would close the form at seven in the evening on the day of the feast,
	// and nothing would report it.
	if got, want := f.ClosesAt.UTC().Format(time.RFC3339), "2026-10-18T04:59:59Z"; got != want {
		t.Errorf("the form closes at %s, want %s: 23:59:59 CDT on Saturday 17 October", got, want)
	}

	if !f.PaymentRequired {
		t.Error("the form accepts a submission with no ticket and no donation")
	}
	if f.PaymentNote == "" {
		t.Error("the form has nothing to tell somebody who submits it empty")
	}

	// The donation floor, parsed from "5.00" into cents.
	donation, ok := f.Field("donation")
	if !ok {
		t.Fatal("there is nowhere to make a donation")
	}
	if donation.Kind != formbus.KindAmount {
		t.Errorf("the donation field is a %s", donation.Kind)
	}
	if donation.Min == nil || *donation.Min != 500 {
		t.Errorf("the donation floor is %v, want 500 cents", donation.Min)
	}

	// Who may frame it. An empty list fails closed to frame-ancestors 'none',
	// so a missing origin is a blank box on the live page.
	if len(f.Origins) != 1 || f.Origins[0].String() != "https://schoenstatt-austin.us" {
		t.Errorf("origins = %v, want just the Squarespace site", f.Origins)
	}

	// And it takes a real order end to end.
	ans, err := f.Validate(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), formbus.Values{
		"name":       {"Jeff Roedel"},
		"email":      {"frjeff@example.org"},
		"qty_ticket": {"2"},
		"donation":   {"25"},
	})
	if err != nil {
		t.Fatalf("the real form refused a real order: %v", err)
	}
	if want := types.Money(2400 + 2500); ans.Total != want {
		t.Errorf("Total = %s, want %s: two tickets and a $25 donation", ans.Total, want)
	}
	if ans.Version != f.Version {
		t.Errorf("the answers carry version %q, want the form's own %q", ans.Version, f.Version)
	}
}

func TestByIDAndAll(t *testing.T) {
	store, err := formtoml.Load(fstest.MapFS{
		"beta.toml":  {Data: []byte(minimal("beta"))},
		"alpha.toml": {Data: []byte(minimal("alpha"))},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	all := store.All()
	if len(all) != 2 {
		t.Fatalf("loaded %d forms, want 2", len(all))
	}

	// Sorted by slug, so a listing page and a startup log line are stable
	// rather than depending on Go's map iteration.
	if all[0].ID.String() != "alpha" || all[1].ID.String() != "beta" {
		t.Errorf("All() returned %s then %s, want alpha then beta", all[0].ID, all[1].ID)
	}

	slug, err := types.ParseSlug("missing")
	if err != nil {
		t.Fatalf("ParseSlug: %v", err)
	}

	if _, err := store.ByID(slug); !errors.Is(err, formtoml.ErrNotFound) {
		t.Errorf("ByID for an unknown slug returned %v, want ErrNotFound", err)
	}
}

func TestLoadRefuses(t *testing.T) {
	tests := []struct {
		name  string
		files fstest.MapFS
		want  string
	}{
		{
			name:  "no definitions at all",
			files: fstest.MapFS{},
			want:  "no form definitions",
		},
		{
			name:  "not TOML",
			files: fstest.MapFS{"a.toml": {Data: []byte("this is not toml {{{")}},
			want:  "not readable TOML",
		},
		{
			// The failure this whole wire type exists to catch: a typo in a
			// setting name leaves its rule switched off and nothing says so.
			name: "a misspelled setting",
			files: fstest.MapFS{"alpha.toml": {Data: []byte(
				minimal("alpha", "require_payment = true"))}},
			want: "settings this service does not know: require_payment",
		},
		{
			name: "a misspelled setting inside a field",
			files: fstest.MapFS{"alpha.toml": {Data: []byte(
				minimal("alpha") + "\n[[field]]\nname = \"x\"\nlabel = \"X\"\nkind = \"text\"\nrequird = true\n")}},
			want: "requird",
		},
		{
			name:  "the filename disagrees with the id",
			files: fstest.MapFS{"lunch.toml": {Data: []byte(minimal("alpha"))}},
			want:  "name the file alpha.toml",
		},
		{
			name: "two files claiming one form",
			files: fstest.MapFS{
				"alpha.toml": {Data: []byte(minimal("alpha"))},
				"beta.toml":  {Data: []byte(strings.Replace(minimal("beta"), `id = "beta"`, `id = "alpha"`, 1))},
			},
			// The filename check catches this one first, which is the better
			// message anyway: it names the file to fix.
			want: "name the file",
		},
		{
			name:  "a bad slug",
			files: fstest.MapFS{"a.toml": {Data: []byte(minimal("Alpha"))}},
			want:  "capital letter",
		},
		{
			// A negative cap is a form that takes nothing, with nothing on the
			// page to say so. Refused at load rather than discovered by the
			// first person to submit.
			name: "a negative daily cap",
			files: fstest.MapFS{"alpha.toml": {Data: []byte(
				minimal("alpha", "daily_cap = -1"))}},
			want: "daily cap is negative",
		},

		// Times. This is the strictness the loader is built around.
		{
			name: "a closing time with no offset",
			files: fstest.MapFS{"alpha.toml": {Data: []byte(
				minimal("alpha", `closes_at = "2026-10-17T23:59:59"`))}},
			want: "offset from UTC",
		},
		{
			name: "a closing date with no time",
			files: fstest.MapFS{"alpha.toml": {Data: []byte(
				minimal("alpha", `closes_at = "2026-10-17"`))}},
			want: "offset from UTC",
		},
		{
			name: "a closing time written as a TOML date, not a string",
			files: fstest.MapFS{"alpha.toml": {Data: []byte(
				minimal("alpha", "closes_at = 2026-10-17T23:59:59-05:00"))}},
			// Decoded as a TOML datetime rather than a string, which the wire
			// type refuses outright rather than silently accepting a shape it
			// cannot check the offset of.
			want: "not readable TOML",
		},

		// Money.
		{
			name: "a price that is not an amount",
			files: fstest.MapFS{"alpha.toml": {Data: []byte(
				minimal("alpha") + "\n[[item]]\nid = \"t\"\nlabel = \"T\"\nprice = \"12,00\"\n")}},
			want: "digits only",
		},
		{
			name: "a price in scientific notation",
			files: fstest.MapFS{"alpha.toml": {Data: []byte(
				minimal("alpha") + "\n[[item]]\nid = \"t\"\nlabel = \"T\"\nprice = \"1e3\"\n")}},
			want: "not a plain number",
		},
		{
			name: "a price written as a number rather than a string",
			files: fstest.MapFS{"alpha.toml": {Data: []byte(
				minimal("alpha") + "\n[[item]]\nid = \"t\"\nlabel = \"T\"\nprice = 12.00\n")}},
			// Refused rather than accepted, because a TOML float is the one
			// place a price could pick up a rounding error on its way in.
			want: "not readable TOML",
		},
		{
			name: "an item with no price",
			files: fstest.MapFS{"alpha.toml": {Data: []byte(
				minimal("alpha") + "\n[[item]]\nid = \"t\"\nlabel = \"T\"\n")}},
			want: "has no price",
		},

		// And the definition's own rules still apply, through Check.
		{
			name: "a definition Check refuses",
			files: fstest.MapFS{"alpha.toml": {Data: []byte(
				minimal("alpha") + "\n[[field]]\nname = \"x\"\nlabel = \"X\"\nkind = \"signature\"\n")}},
			want: "not a kind of field",
		},
		{
			name: "a condition naming a later field",
			files: fstest.MapFS{"alpha.toml": {Data: []byte(minimal("alpha") + `
[[field]]
name = "early"
label = "Early"
kind = "text"
[field.show_if]
field = "late"
is = ["yes"]

[[field]]
name = "late"
label = "Late"
kind = "text"
`)}},
			want: "comes after it",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := formtoml.Load(tt.files)
			if err == nil {
				t.Fatal("Load accepted it")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("the report does not mention %q:\n%v", tt.want, err)
			}
		})
	}
}

// A store must refuse to exist if any definition is unusable, and it must
// report all of them rather than the first -- this is a startup failure
// somebody is about to go and fix in files.
func TestLoadReportsEveryBadFile(t *testing.T) {
	_, err := formtoml.Load(fstest.MapFS{
		"alpha.toml": {Data: []byte(minimal("alpha", `closes_at = "2026-10-17"`))},
		"beta.toml":  {Data: []byte(strings.Replace(minimal("beta"), `currency = "usd"`, `currency = "gbp"`, 1))},
		"gamma.toml": {Data: []byte(minimal("gamma"))},
	})
	if err == nil {
		t.Fatal("Load returned a store although two definitions were unusable")
	}

	for _, want := range []string{"alpha.toml", "beta.toml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the report does not mention %s:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "gamma.toml") {
		t.Errorf("the report blames a file that is fine:\n%v", err)
	}
}

// Bounds carry the unit their kind implies, which is the one place a number in
// a definition file means two different things.
func TestBoundsTakeTheUnitOfTheirKind(t *testing.T) {
	store, err := formtoml.Load(fstest.MapFS{"alpha.toml": {Data: []byte(minimal("alpha") + `
[[field]]
name = "guests"
label = "Guests"
kind = "number"
min = "1"
max = "12"

[[field]]
name = "gift"
label = "Gift"
kind = "amount"
min = "5.00"
max = "250.50"

[[field]]
name = "plain"
label = "Plain"
kind = "text"
`)}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	slug, _ := types.ParseSlug("alpha")

	f, err := store.ByID(slug)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	guests, _ := f.Field("guests")
	if guests.Min == nil || *guests.Min != 1 || guests.Max == nil || *guests.Max != 12 {
		t.Errorf("a number field's bounds are %v..%v, want 1..12 as counts", guests.Min, guests.Max)
	}

	gift, _ := f.Field("gift")
	if gift.Min == nil || *gift.Min != 500 || gift.Max == nil || *gift.Max != 25050 {
		t.Errorf("an amount field's bounds are %v..%v, want 500..25050 in cents", gift.Min, gift.Max)
	}

	// Nil, not zero. For a donation with a floor, no minimum and a minimum of
	// nothing are different rules.
	plain, _ := f.Field("plain")
	if plain.Min != nil || plain.Max != nil {
		t.Errorf("an unbounded field came back bounded: %v..%v", plain.Min, plain.Max)
	}
}

// minimal is the smallest definition that loads and passes Check, with any
// extra top-level settings spliced in above the first field table.
//
// Above, and that is the point of the argument rather than a convenience: a
// key appended after `[[field]]` belongs to the field, not to the form. Every
// one of these tests got that wrong at first and reported settings the form
// does not have.
func minimal(id string, top ...string) string {
	return `id = "` + id + `"
title = "A form"
currency = "usd"
return_url = "https://example.test/form"
` + strings.Join(top, "\n") + `

[[field]]
name = "who"
label = "Your name"
kind = "text"
required = true
`
}

// The daily cap reaches the domain type, which is the whole job of the wire
// type: a setting that parses and then goes nowhere is a rule switched off
// with nothing to say so.
func TestTheDailyCapIsRead(t *testing.T) {
	store, err := formtoml.Load(fstest.MapFS{"alpha.toml": {Data: []byte(
		minimal("alpha", "daily_cap = 400"))}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	alpha, _ := types.ParseSlug("alpha")

	f, err := store.ByID(alpha)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if f.DailyCap != 400 {
		t.Errorf("DailyCap = %d, want 400", f.DailyCap)
	}

	// And the ordinary case: a form that says nothing has no cap at all,
	// rather than a cap of zero that would refuse everything.
	plain, err := formtoml.Load(fstest.MapFS{"beta.toml": {Data: []byte(minimal("beta"))}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	beta, _ := types.ParseSlug("beta")

	b, err := plain.ByID(beta)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if b.DailyCap != 0 {
		t.Errorf("a form with no daily_cap came back with %d", b.DailyCap)
	}
}

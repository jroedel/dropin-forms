// Package formtoml reads form definitions from TOML files.
//
// A read-only store. Definitions live in the repository under forms/ and
// are embedded in the binary, so a deploy carries its forms with it and a form
// cannot be half-changed on a running server. The visual builder writes to
// SQLite instead and is a separate store; both produce the same
// [formbus.Form], and both call Stamp and Check, so neither can serve a
// definition the other would refuse.
//
// # Every file is checked at startup
//
// Load refuses to return a store if any definition is unusable, and the
// process is expected to fail rather than start. A form that takes money is
// worth refusing to boot over: the alternative is discovering a broken rule as
// a 500 in front of somebody trying to buy a ticket, or -- worse -- as a
// field whose rule quietly did not apply.
package formtoml

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// ErrNotFound is returned for a slug no definition claims.
var ErrNotFound = errors.New("no such form")

// Store holds every definition read at startup. It is immutable after Load and
// therefore safe to read from any number of request goroutines with no lock.
type Store struct {
	forms map[string]formbus.Form
}

// Load reads every .toml file in fsys and checks all of them.
//
// The file's own name is not the form's name: the slug comes from inside the
// file, and Load asserts the two agree. Deriving the slug from the filename
// would mean a rename silently changes the URL of a form that is already
// pasted into somebody's page.
func Load(fsys fs.FS) (*Store, error) {
	names, err := fs.Glob(fsys, "*.toml")
	if err != nil {
		return nil, fmt.Errorf("the form definitions could not be listed: %w", err)
	}

	if len(names) == 0 {
		return nil, errors.New("there are no form definitions to load")
	}

	// Sorted, so that two definitions claiming one slug are reported the same
	// way on every start rather than depending on directory order.
	slices.Sort(names)

	store := Store{forms: make(map[string]formbus.Form, len(names))}

	var problems []string

	for _, name := range names {
		f, err := loadFile(fsys, name)
		if err != nil {
			problems = append(problems, err.Error())

			continue
		}

		if _, taken := store.forms[f.ID.String()]; taken {
			problems = append(problems, fmt.Sprintf("%s: another file already defines the form %s", name, f.ID))

			continue
		}

		store.forms[f.ID.String()] = f
	}

	if len(problems) > 0 {
		// Every file at once. This is a startup failure somebody is about to
		// go and fix, and fixing them one restart at a time is a bad
		// afternoon.
		return nil, fmt.Errorf("the form definitions cannot be used:\n  - %s", strings.Join(problems, "\n  - "))
	}

	return &store, nil
}

// ByID returns the definition for a slug.
func (s *Store) ByID(slug types.Slug) (formbus.Form, error) {
	f, ok := s.forms[slug.String()]
	if !ok {
		return formbus.Form{}, fmt.Errorf("%w: %s", ErrNotFound, slug)
	}

	return f, nil
}

// All returns every definition, ordered by slug so that a listing page and a
// startup log line are stable.
func (s *Store) All() []formbus.Form {
	out := make([]formbus.Form, 0, len(s.forms))
	for _, f := range s.forms {
		out = append(out, f)
	}

	slices.SortFunc(out, func(a, b formbus.Form) int {
		return strings.Compare(a.ID.String(), b.ID.String())
	})

	return out
}

func loadFile(fsys fs.FS, name string) (formbus.Form, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return formbus.Form{}, fmt.Errorf("%s could not be read: %w", name, err)
	}

	var wire form

	md, err := toml.Decode(string(b), &wire)
	if err != nil {
		return formbus.Form{}, fmt.Errorf("%s is not readable TOML: %w", name, err)
	}

	// A key nobody decoded is a typo, and a typo in a definition is silent:
	// `require_payment` where `payment_required` was meant leaves the rule
	// switched off and nothing says so. The same reasoning as the server's
	// own config loader.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}

		slices.Sort(keys)

		return formbus.Form{}, fmt.Errorf("%s has settings this service does not know: %s", name, strings.Join(keys, ", "))
	}

	f, err := wire.toForm()
	if err != nil {
		return formbus.Form{}, fmt.Errorf("%s: %w", name, err)
	}

	// The slug comes from inside the file; the filename only has to agree.
	if stem := strings.TrimSuffix(path.Base(name), ".toml"); stem != f.ID.String() {
		return formbus.Form{}, fmt.Errorf("%s defines the form %s; name the file %s.toml or change the id", name, f.ID, f.ID)
	}

	f.Stamp()

	if err := f.Check(); err != nil {
		return formbus.Form{}, fmt.Errorf("%s: %w", name, err)
	}

	return f, nil
}

// form is the TOML shape, deliberately separate from [formbus.Form].
//
// Two reasons it is its own type. The domain type holds a types.Slug, a
// types.Money and a time.Time, none of which a person writes directly -- each
// arrives as a string that has to be parsed and refused. And the wire shape is
// a compatibility surface: renaming a field in the domain type should not
// rewrite every definition file, and vice versa.
type form struct {
	ID    string `toml:"id"`
	Title string `toml:"title"`
	Intro string `toml:"intro"`

	// RFC 3339, and the offset is mandatory -- see parseInstant.
	OpensAt  string `toml:"opens_at"`
	ClosesAt string `toml:"closes_at"`

	ClosedNote string `toml:"closed_note"`

	Currency string   `toml:"currency"`
	Origins  []string `toml:"origins"`

	// ReturnURL is where Stripe sends the browser after a payment, and it is
	// the page this form is embedded on rather than anything on this service.
	// Required on a form that sells something; formbus.Form.Check says so.
	ReturnURL string `toml:"return_url"`

	MinPerOrder int `toml:"min_per_order"`
	MaxPerOrder int `toml:"max_per_order"`

	MinTotal string `toml:"min_total"`
	MaxTotal string `toml:"max_total"`

	PaymentRequired bool   `toml:"payment_required"`
	PaymentNote     string `toml:"payment_note"`

	// DailyCap is an abuse control rather than a stock level -- see
	// formbus.Form.DailyCap, which says why the number to write is far above
	// any real day rather than near it.
	DailyCap int `toml:"daily_cap"`

	Confirmation string   `toml:"confirmation"`
	Notify       []string `toml:"notify"`

	Fields []field `toml:"field"`
	Items  []item  `toml:"item"`
}

type field struct {
	Name  string `toml:"name"`
	Label string `toml:"label"`
	Kind  string `toml:"kind"`

	Required bool `toml:"required"`

	Help         string `toml:"help"`
	Placeholder  string `toml:"placeholder"`
	Autocomplete string `toml:"autocomplete"`

	MinLen int `toml:"min_length"`
	MaxLen int `toml:"max_length"`

	// Strings, not numbers, and the unit follows the kind exactly as it does
	// on formbus.Field.Min: a count for a number or a set of choices, and an
	// amount in dollars and cents for an amount field. Strings because "5.00"
	// has to go through the strict money parser rather than through TOML's
	// float decoding, which is the one place a price could pick up a rounding
	// error on its way in.
	Min string `toml:"min"`
	Max string `toml:"max"`

	Pattern     string `toml:"pattern"`
	PatternNote string `toml:"pattern_note"`

	Options []option `toml:"option"`

	ShowIf *condition `toml:"show_if"`
}

type option struct {
	Value string `toml:"value"`
	Label string `toml:"label"`
}

type condition struct {
	Field string   `toml:"field"`
	Is    []string `toml:"is"`
}

type item struct {
	ID    string `toml:"id"`
	Label string `toml:"label"`
	Note  string `toml:"note"`
	Price string `toml:"price"`
	Max   int    `toml:"max"`
}

func (w form) toForm() (formbus.Form, error) {
	f := formbus.Form{
		Title:           w.Title,
		Intro:           w.Intro,
		ClosedNote:      w.ClosedNote,
		Currency:        w.Currency,
		MinPerOrder:     w.MinPerOrder,
		MaxPerOrder:     w.MaxPerOrder,
		PaymentRequired: w.PaymentRequired,
		PaymentNote:     w.PaymentNote,
		DailyCap:        w.DailyCap,
		Confirmation:    w.Confirmation,
		Notify:          w.Notify,
		ReturnURL:       w.ReturnURL,
	}

	slug, err := types.ParseSlug(w.ID)
	if err != nil {
		return formbus.Form{}, err
	}
	f.ID = slug

	if f.OpensAt, err = parseInstant("opens_at", w.OpensAt); err != nil {
		return formbus.Form{}, err
	}
	if f.ClosesAt, err = parseInstant("closes_at", w.ClosesAt); err != nil {
		return formbus.Form{}, err
	}

	if f.MinTotal, err = parseAmount("min_total", w.MinTotal); err != nil {
		return formbus.Form{}, err
	}
	if f.MaxTotal, err = parseAmount("max_total", w.MaxTotal); err != nil {
		return formbus.Form{}, err
	}

	for _, raw := range w.Origins {
		o, err := types.ParseOrigin(raw)
		if err != nil {
			return formbus.Form{}, fmt.Errorf("origins: %w", err)
		}

		f.Origins = append(f.Origins, o)
	}

	for i, wf := range w.Fields {
		fld, err := wf.toField()
		if err != nil {
			return formbus.Form{}, fmt.Errorf("field %d: %w", i+1, err)
		}

		f.Fields = append(f.Fields, fld)
	}

	for i, wi := range w.Items {
		it, err := wi.toItem()
		if err != nil {
			return formbus.Form{}, fmt.Errorf("item %d: %w", i+1, err)
		}

		f.Items = append(f.Items, it)
	}

	return f, nil
}

func (w field) toField() (formbus.Field, error) {
	fld := formbus.Field{
		Name:         w.Name,
		Label:        w.Label,
		Kind:         formbus.Kind(w.Kind),
		Required:     w.Required,
		Help:         w.Help,
		Placeholder:  w.Placeholder,
		Autocomplete: w.Autocomplete,
		MinLen:       w.MinLen,
		MaxLen:       w.MaxLen,
		Pattern:      w.Pattern,
		PatternNote:  w.PatternNote,
	}

	for _, o := range w.Options {
		fld.Options = append(fld.Options, formbus.Option{Value: o.Value, Label: o.Label})
	}

	if w.ShowIf != nil {
		fld.ShowIf = &formbus.Condition{Field: w.ShowIf.Field, Is: w.ShowIf.Is}
	}

	// The unit follows the kind, which is why this cannot be decoded straight
	// into a number: on an amount field "5.00" means 500, and on a number
	// field "5" means 5.
	money := fld.Kind == formbus.KindAmount

	var err error
	if fld.Min, err = parseBound("min", w.Min, money); err != nil {
		return formbus.Field{}, err
	}
	if fld.Max, err = parseBound("max", w.Max, money); err != nil {
		return formbus.Field{}, err
	}

	return fld, nil
}

func (w item) toItem() (formbus.Item, error) {
	price, err := parseAmount("price", w.Price)
	if err != nil {
		return formbus.Item{}, err
	}

	// An item with no price at all is far more likely to be a forgotten line
	// than a deliberately free thing to order, and the cost of guessing wrong
	// is selling tickets for nothing.
	if w.Price == "" {
		return formbus.Item{}, fmt.Errorf("the item %q has no price; write price = \"0.00\" if it really is free", w.ID)
	}

	return formbus.Item{
		ID:    w.ID,
		Label: w.Label,
		Note:  w.Note,
		Price: price,
		Max:   w.Max,
	}, nil
}

// parseInstant reads an RFC 3339 timestamp and requires an explicit offset.
//
// This is the strictness that earns the function. TOML has a native date-time
// type, and BurntSushi decodes a *local* date-time -- one written without an
// offset -- into a time.Time in UTC. So `closes_at = 2026-10-17T23:59:59`
// would silently mean five hours earlier than the author meant, and the form
// would stop taking donations at seven in the evening on the day of the feast.
// Nothing would report it.
//
// Reading the value as a string and parsing it with time.RFC3339 makes the
// offset mandatory: the layout has no optional part, so a value without one
// fails to parse and says so at startup.
func parseInstant(key, raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}

	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %q needs a date, a time and an offset from UTC, like \"2026-10-17T23:59:59-05:00\"", key, raw)
	}

	return t, nil
}

func parseAmount(key, raw string) (types.Money, error) {
	if raw == "" {
		return 0, nil
	}

	m, err := types.ParseMoney(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}

	return m, nil
}

// parseBound reads a field bound, in whichever unit the kind implies.
func parseBound(key, raw string, money bool) (*int64, error) {
	if raw == "" {
		// Nil, not zero. For a donation with a five dollar floor, no minimum
		// and a minimum of nothing are different rules.
		return nil, nil
	}

	if money {
		m, err := types.ParseMoney(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}

		return ptrTo(int64(m)), nil
	}

	n, err := parseCount(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}

	return ptrTo(n), nil
}

func parseCount(raw string) (int64, error) {
	var n int64

	for i := range len(raw) {
		c := raw[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%q must be a whole number, written as a string", raw)
		}

		n = n*10 + int64(c-'0')

		if n > 1_000_000 {
			return 0, fmt.Errorf("%q is larger than this service will count to", raw)
		}
	}

	return n, nil
}

func ptrTo[T any](v T) *T { return &v }

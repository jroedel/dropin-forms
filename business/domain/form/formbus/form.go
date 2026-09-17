// Package formbus holds what a form is and what it means for a submission to
// be acceptable. It is the single source of truth for every rule.
//
// Nothing else in this service states a rule about a field. The server decides
// by calling [Form.Validate]; the browser is handed HTML5 constraint
// attributes derived from the same [Form] and presents them with the
// Constraint Validation API. There is no second copy of any rule in
// JavaScript, which is the whole reason there is no JavaScript validation
// library here: every one of them wants its schema authored in its own
// language, and a hand-maintained second copy of a price or a maximum is the
// bug this package exists to make impossible.
//
// This package knows nothing about HTTP. It takes a map of submitted strings
// and a wall-clock time, and it returns either answers with a derived total or
// typed violations. That is what makes the rules testable without a server and
// what keeps the app layer's job down to choosing a status code.
//
// # The money rule
//
// The browser never sends a price. It sends item selections, quantities and --
// for donation-style forms -- a typed amount. Every total is recomputed here
// from the definition, so a tampered price field is not wrong, it is simply
// not read.
package formbus

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jroedel/dropin-forms/business/types"
)

// Kind is what a field collects.
//
// The set is closed on purpose. Every kind here has a server-side rule in
// validate.go and a rendering in the app layer, and adding one without both is
// a field that either cannot be filled in or cannot be trusted.
type Kind string

const (
	KindText      Kind = "text"      // one line of prose
	KindParagraph Kind = "paragraph" // several lines: a textarea
	KindEmail     Kind = "email"
	KindTel       Kind = "tel"
	KindNumber    Kind = "number"   // a whole number, not money
	KindSelect    Kind = "select"   // one of a list, as a dropdown
	KindRadio     Kind = "radio"    // one of a list, as buttons
	KindCheckbox  Kind = "checkbox" // a single box: agreed, or not
	KindChoices   Kind = "choices"  // several boxes: any number of a list
	KindAmount    Kind = "amount"   // an amount of money the person types
)

// kinds is every Kind, in the order a builder UI should offer them.
var kinds = []Kind{
	KindText, KindParagraph, KindEmail, KindTel, KindNumber,
	KindSelect, KindRadio, KindCheckbox, KindChoices, KindAmount,
}

// Kinds returns every field kind this service can validate, for a builder UI
// to offer. The slice is a copy, so a caller cannot reorder the original.
func Kinds() []Kind { return slices.Clone(kinds) }

// Known reports whether this is a kind this service can validate. A definition
// naming an unknown kind is refused at load time rather than rendered as a
// text box, because silently downgrading a field is how a rule disappears.
func (k Kind) Known() bool { return slices.Contains(kinds, k) }

// HasOptions reports whether the kind draws its permitted values from a list.
// Such a field is validated by membership, so the list is what the person can
// possibly submit -- no separate length or pattern rule applies.
func (k Kind) HasOptions() bool {
	switch k {
	case KindSelect, KindRadio, KindChoices:
		return true
	}

	return false
}

// MultiValue reports whether the kind may legitimately arrive more than once
// in a submission. For every other kind a second value means the request did
// not come from the page we rendered, which is refused rather than resolved by
// taking the first.
func (k Kind) MultiValue() bool { return k == KindChoices }

// Textual reports whether the kind collects free text, and therefore whether
// the length and pattern rules apply to it.
func (k Kind) Textual() bool {
	switch k {
	case KindText, KindParagraph, KindEmail, KindTel:
		return true
	}

	return false
}

// Bounded reports whether the kind's Min and Max mean something. What they
// mean differs by kind and is documented on [Field.Min].
func (k Kind) Bounded() bool {
	switch k {
	case KindNumber, KindAmount, KindChoices:
		return true
	}

	return false
}

// Option is one permitted value of a choice field.
//
// Value is what is submitted, stored and exported; Label is what is shown. They
// are separate so that renaming what a person reads does not rewrite history
// in the submissions already collected.
type Option struct {
	Value string
	Label string
}

// Condition hides a field unless another field holds one of the listed values.
//
// The condition is enforced here and mirrored in the browser. A hidden field
// has its submitted value ignored and its requiredness suspended -- so
// switching a dropdown cannot leave a person unable to submit because of a
// rule attached to a field they cannot see.
//
// A condition may only name a field that appears *earlier* in the definition.
// That is checked at load time, and it is what lets visibility and validation
// happen in one forward pass with no possibility of a cycle.
type Condition struct {
	Field string   // the name of an earlier field
	Is    []string // shown when that field's value is any of these
}

// Field is one thing a person fills in.
type Field struct {
	Name  string // the HTML name, the CSV column, the key in a submission
	Label string // what the person reads
	Kind  Kind

	Required bool

	Help         string // a sentence under the field
	Placeholder  string
	Autocomplete string // an HTML autocomplete token, passed through as given

	// MinLen and MaxLen bound a textual field in runes rather than bytes, so
	// the limit means the same thing to someone typing an accented name as to
	// someone typing ASCII. Zero means unbounded.
	MinLen int
	MaxLen int

	// Min and Max bound a field whose kind is [Kind.Bounded]. Nil means
	// unbounded at that end, which is why these are pointers: for a donation
	// with a $1 floor, a Min of zero and no Min at all are different rules.
	//
	// The unit follows the kind, and this is the one place in the package where
	// a number means different things in different rows:
	//
	//	KindNumber   the number itself      -- 1 to 20 tickets
	//	KindAmount   minor units            -- 500 is a five dollar floor
	//	KindChoices  how many boxes         -- 1 to 3 selections
	Min *int64
	Max *int64

	// Pattern is an RE2 regular expression a textual value must match in full.
	//
	// RE2 rather than JavaScript's dialect, because this is Go's regexp and
	// there is no second engine: no backreferences and no lookahead. The same
	// string is handed to the browser's pattern attribute, so a pattern using
	// anything outside the common subset would be enforced on one side only.
	// Keep patterns to character classes, quantifiers and alternation.
	Pattern string

	// PatternNote is what a person is told when the pattern does not match,
	// and a pattern without one is refused at load time. "Does not match the
	// required format" tells somebody nothing they can act on; "use five
	// digits, like 78745" tells them what to type.
	PatternNote string

	Options []Option

	ShowIf *Condition

	// pattern is Pattern compiled, filled in by Check so that no request pays
	// for a compile and no handler has to deal with a compile failure.
	pattern *regexp.Regexp
}

// Item is a fixed-price thing a form sells, with a quantity per order.
//
// Price lives here, in the definition, and is the only place a price exists.
// Changing it is an edit to the form, which bumps the version, which
// invalidates every submission grant already sitting in somebody's browser --
// so a tab opened at the old price is re-rendered at the new one rather than
// charged a number nobody agreed to. That is why there is no price schedule
// and no effective window: the version is the mechanism.
type Item struct {
	ID    string // stable; it is stored on every submission
	Label string
	Note  string
	Price types.Money

	// Max is the most of this one item a single order may contain. Zero means
	// no item-specific cap, in which case [Form.MaxPerOrder] still applies.
	Max int
}

// Form is a versioned definition: the fields, the things for sale, who may
// embed it, and when it takes submissions.
type Form struct {
	ID types.Slug

	// Version is a fingerprint of everything in this definition that affects
	// what a submission means, and it is computed rather than typed. [Stamp]
	// sets it; Check refuses a form where it does not match the content.
	//
	// It is computed because the safety of changing a price depends on it. A
	// submission grant pins the version it was minted against, so an edit that
	// changes the version invalidates every grant already sitting in somebody's
	// browser, and a tab opened at the old price is re-rendered at the new one
	// rather than charged a number nobody agreed to. That is the whole
	// mechanism -- there is no price schedule and no timed cutover.
	//
	// An author-maintained integer would have worked exactly as long as
	// everybody remembered to increment it. The most likely moment to forget
	// is the moment it matters: editing a price late at night before a
	// deadline. So the field is not something anybody can forget.
	Version string

	Title string
	Intro string

	// OpensAt and ClosesAt are absolute instants, and deliberately not a date
	// plus a local time. "Closes on the 9th at midnight" is ambiguous about
	// which midnight in which offset, and resolving it at submission time in
	// whatever zone the process happens to run in is the classic version of
	// this bug. The definition store is responsible for turning whatever an
	// author wrote into an instant.
	//
	// A zero OpensAt means already open; a zero ClosesAt means never closes.
	OpensAt  time.Time
	ClosesAt time.Time

	// ClosedNote is shown instead of the form when it is not taking
	// submissions -- where to write, or when it will open. Empty gets a
	// generic sentence.
	ClosedNote string

	// Currency is a lowercase ISO 4217 code, as Stripe wants it.
	Currency string

	// Origins are the sites permitted to frame this form. An empty list is
	// honoured as written and yields frame-ancestors 'none': the page still
	// works when opened directly, and nothing can embed it. That is the
	// fail-closed default and is not an error.
	Origins []types.Origin

	Fields []Field
	Items  []Item

	// MinPerOrder and MaxPerOrder bound the quantities of all items added
	// together. Zero means unbounded at that end.
	//
	// The minimum is what refuses an order of nothing on a form where an order
	// of nothing is wrong -- a ticket form submitted with the quantity left at
	// zero. It is separate from MinTotal because the sentence somebody needs
	// is "choose at least one ticket", not "the smallest order we take is
	// $12.00".
	MinPerOrder int
	MaxPerOrder int

	// MinTotal and MaxTotal bound the derived total when it is non-zero. A
	// floor makes a form a poor instrument for card testing; a ceiling turns a
	// runaway quantity into a refusal rather than a charge.
	MinTotal types.Money
	MaxTotal types.Money

	// PaymentRequired refuses a submission that comes to nothing.
	//
	// The shape that needs it: a page selling optional lunch tickets and also
	// taking donations. Neither is individually required, so without this rule
	// a submission with no ticket and no donation is accepted -- a row nobody
	// can act on, and one the payment step would have to special-case because
	// there is nothing to charge.
	//
	// It is not the same rule as MinPerOrder, which counts items, or MinTotal,
	// which sets a floor above zero. This one only asks that the submission be
	// worth something.
	PaymentRequired bool

	// PaymentNote is what somebody is told when it comes to nothing, and a
	// form with PaymentRequired and no note is refused at load time -- the
	// same bargain as Pattern and PatternNote.
	//
	// The sentence has to be written by the author because only the author
	// knows what is on the page. "This form requires a payment" tells nobody
	// which of the two things in front of them to fill in; "Choose at least
	// one lunch ticket, or enter a donation for the Shrine" tells them
	// exactly.
	PaymentNote string

	// Confirmation is the message rendered in place after a successful
	// submission -- the common path, in the iframe, with no navigation.
	Confirmation string

	// Notify are addresses told about each submission, over and above the
	// accounts holding the results role on this form.
	Notify []string
}

// Currencies this service will take. One, today. The list exists so that
// adding a second is a deliberate edit that makes somebody check the minimum
// below at the same time.
var currencies = []string{"usd"}

// stripeMinimum is the smallest charge Stripe accepts, per currency, in minor
// units. Verify against Stripe's published table before adding a currency --
// these differ per currency and Stripe has changed them.
var stripeMinimum = map[string]types.Money{
	"usd": 50,
}

// reserved are name prefixes the app layer uses for its own inputs: item
// quantities arrive as qty_<item id>, and our hidden inputs are prefixed
// dropin_. A field claiming one of those would shadow the real thing, so the
// collision is refused at load time rather than discovered as a wrong total.
var reserved = []string{"qty_", "dropin_"}

const fieldNameMaxLen = 40

// DefinitionError is a form that is not a valid definition. It lists every
// problem rather than the first, because it is a startup failure that somebody
// is about to go and fix in a file, and fixing them one restart at a time is a
// bad afternoon.
type DefinitionError struct {
	FormID   string
	Problems []string
}

func (e DefinitionError) Error() string {
	name := e.FormID
	if name == "" {
		name = "(unnamed)"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "the form %s cannot be used:", name)

	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}

	return b.String()
}

// Check reports every way this definition is unusable, and compiles the
// patterns it will need later.
//
// Called by whatever loads definitions, at startup. A definition that fails
// here must not be served: the alternative is a 500 in front of somebody
// trying to buy a ticket, or worse, a field whose rule quietly did not apply.
func (f *Form) Check() error {
	var p []string
	add := func(format string, args ...any) {
		p = append(p, fmt.Sprintf(format, args...))
	}

	if f.ID.Zero() {
		add("it has no name")
	}
	switch {
	case f.Version == "":
		add("it has no version; whatever built it must call Stamp")
	case f.Version != f.Fingerprint():
		// Structural rather than advisory: no store can serve a form whose
		// version has stopped tracking its content, whichever store it is.
		add("its version no longer matches its content; whatever built it must call Stamp after changing it")
	}
	if strings.TrimSpace(f.Title) == "" {
		add("it has no title")
	}
	if !slices.Contains(currencies, f.Currency) {
		add("its currency is %q; this service handles %s", f.Currency, strings.Join(currencies, ", "))
	}
	if !f.OpensAt.IsZero() && !f.ClosesAt.IsZero() && !f.ClosesAt.After(f.OpensAt) {
		add("it closes at %s, which is not after it opens at %s",
			f.ClosesAt.Format(time.RFC3339), f.OpensAt.Format(time.RFC3339))
	}
	if len(f.Fields) == 0 {
		add("it has no fields")
	}
	if f.MinPerOrder < 0 {
		add("its per-order minimum is negative")
	}
	if f.MaxPerOrder < 0 {
		add("its per-order maximum is negative")
	}
	if f.MaxPerOrder > 0 && f.MinPerOrder > f.MaxPerOrder {
		add("its per-order minimum %d is above its per-order maximum %d", f.MinPerOrder, f.MaxPerOrder)
	}
	switch {
	case f.PaymentRequired && !f.Sells():
		add("it requires a payment and has nothing to pay for")
	case f.PaymentRequired && strings.TrimSpace(f.PaymentNote) == "":
		add("it requires a payment but has nothing to tell somebody who submits it empty")
	case !f.PaymentRequired && f.PaymentNote != "":
		add("it explains a payment it does not require")
	}
	if f.MinPerOrder > 0 && len(f.Items) == 0 {
		add("it requires at least %d of something and has nothing for sale", f.MinPerOrder)
	}
	if f.MinTotal < 0 {
		add("its minimum total is negative")
	}
	if f.MaxTotal < 0 {
		add("its maximum total is negative")
	}
	if f.MaxTotal > 0 && f.MinTotal > f.MaxTotal {
		add("its minimum total %s is above its maximum total %s", f.MinTotal, f.MaxTotal)
	}

	p = append(p, f.checkFields()...)
	p = append(p, f.checkItems()...)

	if len(p) > 0 {
		return DefinitionError{FormID: f.ID.String(), Problems: p}
	}

	return nil
}

func (f *Form) checkFields() []string {
	var p []string
	add := func(format string, args ...any) {
		p = append(p, fmt.Sprintf(format, args...))
	}

	seen := make(map[string]int, len(f.Fields))

	for i := range f.Fields {
		fld := &f.Fields[i]
		where := fmt.Sprintf("field %d", i+1)
		if fld.Name != "" {
			where = fmt.Sprintf("field %q", fld.Name)
		}

		switch problem := checkFieldName(fld.Name); {
		case problem != "":
			add("%s: %s", where, problem)
		default:
			if at, dup := seen[fld.Name]; dup {
				add("%s: there is already a field with that name, at position %d", where, at+1)
			}
			seen[fld.Name] = i
		}

		if strings.TrimSpace(fld.Label) == "" {
			add("%s: it has no label", where)
		}
		if !fld.Kind.Known() {
			add("%s: %q is not a kind of field this service knows", where, fld.Kind)

			// Every check below asks the kind a question, and the answers are
			// meaningless for a kind that does not exist.
			continue
		}

		p = append(p, fld.checkOptions(where)...)
		p = append(p, fld.checkBounds(where)...)
		p = append(p, fld.checkPattern(where)...)
		p = append(p, f.checkCondition(fld, where, i)...)
	}

	return p
}

// checkFieldName returns why the name is unusable, or "".
//
// Narrower than a slug: a field name is an HTML name attribute, a CSV column
// heading and a key in exported JSON, so underscores are in and hyphens are
// out -- a hyphen makes the name awkward to reach in every consumer a CSV ends
// up in.
func checkFieldName(name string) string {
	switch {
	case name == "":
		return "it has no name"
	case len(name) > fieldNameMaxLen:
		return fmt.Sprintf("its name is longer than %d characters", fieldNameMaxLen)
	case name[0] < 'a' || name[0] > 'z':
		return fmt.Sprintf("its name %q must begin with a lowercase letter", name)
	}

	for i := range len(name) {
		c := name[i]

		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
		default:
			return fmt.Sprintf("its name %q may only contain lowercase letters, digits and underscores", name)
		}
	}

	for _, prefix := range reserved {
		if strings.HasPrefix(name, prefix) {
			return fmt.Sprintf("its name %q begins with %q, which this service uses for its own inputs", name, prefix)
		}
	}

	return ""
}

func (fld *Field) checkOptions(where string) []string {
	var p []string
	add := func(format string, args ...any) {
		p = append(p, fmt.Sprintf(format, args...))
	}

	if !fld.Kind.HasOptions() {
		if len(fld.Options) > 0 {
			add("%s: a %s field has no list to choose from, so its options would never be shown", where, fld.Kind)
		}

		return p
	}

	if len(fld.Options) == 0 {
		add("%s: a %s field needs at least one option", where, fld.Kind)

		return p
	}

	seen := make(map[string]bool, len(fld.Options))

	for j, opt := range fld.Options {
		switch {
		case opt.Value == "":
			// An empty value is indistinguishable from the field being left
			// blank, so a required field with one could be satisfied by
			// submitting nothing.
			add("%s: option %d has no value", where, j+1)
		case strings.TrimSpace(opt.Value) != opt.Value:
			add("%s: option %q has a space at one end, which a browser will not send back unchanged", where, opt.Value)
		case seen[opt.Value]:
			add("%s: there is more than one option with the value %q", where, opt.Value)
		default:
			seen[opt.Value] = true
		}

		if strings.TrimSpace(opt.Label) == "" {
			add("%s: the option %q has no label", where, opt.Value)
		}
	}

	return p
}

func (fld *Field) checkBounds(where string) []string {
	var p []string
	add := func(format string, args ...any) {
		p = append(p, fmt.Sprintf(format, args...))
	}

	if fld.Kind.Bounded() {
		switch {
		case fld.Min != nil && *fld.Min < 0:
			add("%s: its minimum %d is negative", where, *fld.Min)
		case fld.Min != nil && fld.Max != nil && *fld.Min > *fld.Max:
			add("%s: its minimum %d is above its maximum %d", where, *fld.Min, *fld.Max)
		}

		if fld.Kind == KindAmount && fld.Max != nil && types.Money(*fld.Max) > types.MaxAmount() {
			add("%s: its maximum is larger than this service will take", where)
		}
	} else if fld.Min != nil || fld.Max != nil {
		add("%s: a %s field has no number to bound; use a length instead", where, fld.Kind)
	}

	if fld.Kind.Textual() {
		switch {
		case fld.MinLen < 0 || fld.MaxLen < 0:
			add("%s: its length bounds cannot be negative", where)
		case fld.MaxLen > 0 && fld.MinLen > fld.MaxLen:
			add("%s: its minimum length %d is above its maximum length %d", where, fld.MinLen, fld.MaxLen)
		}
	} else if fld.MinLen > 0 || fld.MaxLen > 0 {
		add("%s: a %s field has no text to measure", where, fld.Kind)
	}

	return p
}

func (fld *Field) checkPattern(where string) []string {
	if fld.Pattern == "" {
		if fld.PatternNote != "" {
			return []string{fmt.Sprintf("%s: it explains a pattern it does not have", where)}
		}

		return nil
	}

	var p []string

	if !fld.Kind.Textual() {
		p = append(p, fmt.Sprintf("%s: a %s field has no text for a pattern to match", where, fld.Kind))
	}
	if strings.TrimSpace(fld.PatternNote) == "" {
		p = append(p, fmt.Sprintf("%s: it has a pattern but nothing to tell somebody whose answer does not match it", where))
	}

	// Anchored here rather than in the definition, so that an author cannot
	// write a pattern that matches a substring and believe it matches the
	// whole value -- which is how a validated field accepts anything with a
	// valid prefix. \A and \z rather than ^ and $, which in Go match at line
	// boundaries and would let a newline smuggle the rest of a value past.
	re, err := regexp.Compile(`\A(?:` + fld.Pattern + `)\z`)
	if err != nil {
		p = append(p, fmt.Sprintf("%s: its pattern is not one this service can read: %s", where, err))

		return p
	}

	fld.pattern = re

	return p
}

func (f *Form) checkCondition(fld *Field, where string, at int) []string {
	if fld.ShowIf == nil {
		return nil
	}

	var p []string
	add := func(format string, args ...any) {
		p = append(p, fmt.Sprintf(format, args...))
	}

	cond := fld.ShowIf

	if cond.Field == fld.Name {
		add("%s: it is shown only when it holds a value itself, which it can never do", where)

		return p
	}

	// Only an earlier field, which is what guarantees no cycle and lets
	// Validate resolve visibility in one forward pass.
	i := slices.IndexFunc(f.Fields[:at], func(c Field) bool { return c.Name == cond.Field })
	if i < 0 {
		later := slices.IndexFunc(f.Fields[at:], func(c Field) bool { return c.Name == cond.Field })
		if later >= 0 {
			add("%s: it is shown depending on %q, which comes after it; move %q above it",
				where, cond.Field, cond.Field)
		} else {
			add("%s: it is shown depending on %q, and there is no such field", where, cond.Field)
		}

		return p
	}

	if len(cond.Is) == 0 {
		add("%s: it is shown depending on %q but no value is named, so it would never be shown", where, cond.Field)

		return p
	}

	// A typo in a condition's value is invisible at runtime -- the field
	// simply never appears -- so it is caught here, where the list of
	// possible values is known.
	on := f.Fields[i]
	if on.Kind.HasOptions() {
		for _, want := range cond.Is {
			if !slices.ContainsFunc(on.Options, func(o Option) bool { return o.Value == want }) {
				add("%s: it is shown when %q is %q, which is not one of that field's options",
					where, cond.Field, want)
			}
		}
	}

	return p
}

func (f *Form) checkItems() []string {
	var p []string
	add := func(format string, args ...any) {
		p = append(p, fmt.Sprintf(format, args...))
	}

	seen := make(map[string]bool, len(f.Items))

	for i, it := range f.Items {
		where := fmt.Sprintf("item %d", i+1)
		if it.ID != "" {
			where = fmt.Sprintf("item %q", it.ID)
		}

		// An item ID becomes part of the quantity input's name, so it takes
		// the field-name rules rather than looser ones.
		if problem := checkFieldName(it.ID); problem != "" {
			add("%s: %s", where, problem)
		} else if seen[it.ID] {
			add("%s: there is already an item with that name", where)
		} else {
			seen[it.ID] = true
		}

		if strings.TrimSpace(it.Label) == "" {
			add("%s: it has no label", where)
		}
		if it.Price < 0 {
			add("%s: its price is negative", where)
		}
		if it.Price > types.MaxAmount() {
			add("%s: its price is larger than this service will take", where)
		}
		if it.Max < 0 {
			add("%s: its per-order maximum is negative", where)
		}
		if !utf8.ValidString(it.Label) {
			add("%s: its label is not valid text", where)
		}
	}

	return p
}

// Field finds a field by name.
func (f Form) Field(name string) (Field, bool) {
	i := slices.IndexFunc(f.Fields, func(c Field) bool { return c.Name == name })
	if i < 0 {
		return Field{}, false
	}

	return f.Fields[i], true
}

// Item finds an item by its ID.
func (f Form) Item(id string) (Item, bool) {
	i := slices.IndexFunc(f.Items, func(c Item) bool { return c.ID == id })
	if i < 0 {
		return Item{}, false
	}

	return f.Items[i], true
}

// Sells reports whether this form can produce a charge. A form that sells
// nothing needs no payment step at all, and the app layer branches on this
// rather than on whether a particular total happened to come out at zero.
func (f Form) Sells() bool {
	if len(f.Items) > 0 {
		return true
	}

	return slices.ContainsFunc(f.Fields, func(c Field) bool { return c.Kind == KindAmount })
}

// Open reports whether the form takes submissions at this instant.
func (f Form) Open(now time.Time) bool {
	if !f.OpensAt.IsZero() && now.Before(f.OpensAt) {
		return false
	}
	if !f.ClosesAt.IsZero() && !now.Before(f.ClosesAt) {
		return false
	}

	return true
}

// symbols is how an amount is written for a person, per currency.
//
// A currency symbol is presentation, and types.Money deliberately has none --
// but the sentences in a violation are written here, in the only place that
// knows which currency a form sells in. So the table lives next to the list of
// currencies this service handles, and adding one means adding both rows.
var symbols = map[string]string{
	"usd": "$",
}

// show writes an amount the way a person reads it. An unknown currency falls
// back to the bare number rather than guessing a symbol; Check refuses such a
// form at load time, so this is the belt to that braces.
func show(m types.Money, currency string) string {
	return symbols[currency] + m.String()
}

// Symbol is the currency's symbol on its own, for a page that puts one beside
// an input rather than in front of a number.
//
// An unknown currency gives an empty string, the same way Show falls back to
// the bare number. Check refuses such a form at load time.
func Symbol(currency string) string { return symbols[currency] }

// Show is show, for the app layer.
//
// Exported rather than left to whatever renders a page, because the table it
// reads is here -- next to the list of currencies this service handles -- and
// a second copy in a template helper is a second thing to remember when a
// currency is added. A price beside a field and a price inside a violation
// have to be written the same way, and this is the only way to be sure they
// are.
func Show(m types.Money, currency string) string { return show(m, currency) }

// Stamp sets Version from the definition's content, and is what every store
// calls after building a Form and before Check.
func (f *Form) Stamp() { f.Version = f.Fingerprint() }

// Fingerprint is a short hash of everything in this definition that changes
// what a submission means.
//
// Everything that changes meaning, and nothing that does not. A price, a
// bound, an option's value, a closing time and the field order are all in;
// help text, placeholders and labels are out. That line is where the value of
// this function lives: a grant is invalidated whenever the fingerprint
// changes, so hashing a typo fix in a help sentence would throw away every
// half-filled form on the site for no reason, and hashing too little would
// let a price change through unnoticed.
//
// An option's Label is the one judgement call. It is excluded, on the grounds
// that renaming what a person reads does not change what their answer means --
// the Value is what is stored, and the Value is hashed.
//
// SHA-256 truncated to 12 hex characters. Not a security boundary: a grant is
// signed with an HMAC over the fingerprint, so forging one means forging the
// MAC, and the fingerprint only has to be long enough that two versions of one
// form do not collide by accident.
func (f Form) Fingerprint() string {
	h := sha256.New()

	// Length-prefixed, never plain concatenation. Two definitions whose
	// concatenated fields differ only in where one string ends and the next
	// begins would otherwise hash the same -- the same ambiguity the
	// submission grant is length-prefixed to avoid.
	write := func(parts ...string) {
		for _, p := range parts {
			fmt.Fprintf(h, "%d:%s|", len(p), p)
		}
	}
	num := func(n int64) { fmt.Fprintf(h, "%d|", n) }
	opt := func(p *int64) {
		if p == nil {
			// Distinct from a present zero: for a donation with a floor, no
			// minimum and a minimum of nothing are different rules.
			h.Write([]byte("-|"))

			return
		}
		num(*p)
	}
	when := func(t time.Time) {
		if t.IsZero() {
			h.Write([]byte("-|"))

			return
		}
		// UTC, so that writing the same instant with a different offset is the
		// same version.
		num(t.UTC().UnixMilli())
	}

	write("form", f.ID.String(), f.Currency)
	when(f.OpensAt)
	when(f.ClosesAt)
	num(int64(f.MinPerOrder))
	num(int64(f.MaxPerOrder))
	num(int64(f.MinTotal))
	num(int64(f.MaxTotal))

	if f.PaymentRequired {
		h.Write([]byte("pay|"))
	}

	for _, o := range f.Origins {
		write("origin", o.String())
	}

	for _, fld := range f.Fields {
		write("field", fld.Name, string(fld.Kind), fld.Pattern)
		num(int64(fld.MinLen))
		num(int64(fld.MaxLen))
		opt(fld.Min)
		opt(fld.Max)

		if fld.Required {
			h.Write([]byte("req|"))
		}

		for _, o := range fld.Options {
			write("option", o.Value)
		}

		if fld.ShowIf != nil {
			write("showif", fld.ShowIf.Field)
			write(fld.ShowIf.Is...)
		}
	}

	for _, it := range f.Items {
		write("item", it.ID)
		num(int64(it.Price))
		num(int64(it.Max))
	}

	return hex.EncodeToString(h.Sum(nil))[:12]
}

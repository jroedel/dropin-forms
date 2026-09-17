package formbus

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jroedel/dropin-forms/business/types"
)

// QuantityField is the input name a quantity arrives under, and the field a
// violation about it is keyed by.
//
// One function rather than a string built in two places. The app layer renders
// the input and this package reads it, and a disagreement between them is a
// form whose quantity is silently always zero -- not a crash, not a violation,
// just a submission for nothing.
func QuantityField(itemID string) string { return "qty_" + itemID }

// Values is a submitted body: field names to the values sent under them.
//
// Its own type rather than net/url.Values, so this package does not import a
// web shape. The underlying type is the same, so the app layer converts for
// free with formbus.Values(r.PostForm).
type Values map[string][]string

// Violation is one thing wrong with a submission, in a sentence meant for the
// person who submitted it.
//
// Field is the name of the field it belongs beside, or empty for a problem with
// the order as a whole. Message includes the field's label, so the same
// sentence works both in the slot next to the field and in the error summary a
// screen reader reads at the top.
type Violation struct {
	Field   string
	Message string
}

// Invalid is a submission that broke the form's rules. It carries every
// violation, because sending somebody back to fix one field at a time is the
// worst version of this experience.
type Invalid struct {
	Violations []Violation
}

func (inv Invalid) Error() string {
	msgs := make([]string, 0, len(inv.Violations))
	for _, v := range inv.Violations {
		msgs = append(msgs, v.Message)
	}

	return strings.Join(msgs, " ")
}

// For returns the violations belonging to one field, for a template rendering
// that field's slot.
func (inv Invalid) For(field string) []Violation {
	var out []Violation

	for _, v := range inv.Violations {
		if v.Field == field {
			out = append(out, v)
		}
	}

	return out
}

// Closed is a form that is not taking submissions right now.
//
// A distinct type from [Invalid] because the app answers it differently: there
// is nothing for the person to correct, so the page shows the form's closing
// note instead of the form with errors on it.
type Closed struct {
	FormID   types.Slug
	Note     string
	OpensAt  time.Time
	ClosesAt time.Time
}

func (c Closed) Error() string {
	if c.Note != "" {
		return c.Note
	}

	return "This form is not accepting submissions."
}

// NotYetOpen distinguishes "too early" from "too late", which want different
// sentences: one is worth coming back for.
func (c Closed) NotYetOpen(now time.Time) bool {
	return !c.OpensAt.IsZero() && now.Before(c.OpensAt)
}

// Answer is what one field collected.
type Answer struct {
	Name   string
	Label  string
	Kind   Kind
	Values []string
}

// Value is the single answer, for the kinds that have exactly one.
func (a Answer) Value() string {
	if len(a.Values) == 0 {
		return ""
	}

	return a.Values[0]
}

// Line is one item ordered, priced from the definition.
type Line struct {
	ItemID string
	Label  string
	Price  types.Money
	Qty    int
	Amount types.Money
}

// Answers is an accepted submission: what was filled in, what was ordered, and
// what it comes to.
//
// Fields is an ordered slice rather than a map, and the order is the
// definition's. A map would be more convenient to look things up in and would
// make the CSV export's column order depend on Go's map iteration, which is
// deliberately random -- so two exports of the same form would not have the
// same columns.
//
// Only fields that were both visible and answered appear. A field hidden by
// its condition is absent entirely, and so is an optional field left blank --
// so the record says "not answered" rather than "answered with nothing", which
// is the honest account of what the person was asked and what they said. A
// consumer that needs a column per field, such as the CSV export, walks the
// definition and looks each one up here.
type Answers struct {
	FormID types.Slug

	// Version is the fingerprint of the definition these answers were checked
	// against. Stored with the submission, so a row read back a year later can
	// be read against the rules that actually applied to it rather than
	// against whatever the form says today.
	Version string

	Currency string
	Fields   []Answer
	Lines    []Line
	Total    types.Money
}

// Field finds an answer by field name.
func (a Answers) Field(name string) (Answer, bool) {
	i := slices.IndexFunc(a.Fields, func(c Answer) bool { return c.Name == name })
	if i < 0 {
		return Answer{}, false
	}

	return a.Fields[i], true
}

// SubmitterEmail returns the address to send a receipt to, and whether the
// form collected one at all.
//
// The first answered email field, in the definition's order. A convention
// rather than a declared role, and it is the right one as long as a form asks
// for the submitter's address before it asks for anybody else's -- which is
// the order every form here is written in, because that is the order a person
// fills one in. If a form ever needs to distinguish "your address" from "the
// address to mail the tickets to", that becomes a flag on the field and this
// reads it; until then a second flag nobody sets is worse than a convention
// written down.
func (a Answers) SubmitterEmail() (types.Email, bool) {
	for _, f := range a.Fields {
		if f.Kind != KindEmail {
			continue
		}

		// Already validated, so a parse failure here is impossible. Handled
		// rather than discarded so that it cannot silently become the zero
		// address if the validator ever changes.
		email, err := types.ParseEmail(f.Value())
		if err != nil {
			continue
		}

		return email, true
	}

	return types.Email{}, false
}

// Validate decides whether a submission is acceptable and, if it is, what it
// comes to.
//
// It returns [Closed] if the form is not taking submissions, [Invalid] with
// every violation if any rule was broken, and otherwise the answers with a
// total derived entirely from the definition. On any error the returned
// [Answers] is the zero value: there is no half-accepted submission.
//
// now is passed in rather than read from the clock, so that the open and close
// windows are testable and so that one request decides its own time once.
func (f Form) Validate(now time.Time, in Values) (Answers, error) {
	if !f.Open(now) {
		return Answers{}, Closed{
			FormID:   f.ID,
			Note:     f.ClosedNote,
			OpensAt:  f.OpensAt,
			ClosesAt: f.ClosesAt,
		}
	}

	ans := Answers{
		FormID:   f.ID,
		Version:  f.Version,
		Currency: f.Currency,
	}

	var vs []Violation

	// One forward pass. Visibility and validation happen together because a
	// condition may only name an earlier field -- Check enforces that -- so by
	// the time a field is reached, everything it could depend on is decided.
	index := make(map[string]int, len(f.Fields))
	for i, fld := range f.Fields {
		index[fld.Name] = i
	}

	visible := make([]bool, len(f.Fields))
	taken := make([][]string, len(f.Fields))

	var typed types.Money

	for i, fld := range f.Fields {
		visible[i] = shown(fld, index, visible, taken)
		if !visible[i] {
			// Ignored, not refused. A value arriving for a hidden field is
			// what happens when somebody fills a field in and then changes the
			// dropdown above it, and treating that as tampering would refuse
			// ordinary use of the form.
			continue
		}

		values, problems := fld.take(in[fld.Name], f.Currency)
		vs = append(vs, problems...)
		taken[i] = values

		if len(values) == 0 {
			continue
		}

		if fld.Kind == KindAmount {
			// take has already reported an unparseable amount, so the error
			// here is not worth a second violation -- but it is worth not
			// adding a zero silently, because a caller reading Total without
			// checking the error would see a plausible number.
			if amount, err := types.ParseMoney(values[0]); err == nil {
				sum, err := typed.Add(amount)
				if err != nil {
					vs = append(vs, Violation{
						Field:   fld.Name,
						Message: "That amount is larger than we can take.",
					})

					continue
				}

				typed = sum
			}
		}

		ans.Fields = append(ans.Fields, Answer{
			Name:   fld.Name,
			Label:  fld.Label,
			Kind:   fld.Kind,
			Values: values,
		})
	}

	lines, ordered, charged, problems := f.order(in)
	vs = append(vs, problems...)
	ans.Lines = lines

	total, err := charged.Add(typed)
	if err != nil {
		vs = append(vs, Violation{Message: "That order is larger than we can take."})
	} else {
		ans.Total = total
	}

	vs = append(vs, f.checkOrderSize(ordered)...)
	vs = append(vs, f.checkTotal(ans.Total)...)

	if len(vs) > 0 {
		return Answers{}, Invalid{Violations: vs}
	}

	return ans, nil
}

// shown decides whether a field's condition is satisfied.
//
// Fails closed: a condition naming a field that does not exist hides the
// field. Check refuses such a definition at load time, so reaching this is a
// bug -- and hiding a field is the safe way to be wrong, because the
// alternative shows a field whose rules were never checked.
func shown(fld Field, index map[string]int, visible []bool, taken [][]string) bool {
	if fld.ShowIf == nil {
		return true
	}

	on, ok := index[fld.ShowIf.Field]
	if !ok || !visible[on] {
		return false
	}

	return slices.ContainsFunc(taken[on], func(got string) bool {
		return slices.Contains(fld.ShowIf.Is, got)
	})
}

// take cleans and checks the values submitted for one field, returning what to
// keep and what was wrong with it.
func (fld Field) take(raw []string, currency string) ([]string, []Violation) {
	// A second value for a single-value field did not come from the page we
	// rendered. Refused rather than resolved by taking the first, because
	// which one a caller would have taken is exactly what an attacker would be
	// choosing between.
	if !fld.Kind.MultiValue() && len(raw) > 1 {
		return nil, []Violation{{
			Field:   fld.Name,
			Message: fmt.Sprintf("We could not read your answer for %s. Please reload the form and try again.", fld.Label),
		}}
	}

	values := make([]string, 0, len(raw))

	for _, s := range raw {
		// A textarea submits CRLF per the HTML spec, so normalising is not
		// defensive -- it is reading the format the browser actually sends.
		// Done before trimming, or a value ending in CRLF keeps a stray \n.
		s = strings.ReplaceAll(s, "\r\n", "\n")
		s = strings.ReplaceAll(s, "\r", "\n")
		s = strings.TrimSpace(s)

		if s == "" {
			// An empty value is an unfilled field, not an answer of "". This
			// is what makes an unchecked checkbox and a cleared text box the
			// same thing.
			continue
		}

		values = append(values, s)
	}

	if len(values) == 0 {
		if fld.Required {
			return nil, []Violation{{Field: fld.Name, Message: fld.missing()}}
		}

		return nil, nil
	}

	var vs []Violation

	for _, s := range values {
		if !utf8.ValidString(s) {
			return nil, []Violation{{
				Field:   fld.Name,
				Message: fmt.Sprintf("We could not read what you typed in %s. Please retype it.", fld.Label),
			}}
		}

		if bad := strings.IndexFunc(s, func(r rune) bool {
			// Control characters are stripped by no browser and typed by no
			// person: they arrive from a paste of something binary, or from a
			// crafted request. A newline is allowed only where a newline makes
			// sense, and tab is not -- it would break a CSV cell.
			return r < 0x20 && !(r == '\n' && fld.Kind == KindParagraph) || r == 0x7f
		}); bad >= 0 {
			vs = append(vs, Violation{
				Field:   fld.Name,
				Message: fmt.Sprintf("%s cannot contain that character.", fld.Label),
			})

			return nil, vs
		}
	}

	return values, append(vs, fld.check(values, currency)...)
}

// missing is the sentence for a required field left blank. It differs by kind
// because "Terms is required" is a worse sentence than "Please tick Terms".
func (fld Field) missing() string {
	switch fld.Kind {
	case KindCheckbox:
		return fmt.Sprintf("Please tick %s.", fld.Label)
	case KindSelect, KindRadio, KindChoices:
		return fmt.Sprintf("Please choose %s.", fld.Label)
	}

	return fmt.Sprintf("Please fill in %s.", fld.Label)
}

// check applies the rules that belong to the kind, on values already cleaned
// and known non-empty.
func (fld Field) check(values []string, currency string) []Violation {
	switch fld.Kind {
	case KindSelect, KindRadio:
		return fld.checkChoice(values[0])
	case KindChoices:
		return fld.checkChoices(values)
	case KindCheckbox:
		// Any value at all means checked, because a browser sends the value
		// attribute if there is one and "on" if there is not, and depending on
		// which would be depending on how the field was rendered.
		return nil
	case KindNumber:
		return fld.checkNumber(values[0])
	case KindAmount:
		return fld.checkAmount(values[0], currency)
	case KindEmail:
		return append(fld.checkEmail(values[0]), fld.checkText(values[0])...)
	}

	return fld.checkText(values[0])
}

func (fld Field) checkText(s string) []Violation {
	var vs []Violation

	// Runes, not bytes. A name limited to 40 bytes is a name limited to 40
	// Latin letters or 13 Chinese characters, which is not one limit.
	n := utf8.RuneCountInString(s)

	switch {
	case fld.MinLen > 0 && n < fld.MinLen:
		vs = append(vs, Violation{
			Field:   fld.Name,
			Message: fmt.Sprintf("%s needs at least %d characters.", fld.Label, fld.MinLen),
		})
	case fld.MaxLen > 0 && n > fld.MaxLen:
		vs = append(vs, Violation{
			Field:   fld.Name,
			Message: fmt.Sprintf("%s can be at most %d characters, and you typed %d.", fld.Label, fld.MaxLen, n),
		})
	}

	if fld.pattern != nil && !fld.pattern.MatchString(s) {
		vs = append(vs, Violation{
			Field:   fld.Name,
			Message: fmt.Sprintf("%s: %s", fld.Label, fld.PatternNote),
		})
	}

	return vs
}

// checkEmail defers the shape of the address to types.ParseEmail and keeps
// only the sentence.
//
// One parser, two callers: a form field collecting a receipt address and a
// user account being created hold the same thing to the same standard, and the
// consequences of getting it wrong differ only in which of them is worse -- a
// receipt nobody receives, or a sign-in link nobody can click. What this
// function adds is the field's label, because the parser cannot know it.
func (fld Field) checkEmail(s string) []Violation {
	if _, err := types.ParseEmail(s); err != nil {
		// The parser's own reason is deliberately not shown. It is written for
		// a log and names the offending character; "check for a typo" is what
		// somebody filling in a form can act on.
		return []Violation{{
			Field:   fld.Name,
			Message: fmt.Sprintf("%s does not look like an email address. Check for a typo.", fld.Label),
		}}
	}

	return nil
}

func (fld Field) checkChoice(s string) []Violation {
	if slices.ContainsFunc(fld.Options, func(o Option) bool { return o.Value == s }) {
		return nil
	}

	// The submitted value is deliberately not echoed. It came from outside the
	// list we rendered, so it is either tampering or a stale page, and neither
	// is improved by quoting it back.
	return []Violation{{
		Field:   fld.Name,
		Message: fmt.Sprintf("Please choose one of the listed options for %s.", fld.Label),
	}}
}

func (fld Field) checkChoices(values []string) []Violation {
	var vs []Violation

	seen := make(map[string]bool, len(values))

	for _, s := range values {
		if seen[s] {
			return []Violation{{
				Field:   fld.Name,
				Message: fmt.Sprintf("We could not read your choices for %s. Please reload the form and try again.", fld.Label),
			}}
		}
		seen[s] = true

		if !slices.ContainsFunc(fld.Options, func(o Option) bool { return o.Value == s }) {
			vs = append(vs, Violation{
				Field:   fld.Name,
				Message: fmt.Sprintf("Please choose from the listed options for %s.", fld.Label),
			})

			return vs
		}
	}

	// For this kind the bounds count selections rather than measure a value.
	n := int64(len(values))

	switch {
	case fld.Min != nil && n < *fld.Min:
		vs = append(vs, Violation{
			Field:   fld.Name,
			Message: fmt.Sprintf("Choose at least %d for %s.", *fld.Min, fld.Label),
		})
	case fld.Max != nil && n > *fld.Max:
		vs = append(vs, Violation{
			Field:   fld.Name,
			Message: fmt.Sprintf("Choose at most %d for %s.", *fld.Max, fld.Label),
		})
	}

	return vs
}

func (fld Field) checkNumber(s string) []Violation {
	n, ok := parseCount(s)
	if !ok {
		return []Violation{{
			Field:   fld.Name,
			Message: fmt.Sprintf("%s needs to be a whole number, like 2.", fld.Label),
		}}
	}

	switch {
	case fld.Min != nil && n < *fld.Min:
		return []Violation{{
			Field:   fld.Name,
			Message: fmt.Sprintf("%s needs to be at least %d.", fld.Label, *fld.Min),
		}}
	case fld.Max != nil && n > *fld.Max:
		return []Violation{{
			Field:   fld.Name,
			Message: fmt.Sprintf("%s can be at most %d.", fld.Label, *fld.Max),
		}}
	}

	return nil
}

func (fld Field) checkAmount(s string, currency string) []Violation {
	amount, err := types.ParseMoney(s)
	if err != nil {
		return []Violation{{
			Field:   fld.Name,
			Message: fmt.Sprintf("%s needs to be an amount, like 25 or 25.50.", fld.Label),
		}}
	}

	switch {
	case fld.Min != nil && int64(amount) < *fld.Min:
		return []Violation{{
			Field:   fld.Name,
			Message: fmt.Sprintf("%s needs to be at least %s.", fld.Label, show(types.Money(*fld.Min), currency)),
		}}
	case fld.Max != nil && int64(amount) > *fld.Max:
		return []Violation{{
			Field:   fld.Name,
			Message: fmt.Sprintf("%s can be at most %s.", fld.Label, show(types.Money(*fld.Max), currency)),
		}}
	}

	return nil
}

// order reads the quantities and prices every line from the definition.
//
// It returns the lines with a quantity, the total count across all items, what
// they come to, and anything wrong. The price is never read from the request:
// that is the rule this whole package exists to hold.
func (f Form) order(in Values) ([]Line, int, types.Money, []Violation) {
	var (
		lines   []Line
		ordered int
		charged types.Money
		vs      []Violation
	)

	for _, it := range f.Items {
		name := QuantityField(it.ID)
		raw := in[name]

		if len(raw) > 1 {
			vs = append(vs, Violation{
				Field:   name,
				Message: fmt.Sprintf("We could not read how many of %s you wanted. Please reload the form and try again.", it.Label),
			})

			continue
		}

		var qty int64

		if len(raw) == 1 {
			s := strings.TrimSpace(raw[0])
			if s != "" {
				n, ok := parseCount(s)
				if !ok {
					vs = append(vs, Violation{
						Field:   name,
						Message: fmt.Sprintf("How many of %s would you like? Enter a whole number.", it.Label),
					})

					continue
				}
				qty = n
			}
		}

		// Bounded before it is multiplied, and bounded against a cap that came
		// from the definition. Money.Times would refuse an overflow anyway,
		// but "we cannot take that" is a worse answer than "you can order at
		// most eight".
		if it.Max > 0 && qty > int64(it.Max) {
			vs = append(vs, Violation{
				Field:   name,
				Message: fmt.Sprintf("You can order at most %d of %s.", it.Max, it.Label),
			})

			continue
		}

		if qty > int64(maxInt) {
			vs = append(vs, Violation{
				Field:   name,
				Message: fmt.Sprintf("That is more of %s than we can sell.", it.Label),
			})

			continue
		}

		amount, err := it.Price.Times(int(qty))
		if err != nil {
			vs = append(vs, Violation{
				Field:   name,
				Message: fmt.Sprintf("That is more of %s than we can sell.", it.Label),
			})

			continue
		}

		sum, err := charged.Add(amount)
		if err != nil {
			vs = append(vs, Violation{Message: "That order is larger than we can take."})

			continue
		}
		charged = sum

		ordered += int(qty)

		if qty > 0 {
			lines = append(lines, Line{
				ItemID: it.ID,
				Label:  it.Label,
				Price:  it.Price,
				Qty:    int(qty),
				Amount: amount,
			})
		}
	}

	return lines, ordered, charged, vs
}

func (f Form) checkOrderSize(ordered int) []Violation {
	if len(f.Items) == 0 {
		return nil
	}

	switch {
	case f.MinPerOrder > 0 && ordered < f.MinPerOrder:
		return []Violation{{Message: fmt.Sprintf("Choose at least %d.", f.MinPerOrder)}}
	case f.MaxPerOrder > 0 && ordered > f.MaxPerOrder:
		return []Violation{{Message: fmt.Sprintf("You can order at most %d in one go. Please submit a second order, or write to us.", f.MaxPerOrder)}}
	}

	return nil
}

func (f Form) checkTotal(total types.Money) []Violation {
	if total == 0 {
		// Nothing to charge, so neither the form's floor nor the payment
		// processor's applies -- a form that sells things and also accepts a
		// free response is a legitimate shape. PaymentRequired is how a form
		// says it is not one of those.
		if f.PaymentRequired {
			return []Violation{{Message: f.PaymentNote}}
		}

		return nil
	}

	var vs []Violation

	switch {
	case f.MinTotal > 0 && total < f.MinTotal:
		vs = append(vs, Violation{Message: fmt.Sprintf("The smallest order this form takes is %s, and yours comes to %s.", show(f.MinTotal, f.Currency), show(total, f.Currency))})
	case f.MaxTotal > 0 && total > f.MaxTotal:
		vs = append(vs, Violation{Message: fmt.Sprintf("The largest order this form takes is %s, and yours comes to %s.", show(f.MaxTotal, f.Currency), show(total, f.Currency))})
	}

	// The payment processor's own floor, which is per currency and which it
	// enforces by refusing the charge -- so catching it here is the difference
	// between a sentence and a failed payment.
	if floor, ok := stripeMinimum[f.Currency]; ok && total < floor {
		vs = append(vs, Violation{Message: fmt.Sprintf("The smallest payment we can take is %s.", show(floor, f.Currency))})
	}

	return vs
}

// maxInt is the largest int on this platform, used to keep a quantity that
// arrived as a 64-bit number from wrapping when it is narrowed.
const maxInt = int(^uint(0) >> 1)

// parseCount reads a non-negative whole number, and is strict for the same
// reasons ParseMoney is: strconv.Atoi accepts "+5" and " 5", and a quantity is
// multiplied by a price.
func parseCount(s string) (int64, bool) {
	if s == "" || !allASCIIDigits(s) {
		return 0, false
	}

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}

	return n, true
}

func allASCIIDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}

	return s != ""
}

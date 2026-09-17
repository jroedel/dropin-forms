package embedapp

import (
	"strconv"
	"strings"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// The view types, and why the mapping is here rather than in the template.
//
// A template full of {{if eq .Kind "email"}} branches would be a second
// statement of what each kind means, in a language with no type checking and
// no tests. So the handler turns each field of the definition into a shape and
// a set of attribute values, and the template renders those without deciding
// anything.
//
// Every attribute is a plain string or a number that html/template escapes
// normally. Nothing here produces markup, and template.HTML appears nowhere in
// this repository -- an author-written label must never be able to become a
// tag. That ban is why the mapping produces values rather than HTML.

// shape is how a field is drawn.
type shape string

const (
	shapeInput      shape = "input"      // one-line input of some type
	shapeTextarea   shape = "textarea"   // several lines
	shapeSelect     shape = "select"     // a dropdown
	shapeRadios     shape = "radios"     // one of several buttons
	shapeCheckbox   shape = "checkbox"   // a single box
	shapeCheckboxes shape = "checkboxes" // any number of boxes
	shapeAmount     shape = "amount"     // money the person types
)

// fieldView is one field, ready to render.
type fieldView struct {
	Name     string
	Label    string
	Help     string
	Required bool
	Shape    shape

	// InputType is the type attribute for shapeInput and shapeAmount.
	InputType string

	// BoxType is "radio" or "checkbox" for the two grouped shapes, so the
	// template does not have to map a shape to an input type -- which would
	// be this file's job done in a language with no tests.
	BoxType string

	Placeholder  string
	Autocomplete string
	Pattern      string

	// PatternNote is shown beside the field when a pattern is set, because a
	// pattern nobody explains is a field somebody cannot fill in. The
	// definition loader refuses a pattern without one.
	PatternNote string

	// InputMode is the on-screen keyboard to ask for. A phone showing a
	// numeric pad for an amount and a full keyboard for a name is most of
	// what makes a form bearable on a phone.
	InputMode string

	// Min, Max and Step are the numeric attributes, already in the unit the
	// input works in -- so an amount's bounds are dollars here even though the
	// definition holds cents. Empty means the attribute is omitted, which is
	// not the same as zero: a donation with a $5 floor and one with no floor
	// are different rules.
	Min  string
	Max  string
	Step string

	// MinLength and MaxLength are rune bounds on a textual field. Zero means
	// omitted.
	MinLength int
	MaxLength int

	Options []optionView

	// ShowIfField and ShowIfValues carry the condition to the browser. The
	// values are joined with the ASCII unit separator, which cannot occur in
	// an option value, so the script splits on it with no escaping scheme.
	ShowIfField  string
	ShowIfValues string

	// Value and Values are what to put back after a refusal.
	Value  string
	Values []string

	Problems []formbus.Violation
}

// optionView is one choice, with whether it was chosen.
type optionView struct {
	Value  string
	Label  string
	Chosen bool
}

// itemView is one thing for sale.
type itemView struct {
	ID string

	// Name is the input's name, which the validator spells qty_<id> and keys
	// its violations by. Taken from there rather than invented here: two
	// spellings of the same field name is a submission whose quantity is
	// silently always zero.
	Name string

	Label string
	Note  string

	// Price is formatted for reading, without a currency symbol -- the
	// template supplies that, because it knows the form's currency.
	Price string

	// Max is the most of this item one order may hold, as an attribute value.
	// Empty when neither the item nor the order has a cap.
	Max string

	Qty      string
	Problems []formbus.Violation
}

// conditionSeparator is what joins a condition's values in the DOM: the ASCII
// unit separator, 0x1F, which no option value can contain.
const conditionSeparator = "\x1f"

// viewFields turns the definition's fields into what the template renders.
func viewFields(f formbus.Form, values formbus.Values, problems formbus.Invalid) []fieldView {
	out := make([]fieldView, 0, len(f.Fields))

	for _, fld := range f.Fields {
		v := fieldView{
			Name:         fld.Name,
			Label:        fld.Label,
			Help:         fld.Help,
			Required:     fld.Required,
			Placeholder:  fld.Placeholder,
			Autocomplete: fld.Autocomplete,
			Pattern:      fld.Pattern,
			PatternNote:  fld.PatternNote,
			MinLength:    fld.MinLen,
			MaxLength:    fld.MaxLen,
			Values:       values[fld.Name],
			Problems:     problems.For(fld.Name),
		}

		if len(v.Values) > 0 {
			v.Value = v.Values[0]
		}

		switch fld.Kind {
		case formbus.KindText:
			v.Shape, v.InputType = shapeInput, "text"

		case formbus.KindParagraph:
			v.Shape = shapeTextarea

		case formbus.KindEmail:
			v.Shape, v.InputType, v.InputMode = shapeInput, "email", "email"

		case formbus.KindTel:
			v.Shape, v.InputType, v.InputMode = shapeInput, "tel", "tel"

		case formbus.KindNumber:
			v.Shape, v.InputType, v.InputMode = shapeInput, "number", "numeric"
			v.Step = "1"
			v.Min = boundAsCount(fld.Min)
			v.Max = boundAsCount(fld.Max)

		case formbus.KindAmount:
			// type=number with a hundredth step. The definition's bounds are
			// minor units and the input works in major ones, so they are
			// converted here and nowhere else.
			v.Shape, v.InputType, v.InputMode = shapeAmount, "number", "decimal"
			v.Step = "0.01"
			v.Min = boundAsMoney(fld.Min)
			v.Max = boundAsMoney(fld.Max)

		case formbus.KindSelect:
			v.Shape = shapeSelect

		case formbus.KindRadio:
			v.Shape, v.BoxType = shapeRadios, "radio"

		case formbus.KindCheckbox:
			v.Shape = shapeCheckbox

		case formbus.KindChoices:
			// The bounds on this kind count boxes, and HTML has no attribute
			// for that. The server enforces them and the help text is where
			// somebody is told; a min on a checkbox would be a constraint
			// about the wrong thing.
			v.Shape, v.BoxType = shapeCheckboxes, "checkbox"
		}

		if fld.Kind.HasOptions() {
			v.Options = make([]optionView, 0, len(fld.Options))

			for _, o := range fld.Options {
				v.Options = append(v.Options, optionView{
					Value:  o.Value,
					Label:  o.Label,
					Chosen: chosen(v.Values, o.Value),
				})
			}
		}

		// A single box. Any value at all means checked -- the validator says
		// so explicitly, because a browser sends the value attribute when
		// there is one and "on" when there is not -- so the value is chosen
		// here and the redisplay is whether anything came back.
		if fld.Kind == formbus.KindCheckbox {
			v.BoxType = "checkbox"
			v.Value = "yes"
			v.Options = nil
		}

		if c := fld.ShowIf; c != nil {
			v.ShowIfField = c.Field
			v.ShowIfValues = strings.Join(c.Is, conditionSeparator)
		}

		out = append(out, v)
	}

	return out
}

// viewItems turns the things for sale into what the template renders.
func viewItems(f formbus.Form, values formbus.Values, problems formbus.Invalid) []itemView {
	out := make([]itemView, 0, len(f.Items))

	for _, it := range f.Items {
		name := formbus.QuantityField(it.ID)

		v := itemView{
			ID:       it.ID,
			Name:     name,
			Label:    it.Label,
			Note:     it.Note,
			Price:    formbus.Show(it.Price, f.Currency),
			Problems: problems.For(name),
		}

		switch {
		case it.Max > 0:
			v.Max = strconv.Itoa(it.Max)

		case f.MaxPerOrder > 0:
			// No cap of its own, so the order's cap is the only bound there
			// is -- and it is a better attribute than none, since a spinner
			// that stops at the limit is kinder than a refusal after the fact.
			v.Max = strconv.Itoa(f.MaxPerOrder)
		}

		if got := values[it.ID]; len(got) > 0 {
			v.Qty = got[0]
		}

		out = append(out, v)
	}

	return out
}

// chosen reports whether value is among what came back.
func chosen(values []string, value string) bool {
	for _, got := range values {
		if got == value {
			return true
		}
	}

	return false
}

// boundAsCount renders a bound that counts things.
func boundAsCount(bound *int64) string {
	if bound == nil {
		return ""
	}

	return strconv.FormatInt(*bound, 10)
}

// boundAsMoney renders a bound held in minor units as the major-unit decimal
// an input works in.
func boundAsMoney(bound *int64) string {
	if bound == nil {
		return ""
	}

	return types.Money(*bound).String()
}

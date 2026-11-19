package formbus

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jroedel/dropin-forms/business/types/phone"
)

// The three answers to a carpool field.
//
// The values are fixed and the labels are the author's. That split is the
// whole design of the kind: the service places riders in cars by what the
// value means, so an author cannot rename "drive" into something it no longer
// recognises -- and the words a student reads are whatever the person running
// the event would say, in whatever language the form is in.
const (
	CarpoolHave  = "have"  // has a ride already, and is in nobody's car here
	CarpoolDrive = "drive" // can drive, and has seats for others
	CarpoolNeed  = "need"  // needs a driver; the service finds one
)

// carpoolValues is the three, in the order a form shows them by default.
var carpoolValues = []string{CarpoolHave, CarpoolDrive, CarpoolNeed}

// CarpoolValues returns the fixed option values of a carpool field, in order,
// for a builder to lay out a new one.
func CarpoolValues() []string { return slices.Clone(carpoolValues) }

// DefaultMaxSeats is the most seats a driver may offer when the field does not
// say. Seven is a minivan less its driver; a form for a bus trip sets its own.
const DefaultMaxSeats = 7

// SeatsField is the name the seats box is submitted under.
//
// A dot, because a field name may not contain one (see checkFieldName), so no
// field can ever collide with it; and derived from the carpool field's name
// rather than fixed, so that the inverse in [Answers.Values] needs nothing but
// the answer to find its way back.
func SeatsField(name string) string { return name + ".seats" }

// MaxSeats is the most seats a driver may offer on this field.
func (fld Field) MaxSeats() int {
	if fld.Max != nil {
		return int(*fld.Max)
	}

	return DefaultMaxSeats
}

// MinSeats is the fewest seats a driver may offer, which is one unless the
// field says otherwise: a driver with no seats for anybody is somebody who has
// a ride.
func (fld Field) MinSeats() int {
	if fld.Min != nil {
		return int(*fld.Min)
	}

	return 1
}

// SeatsQuestion is what a driver is asked about seats.
func (fld Field) SeatsQuestion() string {
	if s := strings.TrimSpace(fld.SeatsLabel); s != "" {
		return s
	}

	return "How many people can you take?"
}

// CarpoolOf is the form's carpool field, and whether it has one. There is at
// most one; Check refuses a second.
func (f Form) CarpoolOf() (Field, bool) {
	i := slices.IndexFunc(f.Fields, func(fld Field) bool { return fld.Kind == KindCarpool })
	if i < 0 {
		return Field{}, false
	}

	return f.Fields[i], true
}

// HasCarpool reports whether the form has a carpool, for a template, which
// cannot take CarpoolOf's two results.
func (f Form) HasCarpool() bool {
	_, ok := f.CarpoolOf()

	return ok
}

// checkCarpool is the rules of a carpool field, and the refusal of a
// carpool's settings on any other kind.
func (f *Form) checkCarpool(fld *Field, where string, at int) []string {
	var p []string
	add := func(format string, args ...any) {
		p = append(p, fmt.Sprintf(format, args...))
	}

	if fld.Kind != KindCarpool {
		if fld.Phone != "" || fld.Who != "" || fld.SeatsLabel != "" {
			add("%s: only a carpool field has a carpool's settings", where)
		}

		return p
	}

	if first := slices.IndexFunc(f.Fields, func(c Field) bool { return c.Kind == KindCarpool }); first != at {
		// One per form, because a submission is one person in one car. Two
		// would be two trips, and that is two forms.
		add("%s: a form has at most one carpool, and %q is already one", where, f.Fields[first].Name)
	}

	// Exactly the three, each once. checkOptions has already refused an
	// empty, repeated or unlabelled value; this refuses the wrong set.
	values := make([]string, 0, len(fld.Options))
	for _, o := range fld.Options {
		values = append(values, o.Value)

		if o.Limit != 0 {
			add("%s: a carpool's choices cannot have a limit; the seats in each car are the limit", where)
		}
	}

	if !sameSet(values, carpoolValues) {
		add("%s: a carpool's choices are exactly %s, each with your own words as its label",
			where, strings.Join(carpoolValues, ", "))
	}

	if fld.Min != nil && *fld.Min < 1 {
		add("%s: a driver offers at least one seat; leave the fewest out to mean one", where)
	}

	if fld.Max != nil && *fld.Max > 60 {
		add("%s: %d seats is more than one vehicle; a carpool offers at most 60", where, *fld.Max)
	}

	// The two fields the carpool texts from. Both earlier, so that Validate
	// has their answers by the time it reaches this one, and of the kinds
	// that can hold a number and a name.
	refer := func(setting, name string, kind Kind, noun string) {
		if name == "" {
			add("%s: a carpool needs %s -- the earlier question that asks for %s", where, setting, noun)

			return
		}

		i := slices.IndexFunc(f.Fields[:at], func(c Field) bool { return c.Name == name })

		switch {
		case i < 0 && slices.ContainsFunc(f.Fields[at:], func(c Field) bool { return c.Name == name }):
			add("%s: its %s %q comes after it; move %q above the carpool", where, setting, name, name)
		case i < 0:
			add("%s: its %s %q is not a question on this form", where, setting, name)
		case f.Fields[i].Kind != kind:
			add("%s: its %s %q is a %s question, and needs to be a %s one", where, setting, name, f.Fields[i].Kind, kind)
		}
	}

	refer("phone", fld.Phone, KindTel, "a mobile number")
	refer("name", fld.Who, KindText, "the person's name")

	return p
}

// sameSet reports whether a and b hold the same strings, each once.
func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for _, s := range b {
		if !slices.Contains(a, s) {
			return false
		}
	}

	return true
}

// takeCarpool is take for a carpool field, which is the one kind with a
// second input: the seats, under [SeatsField].
//
// What is kept is the choice, and for a driver the seats after it:
// ["drive", "3"]. One answer rather than two so that everything that walks
// answers -- the export, the messages, a condition naming this field -- sees
// one question with one answer, and a condition on "drive" works as it does on
// a radio.
func (fld Field) takeCarpool(raw, seats []string) ([]string, []Violation) {
	if len(raw) > 1 || len(seats) > 1 {
		return nil, []Violation{{
			Field:   fld.Name,
			Message: fmt.Sprintf("We could not read your answer for %s. Please reload the form and try again.", fld.Label),
		}}
	}

	choice := ""
	if len(raw) == 1 {
		choice = strings.TrimSpace(raw[0])
	}

	if choice == "" {
		if fld.Required {
			return nil, []Violation{{Field: fld.Name, Message: fld.missing()}}
		}

		return nil, nil
	}

	if vs := fld.checkChoice(choice); vs != nil {
		return nil, vs
	}

	// Seats are asked of a driver only. Sent by anybody else -- somebody who
	// typed a number and then chose "I need a ride" -- they are ignored for
	// the reason a hidden field's value is.
	if choice != CarpoolDrive {
		return []string{choice}, nil
	}

	lo, hi := fld.MinSeats(), fld.MaxSeats()
	wrong := []Violation{{
		Field:   fld.Name,
		Message: fmt.Sprintf("Please say how many people you can take, from %d to %d.", lo, hi),
	}}

	if len(seats) == 0 {
		return nil, wrong
	}

	n, err := strconv.Atoi(strings.TrimSpace(seats[0]))
	if err != nil || n < lo || n > hi {
		return nil, wrong
	}

	return []string{choice, strconv.Itoa(n)}, nil
}

// checkCarpoolContact requires the number and the name a carpool texts with,
// from anybody who will be in a car: a driver or a rider. Somebody with their
// own ride is texted about nothing, and is not asked for either.
//
// Here rather than as Required on those two fields, because whether they are
// required depends on this answer, and a required phone number on a form where
// most people are coming on their own would be asking everybody for a number
// only some of them will be texted on.
func (f Form) checkCarpoolContact(fld Field, values []string, index map[string]int, taken [][]string) []Violation {
	if len(values) == 0 || values[0] == CarpoolHave {
		return nil
	}

	answer := func(name string) string {
		i, ok := index[name]
		if !ok || len(taken[i]) == 0 {
			return ""
		}

		return taken[i][0]
	}

	var vs []Violation

	labelOf := func(name string) string {
		if i, ok := index[name]; ok {
			return f.Fields[i].Label
		}

		return name
	}

	switch number := answer(fld.Phone); {
	case number == "":
		vs = append(vs, Violation{
			Field:   fld.Phone,
			Message: fmt.Sprintf("Please fill in %s, so we can text you about your ride.", labelOf(fld.Phone)),
		})
	default:
		if _, err := phone.Parse(number); err != nil {
			vs = append(vs, Violation{
				Field:   fld.Phone,
				Message: fmt.Sprintf("%s needs a mobile number we can text, with its area code, such as 512-555-0100.", labelOf(fld.Phone)),
			})
		}
	}

	if answer(fld.Who) == "" {
		vs = append(vs, Violation{
			Field:   fld.Who,
			Message: fmt.Sprintf("Please fill in %s, so the others in your car know who you are.", labelOf(fld.Who)),
		})
	}

	return vs
}

// Carpooler is one person's part in a carpool, read from their answers.
type Carpooler struct {
	Choice string // CarpoolHave, CarpoolDrive or CarpoolNeed
	Seats  int    // a driver's, and zero for anybody else
	Name   string
	Phone  phone.Number
}

// Driving reports whether this person offered a car.
func (c Carpooler) Driving() bool { return c.Choice == CarpoolDrive }

// Riding reports whether this person asked for a driver.
func (c Carpooler) Riding() bool { return c.Choice == CarpoolNeed }

// Carpooler reads one submission's part in the form's carpool, and whether it
// has one: false when the form has no carpool or the question was not
// answered (hidden by a condition, or optional and left blank).
//
// The phone number is parsed again rather than trusted, because answers are
// read back from storage and may have been accepted by an earlier definition;
// a number that does not parse is the zero Number, and whoever texts reads
// that as "nobody to text".
func (f Form) Carpooler(a Answers) (Carpooler, bool) {
	fld, ok := f.CarpoolOf()
	if !ok {
		return Carpooler{}, false
	}

	ans, ok := a.Field(fld.Name)
	if !ok || len(ans.Values) == 0 {
		return Carpooler{}, false
	}

	c := Carpooler{Choice: ans.Values[0]}

	if c.Driving() && len(ans.Values) > 1 {
		c.Seats, _ = strconv.Atoi(ans.Values[1])
	}

	if who, ok := a.Field(fld.Who); ok {
		c.Name = who.Value()
	}

	if number, ok := a.Field(fld.Phone); ok {
		c.Phone, _ = phone.Parse(number.Value())
	}

	return c, true
}

// carpoolReadable is a carpool answer in words, for a page or a message.
//
// The service's words rather than the form's labels, because an Answer does
// not carry its field's options -- and these three sentences are the same
// thing the labels say, written for the person reading the answer rather than
// the person giving it.
func (a Answer) carpoolReadable() string {
	if len(a.Values) == 0 {
		return ""
	}

	switch a.Values[0] {
	case CarpoolHave:
		return "Has a ride"
	case CarpoolNeed:
		return "Needs a ride"
	case CarpoolDrive:
		if len(a.Values) > 1 {
			if n, err := strconv.Atoi(a.Values[1]); err == nil {
				if n == 1 {
					return "Can drive, 1 seat"
				}

				return fmt.Sprintf("Can drive, %d seats", n)
			}
		}

		return "Can drive"
	}

	return strings.Join(a.Values, ", ")
}

package formapp

import (
	"net/http"
	"slices"
	"strings"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
)

// fieldView is one field's own page.
//
// Every kind's attributes are on it at once, and the template shows the ones
// that mean something for the kind that is selected. That is a deliberate
// choice over a page per kind: with no script there is no way to reveal a
// section when a dropdown changes, so the alternatives were ten templates or
// one page that says what each box is for. One page, with the irrelevant parts
// labelled rather than hidden, is the one somebody can read.
type fieldView struct {
	FormID string
	Live   bool

	// Name is the field's current name and is what the route is keyed by. It
	// is not editable: it is the HTML name, the CSV column heading and the key
	// in every submission already collected, so changing it would silently
	// orphan the answers under the old one.
	Name string

	Label        string
	Kind         string
	Required     bool
	Help         string
	Placeholder  string
	Autocomplete string

	MinLen string
	MaxLen string
	Min    string
	Max    string

	Pattern     string
	PatternNote string

	Options string

	// ShowIfField and ShowIfIs are the condition. Empty means always shown.
	ShowIfField string
	ShowIfIs    string

	// Kinds is every kind this service can validate, in formbus's own order,
	// which its comment says is the order a builder should offer them in.
	Kinds []kindChoice

	// Earlier is the fields this one may be shown conditionally on: the ones
	// above it, because a condition may only name an earlier field and that is
	// what guarantees there is no cycle to resolve.
	Earlier []string

	// What the selected kind actually uses, so the page can say which boxes
	// are doing nothing rather than leaving somebody to guess.
	HasOptions bool
	Bounded    bool
	Textual    bool
	Money      bool

	Problems []string
	Problem  string
}

// kindChoice is one entry in the kind dropdown: the value the domain uses and
// a sentence for a person. The sentence is here rather than on formbus.Kind
// because it is wording, and the business package should not be where a
// wording change is made -- the same line peopleapp draws for a role.
type kindChoice struct {
	Value    string
	Label    string
	Selected bool
}

// addField appends a field and goes to its page.
//
// The three things a field cannot be created without are asked for here: a
// name, a label and a kind. A kind that needs a list of options asks for that
// too, and the reason is the live/draft bargain in the package comment -- on a
// form that is already on the web, a save that would leave it unusable is
// refused, and an option-less dropdown is exactly that. Asking for the options
// in the same request is what makes adding one to a live form possible at all.
func (a app) addField(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}

	if err := r.ParseForm(); err != nil {
		a.show(w, r, http.StatusBadRequest, s, buildView{
			Problem: "We could not read that. Please try again.",
		})

		return
	}

	fld := formbus.Field{
		Name:     value(r, "name"),
		Label:    value(r, "label"),
		Kind:     formbus.Kind(value(r, "kind")),
		Required: checked(r, "required"),
		Options:  parseOptions(r.PostFormValue("options")),
	}

	if _, taken := s.Form.Field(fld.Name); taken {
		a.show(w, r, http.StatusConflict, s, buildView{
			Problem: "There is already a field called " + fld.Name + " on this form.",
		})

		return
	}

	f := s.Form
	f.Fields = append(slices.Clone(f.Fields), fld)

	problems, ok := a.save(w, r, f, s.Live)
	if !ok {
		if problems == nil {
			return
		}

		a.show(w, r, http.StatusBadRequest, s, buildView{
			Problem:  "That field was not added.",
			Problems: problems,
		})

		return
	}

	// To the field's own page rather than back to the list. Somebody who has
	// just named a field is about to say what it may contain, and that is the
	// next page rather than two clicks away.
	http.Redirect(w, r, "/forms/"+f.ID.String()+"/edit/fields/"+fld.Name, http.StatusSeeOther)
}

// field shows one field's page.
func (a app) field(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}

	fld, at := findField(s.Form, r.PathValue("name"))
	if at < 0 {
		a.notFound(w)

		return
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "build-field", fieldOf(s, fld, at))
}

// fieldOf fills the page in from a field.
func fieldOf(s formbus.Stored, fld formbus.Field, at int) fieldView {
	money := fld.Kind == formbus.KindAmount

	v := fieldView{
		FormID:       s.Form.ID.String(),
		Live:         s.Live,
		Name:         fld.Name,
		Label:        fld.Label,
		Kind:         string(fld.Kind),
		Required:     fld.Required,
		Help:         fld.Help,
		Placeholder:  fld.Placeholder,
		Autocomplete: fld.Autocomplete,
		MinLen:       blankZero(fld.MinLen),
		MaxLen:       blankZero(fld.MaxLen),
		Min:          writeBound(fld.Min, money),
		Max:          writeBound(fld.Max, money),
		Pattern:      fld.Pattern,
		PatternNote:  fld.PatternNote,
		Options:      writeOptions(fld.Options),
		Kinds:        kindsFor(fld.Kind),
		HasOptions:   fld.Kind.HasOptions(),
		Bounded:      fld.Kind.Bounded(),
		Textual:      fld.Kind.Textual(),
		Money:        money,
	}

	if fld.ShowIf != nil {
		v.ShowIfField = fld.ShowIf.Field
		v.ShowIfIs = strings.Join(fld.ShowIf.Is, "\n")
	}

	// Only the fields above this one, because formbus.checkCondition refuses a
	// condition naming a later field -- one forward pass, no cycle. Offering
	// the others in the dropdown would be offering a choice that is then
	// refused.
	for _, earlier := range s.Form.Fields[:at] {
		v.Earlier = append(v.Earlier, earlier.Name)
	}

	return v
}

// saveField writes one field back.
func (a app) saveField(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}

	was, at := findField(s.Form, r.PathValue("name"))
	if at < 0 {
		a.notFound(w)

		return
	}

	if err := r.ParseForm(); err != nil {
		view := fieldOf(s, was, at)
		view.Problem = "We could not read that. Please try again."
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "build-field", view)

		return
	}

	// The name is not read from the request. It keys the route and it keys
	// every answer already collected under it; a rename would have to rewrite
	// the submissions, and a form builder that can silently orphan a column is
	// not one anybody should have.
	fld := formbus.Field{
		Name:         was.Name,
		Label:        value(r, "label"),
		Kind:         formbus.Kind(value(r, "kind")),
		Required:     checked(r, "required"),
		Help:         value(r, "help"),
		Placeholder:  value(r, "placeholder"),
		Autocomplete: value(r, "autocomplete"),
		Pattern:      value(r, "pattern"),
		PatternNote:  value(r, "pattern_note"),
		Options:      parseOptions(r.PostFormValue("options")),
	}

	money := fld.Kind == formbus.KindAmount

	var problems []string

	fail := func(err error) {
		if err != nil {
			problems = append(problems, err.Error())
		}
	}

	var err error

	fld.MinLen, err = parseCount("The shortest answer", value(r, "min_length"))
	fail(err)

	fld.MaxLen, err = parseCount("The longest answer", value(r, "max_length"))
	fail(err)

	fld.Min, err = parseBound("The smallest value", value(r, "min"), money)
	fail(err)

	fld.Max, err = parseBound("The largest value", value(r, "max"), money)
	fail(err)

	if on := value(r, "show_if_field"); on != "" {
		fld.ShowIf = &formbus.Condition{Field: on, Is: lines(r.PostFormValue("show_if_is"))}
	}

	// Rebuilt from what was typed rather than from storage, so a refusal comes
	// back showing the edit somebody just made instead of the version they
	// were trying to replace.
	said := fieldOf(s, fld, at)

	if len(problems) > 0 {
		said.Problems = problems
		said.Problem = "Nothing was saved."
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "build-field", said)

		return
	}

	f := s.Form
	f.Fields = slices.Clone(f.Fields)
	f.Fields[at] = fld

	problems, ok = a.save(w, r, f, s.Live)
	if !ok {
		if problems == nil {
			return
		}

		said.Problems = problems
		said.Problem = refusal(s.Live)
		a.cfg.Render.Render(w, r, http.StatusConflict, "build-field", said)

		return
	}

	s.Form = f

	a.show(w, r, http.StatusOK, s, buildView{Done: "The field " + fld.Name + " is saved."})
}

// moveField shifts one field up or down.
//
// Order is part of what a submission means -- it is in the fingerprint, and a
// condition may only name a field above the one it governs -- so this is a
// real edit rather than a presentation preference, and it goes through the
// same Check as any other.
func (a app) moveField(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}

	_, at := findField(s.Form, r.PathValue("name"))
	if at < 0 {
		a.notFound(w)

		return
	}

	if err := r.ParseForm(); err != nil {
		a.show(w, r, http.StatusBadRequest, s, buildView{
			Problem: "We could not read that. Please try again.",
		})

		return
	}

	f := s.Form
	f.Fields = slices.Clone(f.Fields)

	to, ok := moved(at, len(f.Fields), r.PostFormValue("to"))
	if !ok {
		// The button at the end of the list, pressed anyway. Nothing to
		// report: the form is already in the order it was asked for.
		a.show(w, r, http.StatusOK, s, buildView{})

		return
	}

	swap := f.Fields[at]
	f.Fields = slices.Delete(f.Fields, at, at+1)
	f.Fields = slices.Insert(f.Fields, to, swap)

	problems, ok := a.save(w, r, f, s.Live)
	if !ok {
		if problems == nil {
			return
		}

		// The refusal this is written for: moving a field above the one whose
		// value decides whether it is shown. Check says exactly that, and it
		// is more useful than anything this handler could add.
		a.show(w, r, http.StatusConflict, s, buildView{
			Problem:  "That field was not moved.",
			Problems: problems,
		})

		return
	}

	s.Form = f

	a.show(w, r, http.StatusOK, s, buildView{Done: "The order is saved."})
}

// removeField takes a field off the form.
//
// The answers already collected under its name are untouched and stay in the
// submissions table, which is the right thing and is also the reason the
// submissions page shows a column for a field that no longer exists rather
// than dropping it.
func (a app) removeField(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}

	fld, at := findField(s.Form, r.PathValue("name"))
	if at < 0 {
		a.notFound(w)

		return
	}

	f := s.Form
	f.Fields = slices.Delete(slices.Clone(f.Fields), at, at+1)

	problems, ok := a.save(w, r, f, s.Live)
	if !ok {
		if problems == nil {
			return
		}

		// Most often: another field is shown only when this one holds a
		// particular value. Check names both, which is what somebody needs in
		// order to decide which of the two to remove.
		a.show(w, r, http.StatusConflict, s, buildView{
			Problem:  "That field was not removed.",
			Problems: problems,
		})

		return
	}

	s.Form = f

	a.show(w, r, http.StatusOK, s, buildView{
		Done: "The field " + fld.Name + " is gone. Anything already submitted under that name is still in the submissions.",
	})
}

// findField locates a field by name, returning its position or -1.
func findField(f formbus.Form, name string) (formbus.Field, int) {
	at := slices.IndexFunc(f.Fields, func(c formbus.Field) bool { return c.Name == name })
	if at < 0 {
		return formbus.Field{}, -1
	}

	return f.Fields[at], at
}

// moved reads an up/down button and answers where the thing lands, or false
// when it is already at that end.
func moved(at, n int, to string) (int, bool) {
	switch to {
	case "up":
		if at == 0 {
			return 0, false
		}

		return at - 1, true

	case "down":
		if at >= n-1 {
			return 0, false
		}

		return at + 1, true
	}

	return 0, false
}

// refusal is what a rejected save says, and it differs by whether anybody is
// looking at the form: on a live one the news is that nothing changed and the
// page on the web is still working, which is the reassurance somebody needs
// first.
func refusal(live bool) string {
	if live {
		return "This form is on the web, so a change that would stop it working is refused. Nothing was saved and it is still serving what it was."
	}

	return "Nothing was saved."
}

// kindsFor is the dropdown, in formbus's own order.
func kindsFor(selected formbus.Kind) []kindChoice {
	all := formbus.Kinds()
	out := make([]kindChoice, 0, len(all))

	for _, k := range all {
		out = append(out, kindChoice{
			Value:    string(k),
			Label:    describeKind(k),
			Selected: k == selected,
		})
	}

	return out
}

// describeKind is the wording for each kind. A kind with no sentence written
// for it shows its own name, so adding one to formbus produces a usable
// dropdown entry before anybody edits this file.
func describeKind(k formbus.Kind) string {
	switch k {
	case formbus.KindText:
		return "A line of text"
	case formbus.KindParagraph:
		return "Several lines of text"
	case formbus.KindEmail:
		return "An email address"
	case formbus.KindTel:
		return "A telephone number"
	case formbus.KindNumber:
		return "A whole number"
	case formbus.KindSelect:
		return "A dropdown: one of a list"
	case formbus.KindRadio:
		return "Buttons: one of a list"
	case formbus.KindCheckbox:
		return "A single tick box"
	case formbus.KindChoices:
		return "Tick boxes: any number of a list"
	case formbus.KindAmount:
		return "An amount of money somebody types"
	default:
		return string(k)
	}
}

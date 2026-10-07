package formbus

import (
	"fmt"
	"slices"
	"strings"

	"github.com/jroedel/dropin-forms/business/types"
)

// Listing is what a form shows of its earlier submissions, beneath itself: a
// sign-up sheet, a "who is bringing what", a list of names already down for a
// slot. It is off unless Line is set.
//
// One line per submission, written by the author as a template, rather than a
// table with a column per chosen field. A table is the shape of the admin
// list, and it is the wrong shape here: what somebody wants to read on the
// parish's own page is "Maria G. -- bringing tamales", and a table with a
// "Name" heading and a "Dish" heading is that sentence taken apart. The
// template is also the whole of the choice about which fields appear, so
// there is no second setting that can disagree with it.
//
// # This publishes what people typed
//
// The form page is public and unauthenticated, framed on somebody else's
// website, so every field the line names is shown to anybody who opens that
// page. Two things follow, and both are enforced rather than advised:
//
//   - The person filling the form in is told, beside each field the line
//     names, that their answer will be shown. The app layer asks [Form.Listed]
//     which fields those are; there is no way to publish a field without the
//     sentence appearing beside it.
//   - An email or telephone field cannot be named at all. An address printed
//     on a public page is an address harvested by the next robot to crawl it,
//     and there is no form on a parish website whose purpose is served by
//     printing somebody's phone number.
//
// Only settled submissions are listed -- see submissionbus -- so an order
// abandoned at the checkout page never appears as though it were a booking.
type Listing struct {
	// Heading is what the list is called on the page, like "Who is coming".
	// Empty gets a generic one.
	Heading string

	// Line is the template for one submission. {name} is replaced by what the
	// field called name holds; everything else is written as it stands. A
	// brace that does not open a placeholder naming a field is refused by
	// Check, so a typo is a problem on the settings page rather than a stray
	// "{nmae}" on the parish's website.
	Line string

	// Limit is how many are shown. Zero is [ListingDefault].
	Limit int

	// OldestFirst lists them in the order they arrived, like a sign-up
	// sheet, rather than newest at the top.
	//
	// It changes the order and never which ones are shown: past the limit,
	// the newest are kept either way. "The first fifty ever" would be the
	// other reading, and on that reading the fifty-first person to sign up
	// submits the form, looks at the list, and is not on it -- which reads
	// as their answer having been lost. Kept newest, they are at the bottom.
	//
	// Not in the fingerprint, with the heading and the limit: it changes
	// how a list reads and nothing anybody agreed to when filling it in.
	OldestFirst bool
}

const (
	// ListingDefault is how many earlier submissions a list shows when the
	// author has not said. Enough for a sign-up sheet to read as a sheet, and
	// few enough that the page stays a form with a list under it rather than
	// the other way round.
	ListingDefault = 50

	// ListingMax is the most a list may be asked for. The page is unauthenticated
	// and each view of it reads this many rows, so the bound is on what one
	// stranger's reload costs as much as on the length of the page.
	ListingMax = 500
)

// On reports whether this form shows its earlier submissions.
func (l Listing) On() bool { return l.Line != "" }

// Shown is how many to read, with the default applied.
func (l Listing) Shown() int {
	if l.Limit == 0 {
		return ListingDefault
	}

	return l.Limit
}

// segment is one piece of a parsed line: literal text, or a field to fill in.
type segment struct {
	text  string
	field string
}

// parseLine splits a template into its pieces.
//
// A placeholder is a brace, a field name, and a closing brace, and nothing
// else is: there is no escape for a literal brace, because nobody writing
// "who is bringing what" needs one, and an escape rule is a second thing to
// explain on the settings page. The field name is only split out here; whether
// it names a field is Check's question.
func parseLine(line string) ([]segment, error) {
	var out []segment

	for line != "" {
		open := strings.IndexAny(line, "{}")
		if open < 0 {
			out = append(out, segment{text: line})

			break
		}

		if line[open] == '}' {
			return nil, fmt.Errorf("it has a } with no { before it")
		}

		if open > 0 {
			out = append(out, segment{text: line[:open]})
		}

		name, rest, ok := strings.Cut(line[open+1:], "}")
		if !ok {
			return nil, fmt.Errorf("it has a { with no } after it")
		}
		if strings.ContainsAny(name, "{") {
			return nil, fmt.Errorf("it has a { inside a placeholder")
		}

		out = append(out, segment{field: name})
		line = rest
	}

	return out, nil
}

// checkListing is Check's part for the list of earlier submissions.
func (f *Form) checkListing() []string {
	l := f.Listing

	var p []string
	add := func(format string, args ...any) {
		p = append(p, "its list of earlier answers "+fmt.Sprintf(format, args...))
	}

	if l.Limit < 0 {
		add("shows a negative number of them; leave it empty for %d", ListingDefault)
	}
	if l.Limit > ListingMax {
		add("shows %d of them; the most it can show is %d", l.Limit, ListingMax)
	}

	if !l.On() {
		// A heading or a limit with no line is left alone rather than refused:
		// switching the list off by emptying the line should not also demand
		// that the other two boxes be cleared.
		return p
	}

	segs, err := parseLine(l.Line)
	if err != nil {
		add("cannot be read: %s", err)

		return p
	}

	named := false

	for _, s := range segs {
		if s.field == "" {
			continue
		}

		named = true

		fld, ok := f.Field(s.field)

		switch {
		case !ok:
			add("names {%s}, which is not one of this form's questions", s.field)
		case fld.Kind == KindEmail:
			add("names {%s}, an email address; an address shown on a public page is collected by spam robots", s.field)
		case fld.Kind == KindTel:
			add("names {%s}, a telephone number, which is not something to show on a public page", s.field)
		case fld.Kind == KindSection:
			add("names {%s}, a section heading, which has no answer to show", s.field)
		}
	}

	if !named {
		// Every line would be the same sentence, which is a counter written
		// out longhand rather than a list.
		add("names no question, so every line would be the same; put a question's name in braces, like {name}")
	}

	return p
}

// Listed reports which fields the list names, so that the form can tell
// whoever fills it in that those answers will be shown.
//
// Empty when the list is off. A line that no longer parses gives nothing too,
// but Check refuses such a form, so a form being served always gets the
// answer its line says.
func (f Form) Listed() map[string]bool {
	if !f.Listing.On() {
		return nil
	}

	segs, err := parseLine(f.Listing.Line)
	if err != nil {
		return nil
	}

	out := map[string]bool{}
	for _, s := range segs {
		if s.field != "" {
			out[s.field] = true
		}
	}

	return out
}

// ListLine writes one submission the way the list shows it, or returns empty
// when it names nothing this submission answered -- a line that would read as
// the template with its holes showing.
//
// Read against the definition as it stands rather than the one the answers
// were checked against, because what is being rendered is today's page. So an
// option renamed since is shown by its new label, and an option since removed
// falls back to the value that was stored, which is still what the person
// chose.
func (f Form) ListLine(a Answers) string {
	segs, err := parseLine(f.Listing.Line)
	if err != nil {
		return ""
	}

	var (
		b        strings.Builder
		answered bool
	)

	for _, s := range segs {
		if s.field == "" {
			b.WriteString(s.text)

			continue
		}

		ans, ok := a.Field(s.field)
		if !ok {
			continue
		}

		fld, _ := f.Field(s.field)
		if text := fld.shown(ans.Values, f.Currency); text != "" {
			b.WriteString(text)
			answered = true
		}
	}

	if !answered {
		return ""
	}

	return strings.TrimSpace(b.String())
}

// shown writes stored values the way a person reads them.
func (fld Field) shown(values []string, currency string) string {
	out := make([]string, 0, len(values))

	for _, v := range values {
		switch {
		case fld.Kind.HasOptions():
			if i := slices.IndexFunc(fld.Options, func(o Option) bool { return o.Value == v }); i >= 0 {
				v = fld.Options[i].Label
			}

		case fld.Kind == KindCheckbox:
			// What a ticked box stores is whatever the browser sent for it,
			// which is "on" or a value attribute nobody chose to read. "Yes"
			// is the only honest rendering of either.
			v = "yes"

		case fld.Kind == KindAmount:
			if m, err := types.ParseMoney(v); err == nil {
				v = show(m, currency)
			}

		case fld.Kind.Temporal():
			// Written out, as the submission page and the mail write it: a
			// list of names with 2026-10-17T09:05 beside each is a list of
			// timestamps.
			v = Answer{Kind: fld.Kind, Values: []string{v}}.Readable()

		case fld.Kind == KindParagraph:
			// One line per submission, so a paragraph is folded onto it
			// rather than breaking the list into pieces nobody can attribute.
			v = strings.Join(strings.Fields(v), " ")
		}

		out = append(out, v)
	}

	return strings.Join(out, ", ")
}

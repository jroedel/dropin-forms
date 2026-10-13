package formbus

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

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
	//
	// With Sort set it is the last word rather than the only one: when every
	// key has left two lines level, they stand in the order they arrived, or
	// that order turned over. So an empty Sort with OldestFirst is exactly the
	// list there was before Sort existed, and nobody's form changed order on
	// the day it shipped.
	OldestFirst bool

	// Sort puts the list in the order of an answer rather than of arrival: a
	// time-slot sign-up read in slot order, so that the empty slots show and
	// everybody can see who is before and after them. Each key settles what
	// the ones before it left level. See [SortKey] for how each kind of
	// answer is ordered.
	//
	// It decides the order among the ones shown and never which ones are, for
	// the reason OldestFirst gives: past the limit the newest are kept, and
	// then those are sorted. A sign-up that sorts to the top of a full sheet
	// still appears, and the oldest drops off the bottom of the window rather
	// than off the bottom of the page.
	//
	// A key may only name a question the line shows. The order of a list is
	// itself something it publishes -- a list of names sorted by age tells
	// everybody who is older than whom -- and the person filling the form in
	// was only told that what the line shows would be shown. Not in the
	// fingerprint, then, because it can never reach an answer the fingerprint
	// does not already cover.
	Sort []SortKey
}

// IsZero reports whether nothing about the list has been set, so that a wire
// format can leave it out entirely.
func (l Listing) IsZero() bool {
	return l.Heading == "" && l.Line == "" && l.Limit == 0 && !l.OldestFirst && len(l.Sort) == 0
}

// SortKey is one step of a list's order: what to compare, and which way.
type SortKey struct {
	// By is a question's name, or [SortSubmitted] for the time a
	// submission arrived.
	//
	// A choice is ordered by where its option stands in the definition, not
	// by its label: "10:00 am" sorts before "9:00 am" as text, and the
	// author has already written the options in the order they mean. A
	// choice of several boxes sorts by the earliest one ticked. Numbers,
	// amounts, dates and times sort by what they are; text sorts without
	// regard to case. A question somebody did not answer goes to the bottom
	// whichever way the key runs, because a gap is not the smallest answer
	// or the largest.
	By string

	// Order is [SortAsc] or [SortDesc]. Empty is ascending, so that a TOML
	// file can say only what it sorts by.
	Order SortOrder
}

// SortOrder is which way a key runs.
type SortOrder string

const (
	SortAsc  SortOrder = "asc"
	SortDesc SortOrder = "desc"
)

// SortSubmitted is the key for when a submission arrived. It is a legal
// question name too, which is why Check refuses it on a form that has a
// question by that name: the key would mean two things.
const SortSubmitted = "submitted_at"

// Desc reports whether the key runs from largest to smallest.
func (k SortKey) Desc() bool { return k.Order == SortDesc }

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

	return append(p, f.checkSort()...)
}

// checkSort is checkListing's part for the order. Only reached with the list
// on, for the same reason as the heading and the limit: switching the list off
// should not also demand the order be cleared.
func (f *Form) checkSort() []string {
	var p []string
	add := func(format string, args ...any) {
		p = append(p, "its list of earlier answers "+fmt.Sprintf(format, args...))
	}

	shown := f.Listed()
	seen := map[string]bool{}

	for _, k := range f.Listing.Sort {
		if k.Order != "" && k.Order != SortAsc && k.Order != SortDesc {
			add("is sorted %q, which is neither %q nor %q", k.Order, SortAsc, SortDesc)
		}

		if k.By == "" {
			add("has a sort step that names nothing; choose a question or the time it was sent")

			continue
		}

		if seen[k.By] {
			// Harmless to the order, but never what was meant, and refusing it
			// is what bounds how long the list of keys can be.
			add("is sorted by %s twice", sortName(k.By))

			continue
		}
		seen[k.By] = true

		fld, isField := f.Field(k.By)

		switch {
		case k.By == SortSubmitted && isField:
			add("is sorted by %s, which is both a question here and the time a submission was sent; rename the question", k.By)
		case k.By == SortSubmitted:
		case !isField:
			add("is sorted by %s, which is not one of this form's questions", sortName(k.By))
		case fld.Kind == KindEmail || fld.Kind == KindTel || fld.Kind == KindSection:
			// The line refuses these too, so the next case would catch them;
			// named on its own so the sentence says why rather than only that
			// the line does not show them.
			add("is sorted by %s, which cannot be shown on a public page and so cannot order one", sortName(k.By))
		case !shown[k.By]:
			add("is sorted by %s, which the line does not show; the order of a public list tells people about the answer it is sorted by, so put %s in the line or sort by something it shows", sortName(k.By), sortName(k.By))
		}
	}

	return p
}

// sortName writes a key the way the line writes a question, so the two
// sentences about one question look alike.
func sortName(by string) string {
	if by == SortSubmitted {
		return by
	}

	return "{" + by + "}"
}

// Arrange puts a list's submissions in the order the list shows them.
//
// They arrive newest first, as the store reads the newest past the limit, and
// of reads each one's answers and arrival time. Generic over what is being
// arranged so that formbus need not know what a stored submission looks like.
//
// The keys are worked out once per submission rather than inside the
// comparison, which would parse the same amount a dozen times; and the sort
// is stable, so after the keys the arrival order stands, which -- turned over
// or not by OldestFirst -- is the tie-break the list promises. Two submissions
// stamped in the same millisecond keep whatever order the store gave them.
func Arrange[S any](f Form, subs []S, of func(S) (Answers, time.Time)) {
	if f.Listing.OldestFirst {
		slices.Reverse(subs)
	}

	if len(f.Listing.Sort) == 0 {
		return
	}

	type keyed struct {
		sub  S
		keys []sortValue
	}

	all := make([]keyed, len(subs))

	for i, s := range subs {
		a, at := of(s)

		all[i] = keyed{sub: s, keys: make([]sortValue, len(f.Listing.Sort))}
		for j, k := range f.Listing.Sort {
			all[i].keys[j] = f.sortValue(k.By, a, at)
		}
	}

	slices.SortStableFunc(all, func(x, y keyed) int {
		for j, k := range f.Listing.Sort {
			if c := x.keys[j].compare(y.keys[j], k.Desc()); c != 0 {
				return c
			}
		}

		return 0
	})

	for i, k := range all {
		subs[i] = k.sub
	}
}

// sortValue is one answer reduced to what a key compares: a rank first, then
// a number, then text. Each kind fills in the part that means something for
// it and leaves the rest zero.
type sortValue struct {
	missing bool
	rank    int
	num     int64
	text    string
}

// compare orders two values one way or the other, with a missing answer last
// in both directions.
func (v sortValue) compare(w sortValue, desc bool) int {
	switch {
	case v.missing && w.missing:
		return 0
	case v.missing:
		return 1
	case w.missing:
		return -1
	}

	c := cmp.Or(cmp.Compare(v.rank, w.rank), cmp.Compare(v.num, w.num), strings.Compare(v.text, w.text))
	if desc {
		return -c
	}

	return c
}

// sortValue reads what one key compares for one submission.
//
// Against today's definition, as ListLine is, so that the order is the one
// the options are in now. A stored value whose option has since been removed
// sorts after every option still there, by its value, rather than wherever
// its old position happens to land among the new ones.
func (f Form) sortValue(by string, a Answers, at time.Time) sortValue {
	if by == SortSubmitted {
		return sortValue{num: at.UnixNano()}
	}

	ans, ok := a.Field(by)
	if !ok || len(ans.Values) == 0 {
		return sortValue{missing: true}
	}

	fld, _ := f.Field(by)
	v := ans.Values[0]

	switch {
	case fld.Kind.HasOptions():
		// The earliest option ticked, for a choice of several: the issue that
		// asked for this offered that or refusing such a question as a key,
		// and the earliest is what somebody reading a sheet sorted by it
		// expects to see first.
		best := sortValue{rank: len(fld.Options), text: strings.ToLower(v)}

		for _, val := range ans.Values {
			i := slices.IndexFunc(fld.Options, func(o Option) bool { return o.Value == val })
			if i >= 0 && i < best.rank {
				best = sortValue{rank: i}
			}
		}

		return best

	case fld.Kind == KindNumber:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return sortValue{num: n}
		}

	case fld.Kind == KindAmount:
		if m, err := types.ParseMoney(v); err == nil {
			return sortValue{num: int64(m)}
		}

	case fld.Kind == KindCheckbox:
		// Ticked is the only answer a box has; unticked is no answer, and
		// goes to the bottom with the rest.
		return sortValue{}

	case fld.Kind.Temporal():
		// Stored as 2006-01-02, 15:04 and 2006-01-02T15:04, each the same
		// width throughout, so the text order is the time order.
		return sortValue{text: v}
	}

	// Text, and anything above that could not be read as what its kind
	// stores: compared as written, without regard to case. A number that
	// fails to parse ranks after every one that did, rather than as zero.
	rank := 0
	if fld.Kind == KindNumber || fld.Kind == KindAmount {
		rank = 1
	}

	return sortValue{rank: rank, text: strings.ToLower(v)}
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

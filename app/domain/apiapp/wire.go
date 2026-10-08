package apiapp

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// The wire types. Field names are spelled out here rather than inherited from
// the domain's, because they are a contract with programs this repository
// cannot see: renaming a Go field must not rename a key somebody's script
// reads. They are also deliberately not formdb's stored shape, although the
// two look alike today -- that one is a compatibility surface with older
// binaries reading the database, and this one is a compatibility surface with
// clients, and the day either has to change it must not drag the other along.
//
// docs/api.md is the reference for all of this, written for whoever -- or
// whatever -- is going to call it.

// definitionDoc is a form's definition: what a client sends to make or replace
// one, and exactly what it is handed back in [formDoc.Definition], so that
// "read it, change it, send it back" is a loop that needs no translation.
//
// Money is whole minor units throughout -- 1200 is twelve dollars -- as it is
// in Stripe's API and in the domain. A decimal string would be friendlier to
// type and is one more thing for a program to round.
type definitionDoc struct {
	// Slug is the form's name, the word in its address. Read when a form is
	// made and ignored afterwards, because the name never changes: the path a
	// PUT is sent to says which form it is.
	Slug string `json:"slug,omitempty"`

	Title string `json:"title"`
	Intro string `json:"intro,omitempty"`

	OpensAt         time.Time `json:"opens_at,omitzero"`
	ClosesAt        time.Time `json:"closes_at,omitzero"`
	ClosedNote      string    `json:"closed_note,omitempty"`
	ChangeableUntil time.Time `json:"changeable_until,omitzero"`

	// Currency is omitted to mean the service's own, which is the only one it
	// takes today.
	Currency string   `json:"currency,omitempty"`
	Origins  []string `json:"origins,omitzero"`

	ReturnURL string `json:"return_url,omitempty"`

	MinPerOrder int   `json:"min_per_order,omitempty"`
	MaxPerOrder int   `json:"max_per_order,omitempty"`
	MinTotal    int64 `json:"min_total,omitempty"`
	MaxTotal    int64 `json:"max_total,omitempty"`

	PaymentRequired bool   `json:"payment_required,omitempty"`
	PaymentNote     string `json:"payment_note,omitempty"`

	DailyCap int `json:"daily_cap,omitempty"`

	Confirmation string   `json:"confirmation,omitempty"`
	Notify       []string `json:"notify,omitzero"`

	Listing *listingDoc `json:"listing,omitempty"`

	Fields []fieldDoc `json:"fields"`
	Items  []itemDoc  `json:"items"`
}

type fieldDoc struct {
	// Name is omitted on a new field, which is then given one -- the same
	// one the builder would have. A field keeps its name for good; see
	// formbus.Revise for what happens to one left out.
	Name  string `json:"name,omitempty"`
	Label string `json:"label"`
	Kind  string `json:"kind"`

	Required bool `json:"required,omitempty"`

	Help         string `json:"help,omitempty"`
	Placeholder  string `json:"placeholder,omitempty"`
	Autocomplete string `json:"autocomplete,omitempty"`

	MinLength int `json:"min_length,omitempty"`
	MaxLength int `json:"max_length,omitempty"`

	// Pointers, because a bound of zero and no bound are different rules.
	Min *int64 `json:"min,omitempty"`
	Max *int64 `json:"max,omitempty"`

	Pattern     string `json:"pattern,omitempty"`
	PatternNote string `json:"pattern_note,omitempty"`

	Earliest string `json:"earliest,omitempty"`
	Latest   string `json:"latest,omitempty"`

	Options []optionDoc `json:"options,omitzero"`

	ShowIf *conditionDoc `json:"show_if,omitempty"`
}

type optionDoc struct {
	Value string `json:"value"`

	// Label is omitted to mean the same as Value, which is what the builder's
	// "one per line" box does with a line that has no bar in it.
	Label string `json:"label,omitempty"`
}

type conditionDoc struct {
	Field string   `json:"field"`
	Is    []string `json:"is"`
}

type itemDoc struct {
	ID    string `json:"id,omitempty"`
	Label string `json:"label"`
	Note  string `json:"note,omitempty"`
	Price int64  `json:"price"`
	Max   int    `json:"max,omitempty"`
}

type listingDoc struct {
	Heading     string `json:"heading,omitempty"`
	Line        string `json:"line"`
	Limit       int    `json:"limit,omitempty"`
	OldestFirst bool   `json:"oldest_first,omitempty"`
}

// formDoc is what every read or write of one form answers with.
type formDoc struct {
	Slug string `json:"slug"`

	// Live says whether the public is being served this form.
	Live bool `json:"live"`

	// Editable is false for a form that ships with the service, which is
	// changed by a release and never through here.
	Editable bool `json:"editable"`

	// Problems is everything that would stop this definition being
	// published: Check's list, in Check's words. Empty means it could go live
	// now. Always present, so a client can test its length without asking
	// whether it is there.
	Problems []string `json:"problems"`

	// Retired is the field names and item ids this form has used and
	// dropped, which nothing may be called again. Read-only: it is the form's
	// memory, not a setting.
	Retired []string `json:"retired"`

	// Version is the fingerprint of what a submission means. It changes when
	// a question or a price does, and every half-filled copy of the form open
	// in somebody's browser is re-rendered when it does.
	Version string `json:"version,omitempty"`

	CreatedAt   time.Time `json:"created_at,omitzero"`
	UpdatedAt   time.Time `json:"updated_at,omitzero"`
	PublishedAt time.Time `json:"first_published_at,omitzero"`

	// PublicURL and EmbedHTML are what somebody does with a live form: open
	// it, or paste it into a page. Present only when the form is live and the
	// service knows its own public address.
	PublicURL string `json:"public_url,omitempty"`
	EmbedHTML string `json:"embed_html,omitempty"`

	Definition definitionDoc `json:"definition"`
}

// formOf decodes a definition into the domain's terms.
//
// Shape only: a string that is not an origin is refused here because there is
// no Origin to put it in, but whether the form makes sense is Check's
// question, asked once over the whole thing. Problems are collected rather
// than returned at the first, for the reason [formbus.DefinitionError] gives.
func formOf(d definitionDoc) (formbus.Form, []string) {
	var problems []string

	f := formbus.Form{
		Title:           d.Title,
		Intro:           d.Intro,
		OpensAt:         d.OpensAt,
		ClosesAt:        d.ClosesAt,
		ClosedNote:      d.ClosedNote,
		ChangeableUntil: d.ChangeableUntil,
		Currency:        d.Currency,
		ReturnURL:       d.ReturnURL,
		MinPerOrder:     d.MinPerOrder,
		MaxPerOrder:     d.MaxPerOrder,
		MinTotal:        types.Money(d.MinTotal),
		MaxTotal:        types.Money(d.MaxTotal),
		PaymentRequired: d.PaymentRequired,
		PaymentNote:     d.PaymentNote,
		DailyCap:        d.DailyCap,
		Confirmation:    d.Confirmation,
		Notify:          slices.Clone(d.Notify),
	}

	if f.Currency == "" {
		f.Currency = formbus.DefaultCurrency()
	}

	for _, raw := range d.Origins {
		o, err := types.ParseOrigin(raw)
		if err != nil {
			problems = append(problems, fmt.Sprintf("origins: %q is not a site address like https://www.example.org", raw))

			continue
		}

		f.Origins = append(f.Origins, o)
	}

	if d.Listing != nil {
		f.Listing = formbus.Listing{
			Heading:     d.Listing.Heading,
			Line:        d.Listing.Line,
			Limit:       d.Listing.Limit,
			OldestFirst: d.Listing.OldestFirst,
		}
	}

	for _, fd := range d.Fields {
		fld := formbus.Field{
			Name:         fd.Name,
			Label:        fd.Label,
			Kind:         formbus.Kind(fd.Kind),
			Required:     fd.Required,
			Help:         fd.Help,
			Placeholder:  fd.Placeholder,
			Autocomplete: fd.Autocomplete,
			MinLen:       fd.MinLength,
			MaxLen:       fd.MaxLength,
			Min:          fd.Min,
			Max:          fd.Max,
			Pattern:      fd.Pattern,
			PatternNote:  fd.PatternNote,
			Earliest:     fd.Earliest,
			Latest:       fd.Latest,
		}

		// An unknown kind would otherwise be given a generated name built from
		// whatever was sent, and then refused by Check under that name -- a
		// problem about "banana_1" for a field somebody called "Diet".
		if !fld.Kind.Known() {
			problems = append(problems, fmt.Sprintf("the field %q has kind %q, which is not one of: %s", fieldWord(fd), fd.Kind, kindList()))
		}

		for _, o := range fd.Options {
			label := o.Label
			if label == "" {
				label = o.Value
			}

			fld.Options = append(fld.Options, formbus.Option{Value: o.Value, Label: label})
		}

		if fd.ShowIf != nil {
			fld.ShowIf = &formbus.Condition{Field: fd.ShowIf.Field, Is: slices.Clone(fd.ShowIf.Is)}
		}

		f.Fields = append(f.Fields, fld)
	}

	for _, it := range d.Items {
		f.Items = append(f.Items, formbus.Item{
			ID:    it.ID,
			Label: it.Label,
			Note:  it.Note,
			Price: types.Money(it.Price),
			Max:   it.Max,
		})
	}

	return f, problems
}

// fieldWord names a field in a problem before it has a name of its own.
func fieldWord(fd fieldDoc) string {
	if fd.Name != "" {
		return fd.Name
	}

	return fd.Label
}

func kindList() string {
	var names []string

	for _, k := range formbus.Kinds() {
		names = append(names, string(k))
	}

	return strings.Join(names, ", ")
}

// definitionOf is formOf the other way.
func definitionOf(f formbus.Form) definitionDoc {
	d := definitionDoc{
		Slug:            f.ID.String(),
		Title:           f.Title,
		Intro:           f.Intro,
		OpensAt:         f.OpensAt,
		ClosesAt:        f.ClosesAt,
		ClosedNote:      f.ClosedNote,
		ChangeableUntil: f.ChangeableUntil,
		Currency:        f.Currency,
		ReturnURL:       f.ReturnURL,
		MinPerOrder:     f.MinPerOrder,
		MaxPerOrder:     f.MaxPerOrder,
		MinTotal:        int64(f.MinTotal),
		MaxTotal:        int64(f.MaxTotal),
		PaymentRequired: f.PaymentRequired,
		PaymentNote:     f.PaymentNote,
		DailyCap:        f.DailyCap,
		Confirmation:    f.Confirmation,
		Notify:          slices.Clone(f.Notify),

		// Empty lists rather than null, so that a client appending to them
		// does not first have to make them.
		Fields: []fieldDoc{},
		Items:  []itemDoc{},
	}

	for _, o := range f.Origins {
		d.Origins = append(d.Origins, o.String())
	}

	if f.Listing != (formbus.Listing{}) {
		d.Listing = &listingDoc{
			Heading:     f.Listing.Heading,
			Line:        f.Listing.Line,
			Limit:       f.Listing.Limit,
			OldestFirst: f.Listing.OldestFirst,
		}
	}

	for _, fld := range f.Fields {
		fd := fieldDoc{
			Name:         fld.Name,
			Label:        fld.Label,
			Kind:         string(fld.Kind),
			Required:     fld.Required,
			Help:         fld.Help,
			Placeholder:  fld.Placeholder,
			Autocomplete: fld.Autocomplete,
			MinLength:    fld.MinLen,
			MaxLength:    fld.MaxLen,
			Min:          fld.Min,
			Max:          fld.Max,
			Pattern:      fld.Pattern,
			PatternNote:  fld.PatternNote,
			Earliest:     fld.Earliest,
			Latest:       fld.Latest,
		}

		for _, o := range fld.Options {
			fd.Options = append(fd.Options, optionDoc{Value: o.Value, Label: o.Label})
		}

		if fld.ShowIf != nil {
			fd.ShowIf = &conditionDoc{Field: fld.ShowIf.Field, Is: slices.Clone(fld.ShowIf.Is)}
		}

		d.Fields = append(d.Fields, fd)
	}

	for _, it := range f.Items {
		d.Items = append(d.Items, itemDoc{
			ID:    it.ID,
			Label: it.Label,
			Note:  it.Note,
			Price: int64(it.Price),
			Max:   it.Max,
		})
	}

	return d
}

// summaryDoc is one row of the list of forms.
type summaryDoc struct {
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Live     bool   `json:"live"`
	Editable bool   `json:"editable"`

	// Role is what this account holds on the form: results, door or admin.
	// Admin is what it takes to read or change the definition; results is
	// enough to read the submissions.
	Role string `json:"role"`

	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// submissionsDoc is a form's submissions laid out against its questions --
// the feed's shape, with what was ordered and paid added, because a program
// reading through somebody's own account may see everything the admin page
// shows.
type submissionsDoc struct {
	Form        string           `json:"form"`
	Title       string           `json:"title"`
	GeneratedAt time.Time        `json:"generated_at"`
	Fields      []questionDoc    `json:"fields"`
	Submissions []submissionDoc  `json:"submissions"`
	Items       []itemSummaryDoc `json:"items,omitzero"`
}

type questionDoc struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
	Multiple bool   `json:"multiple"`
}

type itemSummaryDoc struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Price int64  `json:"price"`
}

type submissionDoc struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Status    string    `json:"status"`
	Email     string    `json:"email,omitempty"`

	// Answers has a key for every question the form asks now: a string, a
	// list of strings for a question that takes several, or null for one not
	// answered. The feed's rule, for the feed's reason.
	Answers map[string]any `json:"answers"`

	Lines    []lineDoc `json:"lines,omitzero"`
	Total    int64     `json:"total"`
	Currency string    `json:"currency,omitempty"`
}

type lineDoc struct {
	Item     string `json:"item"`
	Label    string `json:"label"`
	Price    int64  `json:"price"`
	Quantity int    `json:"quantity"`
	Amount   int64  `json:"amount"`
}

// submissionOf lays one submission out against the definition.
func submissionOf(f formbus.Form, s submissionbus.Submission) submissionDoc {
	answers := make(map[string]any, len(f.Fields))

	for _, fld := range f.Questions() {
		ans, ok := s.Answers.Field(fld.Name)

		switch {
		case !ok || len(ans.Values) == 0:
			answers[fld.Name] = nil
		case fld.Kind.MultiValue():
			answers[fld.Name] = ans.Values
		default:
			answers[fld.Name] = ans.Value()
		}
	}

	// And any answer under a name the form no longer asks, so that a question
	// removed after people answered it is not silently dropped from what a
	// program reads. Its name is retired, so it cannot collide with a key
	// the loop above wrote.
	for _, ans := range s.Answers.Fields {
		if _, asked := answers[ans.Name]; asked {
			continue
		}

		if ans.Kind.MultiValue() {
			answers[ans.Name] = ans.Values
		} else {
			answers[ans.Name] = ans.Value()
		}
	}

	doc := submissionDoc{
		ID:        s.ID.String(),
		CreatedAt: s.CreatedAt.UTC(),
		UpdatedAt: s.UpdatedAt.UTC(),
		Status:    s.Status.String(),
		Email:     s.Email.String(),
		Answers:   answers,
		Total:     int64(s.Answers.Total),
		Currency:  s.Answers.Currency,
	}

	for _, l := range s.Answers.Lines {
		doc.Lines = append(doc.Lines, lineDoc{
			Item:     l.ItemID,
			Label:    l.Label,
			Price:    int64(l.Price),
			Quantity: l.Qty,
			Amount:   int64(l.Amount),
		})
	}

	return doc
}

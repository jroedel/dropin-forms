package formapp

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// buildView is the builder's home: the whole form at a glance, and a button
// beside every part of it.
type buildView struct {
	FormID  string
	Title   string
	Live    bool
	Version string
	Updated string

	Fields []fieldRow
	Items  []itemRow
	Sells  bool

	Settings []settingRow

	// Kinds is the dropdown on the "add a question" form at the bottom of this
	// page, in formbus's own order.
	Kinds []kindChoice

	// Problems is every reason this definition could not go live, from
	// [formbus.Form.Check]. On a draft it reads as a list of what is left to
	// do; on a live form it should be empty, since nothing that fails Check
	// can have been saved.
	Problems []string

	// Deletable is a form that has never been published, and is therefore one
	// no submission can point at. See formbus.Business.Delete.
	Deletable bool

	// Snippet is the two lines somebody pastes into their own website, and
	// Preview is the form's public address. Both are empty when the embed
	// origin is not configured; the page then says which setting to add rather
	// than showing markup naming the wrong host.
	Snippet string
	Preview string

	Done    string
	Problem string
}

// Any reports whether this form has anything in it yet, so that a new one gets
// a sentence rather than an empty table.
func (v buildView) Any() bool { return len(v.Fields) > 0 }

// fieldRow is one field in the list, already turned into the words the page
// shows. The template does no formatting of its own, which is what keeps the
// rules for reading a definition in Go where they can be tested.
type fieldRow struct {
	Name     string
	Label    string
	Kind     string
	Required bool

	// Detail is the field's own rules in one short phrase -- its options, its
	// bounds, its pattern -- so that the list is readable without opening
	// every field in turn.
	Detail string

	// ShownWhen is the condition, in words, or empty for a field that is
	// always shown.
	ShownWhen string

	First bool
	Last  bool
}

// itemRow is one thing the form sells.
type itemRow struct {
	ID    string
	Label string
	Note  string
	Price string
	Max   string

	First bool
	Last  bool
}

// settingRow is one line of the settings summary on the builder's home page.
// A label and what it currently says, with the empty case already turned into
// a sentence rather than left as a blank cell.
type settingRow struct {
	Label string
	Value string

	// Unset marks a value nobody has chosen, so the page can show it as
	// absent rather than as the words a handler happened to substitute.
	Unset bool
}

// load resolves the slug in the path and reads what is being edited.
//
// Three outcomes and they are deliberately different pages. A slug that names
// a definition compiled into the binary gets a page saying so, because the
// reader holds admin on it -- the gate let them through -- and a 404 would be
// a lie about a form they can see listed. A slug nothing claims gets a 404. A
// storage failure gets a 500 and a line in the log.
func (a app) load(w http.ResponseWriter, r *http.Request) (formbus.Stored, bool) {
	slug, err := types.ParseSlug(r.PathValue(mid.FormSlugParam))
	if err != nil {
		a.notFound(w)

		return formbus.Stored{}, false
	}

	if !a.cfg.Catalog.Editable(slug) {
		a.builtIn(w, r, slug)

		return formbus.Stored{}, false
	}

	s, err := a.cfg.Catalog.Draft(r.Context(), slug)

	switch {
	case errors.Is(err, formbus.ErrNotFound):
		a.notFound(w)

		return formbus.Stored{}, false
	case err != nil:
		a.oops(w, r, "a form being edited could not be read", err)

		return formbus.Stored{}, false
	}

	return s, true
}

// save writes a definition back and reports whether it went.
//
// The refusal it exists to handle is a [formbus.DefinitionError]: a change that
// would leave a live form unusable. That is not an error to log and apologise
// for, it is the answer -- a list of what is wrong, which the caller puts on
// the page it re-renders. Anything else is a failure at our end.
func (a app) save(w http.ResponseWriter, r *http.Request, f formbus.Form, live bool) ([]string, bool) {
	me, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "the builder was reached with no account", nil)

		return nil, false
	}

	err := a.cfg.Catalog.Save(r.Context(), time.Now(), me.ID, f, live)

	if bad, is := errors.AsType[formbus.DefinitionError](err); is {
		return bad.Problems, false
	}

	if err != nil {
		a.oops(w, r, "a form could not be saved", err)

		return nil, false
	}

	return nil, true
}

// show renders the builder's home page for a definition.
func (a app) show(w http.ResponseWriter, r *http.Request, status int, s formbus.Stored, view buildView) {
	f := s.Form

	view.FormID = f.ID.String()
	view.Title = f.Title
	view.Live = s.Live
	view.Version = f.Version
	view.Sells = f.Sells()
	view.Deletable = s.PublishedAt.IsZero()
	view.Updated = s.UpdatedAt.Local().Format("2 Jan 2006, 15:04")
	view.Settings = settingsSummary(f)
	view.Kinds = kindsFor("")

	// Check on a copy, because it compiles the patterns into the fields it is
	// given and this one is about to be thrown away. Its answer here is
	// advisory: a to-do list for a draft, and on a live form a list that
	// should be empty, since Save would not have stored anything else.
	check := f
	if err := check.Check(); err != nil {
		if bad, is := errors.AsType[formbus.DefinitionError](err); is {
			view.Problems = bad.Problems
		} else {
			view.Problems = []string{err.Error()}
		}
	}

	for i, fld := range f.Fields {
		view.Fields = append(view.Fields, fieldRow{
			Name:      fld.Name,
			Label:     fld.Label,
			Kind:      string(fld.Kind),
			Required:  fld.Required,
			Detail:    detailOf(fld, f.Currency),
			ShownWhen: shownWhen(fld),
			First:     i == 0,
			Last:      i == len(f.Fields)-1,
		})
	}

	for i, it := range f.Items {
		row := itemRow{
			ID:    it.ID,
			Label: it.Label,
			Note:  it.Note,
			Price: formbus.Show(it.Price, f.Currency),
			First: i == 0,
			Last:  i == len(f.Items)-1,
		}

		if it.Max > 0 {
			row.Max = strconv.Itoa(it.Max)
		}

		view.Items = append(view.Items, row)
	}

	// The paste snippet and the preview link, which are the whole output of
	// this product and are what somebody came here to get. Only when the embed
	// origin is configured -- see Config.EmbedBaseURL -- and only for a live
	// form, because markup naming a form that answers 404 is worse than no
	// markup at all.
	if a.cfg.EmbedBaseURL != "" && s.Live {
		view.Preview = a.cfg.EmbedBaseURL + "/f/" + f.ID.String()
		view.Snippet = fmt.Sprintf(
			"<div data-dropin-form=%q></div>\n<script src=%q async></script>",
			f.ID.String(), a.cfg.EmbedBaseURL+"/embed.js")
	}

	a.cfg.Render.Render(w, r, status, "build", view)
}

// settingsSummary is the settings panel on the home page: what is set, in the
// order somebody thinks about it, with every unset value already turned into
// the sentence that says so.
func settingsSummary(f formbus.Form) []settingRow {
	unset := func(label string) settingRow {
		return settingRow{Label: label, Value: "not set", Unset: true}
	}

	set := func(label, value string) settingRow {
		if value == "" {
			return unset(label)
		}

		return settingRow{Label: label, Value: value}
	}

	// Shown in the reader's own zone with the zone named, never as the RFC
	// 3339 the definition stores. "Closes on the 17th at 23:59 CDT" is the
	// sentence somebody checks against the parish calendar; the offset-bearing
	// timestamp is what stops it being ambiguous, and it has done that job by
	// the time it reaches this page.
	when := func(label string, t time.Time) settingRow {
		if t.IsZero() {
			return unset(label)
		}

		return settingRow{Label: label, Value: t.Local().Format("2 Jan 2006, 15:04 MST")}
	}

	rows := []settingRow{
		set("Title", f.Title),
		set("Introduction", f.Intro),
		when("Opens", f.OpensAt),
		when("Closes", f.ClosesAt),
		set("When closed, it says", f.ClosedNote),
		set("Confirmation", f.Confirmation),
	}

	if len(f.Origins) == 0 {
		rows = append(rows, settingRow{
			Label: "Can be embedded on",
			Value: "the installation's own list",
			Unset: true,
		})
	} else {
		origins := make([]string, 0, len(f.Origins))
		for _, o := range f.Origins {
			origins = append(origins, o.String())
		}

		rows = append(rows, settingRow{Label: "Can be embedded on", Value: strings.Join(origins, ", ")})
	}

	// The money rows are only shown on a form that has something to charge
	// for. On one that does not they would be a panel of zeroes inviting
	// somebody to fill them in, which is how a survey acquires a minimum
	// order.
	if f.Sells() || f.PaymentRequired {
		rows = append(rows,
			set("Currency", strings.ToUpper(f.Currency)),
			set("Sends buyers back to", f.ReturnURL),
			set("Order size", countRange(f.MinPerOrder, f.MaxPerOrder, "no limit")),
			set("Order total", moneyRange(f.MinTotal, f.MaxTotal, f.Currency)),
		)

		if f.PaymentRequired {
			rows = append(rows, set("Empty orders", "refused: "+f.PaymentNote))
		} else {
			rows = append(rows, settingRow{Label: "Empty orders", Value: "accepted", Unset: true})
		}
	}

	if len(f.Notify) > 0 {
		rows = append(rows, set("Also emails", strings.Join(f.Notify, ", ")))
	}

	if f.DailyCap > 0 {
		rows = append(rows, set("Submissions a day", strconv.Itoa(f.DailyCap)))
	}

	return rows
}

// detailOf is a field's own rules in one phrase, for the list.
func detailOf(fld formbus.Field, currency string) string {
	var parts []string

	if fld.Kind.HasOptions() {
		switch n := len(fld.Options); n {
		case 0:
			parts = append(parts, "no options yet")
		case 1:
			parts = append(parts, "1 option")
		default:
			parts = append(parts, strconv.Itoa(n)+" options")
		}
	}

	if fld.Kind.Bounded() {
		// The unit follows the kind, exactly as it does on formbus.Field.Min:
		// an amount field's bounds are money and everything else's are counts.
		if fld.Kind == formbus.KindAmount {
			parts = append(parts, boundPhrase(fld.Min, fld.Max, func(n int64) string {
				return formbus.Show(types.Money(n), currency)
			}))
		} else {
			parts = append(parts, boundPhrase(fld.Min, fld.Max, func(n int64) string {
				return strconv.Itoa(int(n))
			}))
		}
	}

	if fld.Kind.Textual() && (fld.MinLen > 0 || fld.MaxLen > 0) {
		parts = append(parts, countRange(fld.MinLen, fld.MaxLen, "")+" characters")
	}

	if fld.Pattern != "" {
		parts = append(parts, "a set format")
	}

	return strings.Join(compact(parts), ", ")
}

// boundPhrase writes a pair of optional bounds the way somebody reads them.
func boundPhrase(min, max *int64, show func(int64) string) string {
	switch {
	case min != nil && max != nil:
		return show(*min) + " to " + show(*max)
	case min != nil:
		return show(*min) + " or more"
	case max != nil:
		return show(*max) + " at most"
	default:
		return ""
	}
}

// countRange writes a pair of zero-means-unbounded counts. empty is what to
// say when neither end is set.
func countRange(min, max int, empty string) string {
	switch {
	case min > 0 && max > 0:
		return strconv.Itoa(min) + " to " + strconv.Itoa(max)
	case min > 0:
		return strconv.Itoa(min) + " or more"
	case max > 0:
		return strconv.Itoa(max) + " at most"
	default:
		return empty
	}
}

func moneyRange(min, max types.Money, currency string) string {
	switch {
	case min > 0 && max > 0:
		return formbus.Show(min, currency) + " to " + formbus.Show(max, currency)
	case min > 0:
		return formbus.Show(min, currency) + " or more"
	case max > 0:
		return formbus.Show(max, currency) + " at most"
	default:
		return ""
	}
}

// shownWhen is a condition in words, or empty for a field that is always
// shown.
func shownWhen(fld formbus.Field) string {
	if fld.ShowIf == nil {
		return ""
	}

	values := make([]string, 0, len(fld.ShowIf.Is))
	for _, v := range fld.ShowIf.Is {
		values = append(values, strconv.Quote(v))
	}

	switch len(values) {
	case 0:
		return "only when " + fld.ShowIf.Field + " is — nothing is named, so never"
	case 1:
		return "only when " + fld.ShowIf.Field + " is " + values[0]
	default:
		return "only when " + fld.ShowIf.Field + " is one of " + strings.Join(values, ", ")
	}
}

func compact(in []string) []string {
	out := in[:0]

	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}

	return out
}

// builtIn is the page for a definition that ships in this release.
//
// A page rather than a refusal, because the reader holds admin on this form --
// the gate let them in -- and the honest answer is what to do instead, not
// that they may not. The form is listed beside their editable ones, so
// something has to explain the difference the first time they click it.
func (a app) builtIn(w http.ResponseWriter, r *http.Request, slug types.Slug) {
	title := slug.String()
	if f, err := a.cfg.Catalog.ByID(slug); err == nil {
		title = f.Title
	}

	http.Error(w,
		title+" is part of this release rather than something built here, so it is changed by editing its file in the repository and deploying. Everything you build on this site can be edited from the forms list.",
		http.StatusConflict)
}

// notFound is plain text and the same sentence mid.RequireFormRole uses for
// the same case, for the reason peopleapp.notFound sets out: borrowing another
// app's template would make this app depend on that one being mounted.
func (a app) notFound(w http.ResponseWriter) {
	http.Error(w, "there is no form by that name.", http.StatusNotFound)
}

// oops logs the detail and shows a sentence. The error never reaches the page:
// this surface is behind a session, but a database error can still name a
// table.
func (a app) oops(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.cfg.Log.Error(what,
		"request_id", web.RequestIDFrom(r.Context()), "error", err)
	http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)
}

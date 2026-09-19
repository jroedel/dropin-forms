package formapp

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
)

// settingsView is everything about a form that is not one of its fields or one
// of the things it sells.
//
// All strings, including the numbers and the dates. What a person typed goes
// back onto the page exactly as they typed it when something else on the form
// was wrong, and re-deriving "12.0" from a parsed 1200 would hand them back
// something they did not write and would quietly correct a typo they meant to
// look at.
type settingsView struct {
	FormID string
	Live   bool

	Title      string
	Intro      string
	OpensAt    string
	ClosesAt   string
	ClosedNote string

	Zone string

	Origins   string
	ReturnURL string

	Currency string

	MinPerOrder string
	MaxPerOrder string
	MinTotal    string
	MaxTotal    string

	PaymentRequired bool
	PaymentNote     string

	Confirmation string
	Notify       string
	DailyCap     string

	// Sells says whether this form has anything for sale, which decides
	// whether the money half of the page is shown at all. A survey should not
	// have to scroll past a minimum order total to reach its confirmation
	// message.
	Sells bool

	Problems []string
	Problem  string
}

// settings shows the page.
func (a app) settings(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "build-settings", settingsOf(s))
}

// settingsOf fills the page in from a definition.
func settingsOf(s formbus.Stored) settingsView {
	f := s.Form

	v := settingsView{
		FormID:          f.ID.String(),
		Live:            s.Live,
		Title:           f.Title,
		Intro:           f.Intro,
		OpensAt:         writeWhen(f.OpensAt),
		ClosesAt:        writeWhen(f.ClosesAt),
		ClosedNote:      f.ClosedNote,
		Zone:            zoneName(time.Now()),
		Origins:         writeOrigins(f.Origins),
		ReturnURL:       f.ReturnURL,
		Currency:        strings.ToUpper(f.Currency),
		PaymentRequired: f.PaymentRequired,
		PaymentNote:     f.PaymentNote,
		Confirmation:    f.Confirmation,
		Notify:          strings.Join(f.Notify, "\n"),
		Sells:           f.Sells(),
	}

	// Zero means unbounded on every one of these, so it is shown as an empty
	// box rather than as a nought. A nought in a maximum field reads as a
	// limit of nothing, which is the opposite of what it means.
	v.MinPerOrder = blankZero(f.MinPerOrder)
	v.MaxPerOrder = blankZero(f.MaxPerOrder)
	v.DailyCap = blankZero(f.DailyCap)

	if f.MinTotal > 0 {
		v.MinTotal = f.MinTotal.String()
	}
	if f.MaxTotal > 0 {
		v.MaxTotal = f.MaxTotal.String()
	}

	return v
}

func blankZero(n int) string {
	if n == 0 {
		return ""
	}

	return strconv.Itoa(n)
}

// saveSettings writes them back.
//
// Every parse failure is collected rather than returned at the first one, for
// the same reason [formbus.DefinitionError] lists every problem: this is a
// long page, and being told about one bad date at a time is a bad afternoon.
// The two lists then run together on the page, which is right -- "that is not
// a date" and "it closes before it opens" are the same kind of news to whoever
// is fixing them.
func (a app) saveSettings(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}

	if err := r.ParseForm(); err != nil {
		view := settingsOf(s)
		view.Problem = "We could not read that. Please try again."
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "build-settings", view)

		return
	}

	// What was typed, so a refusal comes back filled in. Built before anything
	// is parsed, because the whole point is to show it back unparsed.
	said := settingsOf(s)
	said.Title = value(r, "title")
	said.Intro = value(r, "intro")
	said.OpensAt = value(r, "opens_at")
	said.ClosesAt = value(r, "closes_at")
	said.ClosedNote = value(r, "closed_note")
	said.Origins = r.PostFormValue("origins")
	said.ReturnURL = value(r, "return_url")
	said.MinPerOrder = value(r, "min_per_order")
	said.MaxPerOrder = value(r, "max_per_order")
	said.MinTotal = value(r, "min_total")
	said.MaxTotal = value(r, "max_total")
	said.PaymentRequired = checked(r, "payment_required")
	said.PaymentNote = value(r, "payment_note")
	said.Confirmation = value(r, "confirmation")
	said.Notify = r.PostFormValue("notify")
	said.DailyCap = value(r, "daily_cap")

	// The definition as it stands, with its fields and items carried through
	// untouched: this page is about everything except them.
	f := s.Form

	f.Title = said.Title
	f.Intro = said.Intro
	f.ClosedNote = said.ClosedNote
	f.ReturnURL = said.ReturnURL
	f.PaymentRequired = said.PaymentRequired
	f.PaymentNote = said.PaymentNote
	f.Confirmation = said.Confirmation
	f.Notify = lines(said.Notify)

	var problems []string

	fail := func(err error) {
		if err != nil {
			problems = append(problems, err.Error())
		}
	}

	var err error

	f.OpensAt, err = parseWhen("The date it opens", said.OpensAt)
	fail(err)

	f.ClosesAt, err = parseWhen("The date it closes", said.ClosesAt)
	fail(err)

	f.Origins, err = parseOrigins(said.Origins)
	fail(err)

	f.MinPerOrder, err = parseCount("The smallest order", said.MinPerOrder)
	fail(err)

	f.MaxPerOrder, err = parseCount("The largest order", said.MaxPerOrder)
	fail(err)

	f.MinTotal, err = parseMoney("The smallest total", said.MinTotal)
	fail(err)

	f.MaxTotal, err = parseMoney("The largest total", said.MaxTotal)
	fail(err)

	f.DailyCap, err = parseCount("The daily limit", said.DailyCap)
	fail(err)

	if len(problems) > 0 {
		said.Problems = problems
		said.Problem = "Nothing was saved."
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "build-settings", said)

		return
	}

	problems, ok = a.save(w, r, f, s.Live)
	if !ok {
		if problems == nil {
			return
		}

		said.Problems = problems
		said.Problem = "This form is on the web, so a change that would stop it working is refused. Nothing was saved and it is still serving what it was."
		a.cfg.Render.Render(w, r, http.StatusConflict, "build-settings", said)

		return
	}

	s.Form = f

	a.show(w, r, http.StatusOK, s, buildView{Done: "The settings are saved."})
}

package muxer_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formtoml"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// The date, time and datetime kinds, through the surfaces as mounted.

// visitForm is a catalogue of one, which is all the embed surface asks of one.
type visitForm struct{ f formbus.Form }

func (v visitForm) ByID(slug types.Slug) (formbus.Form, error) {
	if slug != v.f.ID {
		return formbus.Form{}, formtoml.ErrNotFound
	}

	return v.f, nil
}

func visitSurface(t *testing.T) (http.Handler, *submissionbus.Business) {
	t.Helper()

	f := formbus.Form{
		ID:       mustSlug(t, "visit"),
		Title:    "Book a visit",
		Currency: "usd",
		Fields: []formbus.Field{
			{Name: "day", Label: "Which day", Kind: formbus.KindDate, Required: true},
			{Name: "at", Label: "What time", Kind: formbus.KindTime},
			{Name: "when", Label: "Pick-up", Kind: formbus.KindDateTime},
		},
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("the visit form does not pass Check: %v", err)
	}

	cfg := newConfig(t, nil, nil)
	cfg.Embed.Now = func() time.Time { return beforeTheFeast }
	cfg.Embed.Forms = visitForm{f}

	subs, ok := cfg.Embed.Submissions.(*submissionbus.Business)
	if !ok {
		t.Fatalf("the test config holds a %T rather than the submission domain", cfg.Embed.Submissions)
	}

	return embedOf(t, cfg), subs
}

// The browser's own pickers, so that nearly everybody sends the stored shape
// without being told what it is.
func TestTemporalFieldsRenderAsTheBrowsersPickers(t *testing.T) {
	h, _ := visitSurface(t)

	body := getPage(t, h, "/f/visit").Body.String()

	for _, want := range []string{
		`type="date" name="day"`,
		`type="time" name="at"`,
		`type="datetime-local" name="when"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not contain %s:\n%s", want, short(body))
		}
	}
}

func TestATemporalAnswerIsStoredInItsOneShape(t *testing.T) {
	h, subs := visitSurface(t)

	grant := grantIn(t, getPage(t, h, "/f/visit").Body.String())

	w := postTo(t, h, "/f/visit", url.Values{
		embedapp.GrantField: {grant},
		"day":               {"2026-10-17"},
		"at":                {"3:30pm"},
		"when":              {"2026-10-17T09:05"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d:\n%s", w.Code, short(w.Body.String()))
	}

	stored, err := subs.ByForm(t.Context(), mustSlug(t, "visit"))
	if err != nil || len(stored) != 1 {
		t.Fatalf("ByForm = %d, %v; want one", len(stored), err)
	}

	for field, want := range map[string]string{"day": "2026-10-17", "at": "15:30", "when": "2026-10-17T09:05"} {
		if got, _ := stored[0].Answers.Field(field); got.Value() != want {
			t.Errorf("%s stored as %q, want %q", field, got.Value(), want)
		}
	}
}

// A browser that draws a text box instead, and somebody typing the date the
// way they would write it on paper: refused with the shape to use, and what
// they typed still in the box.
func TestADateThatCannotBeReadIsRefusedWithTheShapeToType(t *testing.T) {
	h, _ := visitSurface(t)

	grant := grantIn(t, getPage(t, h, "/f/visit").Body.String())

	w := postTo(t, h, "/f/visit", url.Values{
		embedapp.GrantField: {grant},
		"day":               {"10/17/2026"},
	})

	body := w.Body.String()

	if !strings.Contains(body, "Which day needs a date, written like 2026-10-17.") {
		t.Errorf("the refusal does not say what to type:\n%s", short(body))
	}
	if !strings.Contains(body, `value="10/17/2026"`) {
		t.Errorf("what was typed is not put back:\n%s", short(body))
	}
}

// Each kind can be chosen in the builder, which offers it in words.
func TestTheBuilderAddsTemporalFields(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	page := a.get(t, "/forms/supper-2026/edit", cookie).Body.String()

	for kind, label := range map[formbus.Kind]string{
		formbus.KindDate:     "A date",
		formbus.KindTime:     "A time of day",
		formbus.KindDateTime: "A date and a time",
	} {
		if !strings.Contains(page, label) {
			t.Errorf("the builder does not offer %q", label)
		}

		w := a.post(t, "/forms/supper-2026/edit/fields", url.Values{
			"name": {string(kind) + "_asked"}, "label": {label}, "kind": {string(kind)},
		}, cookie)
		if w.Code != http.StatusSeeOther {
			t.Errorf("adding a %s field = %d:\n%s", kind, w.Code, w.Body)
		}
	}

	s, err := a.catalogue.Draft(t.Context(), mustSlug(t, "supper-2026"))
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}

	if len(s.Form.Fields) != 3 {
		t.Errorf("the form has %d fields, want the three added", len(s.Form.Fields))
	}
}

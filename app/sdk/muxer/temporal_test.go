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
			{
				Name: "slot", Label: "Your appointment", Kind: formbus.KindDateTime,
				Earliest: "2026-09-30", Latest: "2026-10-13",
			},
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
			"label": {label}, "kind": {string(kind)},
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

// The range reaches the picker as min and max -- a day standing for its whole
// self, so the latest is the last minute of the 13th -- and is said in words
// beside the question, for a browser that draws a text box instead.
func TestADatetimeRangeReachesThePickerAndThePage(t *testing.T) {
	h, _ := visitSurface(t)

	body := getPage(t, h, "/f/visit").Body.String()

	for _, want := range []string{
		`name="slot" value=""`,
		`min="2026-09-30T00:00"`,
		`max="2026-10-13T23:59"`,
		"Between Wednesday 30 September 2026 and Tuesday 13 October 2026.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not contain %s:\n%s", want, short(body))
		}
	}
}

func TestADatetimeOutsideItsRangeIsRefused(t *testing.T) {
	h, subs := visitSurface(t)

	for in, ok := range map[string]bool{"2026-10-13T18:00": true, "2026-10-14T09:00": false} {
		grant := grantIn(t, getPage(t, h, "/f/visit").Body.String())

		w := postTo(t, h, "/f/visit", url.Values{
			embedapp.GrantField: {grant},
			"day":               {"2026-10-01"},
			"slot":              {in},
		})

		refused := strings.Contains(w.Body.String(),
			"Your appointment has to be between Wednesday 30 September 2026 and Tuesday 13 October 2026.")

		if refused == ok {
			t.Errorf("%s: refused = %v, want %v:\n%s", in, refused, !ok, short(w.Body.String()))
		}
	}

	if stored, _ := subs.ByForm(t.Context(), mustSlug(t, "visit")); len(stored) != 1 {
		t.Errorf("%d stored, want only the one inside the range", len(stored))
	}
}

// The builder takes a range as a person types it, stores it in the field's
// own shape, and refuses one it cannot read with the shape to use.
func TestTheBuilderSetsADatetimeRange(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	if w := a.post(t, "/forms/supper-2026/edit/fields", url.Values{
		"label": {"Your appointment"}, "kind": {"datetime"},
	}, cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("adding the field = %d:\n%s", w.Code, w.Body)
	}

	w := a.post(t, "/forms/supper-2026/edit/fields/datetime_1", url.Values{
		"label": {"Your appointment"}, "kind": {"datetime"},
		"earliest": {"2026-09-30"}, "latest": {"2026-10-13 18:00"},
	}, cookie)
	if w.Code >= 400 {
		t.Fatalf("saving the range = %d:\n%s", w.Code, w.Body)
	}

	s, err := a.catalogue.Draft(t.Context(), mustSlug(t, "supper-2026"))
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}

	if fld, _ := s.Form.Field("datetime_1"); fld.Earliest != "2026-09-30" || fld.Latest != "2026-10-13T18:00" {
		t.Errorf("stored %q to %q, want 2026-09-30 to 2026-10-13T18:00", fld.Earliest, fld.Latest)
	}

	w = a.post(t, "/forms/supper-2026/edit/fields/datetime_1", url.Values{
		"label": {"Your appointment"}, "kind": {"datetime"}, "earliest": {"30/09/2026"},
	}, cookie)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "a day like 2026-10-13") {
		t.Errorf("an unreadable earliest = %d, want 400 naming the shape:\n%s", w.Code, w.Body)
	}
}

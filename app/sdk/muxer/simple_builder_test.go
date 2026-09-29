package muxer_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
)

// The builder, for somebody who has never written HTML: names are generated,
// a question's kind is fixed once it exists, and money is behind a checkbox.

func TestAQuestionIsNamedForItsKindAndANameIsNeverReused(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	add := func(label, kind string) string {
		t.Helper()

		w := a.post(t, "/forms/supper-2026/edit/fields", url.Values{
			"label": {label}, "kind": {kind},
			// Posted with every kind, as the page does: the box is hidden,
			// not absent. A text question must not be refused over it.
			"options": {"one\ntwo"},
		}, cookie)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("adding %q = %d:\n%s", label, w.Code, w.Body)
		}

		return strings.TrimPrefix(w.Header().Get("Location"), "/forms/supper-2026/edit/fields/")
	}

	if got := add("Your name", "text"); got != "text_1" {
		t.Errorf("the first text question is %q, want text_1", got)
	}
	if got := add("Your parish", "text"); got != "text_2" {
		t.Errorf("the second text question is %q, want text_2", got)
	}
	if got := add("Staying for lunch", "checkbox"); got != "checkbox_1" {
		t.Errorf("the first tick box is %q, want checkbox_1", got)
	}

	if w := a.post(t, "/forms/supper-2026/edit/fields/text_2/remove", nil, cookie); w.Code != http.StatusOK {
		t.Fatalf("removing text_2 = %d:\n%s", w.Code, w.Body)
	}

	// Not text_2 again: answers already given under that name would be read
	// back as answers to this question.
	if got := add("Your town", "text"); got != "text_3" {
		t.Errorf("the text question added after removing text_2 is %q, want text_3", got)
	}

	s, err := a.catalogue.Draft(t.Context(), mustSlug(t, "supper-2026"))
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}

	if fld, _ := s.Form.Field("text_1"); len(fld.Options) != 0 {
		t.Errorf("a text question kept the hidden choices box: %v", fld.Options)
	}
}

func TestAQuestionsKindCannotBeChangedAndItsPageShowsOnlyItsSettings(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	for _, f := range []url.Values{
		{"label": {"Your name"}, "kind": {"text"}},
		{"label": {"Which sitting"}, "kind": {"select"}, "options": {"early\nlate"}},
	} {
		if w := a.post(t, "/forms/supper-2026/edit/fields", f, cookie); w.Code != http.StatusSeeOther {
			t.Fatalf("adding %s = %d:\n%s", f.Get("label"), w.Code, w.Body)
		}
	}

	text := a.get(t, "/forms/supper-2026/edit/fields/text_1", cookie).Body.String()

	switch {
	case strings.Contains(text, `id="kind"`):
		t.Error("a question's page still offers its kind as a dropdown")
	case !strings.Contains(text, "A line of text"):
		t.Error("a question's page does not say what sort of answer it takes")
	case strings.Contains(text, "Its choices"):
		t.Error("a text question's page offers choices")
	case !strings.Contains(text, "How long the answer may be"):
		t.Error("a text question's page does not offer a length")
	}

	if sel := a.get(t, "/forms/supper-2026/edit/fields/select_1", cookie).Body.String(); !strings.Contains(sel, "Its choices") ||
		strings.Contains(sel, "How long the answer may be") {
		t.Error("a dropdown's page should offer its choices and not a length")
	}

	// A kind posted anyway -- an old tab, a hand-made request -- is ignored.
	if w := a.post(t, "/forms/supper-2026/edit/fields/text_1", url.Values{
		"label": {"Your name"}, "kind": {"date"},
	}, cookie); w.Code != http.StatusOK {
		t.Fatalf("saving = %d:\n%s", w.Code, w.Body)
	}

	s, err := a.catalogue.Draft(t.Context(), mustSlug(t, "supper-2026"))
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}

	if fld, _ := s.Form.Field("text_1"); fld.Kind != formbus.KindText {
		t.Errorf("the question became a %s", fld.Kind)
	}
}

// The add-a-question form: no name to invent, and the choices box marked so
// the stylesheet can hide it unless a kind with choices is picked.
func TestTheAddAQuestionFormAsksNoNameAndHidesChoicesByKind(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	page := a.get(t, "/forms/supper-2026/edit", cookie).Body.String()

	for _, want := range []string{
		`<option value="select" class="has-options">`,
		`<option value="text">`,
		`class="field uses-options"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not contain %s", want)
		}
	}

	for _, gone := range []string{`id="name"`, `id="item-id"`} {
		if strings.Contains(page, gone) {
			t.Errorf("the page still asks for a name: %s", gone)
		}
	}
}

func TestSellingSomethingIsBehindACheckboxAndGetsAGeneratedID(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	page := a.get(t, "/forms/supper-2026/edit", cookie).Body.String()

	if !strings.Contains(page, `<input type="checkbox" id="collects-money"`+"\n") ||
		!strings.Contains(page, `<div class="if-collects-money">`) {
		t.Errorf("a form that sells nothing does not offer the money checkbox unticked:\n%s", page)
	}

	if w := a.post(t, "/forms/supper-2026/edit/items", url.Values{
		"label": {"Adult"}, "price": {"15.00"},
	}, cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("adding something for sale = %d:\n%s", w.Code, w.Body)
	}

	s, err := a.catalogue.Draft(t.Context(), mustSlug(t, "supper-2026"))
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}

	if len(s.Form.Items) != 1 || s.Form.Items[0].ID != "item_1" {
		t.Errorf("items = %+v, want one called item_1", s.Form.Items)
	}

	// Once it sells something the box is ticked and fixed: turning selling
	// off is removing what is sold, and the page says so.
	page = a.get(t, "/forms/supper-2026/edit", cookie).Body.String()
	if !strings.Contains(page, "checked disabled") || !strings.Contains(page, "To stop, remove everything it sells") {
		t.Errorf("a form that sells does not show the box ticked and fixed:\n%s", page)
	}
}

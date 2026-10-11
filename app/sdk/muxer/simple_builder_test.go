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

func TestSellingSomethingIsBehindAButtonAndGetsAGeneratedID(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	page := a.get(t, "/forms/supper-2026/edit", cookie).Body.String()

	if !strings.Contains(page, `<details class="adder">`+"\n<summary>Sell something on this form</summary>") {
		t.Errorf("a form that sells nothing does not offer selling behind a closed button:\n%s", page)
	}

	// A refused add opens the panel again, so the reason is beside the form.
	if w := a.post(t, "/forms/supper-2026/edit/items", url.Values{
		"label": {"Adult"},
	}, cookie); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `<details class="adder" open>`+"\n<summary>Sell something") {
		t.Errorf("a priceless item = %d, want 400 with the panel open:\n%s", w.Code, w.Body)
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

	// Once it sells something the button says so, and the way to stop is on
	// the item's own page rather than a Remove on every row.
	page = a.get(t, "/forms/supper-2026/edit", cookie).Body.String()
	if !strings.Contains(page, "<summary>Add something for sale</summary>") || strings.Contains(page, "/remove") {
		t.Errorf("a form that sells does not offer adding another, or still has Remove on its list:\n%s", page)
	}

	if item := a.get(t, "/forms/supper-2026/edit/items/item_1", cookie).Body.String(); !strings.Contains(item, `action="/forms/supper-2026/edit/items/item_1/remove"`) {
		t.Errorf("the item's page does not offer removing it:\n%s", item)
	}
}

// The list of questions is the part of the page somebody comes back to, so it
// shows what they wrote and little else: no generated names, a word for the
// kind, conditions in the questions' own words, and no Remove beside every
// row. Adding is behind a button that is open only when there is nothing else
// to do or an add was just refused.
func TestTheListOfQuestionsShowsWordsRatherThanNames(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	if page := a.get(t, "/forms/supper-2026/edit", cookie).Body.String(); !strings.Contains(page, `<details class="adder" open>`+"\n<summary>Add a question</summary>") {
		t.Errorf("a form with no questions does not open the add panel:\n%s", page)
	}

	for _, f := range []url.Values{
		{"label": {"Will you stay for lunch?"}, "kind": {"radio"}, "options": {"yes | Yes, please\nno | No, thank you"}},
		{"label": {"How many?"}, "kind": {"number"}},
	} {
		if w := a.post(t, "/forms/supper-2026/edit/fields", f, cookie); w.Code != http.StatusSeeOther {
			t.Fatalf("adding %s = %d:\n%s", f.Get("label"), w.Code, w.Body)
		}
	}

	field := a.get(t, "/forms/supper-2026/edit/fields/number_1", cookie).Body.String()
	if !strings.Contains(field, `<option value="radio_1" >Will you stay for lunch?</option>`) {
		t.Errorf("the condition dropdown does not offer the earlier question by its wording:\n%s", field)
	}

	if w := a.post(t, "/forms/supper-2026/edit/fields/number_1", url.Values{
		"label": {"How many?"}, "show_if_field": {"radio_1"}, "show_if_is": {"yes"},
	}, cookie); w.Code != http.StatusOK {
		t.Fatalf("setting the condition = %d:\n%s", w.Code, w.Body)
	}

	page := a.get(t, "/forms/supper-2026/edit", cookie).Body.String()

	for _, want := range []string{
		`<td class="kind">Buttons</td>`,
		`<td class="kind">Number</td>`,
		`only when &#34;Will you stay for lunch?&#34; is &#34;Yes, please&#34;`,
		`<details class="adder">` + "\n<summary>Add a question</summary>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not contain %s", want)
		}
	}

	for _, gone := range []string{`<span class="ref">radio_1</span>`, "/fields/radio_1/remove"} {
		if strings.Contains(page, gone) {
			t.Errorf("the list still shows %s", gone)
		}
	}

	if w := a.post(t, "/forms/supper-2026/edit/fields", url.Values{"label": {"Oops"}}, cookie); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), `<details class="adder" open>`+"\n<summary>Add a question</summary>") {
		t.Errorf("an add with no kind = %d, want 400 with the panel open:\n%s", w.Code, w.Body)
	}

	// On a live form, removing the question another depends on is refused --
	// and the refusal is on the question's own page, where its button is.
	if w := a.post(t, "/forms/supper-2026/edit/state", url.Values{"state": {"live"}}, cookie); w.Code != http.StatusOK {
		t.Fatalf("publishing = %d:\n%s", w.Code, w.Body)
	}

	w := a.post(t, "/forms/supper-2026/edit/fields/radio_1/remove", nil, cookie)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "This question was not removed.") ||
		!strings.Contains(w.Body.String(), "Remove this question") {
		t.Errorf("removing a question another depends on = %d, want 409 on its own page:\n%s", w.Code, w.Body)
	}
}

package muxer_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
)

// Sections: a heading that groups the questions after it and can hide them
// all at once.

func TestASectionIsBuiltInTheBuilder(t *testing.T) {
	a := newAdmin(t, "")
	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "ordination", "Ordination")

	for _, f := range []url.Values{
		{"label": {"Plans"}, "kind": {"radio"}, "options": {"thinking | Thinking about it\ncoming | Coming"}},
		{"label": {"Travel"}, "kind": {"section"}, "options": {"one\ntwo"}},
		{"label": {"Arrival flight"}, "kind": {"text"}},
	} {
		if w := a.post(t, "/forms/ordination/edit/fields", f, cookie); w.Code != http.StatusSeeOther {
			t.Fatalf("adding %s = %d:\n%s", f.Get("label"), w.Code, w.Body)
		}
	}

	page := a.get(t, "/forms/ordination/edit/fields/section_1", cookie).Body.String()
	switch {
	case !strings.Contains(page, "The heading"):
		t.Error("a section's page does not ask for a heading")
	case strings.Contains(page, "It has to be answered"):
		t.Error("a section's page offers to make it required")
	case strings.Contains(page, "Faint text inside the box"):
		t.Error("a section's page offers a placeholder")
	case !strings.Contains(page, "every question under it"):
		t.Error("a section's page does not say its condition hides its questions")
	}

	if w := a.post(t, "/forms/ordination/edit/fields/section_1", url.Values{
		"label":         {"Travel"},
		"help":          {"Once you have your tickets."},
		"show_if_field": {"radio_1"},
		"show_if_is":    {"coming"},
	}, cookie); w.Code != http.StatusOK && w.Code != http.StatusSeeOther {
		t.Fatalf("saving the section = %d:\n%s", w.Code, w.Body)
	}

	// A question under it is offered only questions to depend on, and is
	// told it hides with its section.
	flight := a.get(t, "/forms/ordination/edit/fields/text_1", cookie).Body.String()
	if strings.Contains(flight, `<option value="section_1"`) {
		t.Error("a section heading is offered as something to depend on")
	}
	if !strings.Contains(flight, "hidden whenever its section is") {
		t.Error("a question in a section is not told it hides with it")
	}

	list := a.get(t, "/forms/ordination/edit", cookie).Body.String()
	if !strings.Contains(list, `class="section-row"`) || !strings.Contains(list, `class="in-section"`) {
		t.Errorf("the builder's list does not set questions under their section:\n%s", short(list))
	}

	s, err := a.catalogue.Draft(t.Context(), mustSlug(t, "ordination"))
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}

	sec, _ := s.Form.Field("section_1")
	if sec.Kind != formbus.KindSection || sec.Help != "Once you have your tickets." ||
		sec.ShowIf == nil || sec.ShowIf.Field != "radio_1" || len(sec.Options) != 0 {
		t.Errorf("the stored section is %+v", sec)
	}
	if err := s.Form.Check(); err != nil {
		t.Errorf("the form does not pass Check: %v", err)
	}
}

// sectioned is the retreat form with its plans question deciding a Travel
// section that holds a required question.
func sectioned(rh retreatHarness) {
	rh.form.Fields = append(rh.form.Fields,
		formbus.Field{Name: "section_1", Label: "Travel", Kind: formbus.KindSection, Help: "Once you have tickets.",
			ShowIf: &formbus.Condition{Field: "plans", Is: []string{"booked"}}},
		formbus.Field{Name: "flight", Label: "Arrival flight", Kind: formbus.KindText, Required: true, MaxLen: 50},
	)
	rh.form.Stamp()
}

func TestASectionHidesItsQuestionsOnThePublicForm(t *testing.T) {
	rh := retreatSurface(t)
	sectioned(rh)

	if err := rh.form.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	page := getPage(t, rh.h, retreatPath).Body.String()
	for _, want := range []string{
		`<fieldset class="section" data-section="section_1" data-show-if-field="plans" data-show-if-values="booked">`,
		`<legend>Travel</legend>`,
		`Once you have tickets.`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the form does not contain %q:\n%s", want, short(page))
		}
	}

	// Inside the fieldset: the question renders between the legend and the
	// fieldset's end.
	if i, j, k := strings.Index(page, "<legend>Travel"), strings.Index(page, `name="flight"`), strings.Index(page, "</fieldset>"); i >= j || j >= k {
		t.Error("the section's question is not inside its fieldset")
	}

	// Thinking about it: the required question in the hidden section does not
	// block the submission.
	w := postTo(t, rh.h, retreatPath, url.Values{
		embedapp.GrantField: {grantIn(t, page)},
		"name":              {"Fr. Hector"},
		"email":             {"hector@example.org"},
		"plans":             {"considering"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("a submission with the section hidden = %d:\n%s", w.Code, short(w.Body.String()))
	}

	// Booked: now it is asked.
	w = postTo(t, rh.h, retreatPath, url.Values{
		embedapp.GrantField: {grantIn(t, getPage(t, rh.h, retreatPath).Body.String())},
		"name":              {"Fr. José"},
		"email":             {"jose@example.org"},
		"plans":             {"booked"},
	})
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "Arrival flight") {
		t.Fatalf("a submission with the section shown and its question blank = %d:\n%s", w.Code, short(w.Body.String()))
	}
}

func TestAHeadingIsNotAColumn(t *testing.T) {
	rh := retreatSurface(t)
	sectioned(rh)

	grant := grantIn(t, getPage(t, rh.h, retreatPath).Body.String())
	if w := postTo(t, rh.h, retreatPath, url.Values{
		embedapp.GrantField: {grant},
		"name":              {"Fr. Hector"},
		"email":             {"hector@example.org"},
		"plans":             {"booked"},
		"flight":            {"UA 1234"},
	}); w.Code != http.StatusOK {
		t.Fatalf("POST = %d:\n%s", w.Code, short(w.Body.String()))
	}

	o, cookie := feedOffice(t, rh, accessbus.RoleAdmin)
	key := feedKeyPattern.FindString(o.postValues(t, "/forms/retreat/feed", cookie, url.Values{}).Body.String())

	var feed feedReply
	if err := json.Unmarshal(pullFeed(t, o.admin, "retreat", key).Body.Bytes(), &feed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	for _, f := range feed.Fields {
		if f.Name == "section_1" {
			t.Error("the feed has a column for a section heading")
		}
	}
	if len(feed.Fields) != 4 || feed.Responses[0].Answers["flight"] != "UA 1234" {
		t.Errorf("feed = %+v", feed)
	}

	if csv := o.read(t, "/forms/retreat/submissions.csv", cookie).Body.String(); strings.Contains(strings.SplitN(csv, "\n", 2)[0], "Travel") {
		t.Errorf("the CSV has a column for a section heading: %s", strings.SplitN(csv, "\n", 2)[0])
	}
}

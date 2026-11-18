package muxer_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
)

// The carpool element: built in the builder, drawn on the public form,
// answered and changed. Placing riders in cars is not here yet.

// carpooled adds a mobile number and a carpool to the retreat form, after its
// name question.
func carpooled(t *testing.T, rh retreatHarness) {
	t.Helper()

	rh.form.Fields = append(rh.form.Fields,
		formbus.Field{Name: "mobile", Label: "Mobile", Kind: formbus.KindTel},
		formbus.Field{Name: "ride", Label: "Getting there", Kind: formbus.KindCarpool, Required: true,
			Phone: "mobile", Who: "name", SeatsLabel: "Seats for others?",
			Options: []formbus.Option{
				{Value: formbus.CarpoolHave, Label: "Driving myself, alone"},
				{Value: formbus.CarpoolDrive, Label: "I can drive others"},
				{Value: formbus.CarpoolNeed, Label: "Please find me a driver"},
			}},
	)
	rh.form.Stamp()

	if err := rh.form.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func TestACarpoolIsDrawnOnThePublicForm(t *testing.T) {
	rh := retreatSurface(t)
	carpooled(t, rh)

	page := getPage(t, rh.h, retreatPath).Body.String()

	for _, want := range []string{
		`<input type="radio" name="ride" value="drive">`,
		`Please find me a driver`,
		`<div class="seats" data-show-if-field="ride" data-show-if-values="drive">`,
		`<label for="f-ride.seats">Seats for others?</label>`,
		`name="ride.seats"`,
		`max="7"`,
		`Reply STOP to stop.`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the form does not contain %q:\n%s", want, short(page))
		}
	}
}

func TestADriverAnswersAndIsAskedForANumber(t *testing.T) {
	rh := retreatSurface(t)
	carpooled(t, rh)

	// No number: refused, beside the number question.
	w := postTo(t, rh.h, retreatPath, url.Values{
		embedapp.GrantField: {grantIn(t, getPage(t, rh.h, retreatPath).Body.String())},
		"name":              {"Damien"},
		"email":             {"damien@example.org"},
		"plans":             {"booked"},
		"ride":              {"drive"},
		"ride.seats":        {"3"},
	})
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `data-problem-for="mobile"`) {
		t.Fatalf("a driver with no number = %d:\n%s", w.Code, short(w.Body.String()))
	}

	// The seats come back in their box on the refused page.
	if !strings.Contains(w.Body.String(), `name="ride.seats" value="3"`) {
		t.Errorf("the seats were not put back:\n%s", short(w.Body.String()))
	}

	w = postTo(t, rh.h, retreatPath, url.Values{
		embedapp.GrantField: {grantIn(t, getPage(t, rh.h, retreatPath).Body.String())},
		"name":              {"Damien"},
		"email":             {"damien@example.org"},
		"mobile":            {"512 555 0123"},
		"plans":             {"booked"},
		"ride":              {"drive"},
		"ride.seats":        {"3"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("a driver with a number = %d:\n%s", w.Code, short(w.Body.String()))
	}

	got, _ := rh.only(t).Answers.Field("ride")
	if strings.Join(got.Values, ",") != "drive,3" {
		t.Errorf("stored = %q, want [drive 3]", got.Values)
	}

	// Changing the answers: the seats are back in their box.
	page := getPage(t, rh.h, changeLinkIn(t, w.Body.String())).Body.String()
	if !strings.Contains(page, `name="ride.seats" value="3"`) || !strings.Contains(page, `value="drive" checked`) {
		t.Errorf("the change page does not show the driver's answer:\n%s", short(page))
	}
}

func TestACarpoolIsAddedInTheBuilder(t *testing.T) {
	a := newAdmin(t, "")
	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "youth-retreat", "Youth retreat")

	for _, f := range []url.Values{
		{"label": {"Your name"}, "kind": {"text"}},
		{"label": {"Mobile"}, "kind": {"tel"}},
		{"label": {"Getting there"}, "kind": {"carpool"}},
	} {
		if w := a.post(t, "/forms/youth-retreat/edit/fields", f, cookie); w.Code != http.StatusSeeOther {
			t.Fatalf("adding %s = %d:\n%s", f.Get("label"), w.Code, w.Body)
		}
	}

	page := a.get(t, "/forms/youth-retreat/edit/fields/carpool_1", cookie).Body.String()
	for _, want := range []string{
		`name="carpool_drive" value="I can drive and take others"`,
		`<option value="tel_1" selected>Mobile</option>`,
		`<option value="text_1" selected>Your name</option>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the carpool's page does not contain %q:\n%s", want, short(page))
		}
	}

	if strings.Contains(page, `id="options"`) {
		t.Error("the carpool's page offers the general list of choices")
	}

	if w := a.post(t, "/forms/youth-retreat/edit/fields/carpool_1", url.Values{
		"label":         {"Getting there"},
		"carpool_have":  {"Getting there myself"},
		"carpool_drive": {"I can drive"},
		"carpool_need":  {"Find me a ride"},
		"seats_label":   {"Seats for others?"},
		"max":           {"4"},
		"phone":         {"tel_1"},
		"who":           {"text_1"},
	}, cookie); w.Code != http.StatusOK {
		t.Fatalf("saving the carpool = %d:\n%s", w.Code, short(w.Body.String()))
	}

	s, err := a.catalogue.Draft(t.Context(), mustSlug(t, "youth-retreat"))
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}

	fld, _ := s.Form.Field("carpool_1")
	if fld.Phone != "tel_1" || fld.Who != "text_1" || fld.MaxSeats() != 4 || fld.Options[2].Label != "Find me a ride" {
		t.Errorf("the stored carpool is %+v", fld)
	}

	if err := s.Form.Check(); err != nil {
		t.Errorf("the form does not pass Check: %v", err)
	}
}

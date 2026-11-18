package muxer_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
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

// recordedTexts is the harness's Twilio: it keeps what would have been sent.
type recordedTexts struct {
	mu   sync.Mutex
	sent []string // "to: body"
}

func (r *recordedTexts) Send(_ context.Context, to, body string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.sent = append(r.sent, to+": "+body)

	return "SM1", nil
}

func (r *recordedTexts) to(number string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []string

	for _, m := range r.sent {
		if strings.HasPrefix(m, number+": ") {
			out = append(out, m)
		}
	}

	return out
}

// carpoolForm makes a live form with a carpool through the catalogue.
func carpoolForm(t *testing.T, a harness) formbus.Form {
	t.Helper()

	f := formbus.Form{
		ID: mustSlug(t, "youth-retreat"), Title: "Youth retreat", Currency: "usd",
		Fields: []formbus.Field{
			{Name: "name", Label: "Name", Kind: formbus.KindText, Required: true},
			{Name: "mobile", Label: "Mobile", Kind: formbus.KindTel},
			{Name: "ride", Label: "Getting there", Kind: formbus.KindCarpool, Required: true, Phone: "mobile", Who: "name",
				Options: []formbus.Option{
					{Value: formbus.CarpoolHave, Label: "Own ride"},
					{Value: formbus.CarpoolDrive, Label: "Driving"},
					{Value: formbus.CarpoolNeed, Label: "Need a ride"},
				}},
		},
	}

	if _, err := a.catalogue.Create(t.Context(), time.Now(), types.ID{}, f); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := a.catalogue.Save(t.Context(), time.Now(), types.ID{}, f, true); err != nil {
		t.Fatalf("Save: %v", err)
	}

	live, err := a.catalogue.ByID(f.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	return live
}

// rideSignUp submits one person to the carpool form, the way the embed surface
// would have.
func rideSignUp(t *testing.T, a harness, f formbus.Form, name, number, choice, seats string) submissionbus.Submission {
	t.Helper()

	in := formbus.Values{"name": {name}, "mobile": {number}, "ride": {choice}}
	if seats != "" {
		in["ride.seats"] = []string{seats}
	}

	answers, err := f.Validate(time.Now(), in)
	if err != nil {
		t.Fatalf("Validate %s: %v", name, err)
	}

	g := formbus.Grant{Form: f.ID, Version: f.Version, Nonce: "nonce-" + name}

	// A second apart, in the order called: who signed up first decides
	// whose car fills first, and a millisecond clock cannot tell two calls
	// in a row apart.
	signUps++

	sub, err := a.orders.Accept(t.Context(), time.Now().Add(time.Duration(signUps)*time.Second), g, submissionbus.New{Answers: answers})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	return sub
}

// signUps spaces rideSignUp's submissions apart.
var signUps int

func TestTheCarpoolPageShowsTheCarsAndAnAdminMovesARider(t *testing.T) {
	a := newAdmin(t, "")
	f := carpoolForm(t, a)

	rideSignUp(t, a, f, "Damien", "5125550101", "drive", "1")
	maria := rideSignUp(t, a, f, "Maria", "5125550102", "drive", "2")
	ana := rideSignUp(t, a, f, "Ana", "5125550103", "need", "")

	if err := a.carpool.Arrange(t.Context(), f); err != nil {
		t.Fatalf("Arrange: %v", err)
	}

	if got := a.texts.to("+15125550101"); len(got) != 1 || !strings.Contains(got[0], "Ana, (512) 555-0103, is riding with you") {
		t.Errorf("Damien was texted %q", got)
	}

	reader, readerCookie := func() (userbus.User, string) {
		u, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{Email: mustEmail(t, "reader@schoenstatt.test"), Name: "Reader"})
		if err != nil {
			t.Fatal(err)
		}

		if _, err := a.access.Grant(t.Context(), time.Now(), types.ID{}, u.ID, f.ID, accessbus.RoleResults); err != nil {
			t.Fatal(err)
		}

		return u, signedIn(t, a, u)
	}()
	_ = reader

	page := a.get(t, "/forms/youth-retreat/carpool", readerCookie)
	if page.Code != http.StatusOK {
		t.Fatalf("GET carpool as a reader = %d:\n%s", page.Code, short(page.Body.String()))
	}

	for _, want := range []string{"Damien", "Ana", "(512) 555-0103", "1 of 1 seat taken"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("the page does not show %q:\n%s", want, short(page.Body.String()))
		}
	}

	if strings.Contains(page.Body.String(), "/carpool/move") {
		t.Error("somebody who only reads results is offered a move")
	}

	move := url.Values{"rider": {ana.ID.String()}, "driver": {maria.ID.String()}}

	if w := a.post(t, "/forms/youth-retreat/carpool/move", move, readerCookie); w.Code == http.StatusOK {
		t.Error("somebody who only reads results moved a rider")
	}

	boss := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	if w := a.post(t, "/forms/youth-retreat/carpool/move", move, boss); w.Code != http.StatusOK {
		t.Fatalf("an admin moving a rider = %d:\n%s", w.Code, short(w.Body.String()))
	}

	if got := a.texts.to("+15125550102"); len(got) != 1 || !strings.Contains(got[0], "Ana") {
		t.Errorf("Maria was texted %q", got)
	}

	if csv := a.get(t, "/forms/youth-retreat/carpool.csv", readerCookie).Body.String(); !strings.Contains(csv, "Maria,(512) 555-0102,2,Ana") {
		t.Errorf("the CSV does not put Ana in Maria's car:\n%s", csv)
	}

	// The results page links to it.
	if !strings.Contains(a.get(t, "/forms/youth-retreat/submissions", readerCookie).Body.String(), "/forms/youth-retreat/carpool") {
		t.Error("the results page does not link to the carpool")
	}
}

// An organiser who would be texted about the carpool but has given no number
// is told so on its page, and pointed at the only place they can give one.
func TestTheCarpoolPageTellsAnOrganiserWithNoMobileHowToBeTexted(t *testing.T) {
	a := newAdmin(t, "")
	f := carpoolForm(t, a)

	const prompt = "there is no mobile number on your account"

	boss := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	if body := a.get(t, "/forms/youth-retreat/carpool", boss).Body.String(); !strings.Contains(body, prompt) {
		t.Errorf("an organiser with no mobile is not told:\n%s", short(body))
	}

	if w := a.post(t, "/account/mobile", url.Values{"mobile": {"5125550100"}}, boss); w.Code != http.StatusOK {
		t.Fatalf("saving a number = %d", w.Code)
	}

	if body := a.get(t, "/forms/youth-retreat/carpool", boss).Body.String(); strings.Contains(body, prompt) {
		t.Error("an organiser who has given a number is still told they have none")
	}

	// Somebody who only reads results is never texted, so is not told how.
	u, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{Email: mustEmail(t, "reader@schoenstatt.test"), Name: "Reader"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := a.access.Grant(t.Context(), time.Now(), types.ID{}, u.ID, f.ID, accessbus.RoleResults); err != nil {
		t.Fatal(err)
	}

	if body := a.get(t, "/forms/youth-retreat/carpool", signedIn(t, a, u)).Body.String(); strings.Contains(body, prompt) {
		t.Error("a reader, who is never texted, is told to give a number")
	}
}

func TestAFormWithNoCarpoolHasNoCarpoolPage(t *testing.T) {
	a := newAdmin(t, "")
	boss := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	if w := a.get(t, "/forms/"+theForm+"/carpool", boss); w.Code != http.StatusNotFound {
		t.Errorf("GET carpool on a form without one = %d, want 404", w.Code)
	}
}

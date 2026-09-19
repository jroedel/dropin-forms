package muxer_test

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// Authoring a form in a browser, through the mounted admin surface.
//
// Two gates rather than one, which is what most of this file is about: editing
// a form is admin on that form, and making one is admin over the service,
// because a form that does not exist yet has no grant anybody could hold.

const buildPage = "/build"

// siteBoss holds the site-wide grant, which is the only account that can make
// a form.
func siteBoss(t *testing.T, a harness, address string) userbus.User {
	t.Helper()

	u, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, address),
		Name:  "The Administrator",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The zero slug is the site-wide grant. accessbus.Grant.SiteWide reports
	// on exactly this.
	if _, err := a.access.Grant(t.Context(), time.Now(), types.ID{}, u.ID, types.Slug{}, accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	return u
}

// made creates a form through the builder's own route, which is also the
// assertion that the route works.
func made(t *testing.T, a harness, cookie, slug, title string) {
	t.Helper()

	w := a.post(t, "/build/new", url.Values{"slug": {slug}, "title": {title}}, cookie)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /build/new = %d, want 303:\n%s", w.Code, w.Body)
	}

	if to := w.Header().Get("Location"); to != "/forms/"+slug+"/edit" {
		t.Fatalf("created a form and went to %q", to)
	}
}

// The gate on making one. A form-specific admin is not a site administrator,
// and the page that makes forms is the site's.
func TestOnlyASiteAdministratorReachesTheBuilderIndex(t *testing.T) {
	a := newAdmin(t, "")

	onOneForm := formAdmin(t, a, "boss@schoenstatt.test")
	whole := siteBoss(t, a, "site@schoenstatt.test")

	if w := a.get(t, buildPage, sessionFor(t, a, onOneForm)); w.Code != http.StatusForbidden {
		t.Errorf("an admin on one form GET %s = %d, want 403:\n%s", buildPage, w.Code, w.Body)
	}

	if w := a.get(t, buildPage, sessionFor(t, a, whole)); w.Code != http.StatusOK {
		t.Errorf("a site administrator GET %s = %d, want 200:\n%s", buildPage, w.Code, w.Body)
	}

	// Signed out it is the sign-in page rather than a refusal, because Require
	// comes before the role gate and the two refusals are different answers.
	w := a.get(t, buildPage, "")
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/signin") {
		t.Errorf("signed out GET %s = %d to %q, want a redirect to /signin", buildPage, w.Code, w.Header().Get("Location"))
	}
}

// The gate on editing one, which is the per-form role and not the site-wide
// one: whoever counts the lunches must not be able to change what a ticket
// costs.
func TestOnlyAnAdminOnTheFormReachesItsBuilder(t *testing.T) {
	a := newAdmin(t, "")

	whole := siteBoss(t, a, "site@schoenstatt.test")
	cookie := sessionFor(t, a, whole)

	made(t, a, cookie, "supper-2026", "Parish supper")

	const edit = "/forms/supper-2026/edit"

	counter := reader(t, a, "counter@schoenstatt.test")

	if w := a.get(t, edit, sessionFor(t, a, counter)); w.Code != http.StatusForbidden {
		t.Errorf("a results holder GET %s = %d, want 403:\n%s", edit, w.Code, w.Body)
	}

	if w := a.get(t, edit, cookie); w.Code != http.StatusOK {
		t.Errorf("the site administrator GET %s = %d, want 200:\n%s", edit, w.Code, w.Body)
	}
}

// The form that ships in this release is not editable here, and says so rather
// than answering 404 -- the reader holds admin on it and can see it listed.
func TestAReleaseFormIsNotEditedInTheBrowser(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	w := a.get(t, "/forms/"+theForm+"/edit", cookie)
	if w.Code != http.StatusConflict {
		t.Fatalf("GET the release form's builder = %d, want 409:\n%s", w.Code, w.Body)
	}

	if !strings.Contains(w.Body.String(), "part of this release") {
		t.Errorf("the page does not say why it cannot be edited:\n%s", w.Body)
	}
}

// The whole round trip: make a form, give it a question, publish it, and find
// it in the catalogue the embed surface reads.
func TestAFormBuiltInTheBrowserIsServedOnceItIsPublished(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	slug := mustSlug(t, "supper-2026")

	// A draft is not in the catalogue, which is what makes it safe for it to
	// be unfinished.
	if _, err := a.catalogue.ByID(slug); err == nil {
		t.Fatal("a draft is being served")
	}

	// Publishing it with no fields is refused, with the reasons on the page.
	w := a.post(t, "/forms/supper-2026/edit/state", url.Values{"state": {"live"}}, cookie)
	if w.Code != http.StatusConflict {
		t.Fatalf("publishing an empty form = %d, want 409:\n%s", w.Code, w.Body)
	}

	if !strings.Contains(w.Body.String(), "no fields") {
		t.Errorf("the refusal does not say what is missing:\n%s", w.Body)
	}

	// Give it a question.
	w = a.post(t, "/forms/supper-2026/edit/fields", url.Values{
		"name":     {"who"},
		"label":    {"Your name"},
		"kind":     {"text"},
		"required": {"yes"},
	}, cookie)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("adding a field = %d, want 303:\n%s", w.Code, w.Body)
	}

	// And now it publishes.
	w = a.post(t, "/forms/supper-2026/edit/state", url.Values{"state": {"live"}}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("publishing = %d, want 200:\n%s", w.Code, w.Body)
	}

	f, err := a.catalogue.ByID(slug)
	if err != nil {
		t.Fatalf("the published form is not in the catalogue: %v", err)
	}

	if len(f.Fields) != 1 || f.Fields[0].Name != "who" {
		t.Errorf("served %+v, want the one field", f.Fields)
	}

	// The paste snippet, which is the whole output of this product and is what
	// somebody came to the page for.
	if !strings.Contains(w.Body.String(), "data-dropin-form=&#34;supper-2026&#34;") {
		t.Errorf("the page does not show the markup to paste:\n%s", w.Body)
	}
}

// A dropdown with no options is not a form anybody can fill in, so adding one
// to a live form is refused rather than saved -- and the form keeps serving
// what it was serving.
func TestAChangeThatWouldBreakALiveFormIsRefused(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	if w := a.post(t, "/forms/supper-2026/edit/fields", url.Values{
		"name": {"who"}, "label": {"Your name"}, "kind": {"text"},
	}, cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("adding the first field = %d:\n%s", w.Code, w.Body)
	}

	if w := a.post(t, "/forms/supper-2026/edit/state", url.Values{"state": {"live"}}, cookie); w.Code != http.StatusOK {
		t.Fatalf("publishing = %d:\n%s", w.Code, w.Body)
	}

	before, err := a.catalogue.ByID(mustSlug(t, "supper-2026"))
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	w := a.post(t, "/forms/supper-2026/edit/fields", url.Values{
		"name": {"sitting"}, "label": {"Which sitting"}, "kind": {"select"},
	}, cookie)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("adding an option-less dropdown to a live form = %d, want 400:\n%s", w.Code, w.Body)
	}

	after, err := a.catalogue.ByID(mustSlug(t, "supper-2026"))
	if err != nil {
		t.Fatalf("ByID after the refusal: %v", err)
	}

	if after.Version != before.Version {
		t.Errorf("the refused change altered the form being served")
	}

	// With its options, the same addition goes through -- which is why the add
	// form asks for them in the same request.
	w = a.post(t, "/forms/supper-2026/edit/fields", url.Values{
		"name": {"sitting"}, "label": {"Which sitting"}, "kind": {"select"},
		"options": {"early | Six o'clock\nlate | Eight o'clock"},
	}, cookie)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("adding a dropdown with options = %d, want 303:\n%s", w.Code, w.Body)
	}

	served, err := a.catalogue.ByID(mustSlug(t, "supper-2026"))
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	sitting, ok := served.Field("sitting")
	if !ok {
		t.Fatal("the dropdown is not on the served form")
	}

	if len(sitting.Options) != 2 || sitting.Options[0].Value != "early" || sitting.Options[0].Label != "Six o'clock" {
		t.Errorf("options = %+v, want the two that were typed", sitting.Options)
	}
}

// Taking a form down is never refused, because a form that should not be on
// the web is something somebody needs to be able to do at once.
func TestTakingAFormDownIsNeverRefused(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	if w := a.post(t, "/forms/supper-2026/edit/fields", url.Values{
		"name": {"who"}, "label": {"Your name"}, "kind": {"text"},
	}, cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("adding a field = %d:\n%s", w.Code, w.Body)
	}

	if w := a.post(t, "/forms/supper-2026/edit/state", url.Values{"state": {"live"}}, cookie); w.Code != http.StatusOK {
		t.Fatalf("publishing = %d:\n%s", w.Code, w.Body)
	}

	if w := a.post(t, "/forms/supper-2026/edit/state", url.Values{"state": {"draft"}}, cookie); w.Code != http.StatusOK {
		t.Fatalf("taking it down = %d, want 200:\n%s", w.Code, w.Body)
	}

	if _, err := a.catalogue.ByID(mustSlug(t, "supper-2026")); err == nil {
		t.Error("a form that was taken down is still being served")
	}
}

// And once it has been live it can only be taken down, never deleted: what was
// submitted can only be read through the definition that names its columns.
func TestAFormThatHasBeenLiveCannotBeDeleted(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "gone-2026", "Something abandoned")

	// Never published, so it goes.
	w := a.post(t, "/forms/gone-2026/edit/delete", url.Values{}, cookie)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/build" {
		t.Fatalf("deleting a form that was never live = %d to %q, want 303 to /build:\n%s",
			w.Code, w.Header().Get("Location"), w.Body)
	}

	made(t, a, cookie, "supper-2026", "Parish supper")

	if w := a.post(t, "/forms/supper-2026/edit/fields", url.Values{
		"name": {"who"}, "label": {"Your name"}, "kind": {"text"},
	}, cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("adding a field = %d:\n%s", w.Code, w.Body)
	}

	if w := a.post(t, "/forms/supper-2026/edit/state", url.Values{"state": {"live"}}, cookie); w.Code != http.StatusOK {
		t.Fatalf("publishing = %d:\n%s", w.Code, w.Body)
	}
	if w := a.post(t, "/forms/supper-2026/edit/state", url.Values{"state": {"draft"}}, cookie); w.Code != http.StatusOK {
		t.Fatalf("taking it down = %d:\n%s", w.Code, w.Body)
	}

	w = a.post(t, "/forms/supper-2026/edit/delete", url.Values{}, cookie)
	if w.Code != http.StatusConflict {
		t.Fatalf("deleting a form that has been live = %d, want 409:\n%s", w.Code, w.Body)
	}

	if _, err := a.catalogue.Draft(t.Context(), mustSlug(t, "supper-2026")); err != nil {
		t.Errorf("the refused delete removed it anyway: %v", err)
	}
}

// A field's order is part of what a submission means, and a condition may only
// name a field above the one it governs -- so moving one is a real edit that
// goes through Check like any other.
func TestMovingAFieldAboveWhatItDependsOnIsRefused(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	for _, f := range []url.Values{
		{"name": {"sitting"}, "label": {"Which sitting"}, "kind": {"select"}, "options": {"early\nlate"}},
		{"name": {"why"}, "label": {"Why so early"}, "kind": {"text"}},
	} {
		if w := a.post(t, "/forms/supper-2026/edit/fields", f, cookie); w.Code != http.StatusSeeOther {
			t.Fatalf("adding %s = %d:\n%s", f.Get("name"), w.Code, w.Body)
		}
	}

	// The second field is shown only when the first says "early".
	w := a.post(t, "/forms/supper-2026/edit/fields/why", url.Values{
		"label": {"Why so early"}, "kind": {"text"},
		"show_if_field": {"sitting"}, "show_if_is": {"early"},
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("setting the condition = %d, want 200:\n%s", w.Code, w.Body)
	}

	// Published, because the refusal being tested is the one a live form gets.
	// On a draft the same move is allowed and the problem appears in the
	// to-do list instead -- that is the draft bargain, and it is what lets a
	// form be rearranged into a valid order rather than out of one.
	if w := a.post(t, "/forms/supper-2026/edit/state", url.Values{"state": {"live"}}, cookie); w.Code != http.StatusOK {
		t.Fatalf("publishing = %d:\n%s", w.Code, w.Body)
	}

	// Now move it above the field it depends on.
	w = a.post(t, "/forms/supper-2026/edit/fields/why/move", url.Values{"to": {"up"}}, cookie)
	if w.Code != http.StatusConflict {
		t.Fatalf("moving a field above its condition = %d, want 409:\n%s", w.Code, w.Body)
	}

	if !strings.Contains(w.Body.String(), "comes after it") {
		t.Errorf("the refusal does not explain the order:\n%s", w.Body)
	}

	f, err := a.catalogue.ByID(mustSlug(t, "supper-2026"))
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if f.Fields[0].Name != "sitting" {
		t.Errorf("the refused move happened anyway: %v", f.Fields)
	}
}

// The same move on a draft is allowed, and the problem it creates is reported
// as something left to do rather than as a refusal. Asserted beside the test
// above because the pair is the draft bargain, and either one alone reads as
// the other being a bug.
func TestTheSameMoveOnADraftIsAllowedAndListedAsAProblem(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	for _, f := range []url.Values{
		{"name": {"sitting"}, "label": {"Which sitting"}, "kind": {"select"}, "options": {"early\nlate"}},
		{"name": {"why"}, "label": {"Why so early"}, "kind": {"text"}},
	} {
		if w := a.post(t, "/forms/supper-2026/edit/fields", f, cookie); w.Code != http.StatusSeeOther {
			t.Fatalf("adding %s = %d:\n%s", f.Get("name"), w.Code, w.Body)
		}
	}

	if w := a.post(t, "/forms/supper-2026/edit/fields/why", url.Values{
		"label": {"Why so early"}, "kind": {"text"},
		"show_if_field": {"sitting"}, "show_if_is": {"early"},
	}, cookie); w.Code != http.StatusOK {
		t.Fatalf("setting the condition = %d:\n%s", w.Code, w.Body)
	}

	w := a.post(t, "/forms/supper-2026/edit/fields/why/move", url.Values{"to": {"up"}}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("moving a field on a draft = %d, want 200:\n%s", w.Code, w.Body)
	}

	if !strings.Contains(w.Body.String(), "comes after it") {
		t.Errorf("the page does not list the problem the move created:\n%s", w.Body)
	}

	s, err := a.catalogue.Draft(t.Context(), mustSlug(t, "supper-2026"))
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}

	if s.Form.Fields[0].Name != "why" {
		t.Errorf("the move did not happen: %v", s.Form.Fields)
	}

	// And it cannot be published while it is in that state, which is what
	// makes allowing the move safe.
	if w := a.post(t, "/forms/supper-2026/edit/state", url.Values{"state": {"live"}}, cookie); w.Code != http.StatusConflict {
		t.Errorf("publishing a form with a backwards condition = %d, want 409:\n%s", w.Code, w.Body)
	}
}

// A price lives in the definition and nowhere else, and changing it is an edit
// to the form. This is the route that does it.
func TestSomethingForSaleIsPricedInTheBuilder(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	w := a.post(t, "/forms/supper-2026/edit/items", url.Values{
		"id": {"adult"}, "label": {"Adult"}, "price": {"15.00"},
	}, cookie)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("adding something for sale = %d, want 303:\n%s", w.Code, w.Body)
	}

	w = a.post(t, "/forms/supper-2026/edit/items/adult", url.Values{
		"label": {"Adult"}, "price": {"18.00"}, "max": {"8"},
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("changing the price = %d, want 200:\n%s", w.Code, w.Body)
	}

	s, err := a.catalogue.Draft(t.Context(), mustSlug(t, "supper-2026"))
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}

	it, ok := s.Form.Item("adult")
	if !ok {
		t.Fatal("the item is not on the form")
	}

	if it.Price != types.Money(1800) || it.Max != 8 {
		t.Errorf("item = %+v, want 1800 and a cap of 8", it)
	}

	// A price that is not a price is refused with a sentence, not stored as
	// something else.
	w = a.post(t, "/forms/supper-2026/edit/items/adult", url.Values{
		"label": {"Adult"}, "price": {"eighteen dollars"},
	}, cookie)
	if w.Code != http.StatusBadRequest {
		t.Errorf("a price that is not a number = %d, want 400:\n%s", w.Code, w.Body)
	}
}

// The settings page, and the one thing on it worth asserting in a test rather
// than reading: a date chosen in a picker becomes an instant in the zone this
// service runs in, not one in UTC.
func TestAClosingDateIsReadInTheServersOwnZone(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	w := a.post(t, "/forms/supper-2026/edit/settings", url.Values{
		"title":     {"Parish supper"},
		"closes_at": {"2026-10-17T23:59"},
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("saving the settings = %d, want 200:\n%s", w.Code, w.Body)
	}

	s, err := a.catalogue.Draft(t.Context(), mustSlug(t, "supper-2026"))
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}

	want := time.Date(2026, 10, 17, 23, 59, 0, 0, time.Local)

	if !s.Form.ClosesAt.Equal(want) {
		t.Errorf("closes at %v, want %v -- the wall time somebody picked, in this service's own zone",
			s.Form.ClosesAt, want)
	}
}

// Two forms cannot share a name, because the name is in the address somebody
// has already pasted onto their website.
func TestTwoFormsCannotShareAName(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	w := a.post(t, "/build/new", url.Values{"slug": {"supper-2026"}, "title": {"Another one"}}, cookie)
	if w.Code != http.StatusConflict {
		t.Errorf("a second form with the same name = %d, want 409:\n%s", w.Code, w.Body)
	}

	// And not the name of a form in the release either.
	w = a.post(t, "/build/new", url.Values{"slug": {theForm}, "title": {"Mine"}}, cookie)
	if w.Code != http.StatusConflict {
		t.Errorf("claiming a release form's name = %d, want 409:\n%s", w.Code, w.Body)
	}
}

// The catalogue is what every reader uses, so the release's forms are still
// there beside anything built. Asserted because the composite is new and the
// failure -- the feast form quietly disappearing on the day of the feast -- is
// the worst one available.
func TestTheReleaseFormIsStillServedBesideABuiltOne(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	if _, err := a.catalogue.ByID(mustSlug(t, theForm)); err != nil {
		t.Errorf("the release's form is no longer in the catalogue: %v", err)
	}

	var names []string
	for _, f := range a.catalogue.All() {
		names = append(names, f.ID.String())
	}

	if !slices.Contains(names, theForm) {
		t.Errorf("All = %v, want it to include %s", names, theForm)
	}
}

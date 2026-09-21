package muxer_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// siteRoleOf reports what an account holds site-wide, or "" for nothing.
func siteRoleOf(t *testing.T, a harness, u userbus.User) accessbus.Role {
	t.Helper()

	grants, err := a.access.ForUser(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}

	for _, g := range grants {
		if g.SiteWide() {
			return g.Role
		}
	}

	return ""
}

// roleOnForm reports what an account holds on one named form, or "" for
// nothing. Deliberately ignores the site-wide row: the question here is what
// the grant made at the moment of creation actually says.
func roleOnForm(t *testing.T, a harness, u userbus.User, slug string) accessbus.Role {
	t.Helper()

	grants, err := a.access.ForUser(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}

	for _, g := range grants {
		if g.Form == mustSlug(t, slug) {
			return g.Role
		}
	}

	return ""
}

// The grant a volunteer is given has to survive being written down. This is
// the end-to-end version of the store's own round-trip test, and it is here
// because the failure it guards against was invisible at the business layer
// and a 500 at the page: accessbus wrote the row happily, and everything that
// read it afterwards -- the gate, the listing, the landing page -- failed.
func TestACreatorGrantSurvivesBeingStored(t *testing.T) {
	a := newAdmin(t, "")

	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")

	if got := siteRoleOf(t, a, volunteer); got != accessbus.RoleCreator {
		t.Fatalf("the stored site-wide role reads back as %q, want %q", got, accessbus.RoleCreator)
	}

	can, err := a.access.CanCreateForms(t.Context(), volunteer.ID)
	if err != nil {
		t.Fatalf("CanCreateForms: %v", err)
	}
	if !can {
		t.Error("the account holding a creator grant may not create forms")
	}

	// Every page that lists site-wide grants reads the same rows, so one that
	// cannot parse this role is the same bug somewhere quieter.
	boss := siteBoss(t, a, "site@schoenstatt.test")

	w := a.get(t, sitePeoplePage, sessionFor(t, a, boss))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s with a creator grant on file = %d, want 200:\n%s", sitePeoplePage, w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "volunteer@schoenstatt.test") {
		t.Errorf("the creator is missing from the list of who runs the service:\n%s", w.Body)
	}
}

// The link. A grant nobody can find is a grant nobody has: the account lands
// on /forms after signing in, and until it is offered the builder's new-form
// page there is nothing on this service a creator grant reaches.
func TestTheLandingPageOffersACreatorTheOnePageTheirGrantReaches(t *testing.T) {
	a := newAdmin(t, "")

	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	cookie := sessionFor(t, a, volunteer)

	w := a.get(t, "/forms", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /forms = %d, want 200:\n%s", w.Code, w.Body)
	}

	body := w.Body.String()

	if !strings.Contains(body, `href="/build/new"`) {
		t.Errorf("a creator is offered no way to start a form:\n%s", body)
	}

	// And not the whole-picture listing, which the same account is refused.
	if strings.Contains(body, `href="/build"`) {
		t.Errorf("a creator is offered /build, which answers 403 for them:\n%s", body)
	}

	// The link has to actually work, which is the half of this a template
	// test on its own would miss.
	if w := a.get(t, "/build/new", cookie); w.Code != http.StatusOK {
		t.Errorf("following the offered link = %d, want 200:\n%s", w.Code, w.Body)
	}

	// Telling somebody who may start a form that they must ask an
	// administrator for access is both wrong and a dead end.
	if strings.Contains(body, "can give you access to one") {
		t.Errorf("a creator is told to go and ask for access:\n%s", body)
	}
}

// The other side of it: an account with no site-wide grant is offered
// neither page, because both refuse it.
func TestTheLandingPageOffersNeitherBuilderPageToEverybodyElse(t *testing.T) {
	a := newAdmin(t, "")

	onOneForm := formAdmin(t, a, "boss@schoenstatt.test")
	cookie := sessionFor(t, a, onOneForm)

	w := a.get(t, "/forms", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /forms = %d, want 200:\n%s", w.Code, w.Body)
	}

	if body := w.Body.String(); strings.Contains(body, `href="/build`) {
		t.Errorf("an admin on one form is offered a builder page that refuses them:\n%s", body)
	}

	if w := a.get(t, "/build/new", cookie); w.Code != http.StatusForbidden {
		t.Errorf("an admin on one form GET /build/new = %d, want 403:\n%s", w.Code, w.Body)
	}
}

// A site-wide administrator keeps the whole picture, which is the link they
// had before any of this.
func TestTheLandingPageStillOffersTheWholePictureToASiteAdministrator(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	w := a.get(t, "/forms", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /forms = %d, want 200:\n%s", w.Code, w.Body)
	}

	if !strings.Contains(w.Body.String(), `href="/build"`) {
		t.Errorf("a site administrator is not offered the builder:\n%s", w.Body)
	}
}

// What the grant is actually for, checked at the row rather than at the
// redirect: creating a form makes the creator that form's administrator, and
// that is the whole reason the editor it redirects to is reachable.
func TestCreatingAFormGrantsItsCreatorAdminOnIt(t *testing.T) {
	a := newAdmin(t, "")

	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	cookie := sessionFor(t, a, volunteer)

	made(t, a, cookie, "bake-sale-2026", "Bake sale")

	if got := roleOnForm(t, a, volunteer, "bake-sale-2026"); got != accessbus.RoleAdmin {
		t.Errorf("the creator holds %q on the form they made, want %q", got, accessbus.RoleAdmin)
	}

	// Site-wide they are still only a creator. Making a form must not widen
	// what they hold over the service.
	if got := siteRoleOf(t, a, volunteer); got != accessbus.RoleCreator {
		t.Errorf("after creating a form the site-wide role is %q, want %q", got, accessbus.RoleCreator)
	}

	// A second form is theirs too, and the first one is still theirs.
	made(t, a, cookie, "raffle-2027", "Raffle")

	for _, slug := range []string{"bake-sale-2026", "raffle-2027"} {
		if w := a.get(t, "/forms/"+slug+"/edit", cookie); w.Code != http.StatusOK {
			t.Errorf("GET the editor for %s = %d, want 200:\n%s", slug, w.Code, w.Body)
		}
	}

}

// The volunteer this role was written for, end to end: given the grant, they
// start a form, fill it in, put it on the web, and read what comes back --
// without anybody ever holding their hand and without reaching anything that
// is not theirs.
//
// This is the test that says the feature works, as opposed to the ones that
// say each gate answers correctly.
func TestAVolunteerRunsTheirOwnFormEndToEnd(t *testing.T) {
	a := newAdmin(t, "")

	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	cookie := sessionFor(t, a, volunteer)

	// They arrive at the landing page and follow the one link on it.
	if w := a.get(t, "/forms", cookie); !strings.Contains(w.Body.String(), `href="/build/new"`) {
		t.Fatalf("nothing on the landing page leads to the builder:\n%s", w.Body)
	}

	made(t, a, cookie, "bake-sale-2026", "Bake sale")

	// They fill it in.
	w := a.post(t, "/forms/bake-sale-2026/edit/fields", url.Values{
		"name":     {"who"},
		"label":    {"Your name"},
		"kind":     {"text"},
		"required": {"yes"},
	}, cookie)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("adding a field = %d, want 303:\n%s", w.Code, w.Body)
	}

	// And put it on the web, which is the authority their per-form admin
	// grant carries and the reason the grant is made at creation.
	w = a.post(t, "/forms/bake-sale-2026/edit/state", url.Values{"state": {"live"}}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("publishing their own form = %d, want 200:\n%s", w.Code, w.Body)
	}

	if _, err := a.catalogue.ByID(mustSlug(t, "bake-sale-2026")); err != nil {
		t.Fatalf("the published form is not being served: %v", err)
	}

	// Once it is live it is on their landing page, and they may read what
	// comes back and hand the form to somebody else to help with.
	if w := a.get(t, "/forms", cookie); !strings.Contains(w.Body.String(), "Bake sale") {
		t.Errorf("their own published form is missing from their list:\n%s", w.Body)
	}

	for _, target := range []string{
		"/forms/bake-sale-2026/submissions",
		"/forms/bake-sale-2026/people",
		"/forms/bake-sale-2026/will-call",
	} {
		if w := a.get(t, target, cookie); w.Code != http.StatusOK {
			t.Errorf("the creator GET %s on their own form = %d, want 200:\n%s", target, w.Code, w.Body)
		}
	}

	// Still only a creator over the service itself.
	if got := siteRoleOf(t, a, volunteer); got != accessbus.RoleCreator {
		t.Errorf("after running a form end to end the site-wide role is %q, want %q", got, accessbus.RoleCreator)
	}

	if res := a.get(t, buildPage, cookie); res.Code != http.StatusForbidden {
		t.Errorf("after running a form end to end, GET %s = %d, want 403", buildPage, res.Code)
	}
}

// The way back to a half-built form.
//
// A draft is not in the served catalogue -- that is what makes it safe for it
// to be unfinished -- so it reaches this page through the builder's own store
// rather than through the catalogue everything else on it comes from. Without
// that, a volunteer who closed the tab on a form they had started had no link
// to it anywhere: the one page that lists drafts is the builder's whole
// picture, and a creator is refused that.
func TestACreatorFindsTheirOwnUnpublishedDraftOnTheLandingPage(t *testing.T) {
	a := newAdmin(t, "")

	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	cookie := sessionFor(t, a, volunteer)

	made(t, a, cookie, "bake-sale-2026", "Bake sale")

	// The draft exists and is theirs...
	if got := roleOnForm(t, a, volunteer, "bake-sale-2026"); got != accessbus.RoleAdmin {
		t.Fatalf("the draft is not theirs: %q", got)
	}

	if w := a.get(t, "/forms/bake-sale-2026/edit", cookie); w.Code != http.StatusOK {
		t.Fatalf("the editor for their own draft = %d, want 200", w.Code)
	}

	// ...and it is on the page they land on, marked as what it is and
	// offering the one link that works on a draft.
	w := a.get(t, "/forms", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /forms = %d, want 200:\n%s", w.Code, w.Body)
	}

	body := w.Body.String()

	for _, want := range []string{"Bake sale", `href="/forms/bake-sale-2026/edit"`, "draft"} {
		if !strings.Contains(body, want) {
			t.Errorf("the landing page does not show %q for a draft they administer:\n%s", want, body)
		}
	}

	// Not the links that resolve the form through the served catalogue, all
	// of which answer 404 for a draft. A row whose every link refuses you is
	// worse than no row.
	for _, gone := range []string{
		`href="/forms/bake-sale-2026/submissions"`,
		`href="/forms/bake-sale-2026/will-call"`,
		`href="/forms/bake-sale-2026/people"`,
		`href="/notifications/bake-sale-2026"`,
	} {
		if strings.Contains(body, gone) {
			t.Errorf("the draft row offers %s, which answers 404:\n%s", gone, body)
		}
	}

	// Those really do refuse, which is the assertion behind the one above.
	for _, target := range []string{
		"/forms/bake-sale-2026/submissions",
		"/forms/bake-sale-2026/will-call",
		"/forms/bake-sale-2026/people",
	} {
		if res := a.get(t, target, cookie); res.Code == http.StatusOK {
			t.Errorf("GET %s on a draft = 200; the row was right not to link it", target)
		}
	}

	// And the whole picture is still not theirs.
	if res := a.get(t, buildPage, cookie); res.Code != http.StatusForbidden {
		t.Errorf("GET %s = %d, want 403", buildPage, res.Code)
	}
}

// A draft belongs to whoever administers it and to nobody else.
//
// Split across three tests rather than written as one, because each account
// here has to sign in and the sign-in routes share one allowance of five
// requests in thirty seconds -- four accounts in one test is a 429 rather
// than a result. A fresh harness per test is a fresh allowance.
func TestSomebodyElsesDraftIsNotOnYourLandingPage(t *testing.T) {
	a := newAdmin(t, "")

	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	made(t, a, sessionFor(t, a, volunteer), "bake-sale-2026", "Bake sale")

	// An account with a grant on a different form entirely.
	stranger := formAdmin(t, a, "boss@schoenstatt.test")

	if w := a.get(t, "/forms", sessionFor(t, a, stranger)); strings.Contains(w.Body.String(), "Bake sale") {
		t.Errorf("somebody else's draft is on this account's landing page:\n%s", w.Body)
	}
}

// A site-wide administrator administers every form, present and future, so a
// draft somebody else started is one of theirs -- the same rows the builder's
// own listing shows them.
func TestASiteAdministratorSeesADraftSomebodyElseStarted(t *testing.T) {
	a := newAdmin(t, "")

	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	made(t, a, sessionFor(t, a, volunteer), "bake-sale-2026", "Bake sale")

	boss := siteBoss(t, a, "site@schoenstatt.test")

	if w := a.get(t, "/forms", sessionFor(t, a, boss)); !strings.Contains(w.Body.String(), "Bake sale") {
		t.Errorf("a site administrator cannot see a draft on the service they run:\n%s", w.Body)
	}
}

// Reading a form is not finishing it. The only link a draft row carries is
// the editor, so offering the row to somebody holding results would be
// offering a row that refuses them.
func TestADraftIsNotOfferedToAnAccountThatMayOnlyReadIt(t *testing.T) {
	a := newAdmin(t, "")

	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	made(t, a, sessionFor(t, a, volunteer), "bake-sale-2026", "Bake sale")

	reader, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, "counter@schoenstatt.test"),
		Name:  "A Counter",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := a.access.Grant(t.Context(), time.Now(), volunteer.ID, reader.ID, mustSlug(t, "bake-sale-2026"), accessbus.RoleResults); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	if w := a.get(t, "/forms", sessionFor(t, a, reader)); strings.Contains(w.Body.String(), "Bake sale") {
		t.Errorf("a draft is offered to an account that may only read it:\n%s", w.Body)
	}
}

// Once a draft is published it is a served form like any other, and it must
// appear once rather than twice -- from the catalogue, with its submissions
// count and its real state, not also from the draft store it still lives in.
func TestAPublishedFormIsNotAlsoListedAsADraft(t *testing.T) {
	a := newAdmin(t, "")

	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	cookie := sessionFor(t, a, volunteer)

	made(t, a, cookie, "bake-sale-2026", "Bake sale")

	if w := a.post(t, "/forms/bake-sale-2026/edit/fields", url.Values{
		"name": {"who"}, "label": {"Your name"}, "kind": {"text"}, "required": {"yes"},
	}, cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("adding a field = %d, want 303:\n%s", w.Code, w.Body)
	}

	if w := a.post(t, "/forms/bake-sale-2026/edit/state", url.Values{"state": {"live"}}, cookie); w.Code != http.StatusOK {
		t.Fatalf("publishing = %d, want 200:\n%s", w.Code, w.Body)
	}

	body := a.get(t, "/forms", cookie).Body.String()

	if n := strings.Count(body, "Bake sale"); n != 1 {
		t.Errorf("the published form appears %d times on the landing page, want once:\n%s", n, body)
	}

	if strings.Contains(body, ">draft<") {
		t.Errorf("a published form is still listed as a draft:\n%s", body)
	}

	if !strings.Contains(body, `href="/forms/bake-sale-2026/submissions"`) {
		t.Errorf("the published form does not link to its submissions:\n%s", body)
	}
}

// The boundary the role exists to draw. A creator administers what they made
// and reaches nothing else -- not somebody else's submissions, not their
// will-call table, not their people page, not the export.
func TestACreatorReachesNothingOfAnybodyElses(t *testing.T) {
	a := newAdmin(t, "")

	boss := siteBoss(t, a, "site@schoenstatt.test")
	made(t, a, sessionFor(t, a, boss), "raffle-2026", "Someone else's raffle")

	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	cookie := sessionFor(t, a, volunteer)

	made(t, a, cookie, "bake-sale-2026", "Bake sale")

	// theForm is the released definition every other test in this package
	// uses, and it is not the volunteer's either.
	for _, target := range []string{
		"/forms/raffle-2026/edit",
		"/forms/raffle-2026/people",
		"/forms/" + theForm + "/submissions",
		"/forms/" + theForm + "/submissions.csv",
		"/forms/" + theForm + "/will-call",
		"/forms/" + theForm + "/edit",
		"/forms/" + theForm + "/people",
	} {
		if w := a.get(t, target, cookie); w.Code != http.StatusForbidden {
			t.Errorf("a creator GET %s = %d, want 403:\n%s", target, w.Code, w.Body)
		}
	}

	// And their own list names only their own form.
	w := a.get(t, "/forms", cookie)
	if strings.Contains(w.Body.String(), "Someone else's raffle") {
		t.Errorf("somebody else's form is on the creator's list:\n%s", w.Body)
	}
}

// The escalation worth checking by name: a form that already exists is not a
// form you may claim by asking to create it. The grant that makes a creator
// an administrator is made only after the form was really created, and this
// is the test that keeps it that way.
func TestCreatingAFormThatExistsGrantsNothing(t *testing.T) {
	a := newAdmin(t, "")

	boss := siteBoss(t, a, "site@schoenstatt.test")
	made(t, a, sessionFor(t, a, boss), "raffle-2026", "Someone else's raffle")

	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	cookie := sessionFor(t, a, volunteer)

	// Both shapes of "already taken": a draft somebody else made, and a
	// released definition that is built in.
	for _, slug := range []string{"raffle-2026", theForm} {
		w := a.post(t, "/build/new", url.Values{"slug": {slug}, "title": {"Mine now"}}, cookie)
		if w.Code != http.StatusConflict {
			t.Errorf("POST /build/new for the existing %s = %d, want 409:\n%s", slug, w.Code, w.Body)
		}

		if got := roleOnForm(t, a, volunteer, slug); got != "" {
			t.Errorf("a refused creation granted %q on %s", got, slug)
		}

		if w := a.get(t, "/forms/"+slug+"/edit", cookie); w.Code != http.StatusForbidden {
			t.Errorf("after a refused creation, GET the editor for %s = %d, want 403:\n%s", slug, w.Code, w.Body)
		}
	}
}

// A creator administers the form they made, which includes its people page --
// and that page must not become a way to widen their own authority. The role
// it offers is a per-form role, and "creator" is not one.
func TestACreatorCannotHandOutACreatorRoleFromTheirOwnFormsPeoplePage(t *testing.T) {
	a := newAdmin(t, "")

	// An admin on a served form, which is what a creator becomes on their own
	// form the moment they publish it. Done this way round because a draft
	// has no people page at all -- peopleapp resolves the form through the
	// served catalogue -- and the rule under test is about the role, not
	// about which form it is.
	boss := formAdmin(t, a, "boss@schoenstatt.test")
	cookie := sessionFor(t, a, boss)

	theirPeoplePage := "/forms/" + theForm + "/people"

	w := a.get(t, theirPeoplePage, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET their own form's people page = %d, want 200:\n%s", w.Code, w.Body)
	}

	// The dropdown must not offer it either, since nothing per-form would
	// ever check it.
	if strings.Contains(w.Body.String(), `value="creator"`) {
		t.Errorf("the per-form people page offers a role no per-form gate asks for:\n%s", w.Body)
	}

	res := a.post(t, theirPeoplePage, url.Values{
		"email": {"friend@schoenstatt.test"},
		"name":  {"A Friend"},
		"role":  {"creator"},
	}, cookie)
	if res.Code != http.StatusBadRequest {
		t.Errorf("granting creator on one form = %d, want 400:\n%s", res.Code, res.Body)
	}

	friend, ok := accountFor(t, a, "friend@schoenstatt.test")
	if ok {
		if can, err := a.access.CanCreateForms(t.Context(), friend.ID); err != nil || can {
			t.Errorf("a refused per-form grant made somebody a creator: %v, %v", can, err)
		}
	}
}

// The gate refuses somebody who is not signed in by sending them to sign in,
// rather than by answering 403 to a person who has done nothing wrong. This
// is Require's job and the new gate sits behind it; the assertion is that it
// really is behind it.
func TestTheNewRoutesSendASignedOutVisitorToSignIn(t *testing.T) {
	a := newAdmin(t, "")

	for _, target := range []string{"/build/new", sitePeoplePage} {
		w := a.get(t, target, "")
		if w.Code != http.StatusSeeOther {
			t.Errorf("a signed-out GET %s = %d, want 303:\n%s", target, w.Code, w.Body)
		}

		if to := w.Header().Get("Location"); !strings.HasPrefix(to, "/signin") {
			t.Errorf("a signed-out GET %s went to %q, want the sign-in page", target, to)
		}
	}
}

// An account with no grant at all is refused both, and told which account it
// is signed in as -- the thing that turns "403" into something somebody can
// act on.
func TestAnAccountWithNoGrantsIsRefusedBothNewPages(t *testing.T) {
	a := newAdmin(t, "")

	nobody, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, "nobody@schoenstatt.test"),
		Name:  "Nobody",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cookie := sessionFor(t, a, nobody)

	w := a.get(t, "/build/new", cookie)
	if w.Code != http.StatusForbidden {
		t.Errorf("GET /build/new with no grants = %d, want 403:\n%s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "nobody@schoenstatt.test") {
		t.Errorf("the refusal does not say which account it refused:\n%s", w.Body)
	}

	if w := a.post(t, "/build/new", url.Values{"slug": {"mine-2026"}, "title": {"Mine"}}, cookie); w.Code != http.StatusForbidden {
		t.Errorf("POST /build/new with no grants = %d, want 403:\n%s", w.Code, w.Body)
	}

	if _, err := a.catalogue.Draft(t.Context(), mustSlug(t, "mine-2026")); err == nil {
		t.Error("a refused POST created the form anyway")
	}
}

// Every write on this surface is a form this service rendered, and the new
// ones are no exception: a cross-site POST is refused before it reaches a
// handler. Checked here rather than assumed from the chain, because "the
// middleware is mounted" and "this route is behind it" are different claims.
func TestTheNewWritesRefuseACrossSiteSubmission(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	writes := []struct {
		target string
		form   url.Values
	}{
		{"/build/new", url.Values{"slug": {"elsewhere-2026"}, "title": {"Elsewhere"}}},
		{sitePeoplePage, url.Values{"email": {"them@schoenstatt.test"}, "role": {"creator"}}},
		{sitePeoplePage + "/role", url.Values{"user": {types.NewID().String()}, "role": {"admin"}}},
		{sitePeoplePage + "/revoke", url.Values{"user": {types.NewID().String()}}},
	}

	for _, write := range writes {
		r := httptest.NewRequest(http.MethodPost, write.target, strings.NewReader(write.form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.AddCookie(&http.Cookie{Name: mid.CookieName, Value: cookie})

		w := httptest.NewRecorder()
		a.h.ServeHTTP(w, r)

		if w.Code != http.StatusForbidden {
			t.Errorf("a cross-site POST %s = %d, want 403:\n%s", write.target, w.Code, w.Body)
		}
	}

	// The one that would have left a trace if it had got through.
	if _, err := a.catalogue.Draft(t.Context(), mustSlug(t, "elsewhere-2026")); err == nil {
		t.Error("a cross-site POST created a form")
	}
}

// What the page refuses, and how. Each of these is a person getting it wrong
// rather than an attack, so each is a sentence on the page and a status that
// says whose fault it was -- not a 500, and never a grant made anyway.
func TestTheSitePeoplePageRefusesBadInputWithoutGrantingAnything(t *testing.T) {
	a := newAdmin(t, "")

	boss := siteBoss(t, a, "site@schoenstatt.test")
	cookie := sessionFor(t, a, boss)

	held := formCreator(t, a, "volunteer@schoenstatt.test")

	cases := []struct {
		what   string
		target string
		form   url.Values
		want   int
		says   string
	}{
		{
			what:   "a role that is not offered site-wide",
			target: sitePeoplePage,
			form:   url.Values{"email": {"new@schoenstatt.test"}, "role": {"results"}},
			want:   http.StatusBadRequest,
			says:   "Choose what they may do.",
		},
		{
			what:   "a role that is not a role",
			target: sitePeoplePage,
			form:   url.Values{"email": {"new@schoenstatt.test"}, "role": {"emperor"}},
			want:   http.StatusBadRequest,
			says:   "Choose what they may do.",
		},
		{
			what:   "no role at all",
			target: sitePeoplePage,
			form:   url.Values{"email": {"new@schoenstatt.test"}},
			want:   http.StatusBadRequest,
			says:   "Choose what they may do.",
		},
		{
			what:   "an address that is not one",
			target: sitePeoplePage,
			form:   url.Values{"email": {"not an address"}, "role": {"creator"}},
			want:   http.StatusBadRequest,
			says:   "does not look like an email address",
		},
		{
			what:   "your own address",
			target: sitePeoplePage,
			form:   url.Values{"email": {"site@schoenstatt.test"}, "role": {"creator"}},
			want:   http.StatusConflict,
			says:   "That is your own address",
		},
		{
			what:   "changing the role of somebody who is not identified",
			target: sitePeoplePage + "/role",
			form:   url.Values{"user": {"not-an-id"}, "role": {"admin"}},
			want:   http.StatusBadRequest,
			says:   "could not tell who that was",
		},
		{
			what:   "changing somebody to a role the page does not offer",
			target: sitePeoplePage + "/role",
			form:   url.Values{"user": {held.ID.String()}, "role": {"door"}},
			want:   http.StatusBadRequest,
			says:   "Choose what they may do.",
		},
		{
			what:   "changing the role of somebody who holds none",
			target: sitePeoplePage + "/role",
			form:   url.Values{"user": {types.NewID().String()}, "role": {"admin"}},
			want:   http.StatusConflict,
			says:   "does not hold a site-wide role",
		},
		{
			what:   "revoking from somebody who is not identified",
			target: sitePeoplePage + "/revoke",
			form:   url.Values{"user": {""}},
			want:   http.StatusBadRequest,
			says:   "could not tell who that was",
		},
	}

	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			w := a.post(t, c.target, c.form, cookie)
			if w.Code != c.want {
				t.Errorf("POST %s with %s = %d, want %d:\n%s", c.target, c.what, w.Code, c.want, w.Body)
			}

			if !strings.Contains(w.Body.String(), c.says) {
				t.Errorf("the page does not say %q:\n%s", c.says, w.Body)
			}
		})
	}

	// Nothing above changed anything. The volunteer holds what they held, the
	// administrator still administers, and no account was created for an
	// address that was never valid.
	if got := siteRoleOf(t, a, held); got != accessbus.RoleCreator {
		t.Errorf("the volunteer now holds %q, want %q", got, accessbus.RoleCreator)
	}

	if got := siteRoleOf(t, a, boss); got != accessbus.RoleAdmin {
		t.Errorf("the administrator now holds %q, want %q", got, accessbus.RoleAdmin)
	}

	if _, ok := accountFor(t, a, "new@schoenstatt.test"); ok {
		t.Error("a refused grant created the account anyway")
	}
}

// Asking for the role somebody already holds is not an error and not a
// second grant -- it is the answer "they already can", which is what
// somebody who clicked Save twice needs to read.
func TestAskingForTheRoleSomebodyAlreadyHoldsSaysSo(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))
	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")

	w := a.post(t, sitePeoplePage+"/role", url.Values{
		"user": {volunteer.ID.String()}, "role": {"creator"},
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("re-asking for the role they hold = %d, want 200:\n%s", w.Code, w.Body)
	}

	if !strings.Contains(w.Body.String(), "No change") {
		t.Errorf("the page does not say nothing changed:\n%s", w.Body)
	}

	if got := siteRoleOf(t, a, volunteer); got != accessbus.RoleCreator {
		t.Errorf("they now hold %q, want %q", got, accessbus.RoleCreator)
	}
}

// Giving somebody access to the service makes an account for them if they
// have none, and tells them how to sign in -- the same bargain peopleapp
// makes for one form. What the message says matters, because it is the only
// instruction the person ever gets.
func TestGivingSiteWideAccessMakesAnAccountAndInvitesThem(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	w := a.post(t, sitePeoplePage, url.Values{
		"email": {"volunteer@schoenstatt.test"},
		"name":  {"A Volunteer"},
		"role":  {"creator"},
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200:\n%s", sitePeoplePage, w.Code, w.Body)
	}

	volunteer, ok := accountFor(t, a, "volunteer@schoenstatt.test")
	if !ok {
		t.Fatal("no account was made for the address")
	}

	if volunteer.Name != "A Volunteer" {
		t.Errorf("the account is named %q, want %q", volunteer.Name, "A Volunteer")
	}

	m, sent := a.sent.Last()
	if !sent {
		t.Fatal("nobody was told they had been given access")
	}

	if m.To != "volunteer@schoenstatt.test" {
		t.Errorf("the invitation went to %q", m.To)
	}

	// The link has to be this service's own address rather than anything a
	// request carried, which is why BaseURL is configuration.
	if !strings.Contains(m.Text, "https://forms.test/signin") {
		t.Errorf("the invitation does not say where to sign in:\n%s", m.Text)
	}

	if !strings.Contains(m.Text, "make new forms") {
		t.Errorf("the invitation does not say what they may now do:\n%s", m.Text)
	}

	// And the account it made really can do the thing the message promises.
	if w := a.get(t, "/build/new", sessionFor(t, a, volunteer)); w.Code != http.StatusOK {
		t.Errorf("the invited account GET /build/new = %d, want 200:\n%s", w.Code, w.Body)
	}
}

// An address that already has an account is given the role rather than a
// second account, and keeps the name it already had.
func TestGivingAccessToAnExistingAccountDoesNotMakeASecondOne(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))
	existing := formAdmin(t, a, "boss@schoenstatt.test")

	w := a.post(t, sitePeoplePage, url.Values{
		"email": {"boss@schoenstatt.test"},
		"name":  {"Someone Else Entirely"},
		"role":  {"creator"},
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200:\n%s", sitePeoplePage, w.Code, w.Body)
	}

	again, ok := accountFor(t, a, "boss@schoenstatt.test")
	if !ok {
		t.Fatal("the account went missing")
	}

	if again.ID != existing.ID {
		t.Error("a second account was made for an address that already had one")
	}

	if got := siteRoleOf(t, a, again); got != accessbus.RoleCreator {
		t.Errorf("they hold %q site-wide, want %q", got, accessbus.RoleCreator)
	}

	// The grant they already held on one form is untouched: this page is
	// about the service, and it is not the place a form's own people are
	// changed.
	if got := roleOnForm(t, a, again, theForm); got != accessbus.RoleAdmin {
		t.Errorf("their grant on %s is now %q, want %q", theForm, got, accessbus.RoleAdmin)
	}
}

// Taking away the site-wide grant takes away exactly that, through the page
// as well as in the domain: the forms they ran are still theirs, and the
// thing the grant let them do is gone.
func TestRevokingThroughThePageLeavesTheFormsTheyMade(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	theirs := sessionFor(t, a, volunteer)

	made(t, a, theirs, "bake-sale-2026", "Bake sale")

	if w := a.post(t, sitePeoplePage+"/revoke", url.Values{"user": {volunteer.ID.String()}}, cookie); w.Code != http.StatusOK {
		t.Fatalf("revoking = %d, want 200:\n%s", w.Code, w.Body)
	}

	if w := a.post(t, "/build/new", url.Values{"slug": {"another-2026"}, "title": {"Another"}}, theirs); w.Code != http.StatusForbidden {
		t.Errorf("after revocation, POST /build/new = %d, want 403:\n%s", w.Code, w.Body)
	}

	// Still the administrator of what they already made. Revoking somebody's
	// ability to start new things is not the same decision as taking away the
	// thing they are in the middle of running, and this page makes only the
	// first one.
	if w := a.get(t, "/forms/bake-sale-2026/edit", theirs); w.Code != http.StatusOK {
		t.Errorf("after revocation they lost the form they were running = %d, want 200:\n%s", w.Code, w.Body)
	}
}

// The page lists who runs the service, says which is which, and offers no
// control over the reader's own row -- that last being the thing that stops
// the only administrator from locking everybody out with two clicks.
func TestTheSitePeoplePageShowsWhoRunsTheServiceAndNotYourOwnControls(t *testing.T) {
	a := newAdmin(t, "")

	boss := siteBoss(t, a, "site@schoenstatt.test")
	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")

	w := a.get(t, sitePeoplePage, sessionFor(t, a, boss))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200:\n%s", sitePeoplePage, w.Code, w.Body)
	}

	body := w.Body.String()

	for _, want := range []string{"site@schoenstatt.test", "volunteer@schoenstatt.test", "(you)"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q:\n%s", want, body)
		}
	}

	// The other person's row carries both controls; the reader's carries
	// neither, so the id that appears in a form field is only ever theirs.
	if !strings.Contains(body, `value="`+volunteer.ID.String()+`"`) {
		t.Errorf("the volunteer's row has no control on it:\n%s", body)
	}

	if strings.Contains(body, `value="`+boss.ID.String()+`"`) {
		t.Errorf("the reader's own row carries a control, which is how they lock themselves out:\n%s", body)
	}

	// And the choice offered when adding somebody is the site-wide one, in
	// words rather than in role names.
	for _, want := range []string{"Make new forms", "Administer the whole service"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not explain the choice %q:\n%s", want, body)
		}
	}

	// A role no site-wide gate asks for is not on offer.
	for _, gone := range []string{`value="results"`, `value="door"`} {
		if strings.Contains(body, gone) {
			t.Errorf("the page offers %s site-wide, which nothing mints that way:\n%s", gone, body)
		}
	}
}

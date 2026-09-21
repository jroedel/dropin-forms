package muxer_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const sitePeoplePage = "/site/people"

// Who may administer the whole service, or just start a form of their own, is
// managed here -- behind the same site-wide gate as the builder's
// whole-picture listing, and reachable by nobody administering only a form.
func TestOnlySiteWideAdministratorsReachTheSitePeoplePage(t *testing.T) {
	a := newAdmin(t, "")

	onOneForm := formAdmin(t, a, "boss@schoenstatt.test")
	whole := siteBoss(t, a, "site@schoenstatt.test")

	if w := a.get(t, sitePeoplePage, sessionFor(t, a, onOneForm)); w.Code != http.StatusForbidden {
		t.Errorf("an admin on one form GET %s = %d, want 403:\n%s", sitePeoplePage, w.Code, w.Body)
	}

	if w := a.get(t, sitePeoplePage, sessionFor(t, a, whole)); w.Code != http.StatusOK {
		t.Errorf("a site administrator GET %s = %d, want 200:\n%s", sitePeoplePage, w.Code, w.Body)
	}
}

// The whole point of the page: giving somebody a site-wide role from a
// browser, where previously only the one-time bootstrap secret could. A
// RoleCreator grant made here works exactly like one made by hand -- it
// reaches /build/new and nothing else site-wide -- and it can be widened to
// full administration, or taken away, from the same page.
func TestSiteWideAccessIsGrantedChangedAndRevokedFromItsOwnPage(t *testing.T) {
	a := newAdmin(t, "")

	whole := siteBoss(t, a, "site@schoenstatt.test")
	cookie := sessionFor(t, a, whole)

	w := a.post(t, sitePeoplePage, url.Values{
		"email": {"volunteer@schoenstatt.test"},
		"name":  {"A Volunteer"},
		"role":  {"creator"},
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200:\n%s", sitePeoplePage, w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "make new forms") {
		t.Errorf("the confirmation does not say what they can now do:\n%s", w.Body)
	}

	volunteer, ok := accountFor(t, a, "volunteer@schoenstatt.test")
	if !ok {
		t.Fatal("the invitation did not create an account")
	}

	vCookie := sessionFor(t, a, volunteer)

	// Reaches /build/new and can create a form -- the whole reason the grant
	// exists -- but not the whole-picture listing, which stays behind full
	// administration.
	made(t, a, vCookie, "bake-sale-2026", "Bake sale")

	if w := a.get(t, buildPage, vCookie); w.Code != http.StatusForbidden {
		t.Errorf("a form-creator GET %s = %d, want 403:\n%s", buildPage, w.Code, w.Body)
	}

	if w := a.get(t, sitePeoplePage, vCookie); w.Code != http.StatusForbidden {
		t.Errorf("a form-creator GET %s = %d, want 403:\n%s", sitePeoplePage, w.Code, w.Body)
	}

	// Widened to full administration from the same page.
	w = a.post(t, sitePeoplePage+"/role", url.Values{
		"user": {volunteer.ID.String()}, "role": {"admin"},
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s/role = %d, want 200:\n%s", sitePeoplePage, w.Code, w.Body)
	}

	if w := a.get(t, buildPage, vCookie); w.Code != http.StatusOK {
		t.Errorf("after being made a full administrator, GET %s = %d, want 200:\n%s", buildPage, w.Code, w.Body)
	}

	// And revoked entirely.
	w = a.post(t, sitePeoplePage+"/revoke", url.Values{"user": {volunteer.ID.String()}}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s/revoke = %d, want 200:\n%s", sitePeoplePage, w.Code, w.Body)
	}

	if w := a.get(t, buildPage, vCookie); w.Code != http.StatusForbidden {
		t.Errorf("after revocation, GET %s = %d, want 403:\n%s", buildPage, w.Code, w.Body)
	}

	if w := a.post(t, "/build/new", url.Values{"slug": {"another-2026"}, "title": {"Another"}}, vCookie); w.Code != http.StatusForbidden {
		t.Errorf("after revocation, POST /build/new = %d, want 403:\n%s", w.Code, w.Body)
	}
}

// The same self-protection peopleapp's own people page has: changing or
// removing your own row here is how you lock yourself, and everyone else,
// out of this page, and the way back is another administrator who no longer
// exists to ask.
func TestCannotChangeOrRevokeYourOwnSiteWideRoleFromItsOwnPage(t *testing.T) {
	a := newAdmin(t, "")

	whole := siteBoss(t, a, "site@schoenstatt.test")
	cookie := sessionFor(t, a, whole)

	w := a.post(t, sitePeoplePage+"/role", url.Values{
		"user": {whole.ID.String()}, "role": {"creator"},
	}, cookie)
	if w.Code != http.StatusConflict {
		t.Errorf("changing your own role = %d, want 409:\n%s", w.Code, w.Body)
	}

	w = a.post(t, sitePeoplePage+"/revoke", url.Values{"user": {whole.ID.String()}}, cookie)
	if w.Code != http.StatusConflict {
		t.Errorf("revoking your own access = %d, want 409:\n%s", w.Code, w.Body)
	}
}

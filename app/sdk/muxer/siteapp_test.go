package muxer_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
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

// The shortcut: somebody who already administers a form is listed with a
// button that makes them a site administrator, without anybody retyping
// their address. Pressing it grants the role, emails them, and takes them off
// the list -- their row is in the table above now -- while what they held on
// the form stays as it was.
func TestSomebodyOnAFormIsMadeASiteAdministratorFromTheirRow(t *testing.T) {
	a := newAdmin(t, "")

	whole := siteBoss(t, a, "site@schoenstatt.test")
	cookie := sessionFor(t, a, whole)
	onOneForm := formAdmin(t, a, "boss@schoenstatt.test")

	w := a.get(t, sitePeoplePage, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200:\n%s", sitePeoplePage, w.Code, w.Body)
	}

	if body := w.Body.String(); !strings.Contains(body, "Already working on a form") ||
		!strings.Contains(body, `name="user" value="`+onOneForm.ID.String()+`"`) {
		t.Fatalf("somebody on a form is not offered site-wide access:\n%s", body)
	}

	w = a.post(t, sitePeoplePage+"/promote", url.Values{
		"user": {onOneForm.ID.String()}, "role": {"admin"},
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s/promote = %d, want 200:\n%s", sitePeoplePage, w.Code, w.Body)
	}

	if strings.Contains(w.Body.String(), "Already working on a form") {
		t.Errorf("after being made a site administrator they are still offered it:\n%s", w.Body)
	}

	m, sent := a.sent.Last()
	if !sent || m.To != "boss@schoenstatt.test" || !strings.Contains(m.Text, "administer the whole service") {
		t.Errorf("the mail about it = %+v (sent %v)", m, sent)
	}

	if w := a.get(t, buildPage, sessionFor(t, a, onOneForm)); w.Code != http.StatusOK {
		t.Errorf("after being made a site administrator, GET %s = %d, want 200:\n%s", buildPage, w.Code, w.Body)
	}

	if got := roleOn(t, a, onOneForm); got != accessbus.RoleAdmin {
		t.Errorf("their grant on the form became %q, want it left as admin", got)
	}

	// A second press is not a second announcement: the role they now hold
	// is changed from its dropdown.
	w = a.post(t, sitePeoplePage+"/promote", url.Values{
		"user": {onOneForm.ID.String()}, "role": {"creator"},
	}, cookie)
	if w.Code != http.StatusConflict {
		t.Errorf("promoting somebody already site-wide = %d, want 409:\n%s", w.Code, w.Body)
	}
}

// The route reaches only the people on that list. An account with no grant on
// any form -- which is to say one nobody has added by address -- is refused,
// and so is the reader's own.
func TestPromotingReachesOnlyPeopleAlreadyOnAForm(t *testing.T) {
	a := newAdmin(t, "")

	whole := siteBoss(t, a, "site@schoenstatt.test")
	cookie := sessionFor(t, a, whole)

	stranger, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, "stranger@schoenstatt.test"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, tt := range []struct {
		name string
		id   string
	}{
		{"an account on no form", stranger.ID.String()},
		{"your own", whole.ID.String()},
	} {
		w := a.post(t, sitePeoplePage+"/promote", url.Values{"user": {tt.id}, "role": {"admin"}}, cookie)
		if w.Code != http.StatusConflict {
			t.Errorf("%s = %d, want 409:\n%s", tt.name, w.Code, w.Body)
		}
	}

	if w := a.get(t, buildPage, signedIn(t, a, stranger)); w.Code != http.StatusForbidden {
		t.Errorf("the stranger GET %s = %d, want 403", buildPage, w.Code)
	}

	// And nobody without site-wide administration may press it at all.
	onOneForm := formAdmin(t, a, "boss@schoenstatt.test")

	w := a.post(t, sitePeoplePage+"/promote", url.Values{
		"user": {onOneForm.ID.String()}, "role": {"admin"},
	}, signedIn(t, a, onOneForm))
	if w.Code != http.StatusForbidden {
		t.Errorf("a form administrator promoting themselves = %d, want 403:\n%s", w.Code, w.Body)
	}
}

// signedIn is a session made through the account domain rather than the
// pages, for a test that signs in more people from one address than the
// sign-in throttle lets through.
func signedIn(t *testing.T, a harness, u userbus.User) string {
	t.Helper()

	req, err := a.users.RequestSignIn(t.Context(), time.Now(), u.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}

	_, cookie, err := a.users.SignInWithCode(t.Context(), time.Now(), u.Email, req.Code)
	if err != nil {
		t.Fatalf("SignInWithCode: %v", err)
	}

	return cookie
}

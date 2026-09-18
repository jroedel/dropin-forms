package muxer_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/notify/notifybus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// The page that turns email about a form off, through the mounted admin
// surface.
//
// It is the one route on that surface deliberately outside the session gate,
// because the link that leads to it is opened from a mailbox. So the tests
// that matter are about what stands in for the session: a signed token, a GET
// that changes nothing, and a refusal that still tells somebody what to do.

// reader is an account that can read the feast form's submissions, which is
// what makes it somebody the notifications go to.
func reader(t *testing.T, a harness, address string) userbus.User {
	t.Helper()

	u, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, address),
		Name:  "A Reader",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := a.access.Grant(t.Context(), time.Now(), types.ID{}, u.ID, mustSlug(t, theForm), accessbus.RoleResults); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	return u
}

func linkFor(t *testing.T, a harness, u userbus.User) string {
	t.Helper()

	token, err := notifybus.MintMuteToken(a.muteKey, u.ID, mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("MintMuteToken: %v", err)
	}

	return token
}

// sessionFor signs an account in the ordinary way, because a session minted
// any other way is not the thing the page will meet.
func sessionFor(t *testing.T, a harness, u userbus.User) string {
	t.Helper()

	if w := a.post(t, "/signin", url.Values{"email": {u.Email.String()}}, ""); w.Code != http.StatusOK {
		t.Fatalf("asking for a link = %d", w.Code)
	}

	w := a.post(t, "/signin/link", url.Values{"token": {signInLink(t, a)}}, "")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("redeeming the link = %d:\n%s", w.Code, w.Body)
	}

	cookie := sessionCookie(t, w)
	if cookie == "" {
		t.Fatal("signing in set no session cookie")
	}

	return cookie
}

func mutedNow(t *testing.T, a harness, u userbus.User) bool {
	t.Helper()

	muted, err := a.notify.Muted(t.Context(), u.ID, mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("Muted: %v", err)
	}

	return muted
}

// The property the whole design turns on. Mail scanners and link previewers
// fetch URLs found in messages without anybody clicking, so a link that
// unsubscribed on GET would be spent by software before the person saw it --
// and the symptom is notifications that simply stop, for no reason anybody can
// see.
func TestOpeningTheUnsubscribeLinkChangesNothing(t *testing.T) {
	a := newAdmin(t, "")
	u := reader(t, a, "reader@schoenstatt.test")

	target := "/notifications/" + theForm + "?t=" + url.QueryEscape(linkFor(t, a, u))

	// Twice, as a previewer and then the person would.
	for i := range 2 {
		w := a.get(t, target, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET %d = %d, want 200:\n%s", i+1, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "Stop emailing me") {
			t.Errorf("the page offers no button:\n%s", w.Body)
		}
	}

	if mutedNow(t, a, u) {
		t.Fatal("opening the link turned the notifications off; software opens links")
	}
}

// And the button does work, from a mailbox, with no session anywhere.
func TestTheButtonTurnsItOffAndOnAgainWithNoSession(t *testing.T) {
	a := newAdmin(t, "")
	u := reader(t, a, "reader@schoenstatt.test")
	token := linkFor(t, a, u)

	w := a.post(t, "/notifications/"+theForm, url.Values{"t": {token}, "muted": {"yes"}}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200:\n%s", w.Code, w.Body)
	}
	if !mutedNow(t, a, u) {
		t.Fatal("the preference was not stored")
	}
	if !strings.Contains(w.Body.String(), "We will stop emailing you") {
		t.Errorf("the page does not say what happened:\n%s", w.Body)
	}

	// Pressing a stale page's button again says the same thing rather than
	// flipping back, which is why the value says what to do instead of
	// toggling.
	w = a.post(t, "/notifications/"+theForm, url.Values{"t": {token}, "muted": {"yes"}}, "")
	if w.Code != http.StatusOK || !mutedNow(t, a, u) {
		t.Fatalf("pressing it twice = %d, muted=%v", w.Code, mutedNow(t, a, u))
	}

	// And back on, from the same page, because somebody who turns this off in
	// October wants it back in November.
	w = a.post(t, "/notifications/"+theForm, url.Values{"t": {token}, "muted": {"no"}}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("turning it back on = %d:\n%s", w.Code, w.Body)
	}
	if mutedNow(t, a, u) {
		t.Error("it is still off")
	}
}

// A link nobody signed is not a link. Silencing the person who reads the
// orders is how a paid lunch goes unnoticed, which is why this is a MAC and
// not an identifier.
func TestAForgedOrMisusedLinkIsRefused(t *testing.T) {
	a := newAdmin(t, "")
	u := reader(t, a, "reader@schoenstatt.test")
	token := linkFor(t, a, u)

	other, err := notifybus.ParseMuteKey("some-other-secret-that-is-long-enough")
	if err != nil {
		t.Fatalf("ParseMuteKey: %v", err)
	}

	forged, err := notifybus.MintMuteToken(other, u.ID, mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("MintMuteToken: %v", err)
	}

	tests := map[string]string{
		"signed with another key": forged,
		"truncated":               token[:len(token)-2],
		"not a token at all":      "hello",
	}

	for name, bad := range tests {
		t.Run(name, func(t *testing.T) {
			w := a.post(t, "/notifications/"+theForm, url.Values{"t": {bad}, "muted": {"yes"}}, "")

			if w.Code != http.StatusForbidden {
				t.Errorf("POST = %d, want 403", w.Code)
			}

			// The page still helps: the person holding a broken link is
			// almost always the person it was for.
			if !strings.Contains(w.Body.String(), "Sign in") {
				t.Errorf("the refusal does not say what does work:\n%s", w.Body)
			}
		})
	}

	if mutedNow(t, a, u) {
		t.Fatal("a refused link changed the preference")
	}
}

// A token is for one person on one form, and the path says which form. The two
// disagreeing is nothing legitimate, so it is refused rather than resolved in
// favour of either.
func TestATokenForAnotherFormIsRefused(t *testing.T) {
	a := newAdmin(t, "")
	u := reader(t, a, "reader@schoenstatt.test")

	token, err := notifybus.MintMuteToken(a.muteKey, u.ID, mustSlug(t, "another-form"))
	if err != nil {
		t.Fatalf("MintMuteToken: %v", err)
	}

	w := a.post(t, "/notifications/"+theForm, url.Values{"t": {token}, "muted": {"yes"}}, "")
	if w.Code != http.StatusForbidden {
		t.Errorf("POST = %d, want 403", w.Code)
	}
	if mutedNow(t, a, u) {
		t.Error("a token for another form changed this one")
	}
}

// With neither a token nor a session there is nobody to change, so the page
// sends somebody to sign in and brings them back.
func TestWithNothingToGoOnItAsksYouToSignIn(t *testing.T) {
	a := newAdmin(t, "")

	w := a.get(t, "/notifications/"+theForm, "")

	if w.Code != http.StatusSeeOther {
		t.Fatalf("GET = %d, want 303", w.Code)
	}

	if got := w.Header().Get("Location"); !strings.HasPrefix(got, "/signin?next=") || !strings.Contains(got, "notifications") {
		t.Errorf("Location = %q, want the sign-in page with a way back here", got)
	}
}

// The other way in: signed in, from the forms list, with no token at all.
func TestSignedInYouCanChangeItFromTheFormsPage(t *testing.T) {
	a := newAdmin(t, "")
	u := reader(t, a, "reader@schoenstatt.test")

	cookie := sessionFor(t, a, u)

	// The list says which way round it is, and links to the page.
	w := a.get(t, "/forms", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /forms = %d:\n%s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `href="/notifications/`+theForm+`"`) {
		t.Errorf("the forms list does not link to the email setting:\n%s", w.Body)
	}

	w = a.post(t, "/notifications/"+theForm, url.Values{"muted": {"yes"}}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d:\n%s", w.Code, w.Body)
	}
	if !mutedNow(t, a, u) {
		t.Fatal("the preference was not stored for a signed-in reader")
	}

	// And the list now says off.
	w = a.get(t, "/forms", cookie)
	if !strings.Contains(w.Body.String(), ">off</a>") {
		t.Errorf("the forms list still says the email is on:\n%s", w.Body)
	}
}

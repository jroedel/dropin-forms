package muxer_test

import (
	"strings"
	"testing"
)

// Every page somebody is signed in to links to their account page. Before
// this, nothing did, and an organiser could not find it.
func TestEveryPageLinksToYourAccountOnceSignedIn(t *testing.T) {
	a := newAdmin(t, "")
	link := `<a href="/account">Your account</a>`

	if body := a.get(t, "/signin", "").Body.String(); strings.Contains(body, link) {
		t.Error("the sign-in page links to an account nobody is signed in to")
	}

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	for _, path := range []string{"/forms", "/build", "/forms/" + theForm + "/submissions"} {
		if body := a.get(t, path, cookie).Body.String(); !strings.Contains(body, link) {
			t.Errorf("%s does not link to the account page:\n%s", path, short(body))
		}
	}

	// The account page has its sign-out button there instead of a link to
	// itself.
	body := a.get(t, "/account", cookie).Body.String()
	if strings.Contains(body, link) || !strings.Contains(body, "Sign out") {
		t.Errorf("the account page's masthead:\n%s", short(body))
	}
}

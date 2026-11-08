package muxer_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// Two people writing one form before it goes on the web.
//
// A draft is not in the served catalogue, and the people page used to find a
// form only there -- so a draft could not be shared with anybody, and the
// only way for a second person to help write one was to publish it first.
// These tests are the way it works now: the creator gives somebody admin on
// the draft, and that grant is the editor's gate.

// The whole of it, end to end: share, sign in, find it, edit it.
func TestADraftCanBeSharedWithSomebodyToHelpWriteIt(t *testing.T) {
	a := newAdmin(t, "")

	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	cookie := sessionFor(t, a, volunteer)

	made(t, a, cookie, "pageant-2026", "Christmas pageant")

	// The builder leads to the people page...
	if w := a.get(t, "/forms/pageant-2026/edit", cookie); !strings.Contains(w.Body.String(), `href="/forms/pageant-2026/people"`) {
		t.Errorf("the editor of a draft does not link to its people page:\n%s", w.Body)
	}

	// ...which works on a draft, says what it is, and has already chosen the
	// role that can help write it.
	w := a.get(t, "/forms/pageant-2026/people", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET the people page of a draft = %d, want 200:\n%s", w.Code, w.Body)
	}

	body := w.Body.String()

	for _, want := range []string{
		"not on the web yet",
		`value="admin"
             checked`,
		`href="/forms/pageant-2026/edit"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the people page of a draft does not show %q:\n%s", want, body)
		}
	}

	if strings.Contains(body, `href="/forms/pageant-2026/submissions"`) {
		t.Errorf("the people page of a draft leads to submissions there cannot be:\n%s", body)
	}

	w = a.post(t, "/forms/pageant-2026/people", url.Values{
		"email": {"helper@schoenstatt.test"},
		"name":  {"A Helper"},
		"role":  {"admin"},
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("sharing the draft = %d, want 200:\n%s", w.Code, w.Body)
	}

	helper, ok := accountFor(t, a, "helper@schoenstatt.test")
	if !ok {
		t.Fatal("sharing the draft with a new address made no account")
	}

	if got := roleOnForm(t, a, helper, "pageant-2026"); got != accessbus.RoleAdmin {
		t.Fatalf("the helper holds %q on the draft, want admin", got)
	}

	// The invitation says it is a draft and signs them in to the editor, not
	// to a submissions page that answers 404.
	m, sent := a.sent.Last()
	if !sent {
		t.Fatal("no invitation was sent")
	}

	if !strings.Contains(m.Text, "not on the web yet") {
		t.Errorf("the invitation to a draft does not say so:\n%s", m.Text)
	}

	if !strings.Contains(m.Text, "next=%2Fforms%2Fpageant-2026%2Fedit") {
		t.Errorf("the invitation to a draft does not sign them in to its editor:\n%s", m.Text)
	}

	// They sign in, find it, and change it.
	theirs := sessionFor(t, a, helper)

	if w := a.get(t, "/forms", theirs); !strings.Contains(w.Body.String(), `href="/forms/pageant-2026/edit"`) {
		t.Errorf("the shared draft is not on the helper's landing page:\n%s", w.Body)
	}

	if w := a.get(t, "/forms/pageant-2026/edit", theirs); w.Code != http.StatusOK {
		t.Fatalf("the helper GET the editor = %d, want 200:\n%s", w.Code, w.Body)
	}

	w = a.post(t, "/forms/pageant-2026/edit/fields", url.Values{
		"label": {"Your child's name"},
		"kind":  {"text"},
	}, theirs)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("the helper adding a question = %d, want 303:\n%s", w.Code, w.Body)
	}

	s, err := a.catalogue.Draft(t.Context(), mustSlug(t, "pageant-2026"))
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}

	if len(s.Form.Fields) != 1 {
		t.Errorf("the helper's question is not on the draft: %d fields", len(s.Form.Fields))
	}

	if s.Live {
		t.Error("sharing a draft published it")
	}
}

// Somebody given a role that cannot edit is told the truth: nothing to do
// until it is published, and signing in lands on their list rather than on a
// page that would answer 404.
func TestAnInvitationToReadADraftSaysItIsNotPublishedYet(t *testing.T) {
	a := newAdmin(t, "")

	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	cookie := sessionFor(t, a, volunteer)

	made(t, a, cookie, "pageant-2026", "Christmas pageant")

	w := a.post(t, "/forms/pageant-2026/people", url.Values{
		"email": {"counter@schoenstatt.test"},
		"role":  {"results"},
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("giving results on a draft = %d, want 200:\n%s", w.Code, w.Body)
	}

	m, sent := a.sent.Last()
	if !sent {
		t.Fatal("no invitation was sent")
	}

	for _, want := range []string{"not on the web yet", "Once it is, you can read its submissions", "next=%2Fforms\r\n"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("the invitation to read a draft does not say %q:\n%s", want, m.Text)
		}
	}
}

// Deleting a draft takes away everybody's access to it, so that nobody who
// helped with this one is handed the next form somebody makes by that name.
func TestDeletingADraftTakesAwayAccessToIt(t *testing.T) {
	a := newAdmin(t, "")

	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	cookie := sessionFor(t, a, volunteer)

	made(t, a, cookie, "pageant-2026", "Christmas pageant")

	if w := a.post(t, "/forms/pageant-2026/people", url.Values{
		"email": {"helper@schoenstatt.test"},
		"role":  {"admin"},
	}, cookie); w.Code != http.StatusOK {
		t.Fatalf("sharing the draft = %d, want 200:\n%s", w.Code, w.Body)
	}

	helper, _ := accountFor(t, a, "helper@schoenstatt.test")

	if w := a.post(t, "/forms/pageant-2026/edit/delete", url.Values{}, cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("deleting the draft = %d, want 303:\n%s", w.Code, w.Body)
	}

	for _, u := range []struct {
		who  string
		role accessbus.Role
	}{
		{"the helper", roleOnForm(t, a, helper, "pageant-2026")},
		{"the creator", roleOnForm(t, a, volunteer, "pageant-2026")},
	} {
		if u.role != "" {
			t.Errorf("after the draft was deleted %s still holds %q on its name", u.who, u.role)
		}
	}
}

// And a grant left on a name from before deleting cleared them -- or from
// any other way a row outlived its form -- is cleared when the name is used
// again, which is the line that actually keeps the next form private.
func TestANewFormInheritsNobodysAccessFromAnOldOneOfTheSameName(t *testing.T) {
	a := newAdmin(t, "")

	leftover := reader(t, a, "leftover@schoenstatt.test")

	if _, err := a.access.Grant(t.Context(), time.Now(), types.ID{}, leftover.ID, mustSlug(t, "pageant-2026"), accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	made(t, a, sessionFor(t, a, volunteer), "pageant-2026", "Christmas pageant")

	if got := roleOnForm(t, a, leftover, "pageant-2026"); got != "" {
		t.Errorf("a new form kept a grant somebody held on its name before it existed: %q", got)
	}

	if got := roleOnForm(t, a, volunteer, "pageant-2026"); got != accessbus.RoleAdmin {
		t.Errorf("clearing the old grants took the creator's own: %q", got)
	}
}

package muxer_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// The will-call table, through the mounted admin surface.
//
// Most of this file is the gate, because the whole reason a third role exists
// is that neither of the two either side of it was the right authority for two
// volunteers working a table on their phones.

const willCallPage = "/forms/" + theForm + "/will-call"

// volunteer holds door on the one form: may read its submissions and mark one
// collected, and nothing else.
func volunteer(t *testing.T, a harness, address string) userbus.User {
	t.Helper()

	u, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, address),
		Name:  "A Volunteer",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := a.access.Grant(t.Context(), time.Now(), types.ID{}, u.ID, mustSlug(t, theForm), accessbus.RoleDoor); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	return u
}

// order seeds one submission on the feast form at a given status, the way the
// embed surface would have written it.
func order(t *testing.T, a harness, name string, qty int, status submissionbus.Status) submissionbus.Submission {
	t.Helper()

	slug := mustSlug(t, theForm)

	f, err := a.catalogue.ByID(slug)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	answers := formbus.Answers{
		FormID:   slug,
		Version:  f.Version,
		Currency: "usd",
		Fields: []formbus.Answer{
			{Name: "name", Label: "Your name", Kind: formbus.KindText, Values: []string{name}},
		},
		Lines: []formbus.Line{
			{ItemID: "ticket", Label: "Lunch ticket", Price: types.Money(1200), Qty: qty, Amount: types.Money(1200 * int64(qty))},
		},
		Total: types.Money(1200 * int64(qty)),
	}

	// A distinct nonce per order, because the grant is single-use and that is
	// enforced in the same transaction as the insert.
	g := formbus.Grant{Form: slug, Version: f.Version, Nonce: "nonce-" + name}

	sub, err := a.orders.Accept(t.Context(), time.Now(), g, submissionbus.New{Answers: answers})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	if status != sub.Status {
		if err := a.orders.Settle(t.Context(), time.Now(), sub.ID, status, "pi_test"); err != nil {
			t.Fatalf("Settle: %v", err)
		}

		sub.Status = status
	}

	return sub
}

// The gate. results cannot reach the table, door can, and an administrator can
// because admin includes door.
func TestOnlyTheDoorRoleReachesTheWillCallTable(t *testing.T) {
	// A harness per case rather than three sessions on one, because signing in
	// is throttled -- authapp.DefaultSignInRate allows a burst of five
	// requests from one address and each sign-in costs two. Three roles on one
	// harness is six, and the test would be asserting the rate limiter.
	cases := []struct {
		role string
		who  func(*testing.T, harness, string) userbus.User
		want int
	}{
		{"results", reader, http.StatusForbidden},
		{"door", volunteer, http.StatusOK},
		{"admin", formAdmin, http.StatusOK},
	}

	for _, c := range cases {
		t.Run(c.role, func(t *testing.T) {
			a := newAdmin(t, "")
			u := c.who(t, a, "somebody@schoenstatt.test")

			if w := a.get(t, willCallPage, sessionFor(t, a, u)); w.Code != c.want {
				t.Errorf("a %s holder GET %s = %d, want %d:\n%s",
					c.role, willCallPage, w.Code, c.want, w.Body)
			}
		})
	}

	// Signed out it is the sign-in page rather than a refusal, because Require
	// comes before the role gate and the two are different answers.
	t.Run("signed out", func(t *testing.T) {
		a := newAdmin(t, "")

		w := a.get(t, willCallPage, "")
		if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/signin") {
			t.Errorf("signed out GET %s = %d to %q, want a redirect to /signin",
				willCallPage, w.Code, w.Header().Get("Location"))
		}
	})
}

// The other half of the role's reason for existing: somebody who works the
// table cannot edit the form or grant access.
func TestTheDoorRoleCannotEditTheFormOrGiveItAway(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, volunteer(t, a, "volunteer@schoenstatt.test"))

	for _, path := range []string{
		"/forms/" + theForm + "/edit",
		"/forms/" + theForm + "/people",
		"/build",
	} {
		if w := a.get(t, path, cookie); w.Code != http.StatusForbidden {
			t.Errorf("a door holder GET %s = %d, want 403:\n%s", path, w.Code, w.Body)
		}
	}
}

// Only paid orders are on the list. A pending one is somebody who closed the
// checkout page, and handing them a plate is the mistake the list prevents.
func TestOnlyPaidOrdersAreOnTheList(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, volunteer(t, a, "volunteer@schoenstatt.test"))

	paid := order(t, a, "Maria", 2, submissionbus.StatusPaid)
	pending := order(t, a, "Tomas", 1, submissionbus.StatusPending)

	w := a.get(t, willCallPage, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d:\n%s", willCallPage, w.Code, w.Body)
	}

	body := w.Body.String()

	if !strings.Contains(body, "Maria") {
		t.Errorf("the paid order is not on the list:\n%s", body)
	}

	if strings.Contains(body, "Tomas") {
		t.Errorf("an unpaid order is on the list:\n%s", body)
	}

	// And the unpaid one cannot be collected even by its id, which is the rule
	// rather than the rendering.
	w = a.post(t, willCallPage+"/"+pending.ID.String()+"/collect", url.Values{}, cookie)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("collecting a pending order = %d, want a redirect:\n%s", w.Code, w.Body)
	}

	if to := w.Header().Get("Location"); !strings.Contains(to, "problem=") {
		t.Errorf("collecting a pending order redirected to %q with no sentence", to)
	}

	handed, err := a.orders.Collected(t.Context(), mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("Collected: %v", err)
	}

	if _, wrong := handed[pending.ID]; wrong {
		t.Error("an unpaid order was recorded as collected")
	}

	if _, missing := handed[paid.ID]; missing {
		t.Error("nothing was collected yet and the paid order is marked")
	}
}

// Handing over, and the number the volunteer reads out.
func TestHandingOverMarksTheOrderAndSaysHowManyTokens(t *testing.T) {
	a := newAdmin(t, "")

	hand := volunteer(t, a, "volunteer@schoenstatt.test")
	cookie := sessionFor(t, a, hand)

	sub := order(t, a, "Maria", 3, submissionbus.StatusPaid)

	w := a.post(t, willCallPage+"/"+sub.ID.String()+"/collect", url.Values{}, cookie)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("handing over = %d, want a redirect:\n%s", w.Code, w.Body)
	}

	to := w.Header().Get("Location")
	if !strings.Contains(to, "3+tokens") {
		t.Errorf("redirected to %q, want it to say how many tokens to count out", to)
	}

	handed, err := a.orders.Collected(t.Context(), mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("Collected: %v", err)
	}

	c, ok := handed[sub.ID]
	if !ok {
		t.Fatal("the order was not recorded as collected")
	}

	if c.CollectedBy != hand.ID {
		t.Errorf("recorded against %s, want the volunteer %s", c.CollectedBy, hand.ID)
	}

	// And the page now says who did it, which is the reason the row exists
	// rather than a flag.
	w = a.get(t, willCallPage, cookie)
	if !strings.Contains(w.Body.String(), "handed over by A Volunteer") {
		t.Errorf("the list does not say who handed it over:\n%s", w.Body)
	}
}

// Two volunteers, one order. The second is told who got there first rather
// than shown a failure, and nothing is overwritten.
func TestTheSecondVolunteerIsToldWhoGotThereFirst(t *testing.T) {
	a := newAdmin(t, "")

	first := volunteer(t, a, "first@schoenstatt.test")
	second := volunteer(t, a, "second@schoenstatt.test")

	sub := order(t, a, "Maria", 1, submissionbus.StatusPaid)
	path := willCallPage + "/" + sub.ID.String() + "/collect"

	if w := a.post(t, path, url.Values{}, sessionFor(t, a, first)); w.Code != http.StatusSeeOther {
		t.Fatalf("the first hand-over = %d:\n%s", w.Code, w.Body)
	}

	w := a.post(t, path, url.Values{}, sessionFor(t, a, second))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("the second hand-over = %d, want a redirect:\n%s", w.Code, w.Body)
	}

	to, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parsing %q: %v", w.Header().Get("Location"), err)
	}

	said := to.Query().Get("problem")
	if !strings.Contains(said, "Already handed over") {
		t.Errorf("the second volunteer was told %q", said)
	}

	handed, err := a.orders.Collected(t.Context(), mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("Collected: %v", err)
	}

	if handed[sub.ID].CollectedBy != first.ID {
		t.Errorf("the second hand-over overwrote the first")
	}
}

// The mis-tap, which is certain at a table at eight in the morning. Undoing is
// one button, and it is not restricted to whoever marked it.
func TestEitherVolunteerCanUndoAMistap(t *testing.T) {
	a := newAdmin(t, "")

	first := volunteer(t, a, "first@schoenstatt.test")
	second := volunteer(t, a, "second@schoenstatt.test")

	sub := order(t, a, "Maria", 1, submissionbus.StatusPaid)

	if w := a.post(t, willCallPage+"/"+sub.ID.String()+"/collect", url.Values{}, sessionFor(t, a, first)); w.Code != http.StatusSeeOther {
		t.Fatalf("handing over = %d:\n%s", w.Code, w.Body)
	}

	w := a.post(t, willCallPage+"/"+sub.ID.String()+"/undo", url.Values{}, sessionFor(t, a, second))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("undoing somebody else's mark = %d, want a redirect:\n%s", w.Code, w.Body)
	}

	handed, err := a.orders.Collected(t.Context(), mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("Collected: %v", err)
	}

	if _, still := handed[sub.ID]; still {
		t.Error("the mark is still there after undoing it")
	}
}

// The gate authorises the account against the slug in the path, and the id is
// a separate wildcard nothing has checked. Somebody who works one form's table
// must not reach an order on a form they hold nothing on by editing the URL.
func TestAnOrderOnAnotherFormCannotBeCollected(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	// A second form, built and published, with an order of its own.
	made(t, a, cookie, "supper-2026", "Parish supper")

	if w := a.post(t, "/forms/supper-2026/edit/fields", url.Values{
		"name": {"who"}, "label": {"Your name"}, "kind": {"text"},
	}, cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("adding a field = %d:\n%s", w.Code, w.Body)
	}

	if w := a.post(t, "/forms/supper-2026/edit/state", url.Values{"state": {"live"}}, cookie); w.Code != http.StatusOK {
		t.Fatalf("publishing = %d:\n%s", w.Code, w.Body)
	}

	elsewhere := order(t, a, "Maria", 1, submissionbus.StatusPaid)

	// The volunteer holds door on the feast form and nothing at all on the
	// other one, and reaches for the feast form's table with the other form's
	// order in the path.
	hand := volunteer(t, a, "volunteer@schoenstatt.test")
	hands := sessionFor(t, a, hand)

	w := a.post(t, "/forms/supper-2026/will-call/"+elsewhere.ID.String()+"/collect",
		url.Values{}, hands)
	if w.Code != http.StatusForbidden {
		t.Errorf("reaching another form's table = %d, want 403:\n%s", w.Code, w.Body)
	}

	// And the order really is on the feast form, so the refusal above was the
	// gate rather than the order not existing.
	if elsewhere.Form != mustSlug(t, theForm) {
		t.Fatalf("the seeded order is on %s", elsewhere.Form)
	}

	// The same id, offered to the feast form's own table by somebody who holds
	// it, is collectable -- which is what makes the refusal above about the
	// form in the path and not about the order.
	if w := a.post(t, willCallPage+"/"+elsewhere.ID.String()+"/collect",
		url.Values{}, hands); w.Code != http.StatusSeeOther {
		t.Errorf("the same order on its own form = %d, want a redirect:\n%s", w.Code, w.Body)
	}
}

// The page reloads itself, because a list rendered when the table opened is
// wrong by the time the queue forms. No script, since this surface has no
// script-src at all.
func TestTheListRefreshesItselfAndKeepsTheSearch(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, volunteer(t, a, "volunteer@schoenstatt.test"))

	order(t, a, "Maria", 1, submissionbus.StatusPaid)
	order(t, a, "Tomas", 2, submissionbus.StatusPaid)

	w := a.get(t, willCallPage, cookie)
	body := w.Body.String()

	if !strings.Contains(body, `http-equiv="refresh"`) {
		t.Errorf("the page does not refresh itself:\n%s", body)
	}

	if strings.Contains(body, "<script") {
		t.Errorf("the page loaded a script, which this surface's CSP forbids:\n%s", body)
	}

	// The search filters, and the refresh keeps it -- a filter silently
	// cleared every ten seconds is worse than no filter.
	w = a.get(t, willCallPage+"?q=mar", cookie)
	body = w.Body.String()

	if !strings.Contains(body, "Maria") || strings.Contains(body, "Tomas") {
		t.Errorf("the search did not filter the list:\n%s", body)
	}

	if !strings.Contains(body, "q=mar") {
		t.Errorf("the refresh does not carry the search:\n%s", body)
	}

	// And it can be paused, which is the only way to type without being
	// interrupted.
	w = a.get(t, willCallPage+"?live=off", cookie)
	if strings.Contains(w.Body.String(), `http-equiv="refresh"`) {
		t.Errorf("the page still refreshes itself when paused:\n%s", w.Body)
	}
}

// The three numbers at the top describe the morning rather than the search
// box, because they are what somebody reports to the kitchen.
func TestTheHeadlineNumbersCountTheWholeMorning(t *testing.T) {
	a := newAdmin(t, "")

	hand := volunteer(t, a, "volunteer@schoenstatt.test")
	cookie := sessionFor(t, a, hand)

	maria := order(t, a, "Maria", 3, submissionbus.StatusPaid)
	order(t, a, "Tomas", 2, submissionbus.StatusPaid)

	if w := a.post(t, willCallPage+"/"+maria.ID.String()+"/collect", url.Values{}, cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("handing over = %d:\n%s", w.Code, w.Body)
	}

	// Filtered to one name, and the counts still describe both orders.
	w := a.get(t, willCallPage+"?q=tomas", cookie)
	body := w.Body.String()

	for _, want := range []string{
		"<strong>1</strong> still to come",
		"<strong>2</strong> tokens to hand out",
		"<strong>1</strong> collected",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the headline numbers do not say %q:\n%s", want, body)
		}
	}
}

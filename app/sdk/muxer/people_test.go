package muxer_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// Giving somebody access to a form, through the mounted admin surface.
//
// The thing most worth testing here is the gate: this page can create an
// account that reads other people's names, addresses and what they paid, so
// every test below is either about who may reach it or about what it refuses
// to do for somebody who may.

const peoplePage = "/forms/" + theForm + "/people"

// formAdmin is somebody who administers the one form, which is what this page
// requires -- as distinct from reader(), who holds results and must not see it.
func formAdmin(t *testing.T, a harness, address string) userbus.User {
	t.Helper()

	u, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, address),
		Name:  "An Administrator",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := a.access.Grant(t.Context(), time.Now(), types.ID{}, u.ID, mustSlug(t, theForm), accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	return u
}

// roleOn reports what an account holds on the form, or "" for nothing.
func roleOn(t *testing.T, a harness, u userbus.User) accessbus.Role {
	t.Helper()

	grants, err := a.access.ForUser(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}

	for _, g := range grants {
		if g.Form == mustSlug(t, theForm) {
			return g.Role
		}
	}

	return ""
}

func accountFor(t *testing.T, a harness, address string) (userbus.User, bool) {
	t.Helper()

	u, err := a.users.ByEmail(t.Context(), mustEmail(t, address))
	if err != nil {
		return userbus.User{}, false
	}

	return u, true
}

// The gate, which is the whole reason this page is a separate mount from the
// submissions it sits beside. Reading the numbers is not deciding who else
// may read them.
func TestOnlyAnAdminOnTheFormReachesThePeoplePage(t *testing.T) {
	a := newAdmin(t, "")

	counter := reader(t, a, "counter@schoenstatt.test")
	boss := formAdmin(t, a, "boss@schoenstatt.test")

	if w := a.get(t, peoplePage, sessionFor(t, a, counter)); w.Code != http.StatusForbidden {
		t.Errorf("a results holder GET %s = %d, want 403:\n%s", peoplePage, w.Code, w.Body)
	}

	if w := a.get(t, peoplePage, sessionFor(t, a, boss)); w.Code != http.StatusOK {
		t.Errorf("an admin GET %s = %d, want 200:\n%s", peoplePage, w.Code, w.Body)
	}

	// And signed out, it is the sign-in page rather than a refusal, because
	// the reader may simply have let a session expire.
	w := a.get(t, peoplePage, "")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("signed out GET %s = %d, want 303", peoplePage, w.Code)
	}
	if to := w.Header().Get("Location"); !strings.HasPrefix(to, "/signin") {
		t.Errorf("signed out, sent to %q, want the sign-in page", to)
	}
}

// A results holder may not add anybody either. The POST is behind the same
// gate as the GET, which is worth asserting separately: a refusal on the page
// and an open door on the form it posts to is the shape this mistake takes.
func TestAResultsHolderCannotGiveAnybodyAccess(t *testing.T) {
	a := newAdmin(t, "")

	counter := reader(t, a, "counter@schoenstatt.test")

	w := a.post(t, peoplePage, url.Values{
		"email": {"stranger@schoenstatt.test"},
		"role":  {"admin"},
	}, sessionFor(t, a, counter))

	if w.Code != http.StatusForbidden {
		t.Fatalf("POST %s as a results holder = %d, want 403:\n%s", peoplePage, w.Code, w.Body)
	}

	if _, ok := accountFor(t, a, "stranger@schoenstatt.test"); ok {
		t.Error("the refused request created an account anyway")
	}
}

// The ordinary case, end to end: an address nobody has used before becomes an
// account, a grant, and a message telling the person where to sign in.
func TestGivingAccessToANewAddressMakesAnAccountAndTellsThem(t *testing.T) {
	a := newAdmin(t, "")
	boss := formAdmin(t, a, "boss@schoenstatt.test")

	w := a.post(t, peoplePage, url.Values{
		"email": {"kitchen@schoenstatt.test"},
		"name":  {"The Kitchen"},
		"role":  {"results"},
	}, sessionFor(t, a, boss))

	if w.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200:\n%s", peoplePage, w.Code, w.Body)
	}

	u, ok := accountFor(t, a, "kitchen@schoenstatt.test")
	if !ok {
		t.Fatal("no account was created for the address")
	}

	if got := roleOn(t, a, u); got != accessbus.RoleResults {
		t.Errorf("role on the form = %q, want results", got)
	}

	// The invitation names the sign-in page and carries no credential. A
	// fifteen-minute token in a message read that evening is a dead link, so
	// the person asks for their own.
	m, sent := a.sent.Last()
	switch {
	case !sent:
		t.Fatal("nothing was emailed to the person who was given access")
	case m.To != "kitchen@schoenstatt.test":
		t.Errorf("the invitation went to %q", m.To)
	case !strings.Contains(m.Text, "https://forms.test/signin?email=kitchen%40schoenstatt.test"):
		// The address travels in the link so the field arrives filled in.
		// Most people have several, and picking the wrong one on the sign-in
		// page fails silently -- it says to check your email whichever
		// address is typed, because saying anything else would say which
		// addresses have accounts.
		t.Errorf("the invitation does not carry the address to sign in with:\n%s", m.Text)
	case strings.Contains(m.Text, "/signin/link?t="):
		t.Errorf("the invitation carries a sign-in token, which expires long before it is read:\n%s", m.Text)
	}

	// And the page that answered says who, so nobody has to reload to find
	// out whether it worked.
	if body := w.Body.String(); !strings.Contains(body, "kitchen@schoenstatt.test") {
		t.Errorf("the page does not mention the person who was added:\n%s", body)
	}
}

// Somebody who already has an account keeps it. The second grant changes the
// role rather than making a second person.
func TestGivingAccessToAnExistingAccountChangesTheRole(t *testing.T) {
	a := newAdmin(t, "")
	boss := formAdmin(t, a, "boss@schoenstatt.test")
	cookie := sessionFor(t, a, boss)

	counter := reader(t, a, "counter@schoenstatt.test")

	w := a.post(t, peoplePage, url.Values{
		"email": {"counter@schoenstatt.test"},
		"role":  {"admin"},
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200:\n%s", peoplePage, w.Code, w.Body)
	}

	if got := roleOn(t, a, counter); got != accessbus.RoleAdmin {
		t.Errorf("role on the form = %q, want admin", got)
	}

	again, ok := accountFor(t, a, "counter@schoenstatt.test")
	if !ok {
		t.Fatal("the account is gone")
	}
	if again.ID != counter.ID {
		t.Error("granting a role to an existing address made a second account")
	}

	// The name they were created with is left alone. Quietly renaming
	// somebody because a colleague guessed while granting them a role is a
	// surprise nobody asked for.
	if again.Name != counter.Name {
		t.Errorf("the account was renamed to %q, want %q", again.Name, counter.Name)
	}
}

func TestAMistypedAddressComesBackOnThePage(t *testing.T) {
	a := newAdmin(t, "")
	boss := formAdmin(t, a, "boss@schoenstatt.test")

	w := a.post(t, peoplePage, url.Values{
		"email": {"kitchen at schoenstatt"},
		"name":  {"The Kitchen"},
		"role":  {"results"},
	}, sessionFor(t, a, boss))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST %s with a bad address = %d, want 400", peoplePage, w.Code)
	}

	// Filled in again, so that a typo in one field does not cost the other
	// two.
	if body := w.Body.String(); !strings.Contains(body, "kitchen at schoenstatt") || !strings.Contains(body, "The Kitchen") {
		t.Errorf("the refused entry did not come back on the page:\n%s", body)
	}
}

func TestRemovingSomebodyTakesTheGrantAway(t *testing.T) {
	a := newAdmin(t, "")
	boss := formAdmin(t, a, "boss@schoenstatt.test")
	counter := reader(t, a, "counter@schoenstatt.test")

	w := a.post(t, peoplePage+"/revoke", url.Values{
		"user": {counter.ID.String()},
	}, sessionFor(t, a, boss))

	if w.Code != http.StatusOK {
		t.Fatalf("revoking = %d, want 200:\n%s", w.Code, w.Body)
	}

	if got := roleOn(t, a, counter); got != "" {
		t.Errorf("role on the form = %q, want none", got)
	}

	// The account survives, because it may hold other forms and because
	// deleting a person to remove them from one lunch is not the same act.
	if _, ok := accountFor(t, a, "counter@schoenstatt.test"); !ok {
		t.Error("removing somebody from a form deleted their account")
	}
}

// Removing your own access locks you out of the page you are standing on, and
// the way back is another administrator.
func TestYouCannotRemoveYourself(t *testing.T) {
	a := newAdmin(t, "")
	boss := formAdmin(t, a, "boss@schoenstatt.test")

	w := a.post(t, peoplePage+"/revoke", url.Values{
		"user": {boss.ID.String()},
	}, sessionFor(t, a, boss))

	if w.Code != http.StatusConflict {
		t.Fatalf("removing yourself = %d, want 409:\n%s", w.Code, w.Body)
	}

	if got := roleOn(t, a, boss); got != accessbus.RoleAdmin {
		t.Errorf("role on the form = %q, want admin still", got)
	}
}

// A site-wide grant is not this form's to take away. The page shows no button
// for one, and the route cannot remove one however it is called: it only ever
// revokes a grant on the form named in the path.
func TestASiteWideGrantSurvivesThisPage(t *testing.T) {
	a := newAdmin(t, bootstrapSecret)

	// The founding account, whose only grant is site-wide -- which is also
	// what lets it reach this page for a form it was never named on.
	w := a.post(t, "/signin/bootstrap", url.Values{
		"secret": {bootstrapSecret},
		"email":  {"founder@schoenstatt.test"},
	}, "")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("bootstrap = %d, want 303:\n%s", w.Code, w.Body)
	}

	cookie := sessionCookie(t, w)
	if cookie == "" {
		t.Fatal("the bootstrap set no session")
	}

	founder, ok := accountFor(t, a, "founder@schoenstatt.test")
	if !ok {
		t.Fatal("the bootstrap made no account")
	}

	listing := a.get(t, peoplePage, cookie)
	if listing.Code != http.StatusOK {
		t.Fatalf("a site-wide admin GET %s = %d, want 200:\n%s", peoplePage, listing.Code, listing.Body)
	}

	// Listed, because they really can read this form and really are emailed
	// about it -- and marked, because this page cannot change it.
	if body := listing.Body.String(); !strings.Contains(body, "site-wide") {
		t.Errorf("the site-wide grant is not marked as such:\n%s", body)
	}

	if w := a.post(t, peoplePage+"/revoke", url.Values{"user": {founder.ID.String()}}, cookie); w.Code != http.StatusConflict {
		// It is their own row, so this is refused as self-removal first.
		t.Fatalf("revoking your own site-wide grant = %d, want 409", w.Code)
	}

	// And from somebody else's session, the site-wide grant is still there
	// afterwards: revoke names the form in the path, never the zero slug.
	boss := formAdmin(t, a, "boss@schoenstatt.test")

	if w := a.post(t, peoplePage+"/revoke", url.Values{"user": {founder.ID.String()}}, sessionFor(t, a, boss)); w.Code != http.StatusOK {
		t.Fatalf("revoking somebody else = %d, want 200:\n%s", w.Code, w.Body)
	}

	allowed, err := a.access.Allowed(t.Context(), founder.ID, mustSlug(t, theForm), accessbus.RoleAdmin)
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if !allowed {
		t.Error("the site-wide grant was removed by a page about one form")
	}
}

// The column that ties this page to the notification work: everybody listed
// here is emailed about submissions unless they have said otherwise, and the
// page is where "why am I the only one getting these" is answered.
func TestThePageSaysWhoIsBeingEmailed(t *testing.T) {
	a := newAdmin(t, "")
	boss := formAdmin(t, a, "boss@schoenstatt.test")
	counter := reader(t, a, "counter@schoenstatt.test")

	cookie := sessionFor(t, a, boss)

	if w := a.get(t, peoplePage, cookie); !strings.Contains(w.Body.String(), "Emailed") {
		t.Fatalf("the page has no email column:\n%s", w.Body)
	}

	if err := a.notify.SetMuted(t.Context(), time.Now(), counter.ID, mustSlug(t, theForm), true); err != nil {
		t.Fatalf("SetMuted: %v", err)
	}

	// Read back through the page rather than through the store, because the
	// column is the thing under test: one query for the whole listing, not one
	// per row.
	body := a.get(t, peoplePage, cookie).Body.String()

	row := rowFor(t, body, "counter@schoenstatt.test")
	if !strings.Contains(row, ">no<") {
		t.Errorf("the muted account is not shown as such:\n%s", row)
	}

	row = rowFor(t, body, "boss@schoenstatt.test")
	if !strings.Contains(row, ">yes<") {
		t.Errorf("the account that is emailed is not shown as such:\n%s", row)
	}
}

// rowFor is the one table row mentioning an address. Crude on purpose: the
// alternative is parsing the HTML, and what these tests are about is what the
// page says rather than how it is built.
func rowFor(t *testing.T, body, address string) string {
	t.Helper()

	for row := range strings.SplitSeq(body, "<tr>") {
		if strings.Contains(row, address) {
			return row
		}
	}

	t.Fatalf("no row for %s in:\n%s", address, body)

	return ""
}

// Changing a role after setting it, which is issue #29.
//
// It was always possible and never findable: the form below the table is an
// upsert, so re-typing an address with a different role worked and nothing
// said so. These assert both the control that now says so and the refusal that
// was missing beside it.

func TestARoleCanBeChangedFromThePersonsOwnRow(t *testing.T) {
	a := newAdmin(t, "")

	boss := formAdmin(t, a, "boss@schoenstatt.test")
	cookie := sessionFor(t, a, boss)

	// Somebody added as results, in the ordinary way.
	if w := a.post(t, peoplePage, url.Values{
		"email": {"helper@schoenstatt.test"},
		"name":  {"A Helper"},
		"role":  {"results"},
	}, cookie); w.Code != http.StatusOK {
		t.Fatalf("adding = %d:\n%s", w.Code, w.Body)
	}

	helper, ok := accountFor(t, a, "helper@schoenstatt.test")
	if !ok {
		t.Fatal("the helper has no account")
	}

	// The row carries a control naming them, which is the discoverability the
	// issue was about.
	page := a.get(t, peoplePage, cookie)
	if !strings.Contains(page.Body.String(), `name="user" value="`+helper.ID.String()+`"`) {
		t.Errorf("the person's row has no role control:\n%s", page.Body)
	}

	// Up, and then down again.
	for _, want := range []accessbus.Role{accessbus.RoleDoor, accessbus.RoleAdmin, accessbus.RoleResults} {
		w := a.post(t, peoplePage+"/role", url.Values{
			"user": {helper.ID.String()},
			"role": {want.String()},
		}, cookie)
		if w.Code != http.StatusOK {
			t.Fatalf("changing to %s = %d:\n%s", want, w.Code, w.Body)
		}

		if got := roleOn(t, a, helper); got != want {
			t.Errorf("after asking for %s they hold %s", want, got)
		}
	}
}

// The refusal that was missing: an administrator taking their own
// administration away and losing the page they are standing on. revoke has
// always refused it; the form below the table did not, and three roles made
// picking the middle one for yourself look like a smaller act than it is.
func TestAnAdminCannotDemoteThemselves(t *testing.T) {
	a := newAdmin(t, "")

	boss := formAdmin(t, a, "boss@schoenstatt.test")
	cookie := sessionFor(t, a, boss)

	// Through the add form, which is the path that had no guard.
	w := a.post(t, peoplePage, url.Values{
		"email": {boss.Email.String()},
		"name":  {"The Boss"},
		"role":  {"results"},
	}, cookie)
	if w.Code != http.StatusConflict {
		t.Errorf("demoting yourself by address = %d, want 409:\n%s", w.Code, w.Body)
	}

	// And through the new row control, which has no option for your own row
	// and refuses one anyway.
	w = a.post(t, peoplePage+"/role", url.Values{
		"user": {boss.ID.String()},
		"role": {"results"},
	}, cookie)
	if w.Code != http.StatusConflict {
		t.Errorf("demoting yourself from the row = %d, want 409:\n%s", w.Code, w.Body)
	}

	if got := roleOn(t, a, boss); got != accessbus.RoleAdmin {
		t.Errorf("they now hold %s, want admin", got)
	}

	// The page is still theirs, which is the thing the guard protects.
	if page := a.get(t, peoplePage, cookie); page.Code != http.StatusOK {
		t.Errorf("the people page after the refusals = %d, want 200", page.Code)
	}

	// Their own row offers no control, so the refusal is a second line of
	// defence rather than the only one.
	page := a.get(t, peoplePage, cookie)
	if strings.Contains(page.Body.String(), `name="user" value="`+boss.ID.String()+`"`) {
		t.Errorf("the reader's own row carries a role control:\n%s", page.Body)
	}
}

// The route changes a grant and cannot create one. Granting somebody new means
// typing their address, which is what makes an account and sends an
// invitation.
func TestTheRoleRouteCannotGrantSomebodyNew(t *testing.T) {
	a := newAdmin(t, "")

	boss := formAdmin(t, a, "boss@schoenstatt.test")
	cookie := sessionFor(t, a, boss)

	stranger, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, "stranger@schoenstatt.test"),
		Name:  "A Stranger",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	w := a.post(t, peoplePage+"/role", url.Values{
		"user": {stranger.ID.String()},
		"role": {"admin"},
	}, cookie)
	if w.Code != http.StatusConflict {
		t.Errorf("granting a stranger from the role route = %d, want 409:\n%s", w.Code, w.Body)
	}

	if got := roleOn(t, a, stranger); got != "" {
		t.Errorf("the stranger now holds %s", got)
	}
}

// A site-wide grant is not this form's to change, which is why those rows have
// no control -- the same reason they have no remove button.
func TestASiteWideGrantCannotBeChangedFromAFormsPage(t *testing.T) {
	a := newAdmin(t, "")

	boss := formAdmin(t, a, "boss@schoenstatt.test")
	cookie := sessionFor(t, a, boss)

	whole, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, "site@schoenstatt.test"),
		Name:  "The Administrator",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := a.access.Grant(t.Context(), time.Now(), types.ID{}, whole.ID, types.Slug{}, accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	w := a.post(t, peoplePage+"/role", url.Values{
		"user": {whole.ID.String()},
		"role": {"results"},
	}, cookie)
	if w.Code != http.StatusConflict {
		t.Errorf("changing a site-wide grant = %d, want 409:\n%s", w.Code, w.Body)
	}

	// And they are listed, with no control on the row.
	page := a.get(t, peoplePage, cookie)
	body := page.Body.String()

	if !strings.Contains(body, "site-wide") {
		t.Errorf("the site-wide holder is not listed:\n%s", body)
	}

	if strings.Contains(body, `name="user" value="`+whole.ID.String()+`"`) {
		t.Errorf("a site-wide row carries a role control:\n%s", body)
	}
}

// A site-wide creator grant is not access to this form, and until this test
// the page said it was.
//
// Every site-wide grant used to be accessbus.RoleAdmin, which really does
// reach every form, so listing them all under "Who can see this" was true.
// RoleCreator made it false: it may start a form and reach none, including
// this one, and notifybus never writes to it -- so the row was wrong twice
// over, once in the heading above it and once in the Emailed column beside it.
func TestASiteWideCreatorIsNotListedOnAFormsPeoplePage(t *testing.T) {
	a := newAdmin(t, "")
	boss := formAdmin(t, a, "boss@schoenstatt.test")

	maker, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, "maker@schoenstatt.test"),
		Name:  "A Maker",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := a.access.Grant(t.Context(), time.Now(), types.ID{}, maker.ID, types.Slug{}, accessbus.RoleCreator); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	w := a.get(t, peoplePage, sessionFor(t, a, boss))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200:\n%s", peoplePage, w.Code, w.Body)
	}

	// The table only. Below it is the dropdown of people who can be given
	// access, where this account belongs and is asserted for separately.
	table, _, ok := strings.Cut(w.Body.String(), "</table>")
	if !ok {
		t.Fatalf("the page has no table of people:\n%s", w.Body)
	}

	if strings.Contains(table, "maker@schoenstatt.test") {
		t.Errorf("a site-wide creator is listed as somebody who can see this form:\n%s", table)
	}

	// The site-wide administrator two lines up is still listed, because that
	// grant really does reach this form. Without this the test above passes
	// by listing nobody.
	if !strings.Contains(table, "boss@schoenstatt.test") {
		t.Errorf("the form's own administrator is not listed:\n%s", table)
	}
}

// Giving access to somebody who already has an account, without retyping
// their address.
//
// The address field is what this page had, and for the common case -- the
// dozen people in an office who keep being added to one another's forms -- it
// is a chance to mistype an address that is already in the database. A typo
// there does not fail: it makes a second account and mails an invitation
// nobody reads.
func TestSomebodyWithAnAccountCanBePickedRatherThanTyped(t *testing.T) {
	a := newAdmin(t, "")
	boss := formAdmin(t, a, "boss@schoenstatt.test")

	helper, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, "helper@schoenstatt.test"),
		Name:  "A Helper",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// One session, reused: signing in is rate limited, and three sign-ins for
	// the same account inside one test is the limit doing its job.
	cookie := sessionFor(t, a, boss)

	listing := a.get(t, peoplePage, cookie)
	if listing.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200:\n%s", peoplePage, listing.Code, listing.Body)
	}

	if !strings.Contains(listing.Body.String(), helper.ID.String()) {
		t.Errorf("an account with no access to this form is not offered to pick:\n%s", listing.Body)
	}

	w := a.post(t, peoplePage, url.Values{
		"user": {helper.ID.String()},
		"role": {"results"},
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("POST %s picking somebody = %d, want 200:\n%s", peoplePage, w.Code, w.Body)
	}

	if got := roleOn(t, a, helper); got != accessbus.RoleResults {
		t.Errorf("after being picked, %s holds %q on the form, want results", helper.Email, got)
	}

	// And they are no longer offered, because they are on the list above now.
	again := a.get(t, peoplePage, cookie)

	_, below, ok := strings.Cut(again.Body.String(), "</table>")
	if !ok {
		t.Fatalf("the page has no table of people:\n%s", again.Body)
	}

	if strings.Contains(below, helper.ID.String()) {
		t.Errorf("somebody who already has access is still offered to pick:\n%s", below)
	}
}

// Neither picked nor typed is the one new way to submit this form wrong.
func TestGivingAccessToNobodyIsRefused(t *testing.T) {
	a := newAdmin(t, "")
	boss := formAdmin(t, a, "boss@schoenstatt.test")

	w := a.post(t, peoplePage, url.Values{
		"role": {"results"},
	}, sessionFor(t, a, boss))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST %s with nobody named = %d, want 400:\n%s", peoplePage, w.Code, w.Body)
	}
}

package muxer_test

import (
	"encoding/csv"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/app/sdk/muxer"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formtoml"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/forms"
)

// Reading what was submitted, through the admin surface as it is mounted.
//
// Every submission in here arrives the way a real one does -- posted through
// the public surface, with a minted grant, against the real definition -- so
// what these tests read back is what somebody in the parish office will read
// back. Constructing rows directly in the store would test the templates
// against a shape nothing produces.
//
// The pair that matters most is the last two: a results grant is per form, and
// a submission is reachable by identifier. Nothing but an explicit check ties
// the record in the URL to the form in the URL, and without it one grant would
// read the whole service.

// otherForm is a second definition, because the repository ships one and
// "can a grant on this form read that form" has no answer with one form.
const otherForm = "parish-picnic"

// twoForms is the real feast definition plus that second one.
//
// The feast form's bytes come out of the embedded set rather than being
// copied, so these tests keep testing the definition that is actually served
// when its price or its closing time changes.
func twoForms(t *testing.T) *formtoml.Store {
	t.Helper()

	feast, err := fs.ReadFile(forms.FS, theForm+".toml")
	if err != nil {
		t.Fatalf("reading the shipped definition: %v", err)
	}

	other := `id = "` + otherForm + `"
title = "Parish picnic"
currency = "usd"

[[field]]
name = "who"
label = "Your name"
kind = "text"
required = true
`

	store, err := formtoml.Load(fstest.MapFS{
		theForm + ".toml":   {Data: feast},
		otherForm + ".toml": {Data: []byte(other)},
	})
	if err != nil {
		t.Fatalf("loading the two definitions: %v", err)
	}

	return store
}

// office is both surfaces over one database: submissions go in through the
// public one and are read back through the admin one, which is the whole
// journey this step exists to make possible.
type office struct {
	admin http.Handler
	embed http.Handler
	cfg   muxer.Config
}

func newOffice(t *testing.T) office {
	t.Helper()

	cfg := newConfig(t, []types.Origin{mustOrigin(t, "https://schoenstatt-austin.us")}, nil)

	store := twoForms(t)
	cfg.Forms = store
	cfg.Embed.Forms = store
	cfg.Embed.Now = func() time.Time { return beforeTheFeast }

	return office{
		admin: adminOf(t, cfg),
		embed: embedOf(t, cfg),
		cfg:   cfg,
	}
}

// sell posts one submission through the public surface, overriding whatever
// the caller names on top of a plausible order.
func (o office) sell(t *testing.T, extra url.Values) {
	t.Helper()

	w := getPage(t, o.embed, "/f/"+theForm)
	if w.Code != http.StatusOK {
		t.Fatalf("the blank form = %d:\n%s", w.Code, short(w.Body.String()))
	}

	values := filled(grantIn(t, w.Body.String()))
	for k, v := range extra {
		values[k] = v
	}

	if w := postForm(t, o.embed, values); w.Code != http.StatusOK {
		t.Fatalf("the submission = %d:\n%s", w.Code, short(w.Body.String()))
	}
}

// reader creates an account and signs it in, without going through the
// sign-in pages -- those have their own tests in auth_test.go, and repeating
// the journey here would make every assertion below depend on them.
func (o office) reader(t *testing.T, email string) (userbus.User, string) {
	t.Helper()

	u, err := o.cfg.Users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, email),
		Name:  "A Reader",
	})
	if err != nil {
		t.Fatalf("Create(%s): %v", email, err)
	}

	req, err := o.cfg.Users.RequestSignIn(t.Context(), time.Now(), u.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}

	_, cookie, err := o.cfg.Users.SignIn(t.Context(), time.Now(), req.Secret)
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}

	return u, cookie
}

// grant gives an account a role. An empty form means site-wide.
func (o office) grant(t *testing.T, u userbus.User, form string, role accessbus.Role) {
	t.Helper()

	var on types.Slug
	if form != "" {
		on = mustSlug(t, form)
	}

	if _, err := o.cfg.Access.Grant(t.Context(), time.Now(), u.ID, u.ID, on, role); err != nil {
		t.Fatalf("Grant(%s on %q): %v", role, form, err)
	}
}

func (o office) read(t *testing.T, target string, cookie string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, target, nil)
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: mid.CookieName, Value: cookie})
	}

	w := httptest.NewRecorder()
	o.admin.ServeHTTP(w, r)

	return w
}

// idsOf pulls the submission identifiers out of the list page, which doubles
// as an assertion that each row links to its detail view.
func (o office) idsOf(t *testing.T, form string, cookie string) []string {
	t.Helper()

	w := o.read(t, "/forms/"+form+"/submissions", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("the list = %d:\n%s", w.Code, short(w.Body.String()))
	}

	var out []string

	for part := range strings.SplitSeq(w.Body.String(), `/forms/`+form+`/submissions/`) {
		id, _, found := strings.Cut(part, `"`)
		if found && id != "" && !strings.Contains(id, "<") {
			out = append(out, id)
		}
	}

	return out
}

// --- the list ---------------------------------------------------------------

// The page somebody opens to find out how many lunches to cook.
func TestTheSubmissionListShowsWhatWasSubmitted(t *testing.T) {
	o := newOffice(t)

	o.sell(t, nil)
	o.sell(t, url.Values{"name": {"Tomás Ó Braonáin"}, "email": {"tomas@example.org"}})

	u, cookie := o.reader(t, "reader@schoenstatt.us")
	o.grant(t, u, theForm, accessbus.RoleResults)

	w := o.read(t, "/forms/"+theForm+"/submissions", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("the list = %d:\n%s", w.Code, short(w.Body.String()))
	}

	body := w.Body.String()

	// Both submitters, by name, and the address a receipt would go to.
	for _, want := range []string{"Maria O&#39;Neill", "Tom", "maria@example.org", "tomas@example.org"} {
		if !strings.Contains(body, want) {
			t.Errorf("the list does not mention %q:\n%s", want, short(body))
		}
	}

	// Four tickets at twelve dollars, none of them paid for yet. The owed
	// figure is the one that says this is not money in the bank.
	if !strings.Contains(body, "$48.00") {
		t.Errorf("the list does not total the four tickets at $48.00:\n%s", short(body))
	}

	// A name somebody typed is never markup. The apostrophe above arriving
	// escaped is that guarantee, and this is the assertion that fails if a
	// template ever reaches for template.HTML.
	if strings.Contains(body, "Maria O'Neill") {
		t.Error("a submitted name reached the page unescaped")
	}
}

// The detail view, which is where a conditional field that was never asked
// reads differently from one left blank.
func TestTheDetailPageShowsOneSubmission(t *testing.T) {
	o := newOffice(t)

	o.sell(t, url.Values{"notes": {"No shellfish, please"}})

	u, cookie := o.reader(t, "reader@schoenstatt.us")
	o.grant(t, u, theForm, accessbus.RoleResults)

	ids := o.idsOf(t, theForm, cookie)
	if len(ids) != 1 {
		t.Fatalf("the list links to %d submissions, want 1: %v", len(ids), ids)
	}

	w := o.read(t, "/forms/"+theForm+"/submissions/"+ids[0], cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("the detail page = %d:\n%s", w.Code, short(w.Body.String()))
	}

	body := w.Body.String()

	for _, want := range []string{"No shellfish, please", "maria@example.org", "Lunch ticket", "$24.00"} {
		if !strings.Contains(body, want) {
			t.Errorf("the detail page does not show %q:\n%s", want, short(body))
		}
	}

	// A submission identifier that is not a submission is a 404 rather than a
	// 500, because guessing one is the ordinary way to reach this route
	// wrongly.
	if w := o.read(t, "/forms/"+theForm+"/submissions/"+types.NewID().String(), cookie); w.Code != http.StatusNotFound {
		t.Errorf("an unknown submission = %d, want 404", w.Code)
	}
}

// --- the export -------------------------------------------------------------

// The CSV, parsed rather than string-matched: the point of encoding/csv here
// is that a comma somebody typed does not become a column, and only a parser
// can assert that.
func TestTheExportHasAColumnPerFieldAndNoAddresses(t *testing.T) {
	o := newOffice(t)

	o.sell(t, url.Values{"notes": {"Two of us, one vegetarian"}})

	u, cookie := o.reader(t, "reader@schoenstatt.us")
	o.grant(t, u, theForm, accessbus.RoleResults)

	w := o.read(t, "/forms/"+theForm+"/submissions.csv", cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("the export = %d:\n%s", w.Code, short(w.Body.String()))
	}

	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv", got)
	}

	// An attachment, and named after the form rather than after the route --
	// "submissions.csv" in a downloads folder says nothing about which form.
	if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") || !strings.Contains(got, theForm) {
		t.Errorf("Content-Disposition = %q, want an attachment named after the form", got)
	}

	// A list of names, addresses and amounts must not be cached anywhere.
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store on an export of personal data", got)
	}

	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("the export is not readable CSV: %v", err)
	}

	if len(rows) != 2 {
		t.Fatalf("the export has %d rows, want a header and one submission", len(rows))
	}

	// One column per field of the definition, in the definition's order,
	// followed by one per item. Derived from the rows instead, a form whose
	// first submission skipped a conditional field would export a spreadsheet
	// missing that column entirely.
	want := []string{
		"submitted_at", "submission_id", "status", "total", "currency",
		"Your name", "Email for your receipt", "Donation for the Shrine",
		"Anything we should know", "Lunch ticket", "payment_reference",
	}

	if len(rows[0]) != len(want) {
		t.Fatalf("the header has %d columns, want %d:\n%v", len(rows[0]), len(want), rows[0])
	}

	for i, heading := range want {
		if rows[0][i] != heading {
			t.Errorf("column %d is %q, want %q", i, rows[0][i], heading)
		}
	}

	row := rows[1]
	if row[2] != "pending" {
		t.Errorf("the status column is %q, want pending", row[2])
	}
	if row[5] != "Maria O'Neill" {
		t.Errorf("the name column is %q, and a CSV cell is not escaped HTML", row[5])
	}
	if row[9] != "2" {
		t.Errorf("the ticket column is %q, want 2", row[9])
	}

	// The visitor's IP address is on the submission for Stripe's fraud
	// checks and is deliberately not a column: a spreadsheet gets emailed
	// around a parish office, and that is the wrong place for it.
	if strings.Contains(w.Body.String(), "192.0.2") {
		t.Error("the export carries the submitter's IP address")
	}
}

// --- what each gate decides -------------------------------------------------

// The check with nothing but an explicit comparison behind it.
//
// The role gate in front of the route checked a grant on the slug in the URL.
// It knows nothing about the identifier in the URL, so without tying the
// record to the form, a results grant on the picnic would read every lunch
// order in the service by trying identifiers.
func TestAGrantOnOneFormCannotReadAnothersSubmission(t *testing.T) {
	o := newOffice(t)

	o.sell(t, nil)

	// One account that may read the lunch form, to find an identifier the
	// honest way, and another that may read only the picnic.
	insider, insiderCookie := o.reader(t, "insider@schoenstatt.us")
	o.grant(t, insider, theForm, accessbus.RoleResults)

	ids := o.idsOf(t, theForm, insiderCookie)
	if len(ids) != 1 {
		t.Fatalf("the list links to %d submissions, want 1: %v", len(ids), ids)
	}

	outsider, outsiderCookie := o.reader(t, "picnic@schoenstatt.us")
	o.grant(t, outsider, otherForm, accessbus.RoleResults)

	// Their own form, to prove the grant works at all -- otherwise the 404
	// below could be the gate refusing and the test would pass for the wrong
	// reason.
	if w := o.read(t, "/forms/"+otherForm+"/submissions", outsiderCookie); w.Code != http.StatusOK {
		t.Fatalf("the picnic list = %d for the account granted on it:\n%s", w.Code, short(w.Body.String()))
	}

	// The lunch order, by its real identifier, under the form they may read.
	w := o.read(t, "/forms/"+otherForm+"/submissions/"+ids[0], outsiderCookie)
	if w.Code != http.StatusNotFound {
		t.Fatalf("reading another form's submission = %d, want 404:\n%s", w.Code, short(w.Body.String()))
	}

	// And the same order under its own form, which they may not read.
	if w := o.read(t, "/forms/"+theForm+"/submissions/"+ids[0], outsiderCookie); w.Code != http.StatusForbidden {
		t.Errorf("reading the lunch form without a grant = %d, want 403", w.Code)
	}
}

// The index is filtered by grant, so it cannot tell somebody that a form
// exists which they may not read.
func TestTheIndexListsOnlyFormsYouHoldAGrantOn(t *testing.T) {
	o := newOffice(t)

	o.sell(t, nil)

	only, onlyCookie := o.reader(t, "picnic@schoenstatt.us")
	o.grant(t, only, otherForm, accessbus.RoleResults)

	w := o.read(t, "/forms", onlyCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("the index = %d:\n%s", w.Code, short(w.Body.String()))
	}

	body := w.Body.String()
	if !strings.Contains(body, "Parish picnic") {
		t.Errorf("the index omits the form this account is granted on:\n%s", short(body))
	}
	if strings.Contains(body, theForm) {
		t.Errorf("the index names a form this account holds no grant on:\n%s", short(body))
	}

	// A site-wide grant means every form, which is what the bootstrap account
	// holds and therefore the only reason there is any way in at all before a
	// granting UI exists.
	site, siteCookie := o.reader(t, "admin@schoenstatt.us")
	o.grant(t, site, "", accessbus.RoleAdmin)

	w = o.read(t, "/forms", siteCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("the index = %d for a site-wide admin:\n%s", w.Code, short(w.Body.String()))
	}

	body = w.Body.String()
	for _, want := range []string{"Parish picnic", "Feast of Our Lady of Schoenstatt"} {
		if !strings.Contains(body, want) {
			t.Errorf("a site-wide admin's index omits %q:\n%s", want, short(body))
		}
	}

	// The count, so that the list answers "how many" without a click.
	if !strings.Contains(body, ">1<") {
		t.Errorf("the index does not show the one submission as a count:\n%s", short(body))
	}

	// An account with no grant at all sees an empty list rather than a
	// refusal: they are signed in, and there is nothing wrong with them.
	_, none := o.reader(t, "nobody@schoenstatt.us")

	w = o.read(t, "/forms", none)
	if w.Code != http.StatusOK {
		t.Fatalf("the index = %d for an account with no grants, want 200", w.Code)
	}
	if strings.Contains(w.Body.String(), "Parish picnic") {
		t.Errorf("an account with no grants sees a form:\n%s", short(w.Body.String()))
	}
}

// Reading submissions is behind a session, and a signed-out GET goes to the
// sign-in page carrying where it was going.
func TestReadingSubmissionsNeedsASession(t *testing.T) {
	o := newOffice(t)

	for _, target := range []string{
		"/forms",
		"/forms/" + theForm + "/submissions",
		"/forms/" + theForm + "/submissions.csv",
		"/forms/" + theForm + "/submissions/" + types.NewID().String(),
	} {
		w := o.read(t, target, "")
		if w.Code != http.StatusSeeOther {
			t.Errorf("GET %s with no session = %d, want 303 to the sign-in page", target, w.Code)

			continue
		}

		loc := w.Header().Get("Location")
		if !strings.HasPrefix(loc, "/signin") {
			t.Errorf("GET %s with no session went to %q", target, loc)
		}
		if !strings.Contains(loc, url.QueryEscape(target)) {
			t.Errorf("GET %s lost where it was going: %q", target, loc)
		}
	}
}

// A signed-in account with no grant is told which account it is signed in as.
// The usual cause of this refusal is being signed in as the wrong person, and
// "you are not allowed" with no name on it sends somebody looking in the
// wrong place.
func TestASignedInAccountWithNoGrantIsRefusedByName(t *testing.T) {
	o := newOffice(t)

	_, cookie := o.reader(t, "visitor@schoenstatt.us")

	w := o.read(t, "/forms/"+theForm+"/submissions", cookie)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a grant-less read = %d, want 403:\n%s", w.Code, short(w.Body.String()))
	}

	if !strings.Contains(w.Body.String(), "visitor@schoenstatt.us") {
		t.Errorf("the refusal does not say which account it refused:\n%s", w.Body.String())
	}
}

// A form that has no definition any more. Grants are rows and definitions are
// files, so a grant can outlive the form it names -- the gate lets it through,
// because it has a grant, and the app answers 404.
func TestAGrantOnAFormThatNoLongerExistsIsA404(t *testing.T) {
	o := newOffice(t)

	u, cookie := o.reader(t, "reader@schoenstatt.us")
	o.grant(t, u, "retreat-2019", accessbus.RoleResults)

	if w := o.read(t, "/forms/retreat-2019/submissions", cookie); w.Code != http.StatusNotFound {
		t.Errorf("a grant on a form with no definition = %d, want 404", w.Code)
	}
}

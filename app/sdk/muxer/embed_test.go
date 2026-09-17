package muxer_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formtoml"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/forms"
)

// The embedded form, through the surface as it is mounted in production.
//
// These are the tests that execute the templates. A template that references a
// field no view type has is invisible to the compiler and to every other test
// in this package: it fails at render time, in front of somebody.
//
// They run against the real definition rather than a fixture, so that what is
// asserted is what the shrine's page will serve. That is only safe because the
// clock is injected -- otherwise every test here would start failing the day
// after the feast, which is the worst possible moment for a test suite to go
// red.

const theForm = "feast-lunch-2026"

var grantPattern = regexp.MustCompile(`name="_grant" value="([^"]+)"`)

// grantIn finds the grant in a rendered page, which doubles as an assertion
// that the hidden field is there at all: a form page without one submits
// nothing that can be accepted.
func grantIn(t *testing.T, body string) string {
	t.Helper()

	m := grantPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no grant in the page:\n%s", short(body))
	}

	return m[1]
}

func short(s string) string {
	if len(s) > 2000 {
		return s[:2000] + "\n[...]"
	}

	return s
}

// embedSurface builds the public surface with the clock stopped well before
// the feast, and hands back the submission domain so a test can read what was
// stored.
func embedSurface(t *testing.T) (http.Handler, *submissionbus.Business) {
	t.Helper()

	cfg := newConfig(t, nil, nil)
	cfg.Embed.Now = func() time.Time { return beforeTheFeast }

	subs, ok := cfg.Embed.Submissions.(*submissionbus.Business)
	if !ok {
		t.Fatalf("the test config holds a %T rather than the submission domain", cfg.Embed.Submissions)
	}

	return embedOf(t, cfg), subs
}

// beforeTheFeast is a month before the form closes, so the form is open.
var beforeTheFeast = time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

// afterTheFeast is a day after it closes.
var afterTheFeast = time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)

func getPage(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))

	return w
}

// postForm submits with the headers a browser inside a frame sends: the POST
// is same-origin, because the frame is posting to the page it came from.
func postForm(t *testing.T, h http.Handler, values url.Values) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, "/f/"+theForm, strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	return w
}

// filled is a submission that should be accepted: a name, an address and two
// tickets.
func filled(grant string) url.Values {
	return url.Values{
		embedapp.GrantField:             {grant},
		"name":                          {"Maria O'Neill"},
		"email":                         {"maria@example.org"},
		formbus.QuantityField("ticket"): {"2"},
	}
}

func TestABlankFormRenders(t *testing.T) {
	h, _ := embedSurface(t)

	w := getPage(t, h, "/f/"+theForm)
	if w.Code != http.StatusOK {
		t.Fatalf("GET the form = %d:\n%s", w.Code, short(w.Body.String()))
	}

	body := w.Body.String()

	for _, want := range []string{
		`<form method="post"`,
		`name="_grant"`,
		`name="name"`,
		`name="email"`,
		`name="donation"`,
		`name="notes"`,
		`name="` + formbus.QuantityField("ticket") + `"`,
		`type="submit"`,
		"Feast of Our Lady of Schoenstatt",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not contain %s:\n%s", want, short(body))
		}
	}

	grantIn(t, body)

	// The page carries a single-use grant, so a shared cache handing the same
	// nonce to fifty visitors would break the one property it has.
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// The browser is handed the definition's own rules as real attributes, rather
// than a second copy of them written by hand in JavaScript.
func TestTheFormRendersItsRulesAsAttributes(t *testing.T) {
	h, _ := embedSurface(t)
	body := getPage(t, h, "/f/"+theForm).Body.String()

	for _, want := range []string{
		`type="email"`,        // the email field's kind
		`required`,            // name and email are required
		`maxlength="100"`,     // the name field's own bound
		`maxlength="500"`,     // the notes field's
		`inputmode="decimal"`, // the donation, which is money
		`min="5.00"`,          // its floor, in dollars, from a definition in cents
		`max="5000.00"`,       // its ceiling
		`step="0.01"`,         // hundredths, because it is money
		`max="20"`,            // twenty tickets in one order
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not carry %s:\n%s", want, short(body))
		}
	}
}

func TestASubmissionIsAcceptedAndConfirmedInPlace(t *testing.T) {
	h, subs := embedSurface(t)

	grant := grantIn(t, getPage(t, h, "/f/"+theForm).Body.String())

	w := postForm(t, h, filled(grant))
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d:\n%s", w.Code, short(w.Body.String()))
	}

	body := w.Body.String()

	// Answered in place, in the same frame. No redirect: the person stays
	// exactly where they were on somebody else's page.
	if loc := w.Header().Get("Location"); loc != "" {
		t.Errorf("the submission redirected to %q, want an in-line answer", loc)
	}

	// Two tickets at twelve dollars, derived from the definition and not from
	// anything the browser sent.
	if !strings.Contains(body, "$24.00") {
		t.Errorf("the receipt does not show the derived total:\n%s", short(body))
	}

	stored, err := subs.ByForm(t.Context(), mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("%d submissions stored, want 1", len(stored))
	}

	sub := stored[0]

	if sub.Answers.Total != types.Money(2400) {
		t.Errorf("stored total = %s, want 24.00", sub.Answers.Total)
	}
	if sub.Status != submissionbus.StatusPending {
		t.Errorf("stored status = %q, want pending: there is money to collect", sub.Status)
	}
	if sub.Email.String() != "maria@example.org" {
		t.Errorf("stored address = %q", sub.Email)
	}
	if sub.Version != sub.Answers.Version || sub.Version == "" {
		t.Errorf("the stored version is %q", sub.Version)
	}
}

// The money rule, at the surface. A price in the body is not wrong, it is
// simply not read.
func TestAPriceInTheBodyIsIgnored(t *testing.T) {
	h, subs := embedSurface(t)

	grant := grantIn(t, getPage(t, h, "/f/"+theForm).Body.String())

	values := filled(grant)
	values.Set("price", "0.01")
	values.Set("total", "0.01")
	values.Set("amount", "0.01")
	values.Set("ticket_price", "0.01")

	if w := postForm(t, h, values); w.Code != http.StatusOK {
		t.Fatalf("POST = %d:\n%s", w.Code, short(w.Body.String()))
	}

	stored, err := subs.ByForm(t.Context(), mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("%d submissions, want 1", len(stored))
	}

	if got := stored[0].Answers.Total; got != types.Money(2400) {
		t.Errorf("total = %s, want 24.00 -- a submitted price changed the charge", got)
	}
}

// A refusal keeps the answers. Retyping an address because a quantity was
// wrong is the worst version of this experience.
func TestARefusalExplainsItselfAndKeepsTheAnswers(t *testing.T) {
	h, subs := embedSurface(t)

	grant := grantIn(t, getPage(t, h, "/f/"+theForm).Body.String())

	values := filled(grant)
	values.Set("email", "not-an-address")

	w := postForm(t, h, values)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST with a bad address = %d, want 422:\n%s", w.Code, short(w.Body.String()))
	}

	body := w.Body.String()

	if !strings.Contains(body, "Maria O&#39;Neill") && !strings.Contains(body, "Maria O'Neill") {
		t.Errorf("the name was not put back:\n%s", short(body))
	}
	if !strings.Contains(body, "not-an-address") {
		t.Errorf("the address was not put back:\n%s", short(body))
	}
	if !strings.Contains(body, "does not look like an email address") {
		t.Errorf("the page does not say what is wrong:\n%s", short(body))
	}

	// A fresh grant, and nothing stored.
	if again := grantIn(t, body); again == grant {
		t.Error("the re-rendered page carries the same grant")
	}

	stored, err := subs.ByForm(t.Context(), mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("%d submissions stored for a refused attempt, want none", len(stored))
	}
}

// payment_required: a submission that comes to nothing is refused, with the
// author's own sentence naming which of the two fields to fill in.
func TestASubmissionWorthNothingIsRefused(t *testing.T) {
	h, _ := embedSurface(t)

	grant := grantIn(t, getPage(t, h, "/f/"+theForm).Body.String())

	values := url.Values{
		embedapp.GrantField: {grant},
		"name":              {"Maria"},
		"email":             {"maria@example.org"},
	}

	w := postForm(t, h, values)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an empty order = %d, want 422:\n%s", w.Code, short(w.Body.String()))
	}

	if !strings.Contains(w.Body.String(), "lunch ticket") {
		t.Errorf("the refusal does not name what to fill in:\n%s", short(w.Body.String()))
	}
}

// A donation with no ticket is a whole submission, which is the two-purpose
// shape the feast page needs.
func TestADonationWithNoTicketIsAccepted(t *testing.T) {
	h, subs := embedSurface(t)

	grant := grantIn(t, getPage(t, h, "/f/"+theForm).Body.String())

	values := url.Values{
		embedapp.GrantField: {grant},
		"name":              {"Maria"},
		"email":             {"maria@example.org"},
		"donation":          {"25"},
	}

	if w := postForm(t, h, values); w.Code != http.StatusOK {
		t.Fatalf("POST = %d:\n%s", w.Code, short(w.Body.String()))
	}

	stored, err := subs.ByForm(t.Context(), mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("%d submissions, want 1", len(stored))
	}

	if got := stored[0].Answers.Total; got != types.Money(2500) {
		t.Errorf("total = %s, want 25.00", got)
	}
}

// The same grant twice: a double-clicked button, or a refreshed POST. The
// second one stores nothing and says so without reporting a failure.
func TestAReplayedSubmissionIsNotStoredTwice(t *testing.T) {
	h, subs := embedSurface(t)

	grant := grantIn(t, getPage(t, h, "/f/"+theForm).Body.String())

	if w := postForm(t, h, filled(grant)); w.Code != http.StatusOK {
		t.Fatalf("the first POST = %d:\n%s", w.Code, short(w.Body.String()))
	}

	w := postForm(t, h, filled(grant))
	if w.Code != http.StatusOK {
		t.Fatalf("the second POST = %d, want 200 with an explanation:\n%s", w.Code, short(w.Body.String()))
	}

	if !strings.Contains(w.Body.String(), "already have this") {
		t.Errorf("the second answer does not say it was already received:\n%s", short(w.Body.String()))
	}

	stored, err := subs.ByForm(t.Context(), mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(stored) != 1 {
		t.Errorf("%d submissions stored, want 1", len(stored))
	}
}

func TestASubmissionWithNoGrantIsRefusedAndReRendered(t *testing.T) {
	h, subs := embedSurface(t)

	// The trap this surface is written against: with no session there is
	// nothing to forge, so a gate that passes through when there is no
	// principal would admit this unconditionally.
	values := filled("")

	w := postForm(t, h, values)
	if w.Code != http.StatusOK {
		t.Fatalf("POST with no grant = %d:\n%s", w.Code, short(w.Body.String()))
	}

	if !strings.Contains(w.Body.String(), "refreshed") {
		t.Errorf("the answer does not offer a fresh page:\n%s", short(w.Body.String()))
	}

	stored, err := subs.ByForm(t.Context(), mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("%d submissions stored without a grant, want none", len(stored))
	}
}

func TestAGrantFromAnotherKeyIsRefused(t *testing.T) {
	h, subs := embedSurface(t)

	other, err := formbus.ParseGrantKey("a-completely-different-signing-key-here")
	if err != nil {
		t.Fatalf("ParseGrantKey: %v", err)
	}

	definitions, err := formtoml.Load(forms.FS)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	f, err := definitions.ByID(mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	forged, err := formbus.Mint(other, f, beforeTheFeast)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if w := postForm(t, h, filled(forged)); w.Code != http.StatusOK {
		t.Fatalf("POST = %d:\n%s", w.Code, short(w.Body.String()))
	}

	stored, err := subs.ByForm(t.Context(), mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("a grant signed with another key was accepted")
	}
}

// A cross-site POST is refused before any of that, by the origin check.
func TestACrossSiteSubmissionIsRefused(t *testing.T) {
	h, subs := embedSurface(t)

	grant := grantIn(t, getPage(t, h, "/f/"+theForm).Body.String())

	r := httptest.NewRequest(http.MethodPost, "/f/"+theForm,
		strings.NewReader(filled(grant).Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "cross-site")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code == http.StatusOK {
		t.Errorf("a cross-site POST was accepted: %d", w.Code)
	}

	stored, err := subs.ByForm(t.Context(), mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("%d submissions stored from a cross-site POST", len(stored))
	}
}

func TestAClosedFormShowsItsNoteAndTakesNothing(t *testing.T) {
	cfg := newConfig(t, nil, nil)
	cfg.Embed.Now = func() time.Time { return afterTheFeast }

	h := embedOf(t, cfg)

	w := getPage(t, h, "/f/"+theForm)
	if w.Code != http.StatusOK {
		t.Fatalf("GET a closed form = %d:\n%s", w.Code, short(w.Body.String()))
	}

	body := w.Body.String()

	if !strings.Contains(body, "the feast has passed") {
		t.Errorf("the closed page does not carry the form's own note:\n%s", short(body))
	}
	if strings.Contains(body, `name="_grant"`) {
		t.Error("a closed form handed out a grant")
	}
	if strings.Contains(body, "<form") {
		t.Error("a closed form rendered a form to submit")
	}
}

func TestAnUnknownFormIsNotFound(t *testing.T) {
	h, _ := embedSurface(t)

	for _, target := range []string{
		"/f/no-such-form",
		"/f/NOT-A-SLUG",
		"/f/-",
	} {
		w := getPage(t, h, target)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, w.Code)
		}
	}
}

// frame-ancestors comes from the form's own list, which is what makes it a
// property of the form rather than of the installation.
func TestTheFormPageNamesItsOwnEmbedders(t *testing.T) {
	h, _ := embedSurface(t)

	csp := getPage(t, h, "/f/"+theForm).Header().Get("Content-Security-Policy")

	if !strings.Contains(csp, "frame-ancestors https://schoenstatt-austin.us") {
		t.Errorf("the policy does not name the form's own embedder: %q", csp)
	}

	// Anything that is not a form page fails closed, including a path the mux
	// will answer 404 for.
	for _, target := range []string{"/healthz", "/embed.js", "/f/no-such-form"} {
		got := getPage(t, h, target).Header().Get("Content-Security-Policy")
		if !strings.Contains(got, "frame-ancestors 'none'") {
			t.Errorf("%s: policy is %q, want frame-ancestors 'none'", target, got)
		}
	}

	// One header, never two. Two enforced policies are evaluated
	// independently, so a second one blocks everything while looking like
	// ours is being ignored.
	if got := getPage(t, h, "/f/"+theForm).Header().Values("Content-Security-Policy"); len(got) != 1 {
		t.Errorf("%d CSP headers, want exactly 1", len(got))
	}
}

// The parent origin is attacker-supplied, so it is checked against the form's
// own list before the page will post anything to it.
func TestTheParentOriginIsCheckedAgainstTheFormsOwnList(t *testing.T) {
	h, _ := embedSurface(t)

	allowed := getPage(t, h, "/f/"+theForm+"?parent=https://schoenstatt-austin.us").Body.String()
	if !strings.Contains(allowed, `data-parent-origin="https://schoenstatt-austin.us"`) {
		t.Errorf("a permitted parent origin was not echoed:\n%s", short(allowed))
	}

	for _, asked := range []string{
		"https://evil.example",
		"https://schoenstatt-austin.us.evil.example",
		"*",
		"null",
		"http://schoenstatt-austin.us",
		"https://schoenstatt-austin.us/path",
		"javascript:alert(1)",
	} {
		body := getPage(t, h, "/f/"+theForm+"?parent="+url.QueryEscape(asked)).Body.String()

		if !strings.Contains(body, `data-parent-origin=""`) {
			t.Errorf("parent=%q produced a target origin:\n%s", asked, short(body))
		}

		// And it is not echoed anywhere else either. Skipped for values short
		// enough to occur in the page by coincidence -- "*" appears in the
		// stylesheet -- since for those the attribute check above is the whole
		// assertion.
		if len(asked) > 8 && strings.Contains(body, asked) {
			t.Errorf("parent=%q was echoed into the page", asked)
		}
	}
}

func TestTheSnippetIsServedAndCacheable(t *testing.T) {
	h, _ := embedSurface(t)

	w := getPage(t, h, "/embed.js")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /embed.js = %d", w.Code)
	}

	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
		t.Errorf("Content-Type = %q", got)
	}

	// An hour, not forever: the path is baked into whatever somebody pasted
	// into a page and cannot be changed afterwards.
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "max-age=3600") {
		t.Errorf("Cache-Control = %q, want an hour", got)
	}

	// The parent half of the channel receives; the child posts. So this file
	// listens for a message and clamps a height, and does not call
	// postMessage at all.
	body := w.Body.String()
	for _, want := range []string{
		"data-dropin-form",
		`"message"`,
		"dropin-forms:height",
		"contentWindow",
		"event.origin",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the snippet does not mention %s", want)
		}
	}
}

func TestTheFormsOwnScriptAndStylesheetAreServed(t *testing.T) {
	cfg := newConfig(t, nil, nil)
	cfg.Embed.Now = func() time.Time { return beforeTheFeast }

	h := embedOf(t, cfg)

	for _, path := range []string{
		cfg.Embed.Render.StylesheetPath(),
		cfg.Embed.Render.ScriptPath(),
	} {
		if path == "" {
			t.Fatal("the embed surface has no asset path")
		}

		w := getPage(t, h, path)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d", path, w.Code)

			continue
		}

		// The hashed path is what makes this cacheable forever, overriding
		// the surface's own no-store.
		if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
			t.Errorf("%s: Cache-Control = %q, want immutable", path, got)
		}
	}
}

// An author-written label must never be able to become markup. The ban on
// template.HTML is what guarantees it; this asserts the guarantee holds for
// what a person types, which is the other half of the same worry.
func TestWhatSomebodyTypesIsEscapedWhenItComesBack(t *testing.T) {
	h, _ := embedSurface(t)

	grant := grantIn(t, getPage(t, h, "/f/"+theForm).Body.String())

	values := filled(grant)
	values.Set("email", "still-not-an-address")
	values.Set("name", `<script>alert(1)</script>`)

	w := postForm(t, h, values)
	body := w.Body.String()

	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("a typed script tag came back as markup:\n%s", short(body))
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("the typed value was not put back escaped:\n%s", short(body))
	}
}

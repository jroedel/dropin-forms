package muxer_test

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/payment/paybus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// What happens around the payment rather than in it: the click being
// acknowledged, the way back to the form, and the acknowledgement somebody
// gets when Stripe sends them home again.
//
// All three came out of the first real card payment on the shrine's page. The
// Continue button took long enough to answer that it got clicked twice, there
// was no way back from the total to change the number of tickets, and after
// paying the visitor landed on the feast page in front of a fresh blank form
// with nothing anywhere saying the money had gone through.
//
// These are template tests as much as route tests. A confirmation page that
// references a field no view type has fails at render time, in front of the
// person who has just paid, and no other test in this package would see it.

// postTo submits to a named route with the headers a browser inside the frame
// sends. postForm only ever posts to the form itself.
func postTo(t *testing.T, h http.Handler, target string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	return w
}

// surfaceAt is the public surface with the clock stopped wherever a test needs
// it, which is the only way to reach the after-the-deadline cases without
// waiting for October.
func surfaceAt(t *testing.T, now time.Time) http.Handler {
	t.Helper()

	cfg := newConfig(t, []types.Origin{mustOrigin(t, "https://schoenstatt-austin.us")}, nil)
	cfg.Embed.Now = func() time.Time { return now }

	return embedOf(t, cfg)
}

// --- the click, acknowledged ------------------------------------------------

// The button has to carry the label the script swaps in, because the script
// cannot invent one: the wording differs between a form that sells and a form
// that does not, and that is the template's business.
func TestTheSubmitButtonCarriesItsBusyLabel(t *testing.T) {
	h, _ := embedSurface(t)

	body := getPage(t, h, "/f/"+theForm).Body.String()

	if !strings.Contains(body, `data-busy=`) {
		t.Errorf("the submit button has no busy label, so a slow submission cannot be acknowledged:\n%s", short(body))
	}
	if !strings.Contains(body, "One moment") {
		t.Errorf("the busy label is not the one for a form that sells:\n%s", short(body))
	}
}

// --- Back, from the total page ----------------------------------------------

// The total page has to carry the answers, or Back means retyping a name and
// an address to change a number.
func TestTheTotalPageOffersAWayBackWithTheAnswersInIt(t *testing.T) {
	k := newTill(t)

	w := getPage(t, k.embed, "/f/"+theForm)
	if w.Code != http.StatusOK {
		t.Fatalf("the blank form = %d", w.Code)
	}

	w = postForm(t, k.embed, filled(grantIn(t, w.Body.String())))
	if w.Code != http.StatusOK {
		t.Fatalf("the submission = %d:\n%s", w.Code, short(w.Body.String()))
	}

	body := w.Body.String()

	if !strings.Contains(body, `action="/f/`+theForm+`/edit"`) {
		t.Errorf("the total page offers no way back:\n%s", short(body))
	}

	// Escaped on the way back out, which is what makes echoing a posted body
	// into a page safe. The apostrophe is the interesting character here.
	for _, want := range []string{
		`name="name" value="Maria O&#39;Neill"`,
		`name="email" value="maria@example.org"`,
		`name="` + formbus.QuantityField("ticket") + `" value="2"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Back does not carry %s:\n%s", want, short(body))
		}
	}

	// The grant has been spent. Carrying it back would be a page offering a
	// token that cannot work, and the fresh page mints its own.
	if strings.Contains(body, embedapp.GrantField) {
		t.Errorf("the spent grant is echoed into the Back form:\n%s", short(body))
	}
}

// Back re-renders and stores nothing. The order already placed stays as it
// was, and the new page works: its grant is fresh and a submission made with
// it is accepted.
func TestBackRendersTheFormAgainAndStoresNothing(t *testing.T) {
	k := newTill(t)

	first := k.order(t, nil)
	if first.Status != submissionbus.StatusPending {
		t.Fatalf("the first order is %q, want pending", first.Status)
	}

	// What the Back button posts: everything that was submitted, without the
	// spent grant.
	back := filled("")
	delete(back, embedapp.GrantField)

	w := postTo(t, k.embed, "/f/"+theForm+"/edit", back)
	if w.Code != http.StatusOK {
		t.Fatalf("Back = %d, want the form again:\n%s", w.Code, short(w.Body.String()))
	}

	body := w.Body.String()

	if !strings.Contains(body, `value="Maria O&#39;Neill"`) {
		t.Errorf("the form came back empty, so Back means retyping everything:\n%s", short(body))
	}

	stored, err := k.subs.ByForm(t.Context(), mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("going back stored something: %d submissions, want the 1 already placed", len(stored))
	}

	// And the page it produced is a working form rather than a picture of one.
	v := filled(grantIn(t, body))
	v[formbus.QuantityField("ticket")] = []string{"3"}

	if w := postForm(t, k.embed, v); w.Code != http.StatusOK {
		t.Fatalf("the re-ordered submission = %d:\n%s", w.Code, short(w.Body.String()))
	}

	stored, err = k.subs.ByForm(t.Context(), mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("after ordering again there are %d submissions, want 2", len(stored))
	}
}

// Back after the deadline is the closed page, not a reopened form. The close
// date governs taking orders, and changing one is taking one.
func TestBackAfterTheDeadlineIsRefusedLikeAnyOtherOrder(t *testing.T) {
	h := surfaceAt(t, afterTheFeast)

	back := filled("")
	delete(back, embedapp.GrantField)

	w := postTo(t, h, "/f/"+theForm+"/edit", back)
	if w.Code != http.StatusOK {
		t.Fatalf("Back on a closed form = %d", w.Code)
	}

	body := w.Body.String()

	if !strings.Contains(body, "the feast has passed") {
		t.Errorf("a closed form reopened for an edit:\n%s", short(body))
	}
	if strings.Contains(body, embedapp.GrantField) {
		t.Errorf("the closed page carries a grant, so it can be submitted:\n%s", short(body))
	}
}

// --- coming back from Stripe ------------------------------------------------

// The page that fixes the third thing the first payment found: after paying,
// something in the frame says so.
func TestComingBackPaidSaysSoInTheFrame(t *testing.T) {
	h, _ := embedSurface(t)

	w := getPage(t, h, "/f/"+theForm+"/return?state="+paybus.StatePaid)
	if w.Code != http.StatusOK {
		t.Fatalf("coming back = %d:\n%s", w.Code, short(w.Body.String()))
	}

	body := w.Body.String()

	if !strings.Contains(body, "Thank you") {
		t.Errorf("the page after paying does not thank anybody:\n%s", short(body))
	}

	// The author's own confirmation, which on a form that sells nothing else
	// ever renders: the page after submitting is the receipt and the payment
	// button.
	if !strings.Contains(body, "We look forward to seeing you on October 17th") {
		t.Errorf("the form's own confirmation is not on the page after paying:\n%s", short(body))
	}

	// Not a form. There is nothing to submit from here.
	if strings.Contains(body, "<form") {
		t.Errorf("the page after paying carries a form:\n%s", short(body))
	}
}

// A cancelled payment is not an error and not a dead end.
func TestComingBackCancelledSaysNothingWasChargedAndOffersTheFormAgain(t *testing.T) {
	h, _ := embedSurface(t)

	target := "/f/" + theForm + "/return?state=" + paybus.StateCancelled +
		"&parent=" + url.QueryEscape("https://schoenstatt-austin.us")

	w := getPage(t, h, target)
	if w.Code != http.StatusOK {
		t.Fatalf("coming back cancelled = %d:\n%s", w.Code, short(w.Body.String()))
	}

	body := w.Body.String()

	if !strings.Contains(body, "Nothing has been charged") {
		t.Errorf("a cancelled payment does not say nothing was charged:\n%s", short(body))
	}
	if strings.Contains(body, "Thank you") {
		t.Errorf("a cancelled payment is thanked for something that did not happen:\n%s", short(body))
	}

	// Back to the form, with the parent origin carried forward so the page it
	// lands on can still tell the frame how tall it is.
	if !strings.Contains(body, `href="/f/`+theForm+`?parent=https%3A%2F%2Fschoenstatt-austin.us"`) {
		t.Errorf("no way back to the form, or the parent origin was dropped:\n%s", short(body))
	}
}

// The one that matters most about this route. A deadline that passes while
// somebody is on Stripe's page must not answer "this form is closed" to the
// one person on the site who has just been charged.
func TestComingBackAfterTheFormClosedStillThanksThem(t *testing.T) {
	h := surfaceAt(t, afterTheFeast)

	w := getPage(t, h, "/f/"+theForm+"/return?state="+paybus.StatePaid)
	if w.Code != http.StatusOK {
		t.Fatalf("coming back after the close = %d", w.Code)
	}

	body := w.Body.String()

	if strings.Contains(body, "the feast has passed") {
		t.Errorf("somebody who has just paid was told the form is closed:\n%s", short(body))
	}
	if !strings.Contains(body, "We look forward to seeing you on October 17th") {
		t.Errorf("the confirmation is missing after the close:\n%s", short(body))
	}
}

// The safety property of the whole route: it says words and settles nothing.
// A browser arriving with ?state=paid is a browser repeating something, and
// the only thing in this service that may mark an order paid is a signature
// from Stripe.
func TestTheReturnPageCannotMarkAnythingPaid(t *testing.T) {
	k := newTill(t)

	sub := k.order(t, nil)
	if sub.Status != submissionbus.StatusPending {
		t.Fatalf("the order is %q, want pending", sub.Status)
	}

	for range 3 {
		if w := getPage(t, k.embed, "/f/"+theForm+"/return?state="+paybus.StatePaid); w.Code != http.StatusOK {
			t.Fatalf("coming back = %d", w.Code)
		}
	}

	after, err := k.subs.ByID(t.Context(), sub.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if after.Status != submissionbus.StatusPending {
		t.Errorf("reading a thank-you page moved the order to %q; only the webhook may do that", after.Status)
	}
	if after.PaymentRef != "" {
		t.Errorf("the return page recorded a payment reference %q", after.PaymentRef)
	}
}

// A state we do not recognise -- an old bookmark, or a marker we stop sending
// one day -- lands somebody on a working form rather than on a page explaining
// a query parameter to them.
func TestAnUnrecognisedReturnStateShowsTheForm(t *testing.T) {
	h, _ := embedSurface(t)

	w := getPage(t, h, "/f/"+theForm+"/return?state=marzipan")
	if w.Code != http.StatusOK {
		t.Fatalf("an unknown state = %d", w.Code)
	}

	// A form with a grant in it, which is to say the real thing.
	grantIn(t, w.Body.String())
}

// The return page is framed like every other page here, so the origin it may
// post its height to is checked against the form's own list rather than
// echoed.
func TestTheReturnPageChecksTheParentOriginLikeEveryOtherPage(t *testing.T) {
	h, _ := embedSurface(t)

	allowed := getPage(t, h, "/f/"+theForm+"/return?state="+paybus.StatePaid+
		"&parent="+url.QueryEscape("https://schoenstatt-austin.us")).Body.String()

	if !strings.Contains(allowed, `data-parent-origin="https://schoenstatt-austin.us"`) {
		t.Errorf("a permitted embedder is not named, so the frame stays 120px tall:\n%s", short(allowed))
	}

	rogue := getPage(t, h, "/f/"+theForm+"/return?state="+paybus.StatePaid+
		"&parent="+url.QueryEscape("https://not-the-shrine.example")).Body.String()

	if strings.Contains(rogue, "not-the-shrine.example") {
		t.Errorf("an origin the form does not permit reached the page:\n%s", short(rogue))
	}
	if !strings.Contains(rogue, `data-parent-origin=""`) {
		t.Errorf("the page should name no parent at all:\n%s", short(rogue))
	}
}

// The marker names are written in three places and only two of them are Go.
// This is the test for the third: the pasted snippet reads the parameters that
// paybus puts into the addresses Stripe redirects to, and a rename on either
// side without the other is a visitor who paid and is told nothing.
func TestTheSnippetAndTheReturnAddressesAgreeOnTheMarker(t *testing.T) {
	h, _ := embedSurface(t)

	w := getPage(t, h, "/embed.js")
	if w.Code != http.StatusOK {
		t.Fatalf("/embed.js = %d", w.Code)
	}

	snippet := w.Body.String()

	for _, want := range []string{
		paybus.ReturnMarker,
		paybus.ReturnState,
		paybus.StatePaid,
		paybus.StateCancelled,
	} {
		if !strings.Contains(snippet, `"`+want+`"`) {
			t.Errorf("the snippet does not know %q, so the round trip after paying is silent", want)
		}
	}
}

// --- every page of a form is framed, not just the first one -----------------

// The one that would have caught this step's worst bug before the shrine's
// page did.
//
// frame-ancestors is answered per form by reading the slug out of the path,
// above the mux, where PathValue is not populated yet. That resolver matched
// /f/{slug} exactly -- so /f/{slug}/return and /f/{slug}/edit fell through to
// the fail-closed answer, 'none', and the browser would have refused to draw
// the frame. On a page of somebody else's website the symptom is a blank box
// and a console message nobody reads, and it would have appeared for the first
// time at the end of a real payment.
//
// So this walks every page of the form on the embed surface and asserts the
// policy names the shrine. A route added to embedapp without being added to
// page.formPages fails here.
func TestEveryPageOfAFormMayBeFramedByItsSite(t *testing.T) {
	cfg := newConfig(t, []types.Origin{mustOrigin(t, "https://schoenstatt-austin.us")}, nil)
	cfg.Embed.Now = func() time.Time { return beforeTheFeast }
	h := embedOf(t, cfg)

	pages := []struct {
		method string
		target string
	}{
		{http.MethodGet, "/f/" + theForm},
		{http.MethodPost, "/f/" + theForm},
		{http.MethodPost, "/f/" + theForm + "/edit"},
		{http.MethodGet, "/f/" + theForm + "/return?state=" + paybus.StatePaid},
		{http.MethodGet, "/f/" + theForm + "/return?state=" + paybus.StateCancelled},
	}

	for _, p := range pages {
		t.Run(p.method+" "+p.target, func(t *testing.T) {
			r := httptest.NewRequest(p.method, p.target, strings.NewReader(""))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Sec-Fetch-Site", "same-origin")

			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			csp := w.Header().Get("Content-Security-Policy")
			if !strings.Contains(csp, "frame-ancestors https://schoenstatt-austin.us") {
				t.Errorf("CSP = %q\nthe browser will refuse to draw this page in the frame, and the symptom is a blank box", csp)
			}
		})
	}
}

// And the fail-closed answer still holds for what is not a form page. A slug
// with something unrecognised under it is a 404, and a 404 does not inherit a
// form's embedding permissions.
func TestSomethingElseUnderAFormIsNotAFormPage(t *testing.T) {
	cfg := newConfig(t, []types.Origin{mustOrigin(t, "https://schoenstatt-austin.us")}, nil)
	h := embedOf(t, cfg)

	for _, target := range []string{
		"/f/" + theForm + "/pay",
		"/f/" + theForm + "/edit/again",
		"/f/" + theForm + "/",
	} {
		t.Run(target, func(t *testing.T) {
			w := getPage(t, h, target)

			csp := w.Header().Get("Content-Security-Policy")
			if !strings.Contains(csp, "frame-ancestors 'none'") {
				t.Errorf("CSP = %q, want frame-ancestors 'none' for something that is not a page of this form", csp)
			}
		})
	}
}

// --- every page can tell the frame how tall it is ---------------------------

// The bug behind the tall white box on the shrine's page, asserted end to end.
//
// Every page of this surface learns its parent origin from one query
// parameter, because there is nothing else to learn it from: the surface sends
// Referrer-Policy: no-referrer and holds no cookie. The form page had it and
// its own POST target did not, so the confirmation that POST rendered knew no
// parent, never posted its height, and the frame kept the height the form had
// had -- a short receipt at the top of a tall empty box, which is what a
// screenshot of the live page showed.
//
// The symptom is not an error anywhere. Nothing logs, nothing 500s, and the
// right content is on the page. So it is asserted at every hop instead.
func TestEveryPageCanTellTheFrameHowTallItIs(t *testing.T) {
	const parent = "https://schoenstatt-austin.us"

	k := newTill(t)

	blank := getPage(t, k.embed, "/f/"+theForm+"?parent="+url.QueryEscape(parent))
	if blank.Code != http.StatusOK {
		t.Fatalf("the blank form = %d", blank.Code)
	}

	form := blank.Body.String()

	if !strings.Contains(form, `data-parent-origin="`+parent+`"`) {
		t.Fatalf("the form page does not know its parent:\n%s", short(form))
	}

	// The action it posts to has to carry the origin onward.
	action := actionPattern.FindStringSubmatch(form)
	if action == nil {
		t.Fatalf("no form action in the page:\n%s", short(form))
	}
	if !strings.Contains(action[1], "parent=") {
		t.Errorf("the form posts to %q, which drops the parent origin -- the page it renders will go silent and the frame will keep this page's height", action[1])
	}

	// And the page that POST renders must be able to speak.
	done := postTo(t, k.embed, html.UnescapeString(action[1]), filled(grantIn(t, form)))
	if done.Code != http.StatusOK {
		t.Fatalf("the submission = %d:\n%s", done.Code, short(done.Body.String()))
	}

	confirmation := done.Body.String()

	if !strings.Contains(confirmation, `data-parent-origin="`+parent+`"`) {
		t.Errorf("the confirmation cannot tell the frame how tall it is, so it renders inside a box the size of the form:\n%s", short(confirmation))
	}

	// Including the way back, which is another POST and the same trap.
	back := actionPattern.FindStringSubmatch(confirmation)
	if back == nil {
		t.Fatalf("no Back action on the confirmation:\n%s", short(confirmation))
	}
	if !strings.Contains(back[1], "parent=") {
		t.Errorf("Back posts to %q, which drops the parent origin", back[1])
	}

	edited := postTo(t, k.embed, html.UnescapeString(back[1]), url.Values{"name": {"Maria O'Neill"}})
	if !strings.Contains(edited.Body.String(), `data-parent-origin="`+parent+`"`) {
		t.Errorf("the form came back unable to speak to the frame:\n%s", short(edited.Body.String()))
	}
}

// actionPattern reads a form's target out of a rendered page. There are at
// most two per page here, and the first is always the one in question.
var actionPattern = regexp.MustCompile(`<form[^>]*action="([^"]+)"`)

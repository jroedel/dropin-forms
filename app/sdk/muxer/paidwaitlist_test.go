package muxer_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// A waiting list on a form that takes money: the place is held for the person
// offered it, they are emailed a link, and they pay through it -- or it lapses
// and comes back to the office.

var payLinkPattern = regexp.MustCompile(`https://f\.forms\.test(/f/quiet-day/pay\?t=[A-Za-z0-9_.-]+)`)

// paidQuiet is one bed at a registration fee, with a waiting list. Ana has it;
// Ben is waiting; Ana drops out; Ben is offered the bed.
func paidQuiet(t *testing.T) (quietHarness, submissionbus.Submission, string) {
	t.Helper()

	f := quietDay(t, true)
	f.Capacity = 1
	f.Fee = 15000
	f.FeeLabel = "Quiet day"
	f.ReturnURL = "https://example.test/quiet-day"
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("the paid quiet day does not pass Check: %v", err)
	}

	qh := quietSurfaceFor(t, f)

	if w := qh.sign(t, "Ana"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Continue to payment") {
		t.Fatalf("Ana signing up = %d:\n%s", w.Code, short(w.Body.String()))
	}

	// Ben is told he is waiting, and nobody sends him to pay for a place he
	// does not have.
	if w := qh.sign(t, "Ben"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "You are on the waiting list") || strings.Contains(w.Body.String(), "Continue to payment") {
		t.Fatalf("Ben joining the waiting list = %d:\n%s", w.Code, short(w.Body.String()))
	}

	if len(qh.pay.orders) != 1 {
		t.Fatalf("%d payment pages were made, want only Ana's", len(qh.pay.orders))
	}

	ana := qh.waitingOrIn(t, "Ana")
	if _, _, err := qh.subs.Hide(t.Context(), time.Now(), ana.ID, types.NewID()); err != nil {
		t.Fatalf("Hide: %v", err)
	}

	ben := qh.waiting(t)[0]
	page := "/forms/quiet-day/submissions/" + ben.ID.String()

	if w := qh.post(t, page+"/offer", qh.cookie); w.Code != http.StatusSeeOther || w.Header().Get("Location") != page {
		t.Fatalf("offering Ben the bed = %d to %q", w.Code, w.Header().Get("Location"))
	}

	m := qh.last(t, "ben@example.org")
	if !strings.HasSuffix(m.Subject, ": a place is held for you") || !strings.Contains(m.Text, "$150.00") {
		t.Errorf("Ben was sent %q:\n%s", m.Subject, m.Text)
	}

	link := payLinkPattern.FindStringSubmatch(m.Text)
	if link == nil {
		t.Fatalf("the offer has no link to pay:\n%s", m.Text)
	}

	return qh, ben, link[1]
}

func TestAnOfferedPaidPlaceIsPaidForThroughTheLink(t *testing.T) {
	qh, ben, link := paidQuiet(t)

	got, err := qh.subs.ByID(t.Context(), ben.ID)
	if err != nil || got.Status != submissionbus.StatusOffered {
		t.Fatalf("Ben is %s, %v; want offered", got.Status, err)
	}

	// The office sees it held, and when until.
	if results := qh.read(t, "/forms/quiet-day/submissions", qh.cookie).Body.String(); !strings.Contains(results, "Offered a place, not paid yet") {
		t.Errorf("the results page does not show the held place:\n%s", short(results))
	}

	// Opening the link shows the place and starts nothing: a mail scanner
	// following it must not make a payment page.
	before := len(qh.pay.orders)

	w := getPage(t, qh.embed, link)
	body := w.Body.String()

	if w.Code != http.StatusOK || !strings.Contains(body, "holding it for you until") || !strings.Contains(body, "Pay now") || !strings.Contains(body, "$150.00") {
		t.Fatalf("the payment link = %d:\n%s", w.Code, short(body))
	}

	if len(qh.pay.orders) != before {
		t.Error("opening the payment link made a payment page")
	}

	token := strings.SplitN(link, "?t=", 2)[1]

	w = postTo(t, qh.embed, "/f/quiet-day/pay", url.Values{"t": {token}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Continue to payment") {
		t.Fatalf("pressing Pay now = %d:\n%s", w.Code, short(w.Body.String()))
	}

	if len(qh.pay.orders) != before+1 {
		t.Fatalf("pressing Pay now made %d payment pages", len(qh.pay.orders)-before)
	}

	o := qh.pay.orders[len(qh.pay.orders)-1]
	if o.SubmissionID != ben.ID || o.Total != 15000 || o.Attempt == "" {
		t.Errorf("the payment page was for %+v", o)
	}

	// A second press, after closing the first page, is a second page.
	if w := postTo(t, qh.embed, "/f/quiet-day/pay", url.Values{"t": {token}}); w.Code != http.StatusOK || len(qh.pay.orders) != before+2 {
		t.Errorf("a second Pay now = %d, %d pages", w.Code, len(qh.pay.orders)-before)
	}

	// Stripe says it is paid; the link now says so too.
	if err := qh.subs.Settle(t.Context(), time.Now(), ben.ID, submissionbus.StatusPaid, "pi_ben"); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	if body := getPage(t, qh.embed, link).Body.String(); !strings.Contains(body, "This place is paid for") {
		t.Errorf("the link to a paid place:\n%s", short(body))
	}

	if csv := qh.read(t, "/forms/quiet-day/submissions.csv", qh.cookie).Body.String(); !strings.Contains(csv, "Ben") {
		t.Errorf("Ben, paid, is not in the CSV:\n%s", csv)
	}
}

func TestAnUnpaidOfferLapsesAndTheLinkSaysSo(t *testing.T) {
	qh, ben, link := paidQuiet(t)

	if _, err := qh.subs.Abandon(t.Context(), time.Now().Add(submissionbus.OfferHold+time.Hour), time.Hour); err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	if got, _ := qh.subs.ByID(t.Context(), ben.ID); got.Status != submissionbus.StatusAbandoned {
		t.Fatalf("Ben's lapsed offer is %s, want abandoned", got.Status)
	}

	if body := getPage(t, qh.embed, link).Body.String(); !strings.Contains(body, "no longer being held") || strings.Contains(body, "Pay now") {
		t.Errorf("the link to a lapsed offer:\n%s", short(body))
	}

	before := len(qh.pay.orders)
	token := strings.SplitN(link, "?t=", 2)[1]

	if w := postTo(t, qh.embed, "/f/quiet-day/pay", url.Values{"t": {token}}); !strings.Contains(w.Body.String(), "no longer being held") || len(qh.pay.orders) != before {
		t.Errorf("paying for a lapsed offer = %d:\n%s", w.Code, short(w.Body.String()))
	}
}

// A link that is not ours, or one for changing answers rather than paying,
// opens nothing.
func TestAPaymentLinkThatIsNotOursOpensNothing(t *testing.T) {
	qh, ben, link := paidQuiet(t)

	for _, bad := range []string{
		"/f/quiet-day/pay?t=nonsense",
		strings.Replace(link, "?t=", "?t=x", 1),
		"/f/quiet-day/pay?t=" + mustAnswerLink(t, "quiet-day", ben.ID),
	} {
		if w := getPage(t, qh.embed, bad); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "did not work") {
			t.Errorf("GET %s = %d:\n%s", bad, w.Code, short(w.Body.String()))
		}
	}
}

func mustAnswerLink(t *testing.T, form string, id types.ID) string {
	t.Helper()

	token, err := submissionbus.MintAnswerLink(answerKey, mustSlug(t, form), id)
	if err != nil {
		t.Fatalf("MintAnswerLink: %v", err)
	}

	return token
}

// waitingOrIn finds a submission by its name, whatever its status.
func (qh quietHarness) waitingOrIn(t *testing.T, name string) submissionbus.Submission {
	t.Helper()

	all, err := qh.subs.Unsettled(t.Context(), mustSlug(t, "quiet-day"))
	if err != nil {
		t.Fatalf("Unsettled: %v", err)
	}

	settled, err := qh.subs.ByForm(t.Context(), mustSlug(t, "quiet-day"))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}

	for _, s := range append(all, settled...) {
		if a, ok := s.Answers.Field("name"); ok && a.Value() == name {
			return s
		}
	}

	t.Fatalf("no submission from %s", name)

	return submissionbus.Submission{}
}

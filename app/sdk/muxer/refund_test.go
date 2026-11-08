package muxer_test

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/payment/paybus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// Refunding from a submission's page, through the admin surface as it is
// mounted, and a refund made in Stripe's dashboard arriving by webhook.

// refunder is the refund side of Stripe: it records what it was asked to
// refund and never reaches the network.
type refunder struct {
	orders []paybus.RefundOrder
	err    error
}

func (r *refunder) Refund(_ context.Context, o paybus.RefundOrder) (paybus.RefundDone, error) {
	if r.err != nil {
		return paybus.RefundDone{}, r.err
	}

	r.orders = append(r.orders, o)

	return paybus.RefundDone{ID: fmt.Sprintf("re_test_%d", len(r.orders)), Amount: o.Amount}, nil
}

// paidBen is the one-bed paid quiet day with Ben in the bed, paid for, and an
// administrator's cookie beside the results reader's.
func paidBen(t *testing.T) (quietHarness, submissionbus.Submission, string, string) {
	t.Helper()

	qh, ben, _ := paidQuiet(t)

	if err := qh.subs.Settle(t.Context(), time.Now(), ben.ID, submissionbus.StatusPaid, "pi_ben"); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	boss, cookie := qh.reader(t, "treasurer@schoenstatt.test")
	qh.grant(t, boss, "quiet-day", accessbus.RoleAdmin)

	return qh, ben, "/forms/quiet-day/submissions/" + ben.ID.String(), cookie
}

func (qh quietHarness) now(t *testing.T, id types.ID) submissionbus.Submission {
	t.Helper()

	s, err := qh.subs.ByID(t.Context(), id)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	return s
}

func TestOnlyAnAdministratorIsOfferedARefund(t *testing.T) {
	qh, ben, page, boss := paidBen(t)

	// Whoever reads the sign-ups sees no button, and cannot press one.
	if body := qh.read(t, page, qh.cookie).Body.String(); strings.Contains(body, page+`/refund"`) {
		t.Errorf("a results reader is offered a refund:\n%s", short(body))
	}

	w := qh.postValues(t, page+"/refund", qh.cookie, url.Values{"before": {"0"}, "amount": {"150.00"}, "cancel": {"yes"}})
	if w.Code != http.StatusForbidden || len(qh.refund.orders) != 0 {
		t.Fatalf("a results reader's refund = %d with %d refunds made, want 403 and none", w.Code, len(qh.refund.orders))
	}

	body := qh.read(t, page, boss).Body.String()
	for _, want := range []string{`action="` + page + `/refund"`, `value="150.00"`, "Cancel the registration and give their place back"} {
		if !strings.Contains(body, want) {
			t.Errorf("the administrator's page does not contain %q:\n%s", want, short(body))
		}
	}

	// Another site cannot press it on the administrator's behalf.
	r := httptest.NewRequest(http.MethodPost, page+"/refund", strings.NewReader("before=0&amount=150.00"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.AddCookie(&http.Cookie{Name: mid.CookieName, Value: boss})

	w = httptest.NewRecorder()
	qh.admin.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden || len(qh.refund.orders) != 0 {
		t.Errorf("a cross-site refund = %d with %d refunds made, want 403 and none", w.Code, len(qh.refund.orders))
	}

	if got := qh.now(t, ben.ID); got.Refunded != 0 || got.Status != submissionbus.StatusPaid {
		t.Errorf("after two refused refunds Ben is %s with %s refunded", got.Status, got.Refunded)
	}
}

func TestAPartialRefundKeepsThePlaceAndTheRestIsCollected(t *testing.T) {
	qh, ben, page, boss := paidBen(t)

	w := qh.postValues(t, page+"/refund", boss, url.Values{"before": {"0"}, "amount": {"25"}, "note": {"The Saturday lunch is on us."}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != page+"?refund=done" {
		t.Fatalf("refunding $25 = %d to %q", w.Code, w.Header().Get("Location"))
	}

	if len(qh.refund.orders) != 1 {
		t.Fatalf("%d refunds were made at Stripe, want 1", len(qh.refund.orders))
	}

	if o := qh.refund.orders[0]; o.Amount != 2500 || o.PaymentRef != "pi_ben" || o.SubmissionID != ben.ID || o.Key == "" {
		t.Errorf("Stripe was asked for %+v", o)
	}

	if got := qh.now(t, ben.ID); got.Refunded != 2500 || got.Status != submissionbus.StatusPaid {
		t.Fatalf("Ben is %s with %s refunded, want paid with 25.00", got.Status, got.Refunded)
	}

	m := qh.last(t, "ben@example.org")
	if !strings.HasSuffix(m.Subject, ": your refund") || !strings.Contains(m.Text, "$25.00") || !strings.Contains(m.Text, "The Saturday lunch is on us.") {
		t.Errorf("Ben was sent %q:\n%s", m.Subject, m.Text)
	}

	body := qh.read(t, page+"?refund=done", boss).Body.String()
	for _, want := range []string{"Done. They have been emailed.", "$25.00 refunded; $125.00 kept.", "by treasurer@schoenstatt.test", `value="125.00"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page after the refund does not contain %q:\n%s", want, short(body))
		}
	}

	if results := qh.read(t, "/forms/quiet-day/submissions", qh.cookie).Body.String(); !strings.Contains(results, "Collected <strong>$125.00</strong>") {
		t.Errorf("the results page does not count what was kept:\n%s", short(results))
	}

	rows, err := csv.NewReader(strings.NewReader(qh.read(t, "/forms/quiet-day/submissions.csv", qh.cookie).Body.String())).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("the export: %d rows, %v", len(rows), err)
	}

	if last := rows[1][len(rows[1])-1]; rows[0][len(rows[0])-1] != "refunded" || last != "25.00" {
		t.Errorf("the export's last column is %q = %q, want refunded = 25.00", rows[0][len(rows[0])-1], last)
	}

	// The same page pressed again is out of date, and refunds nothing.
	w = qh.postValues(t, page+"/refund", boss, url.Values{"before": {"0"}, "amount": {"25"}})
	if w.Header().Get("Location") != page+"?refund=changed" || len(qh.refund.orders) != 1 {
		t.Errorf("a refund from a stale page went to %q with %d refunds made", w.Header().Get("Location"), len(qh.refund.orders))
	}

	// More than is left is refused before Stripe is asked.
	w = qh.postValues(t, page+"/refund", boss, url.Values{"before": {"2500"}, "amount": {"125.01"}})
	if w.Header().Get("Location") != page+"?refund=too-much" || len(qh.refund.orders) != 1 {
		t.Errorf("refunding more than is left went to %q with %d refunds made", w.Header().Get("Location"), len(qh.refund.orders))
	}
}

func TestAFullRefundThatCancelsGivesTheBedToTheNextOnTheList(t *testing.T) {
	qh, ben, page, boss := paidBen(t)

	if w := qh.sign(t, "Cora"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "You are on the waiting list") {
		t.Fatalf("Cora joining the waiting list = %d:\n%s", w.Code, short(w.Body.String()))
	}

	cora := "/forms/quiet-day/submissions/" + qh.waiting(t)[0].ID.String()

	// The list draws Offer only while a place is free.
	list := "/forms/quiet-day/submissions"

	if body := qh.read(t, list, qh.cookie).Body.String(); strings.Contains(body, cora+`/offer"`) {
		t.Fatalf("Cora can be offered a bed Ben still holds:\n%s", short(body))
	}

	// The whole amount, as the page fills it in, with the box ticked as the
	// page ticks it.
	w := qh.postValues(t, page+"/refund", boss, url.Values{"before": {"0"}, "amount": {"150.00"}, "cancel": {"yes"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != page+"?refund=done" {
		t.Fatalf("the full refund = %d to %q", w.Code, w.Header().Get("Location"))
	}

	if got := qh.now(t, ben.ID); got.Refunded != 15000 || got.Status != submissionbus.StatusCancelled {
		t.Fatalf("Ben is %s with %s refunded, want cancelled with 150.00", got.Status, got.Refunded)
	}

	if m := qh.last(t, "ben@example.org"); !strings.HasSuffix(m.Subject, ": refunded and cancelled") {
		t.Errorf("Ben was sent %q:\n%s", m.Subject, m.Text)
	}

	if body := qh.read(t, list, qh.cookie).Body.String(); !strings.Contains(body, cora+`/offer"`) {
		t.Errorf("the bed Ben gave back cannot be offered to Cora:\n%s", short(body))
	}

	// Ben has left the list and the totals, and is found under the toggle.
	results := qh.read(t, list, qh.cookie).Body.String()
	if strings.Contains(results, ben.ID.String()) || !strings.Contains(results, "1 cancelled registration is not") {
		t.Errorf("the results page after Ben cancelled:\n%s", short(results))
	}

	if shown := qh.read(t, "/forms/quiet-day/submissions?cancelled=1", qh.cookie).Body.String(); !strings.Contains(shown, ben.ID.String()) {
		t.Errorf("Ben is not under the cancelled toggle:\n%s", short(shown))
	}

	// Nothing left to do on his page: no form.
	if body := qh.read(t, page, boss).Body.String(); strings.Contains(body, page+`/refund"`) || !strings.Contains(body, "This registration is cancelled") {
		t.Errorf("Ben's page after a full refund and cancel:\n%s", short(body))
	}
}

func TestARefundStripeRefusesIsNotRecorded(t *testing.T) {
	qh, ben, page, boss := paidBen(t)
	qh.refund.err = errors.New("the Stripe key cannot make refunds")

	w := qh.postValues(t, page+"/refund", boss, url.Values{"before": {"0"}, "amount": {"150.00"}, "cancel": {"yes"}})
	if w.Header().Get("Location") != page+"?refund=refused" {
		t.Fatalf("a refused refund went to %q", w.Header().Get("Location"))
	}

	if got := qh.now(t, ben.ID); got.Refunded != 0 || got.Status != submissionbus.StatusPaid {
		t.Errorf("after Stripe refused, Ben is %s with %s refunded", got.Status, got.Refunded)
	}

	if body := qh.read(t, page+"?refund=refused", boss).Body.String(); !strings.Contains(body, "Refunds: Write") {
		t.Errorf("the page does not say what to do:\n%s", short(body))
	}
}

// refundedEvent is what Stripe sends when a charge is refunded, from the
// dashboard or anywhere else: the running total, against the payment.
func refundedEvent(id, intent string, total int64) string {
	return `{
  "id": "` + id + `",
  "object": "event",
  "api_version": "2026-03-31.basil",
  "type": "charge.refunded",
  "data": {
    "object": {
      "id": "ch_test_1",
      "object": "charge",
      "amount": 2400,
      "amount_refunded": ` + fmt.Sprint(total) + `,
      "currency": "usd",
      "payment_intent": "` + intent + `"
    }
  }
}`
}

func TestARefundMadeInStripeReachesTheSubmission(t *testing.T) {
	k := newTill(t)

	sub := k.order(t, nil)

	if w := k.deliver(t, paidEvent("evt_1", sub.ID), time.Now()); w.Code != http.StatusOK {
		t.Fatalf("the paid event = %d", w.Code)
	}

	if w := k.deliver(t, refundedEvent("evt_2", "pi_test_1", 1000), time.Now()); w.Code != http.StatusOK {
		t.Fatalf("the refunded event = %d: %s", w.Code, w.Body.String())
	}

	got, err := k.subs.ByID(t.Context(), sub.ID)
	if err != nil || got.Refunded != 1000 || got.Status != submissionbus.StatusPaid {
		t.Fatalf("after a $10 refund in Stripe the order is %s with %s refunded, %v", got.Status, got.Refunded, err)
	}

	// The same notification again, and a later one that says less, change
	// nothing; a second refund raises the total to Stripe's figure.
	k.deliver(t, refundedEvent("evt_2", "pi_test_1", 1000), time.Now())
	k.deliver(t, refundedEvent("evt_3", "pi_test_1", 500), time.Now())
	k.deliver(t, refundedEvent("evt_4", "pi_test_1", 2400), time.Now())

	if got, _ := k.subs.ByID(t.Context(), sub.ID); got.Refunded != 2400 {
		t.Errorf("after the full refund the order has %s refunded, want 24.00", got.Refunded)
	}

	// One for a payment that is not ours is acknowledged, so Stripe stops
	// sending it.
	if w := k.deliver(t, refundedEvent("evt_5", "pi_somebody_else", 100), time.Now()); w.Code != http.StatusOK {
		t.Errorf("a refund on a payment that is not ours = %d", w.Code)
	}
}

package muxer_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/domain/paymentapp"
	"github.com/jroedel/dropin-forms/app/sdk/muxer"
	"github.com/jroedel/dropin-forms/business/domain/payment/paybus"
	"github.com/jroedel/dropin-forms/business/domain/payment/stores/paydb"
	"github.com/jroedel/dropin-forms/business/domain/payment/stores/stripepay"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/logger"
)

// Taking a payment, through the surfaces as they are mounted.
//
// The journey these tests follow is the real one and it crosses two of the
// three surfaces: a submission arrives on the embed surface and is stored
// pending, and a webhook arrives on its own listener and is the only thing
// that may mark it paid. That separation is the point of the whole
// arrangement, so the last test here is the one that asserts the webhook is
// reachable on neither of the other two.
//
// The signature is computed here with Stripe's public scheme -- HMAC-SHA256
// over "<unix>.<body>" -- because a recorded signature cannot be re-signed for
// a different body, and every case below is a different body.

const webhookSecret = "whsec_a_test_endpoint_signing_secret"

func stripeSignature(t *testing.T, body string, at time.Time) string {
	t.Helper()

	mac := hmac.New(sha256.New, []byte(webhookSecret))
	fmt.Fprintf(mac, "%d.%s", at.Unix(), body)

	return fmt.Sprintf("t=%d,v1=%s", at.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

// paidEvent is what Stripe sends when a hosted Checkout session is paid.
func paidEvent(id string, submission types.ID) string {
	return `{
  "id": "` + id + `",
  "object": "event",
  "api_version": "2026-03-31.basil",
  "type": "checkout.session.completed",
  "data": {
    "object": {
      "id": "cs_test_1",
      "object": "checkout.session",
      "amount_total": 2400,
      "currency": "usd",
      "payment_status": "paid",
      "payment_intent": "pi_test_1",
      "metadata": {"submission_id": "` + submission.String() + `", "form": "` + theForm + `"}
    }
  }
}`
}

// counter is a Payments that records what it was asked to start, and never
// reaches the network. Used on the *embed* surface: creating a real Checkout
// session would need a Stripe account.
type counter struct {
	orders []paybus.Order
	err    error
}

func (c *counter) Start(_ context.Context, o paybus.Order) (paybus.Handoff, error) {
	if c.err != nil {
		return paybus.Handoff{}, c.err
	}

	c.orders = append(c.orders, o)

	return paybus.Handoff{
		Ref: "cs_test_1",
		URL: "https://checkout.stripe.com/c/pay/cs_test_1",
	}, nil
}

// till is all three surfaces over one database, with a real payment domain
// behind the webhook and a counting one behind the form.
//
// The webhook side is real all the way down -- real signature verification,
// real event ledger, real submission domain -- because that is the path that
// decides whether money was collected, and a fake anywhere in it would be
// testing the fake.
type till struct {
	embed   http.Handler
	admin   http.Handler
	webhook http.Handler

	subs *submissionbus.Business
	pay  *counter
}

func newTill(t *testing.T) till {
	t.Helper()

	cfg := newConfig(t, []types.Origin{mustOrigin(t, "https://schoenstatt-austin.us")}, nil)
	cfg.Embed.Now = func() time.Time { return beforeTheFeast }

	subs, ok := cfg.Embed.Submissions.(*submissionbus.Business)
	if !ok {
		t.Fatalf("the test config holds a %T rather than the submission domain", cfg.Embed.Submissions)
	}

	if err := paydb.Init(t.Context(), cfg.DB); err != nil {
		t.Fatalf("paydb.Init: %v", err)
	}

	gateway, err := stripepay.New("sk_test_not_a_real_key", webhookSecret)
	if err != nil {
		t.Fatalf("stripepay.New: %v", err)
	}

	cfg.Payments = paybus.NewBusiness(
		logger.New(io.Discard, slog.LevelError), gateway, paydb.NewStore(cfg.DB), subs)

	pay := &counter{}
	cfg.Embed.Payments = pay

	hook, err := muxer.Webhook(cfg)
	if err != nil {
		t.Fatalf("muxer.Webhook: %v", err)
	}

	return till{
		embed:   embedOf(t, cfg),
		admin:   adminOf(t, cfg),
		webhook: hook,
		subs:    subs,
		pay:     pay,
	}
}

// order posts a submission through the public surface and returns it.
func (k till) order(t *testing.T, values url.Values) submissionbus.Submission {
	t.Helper()

	w := getPage(t, k.embed, "/f/"+theForm)
	if w.Code != http.StatusOK {
		t.Fatalf("the blank form = %d", w.Code)
	}

	v := filled(grantIn(t, w.Body.String()))
	for key, val := range values {
		v[key] = val
	}

	w = postForm(t, k.embed, v)
	if w.Code != http.StatusOK {
		t.Fatalf("the submission = %d:\n%s", w.Code, short(w.Body.String()))
	}

	stored, err := k.subs.ByForm(t.Context(), mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored %d submissions, want 1", len(stored))
	}

	return stored[0]
}

// deliver posts a signed event to the webhook surface.
func (k till) deliver(t *testing.T, body string, at time.Time) *httptest.ResponseRecorder {
	t.Helper()

	return k.deliverWith(t, body, stripeSignature(t, body, at))
}

func (k till) deliverWith(t *testing.T, body, signature string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, "/stripe/webhook", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")

	if signature != "" {
		r.Header.Set(paymentapp.SignatureHeader, signature)
	}

	w := httptest.NewRecorder()
	k.webhook.ServeHTTP(w, r)

	return w
}

// --- the whole journey ------------------------------------------------------

// Submit, hand off to Stripe, be told it was paid. The one test here that
// would notice almost any break in the payment path.
func TestASubmissionIsPendingUntilStripeSaysOtherwise(t *testing.T) {
	k := newTill(t)

	sub := k.order(t, nil)

	// Stored, and not paid. Nothing about the person's own request may decide
	// that: they have been handed a link, and that is all.
	if sub.Status != submissionbus.StatusPending {
		t.Fatalf("a new order is %q, want pending", sub.Status)
	}

	// The payment was started once, in the POST, with the order the validator
	// derived.
	if len(k.pay.orders) != 1 {
		t.Fatalf("the payment was started %d times, want once", len(k.pay.orders))
	}

	o := k.pay.orders[0]
	if o.SubmissionID != sub.ID {
		t.Errorf("the payment names submission %q, want %q", o.SubmissionID, sub.ID)
	}
	if o.Total != sub.Answers.Total {
		t.Errorf("the payment is for %s and the order came to %s", o.Total, sub.Answers.Total)
	}
	if o.Email.String() != "maria@example.org" {
		t.Errorf("the payment carries the email %q; Stripe needs it for the receipt and for its own fraud checks", o.Email)
	}

	// Now the webhook, which is the only authority on "paid".
	if w := k.deliver(t, paidEvent("evt_1", sub.ID), time.Now()); w.Code != http.StatusOK {
		t.Fatalf("the webhook = %d:\n%s", w.Code, w.Body.String())
	}

	settled, err := k.subs.ByID(t.Context(), sub.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if settled.Status != submissionbus.StatusPaid {
		t.Errorf("after the webhook the submission is %q, want paid", settled.Status)
	}
	if settled.PaymentRef != "pi_test_1" {
		t.Errorf("the payment reference is %q, want the payment intent", settled.PaymentRef)
	}
}

// The confirmation page hands somebody a link out of the frame, and the
// receipt on it is the same itemisation Stripe was given.
func TestTheConfirmationOffersAWayToPayAndAddsUp(t *testing.T) {
	k := newTill(t)

	w := getPage(t, k.embed, "/f/"+theForm)
	if w.Code != http.StatusOK {
		t.Fatalf("the blank form = %d", w.Code)
	}

	v := filled(grantIn(t, w.Body.String()))
	v["donation"] = []string{"5.00"}

	w = postForm(t, k.embed, v)
	if w.Code != http.StatusOK {
		t.Fatalf("the submission = %d:\n%s", w.Code, short(w.Body.String()))
	}

	body := w.Body.String()

	// A real link, out of the frame, and to Stripe.
	if !strings.Contains(body, `href="https://checkout.stripe.com/c/pay/cs_test_1"`) {
		t.Errorf("the confirmation offers no way to pay:\n%s", short(body))
	}
	if !strings.Contains(body, `target="_top"`) {
		t.Error("the payment link does not leave the frame, so clicking it loads Stripe inside the iframe")
	}
	if !strings.Contains(body, `rel="noopener"`) {
		t.Error("the payment link has no rel=noopener")
	}

	// The receipt shows the donation as well as the tickets. This is the
	// regression test for a real bug: the receipt used to be built from the
	// priced items alone, so a donation appeared in the total and on no line,
	// and the page did not add up.
	for _, want := range []string{"Lunch ticket", "Donation for the Shrine", "$24.00", "$5.00", "$29.00"} {
		if !strings.Contains(body, want) {
			t.Errorf("the receipt does not show %q:\n%s", want, short(body))
		}
	}

	// And the same figures went to Stripe.
	o := k.pay.orders[0]
	if o.Total != 2900 {
		t.Errorf("Stripe was asked for %s and the page says $29.00", o.Total)
	}
	if len(o.Lines) != 2 {
		t.Errorf("Stripe was given %d lines, want the tickets and the donation", len(o.Lines))
	}
}

// A GET of a blank form must create nothing at Stripe. Otherwise an
// unauthenticated crawler mints payment objects at crawl rate.
func TestRenderingAFormCreatesNoPaymentAtAll(t *testing.T) {
	k := newTill(t)

	for range 5 {
		if w := getPage(t, k.embed, "/f/"+theForm); w.Code != http.StatusOK {
			t.Fatalf("the blank form = %d", w.Code)
		}
	}

	if len(k.pay.orders) != 0 {
		t.Errorf("rendering the form started %d payments; a crawler would mint one per request", len(k.pay.orders))
	}
}

// Stripe being unreachable must not lose an order or invite a second one.
func TestAnUnreachableStripeStillKeepsTheOrder(t *testing.T) {
	k := newTill(t)
	k.pay.err = fmt.Errorf("dial tcp: connection refused")

	w := getPage(t, k.embed, "/f/"+theForm)
	v := filled(grantIn(t, w.Body.String()))

	w = postForm(t, k.embed, v)

	// Not a 500. The submission is stored and safe, and answering with an
	// error page would tell somebody their order was lost when it was not --
	// and re-rendering the form would invite them to place it again.
	if w.Code != http.StatusOK {
		t.Fatalf("the submission = %d, want 200 with an explanation:\n%s", w.Code, short(w.Body.String()))
	}

	body := w.Body.String()
	if !strings.Contains(body, "We have your order") {
		t.Errorf("the page does not say the order is safe:\n%s", short(body))
	}
	if strings.Contains(body, "checkout.stripe.com") {
		t.Error("the page offers a payment link although the session was never created")
	}

	stored, err := k.subs.ByForm(t.Context(), mustSlug(t, theForm))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(stored) != 1 || stored[0].Status != submissionbus.StatusPending {
		t.Errorf("the order was not kept as pending: %+v", stored)
	}
}

// --- what the webhook surface refuses ---------------------------------------

func TestTheWebhookRefusesWhatStripeDidNotSign(t *testing.T) {
	k := newTill(t)
	sub := k.order(t, nil)

	body := paidEvent("evt_1", sub.ID)

	// No signature at all, which is what a scanner sends.
	if w := k.deliverWith(t, body, ""); w.Code != http.StatusBadRequest {
		t.Errorf("an unsigned delivery = %d, want 400", w.Code)
	}

	// A signature for a different body: the amount edited after signing.
	tampered := strings.Replace(body, `"amount_total": 2400`, `"amount_total": 1`, 1)
	if tampered == body {
		t.Fatal("the test did not change the body")
	}

	if w := k.deliverWith(t, tampered, stripeSignature(t, body, time.Now())); w.Code != http.StatusBadRequest {
		t.Errorf("an edited body = %d, want 400", w.Code)
	}

	// And an old one, which is a captured delivery being replayed.
	if w := k.deliver(t, body, time.Now().Add(-time.Hour)); w.Code != http.StatusBadRequest {
		t.Errorf("an hour-old delivery = %d, want 400", w.Code)
	}

	// None of which changed anything.
	after, err := k.subs.ByID(t.Context(), sub.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if after.Status != submissionbus.StatusPending {
		t.Errorf("a refused notification moved the submission to %q", after.Status)
	}
}

// Stripe retries until acknowledged, so a second delivery of one event has to
// be a 200. Anything else asks it to keep trying.
func TestTheSameDeliveryTwiceIsAcknowledgedTwice(t *testing.T) {
	k := newTill(t)
	sub := k.order(t, nil)

	body := paidEvent("evt_1", sub.ID)

	for i := range 3 {
		if w := k.deliver(t, body, time.Now()); w.Code != http.StatusOK {
			t.Fatalf("delivery %d = %d:\n%s", i+1, w.Code, w.Body.String())
		}
	}

	settled, err := k.subs.ByID(t.Context(), sub.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if settled.Status != submissionbus.StatusPaid {
		t.Errorf("the submission is %q after three deliveries", settled.Status)
	}
}

// An event about a submission we do not have is acknowledged, not refused: an
// account can have other integrations, and refusing makes Stripe retry for
// days.
func TestAnEventAboutSomebodyElsesPaymentIsAcknowledged(t *testing.T) {
	k := newTill(t)

	body := `{
  "id": "evt_elsewhere",
  "object": "event",
  "api_version": "2026-03-31.basil",
  "type": "checkout.session.completed",
  "data": {"object": {"id": "cs_x", "object": "checkout.session",
    "payment_status": "paid", "payment_intent": "pi_x", "metadata": {}}}
}`

	if w := k.deliver(t, body, time.Now()); w.Code != http.StatusOK {
		t.Errorf("an event with no order = %d, want 200:\n%s", w.Code, w.Body.String())
	}
}

// The method and the route. A GET of the webhook is not a webhook.
func TestTheWebhookAnswersOnlyAPost(t *testing.T) {
	k := newTill(t)

	w := httptest.NewRecorder()
	k.webhook.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/stripe/webhook", nil))

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /stripe/webhook = %d, want 405", w.Code)
	}

	// The listener answers /healthz, because the deploy checks each one
	// separately and a release where two of the three came up is the failure
	// most worth catching.
	w = httptest.NewRecorder()
	k.webhook.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if w.Code != http.StatusOK {
		t.Errorf("GET /healthz on the webhook listener = %d", w.Code)
	}
}

// The separation the three listeners exist for.
//
// If the webhook were reachable on the embed or admin surface it would be
// behind that surface's origin gate, which refuses a request with no
// Sec-Fetch-Site and no Origin -- exactly what Stripe sends. So this asserts
// both halves: the route is not there, and the webhook's own surface does not
// have that gate.
func TestTheWebhookIsOnItsOwnListenerAndHasNoOriginGate(t *testing.T) {
	k := newTill(t)
	sub := k.order(t, nil)

	for name, h := range map[string]http.Handler{"embed": k.embed, "admin": k.admin} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/stripe/webhook", strings.NewReader("{}"))
			r.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code == http.StatusOK {
				t.Fatalf("the %s surface answered the webhook route", name)
			}
		})
	}

	// And on its own listener, a delivery with no browser headers at all is
	// accepted. This is the assertion that fails if somebody adds
	// SameOriginOnly to the webhook chain "for consistency".
	body := paidEvent("evt_1", sub.ID)

	r := httptest.NewRequest(http.MethodPost, "/stripe/webhook", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(paymentapp.SignatureHeader, stripeSignature(t, body, time.Now()))
	// Deliberately no Origin and no Sec-Fetch-Site, which is what a server
	// posting to us sends.

	w := httptest.NewRecorder()
	k.webhook.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("a header-less delivery = %d, want 200: an origin gate here would refuse every real payment notification\n%s",
			w.Code, w.Body.String())
	}
}

// The webhook surface must be refused when it has no payment domain, rather
// than mounted as a public URL that verifies nothing.
func TestTheWebhookSurfaceRefusesToBeBuiltWithoutAPaymentDomain(t *testing.T) {
	cfg := newConfig(t, nil, nil)

	if _, err := muxer.Webhook(cfg); err == nil {
		t.Error("muxer.Webhook built a surface with no payment domain behind it")
	}
}

package stripepay_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/payment/paybus"
	"github.com/jroedel/dropin-forms/business/domain/payment/stores/stripepay"
	"github.com/jroedel/dropin-forms/business/types"
)

// Verifying what Stripe sends, which is the one thing on this surface standing
// between a public URL and a route that marks orders paid.
//
// The signatures below are computed here rather than recorded from Stripe,
// because a recorded one cannot be re-signed for a different body and every
// interesting case in this file is a different body. The scheme is Stripe's
// and is public: HMAC-SHA256 over "<unix timestamp>.<exact body>", hex, in a
// header that names the timestamp separately. Getting that wrong here would
// make every test pass against a verifier that accepts anything, so the first
// test below is the one that proves the harness can fail.

const secret = "whsec_a_test_endpoint_signing_secret"

// sign produces the Stripe-Signature header for a body at an instant.
func sign(t *testing.T, body string, at time.Time) string {
	t.Helper()

	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.%s", at.Unix(), body)

	return fmt.Sprintf("t=%d,v1=%s", at.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

func gateway(t *testing.T) *stripepay.Gateway {
	t.Helper()

	g, err := stripepay.New("sk_test_not_a_real_key", secret)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return g
}

// completed is a checkout.session.completed body.
//
// Written out rather than built with the SDK's types, for the same reason the
// gateway reads the fields by hand: what is being tested is that this service
// understands the JSON Stripe actually sends, and a body produced by
// marshalling the SDK's own structs would test that the SDK agrees with itself.
//
// api_version is deliberately an old one. See the last test in this file.
func completed(submissionID, paymentStatus string) string {
	return `{
  "id": "evt_1PaidExample",
  "object": "event",
  "api_version": "2019-05-16",
  "type": "checkout.session.completed",
  "created": 1760000000,
  "data": {
    "object": {
      "id": "cs_test_aSession",
      "object": "checkout.session",
      "amount_total": 2900,
      "currency": "usd",
      "payment_status": "` + paymentStatus + `",
      "payment_intent": "pi_3PaidExample",
      "metadata": {"submission_id": "` + submissionID + `", "form": "feast-lunch-2026"}
    }
  }
}`
}

func TestAGoodSignatureIsAcceptedAndABadOneIsNot(t *testing.T) {
	g := gateway(t)

	id := types.NewID()
	body := completed(id.String(), "paid")
	now := time.Now()

	e, err := g.Verify([]byte(body), sign(t, body, now))
	if err != nil {
		t.Fatalf("Verify refused a correctly signed body: %v", err)
	}

	if e.ID != "evt_1PaidExample" {
		t.Errorf("event id = %q", e.ID)
	}
	if e.Result != paybus.ResultPaid {
		t.Errorf("result = %q, want paid", e.Result)
	}
	if e.SubmissionID != id {
		t.Errorf("submission = %q, want %q", e.SubmissionID, id)
	}

	// The payment intent, not the session. That is what a refund, a dispute
	// and the dashboard's own search all key on.
	if e.Ref != "pi_3PaidExample" {
		t.Errorf("ref = %q, want the payment intent", e.Ref)
	}
	if e.Total != 2900 {
		t.Errorf("total = %s, want 2900 cents", e.Total)
	}

	// And the harness can fail. One byte changed in the body, with the
	// signature that was correct for the original -- which is what a
	// man-in-the-middle or a replay with an edited amount looks like.
	tampered := strings.Replace(body, `"amount_total": 2900`, `"amount_total": 100`, 1)
	if tampered == body {
		t.Fatal("the test did not actually change the body")
	}

	if _, err := g.Verify([]byte(tampered), sign(t, body, now)); err == nil {
		t.Fatal("Verify accepted a body that had been edited after signing")
	}

	// A signature for a different secret, which is what an endpoint
	// configured with the wrong whsec_ value produces -- and the mistake is
	// common enough that config.go checks the prefix.
	other, err := stripepay.New("sk_test_x", "whsec_someone_elses_secret")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := other.Verify([]byte(body), sign(t, body, now)); err == nil {
		t.Fatal("Verify accepted a signature made with a different secret")
	}
}

// The tolerance, which is a replay window: a captured body and its signature
// stay valid together for as long as it lasts.
func TestAStaleSignatureIsRefused(t *testing.T) {
	g := gateway(t)

	body := completed(types.NewID().String(), "paid")

	// Just inside, and the SDK's default is five minutes.
	if _, err := g.Verify([]byte(body), sign(t, body, time.Now().Add(-4*time.Minute))); err != nil {
		t.Errorf("Verify refused a signature four minutes old: %v", err)
	}

	if _, err := g.Verify([]byte(body), sign(t, body, time.Now().Add(-1*time.Hour))); err == nil {
		t.Error("Verify accepted an hour-old signature, so a captured delivery could be replayed indefinitely")
	}

	// A future timestamp is not refused by Stripe's check, which only looks
	// backwards. Asserted so that nobody reads the test above as proving more
	// than it does.
	if _, err := g.Verify([]byte(body), sign(t, body, time.Now().Add(1*time.Hour))); err != nil {
		t.Logf("a future-dated signature was refused: %v", err)
	}
}

// A session can complete without having been paid for, and marking that paid
// would put a lunch on the list that nobody bought.
func TestACompletedSessionIsNotNecessarilyAPaidOne(t *testing.T) {
	g := gateway(t)

	for status, want := range map[string]paybus.Result{
		"paid":                paybus.ResultPaid,
		"no_payment_required": paybus.ResultPaid,
		"unpaid":              paybus.ResultIgnored,
	} {
		t.Run(status, func(t *testing.T) {
			body := completed(types.NewID().String(), status)

			e, err := g.Verify([]byte(body), sign(t, body, time.Now()))
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}

			if e.Result != want {
				t.Errorf("payment_status %q gave %q, want %q", status, e.Result, want)
			}
		})
	}
}

// A failed intent. Its object is the intent itself, so the identifier is in
// `id` and there is no `payment_intent` field at all -- and our metadata is on
// it only because the session set payment_intent_data.metadata as well as its
// own. That is the whole reason both are set.
func TestAFailedPaymentIsRecognisedAndCarriesItsSubmission(t *testing.T) {
	g := gateway(t)

	id := types.NewID()
	body := `{
  "id": "evt_1FailedExample",
  "object": "event",
  "api_version": "2026-03-31.basil",
  "type": "payment_intent.payment_failed",
  "data": {
    "object": {
      "id": "pi_3FailedExample",
      "object": "payment_intent",
      "currency": "usd",
      "metadata": {"submission_id": "` + id.String() + `", "form": "feast-lunch-2026"}
    }
  }
}`

	e, err := g.Verify([]byte(body), sign(t, body, time.Now()))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if e.Result != paybus.ResultFailed {
		t.Errorf("result = %q, want failed", e.Result)
	}
	if e.SubmissionID != id {
		t.Errorf("submission = %q, want %q", e.SubmissionID, id)
	}
	if e.Ref != "pi_3FailedExample" {
		t.Errorf("ref = %q, want the intent's own id", e.Ref)
	}
}

// A dispute names no submission, because our metadata is on the payment intent
// and a dispute's metadata is the dispute's own. It still has to be recognised
// as a dispute, so that paybus can log it loudly rather than dropping it as an
// event about nothing.
func TestADisputeIsRecognisedEvenWithNoSubmissionOnIt(t *testing.T) {
	g := gateway(t)

	body := `{
  "id": "evt_1DisputeExample",
  "object": "event",
  "api_version": "2026-03-31.basil",
  "type": "charge.dispute.created",
  "data": {
    "object": {
      "id": "dp_1DisputeExample",
      "object": "dispute",
      "currency": "usd",
      "payment_intent": "pi_3DisputedExample",
      "metadata": {}
    }
  }
}`

	e, err := g.Verify([]byte(body), sign(t, body, time.Now()))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if e.Result != paybus.ResultDisputed {
		t.Errorf("result = %q, want disputed", e.Result)
	}
	if !e.SubmissionID.Zero() {
		t.Errorf("submission = %q, want nothing: a dispute carries no metadata of ours", e.SubmissionID)
	}
	if e.Ref != "pi_3DisputedExample" {
		t.Errorf("ref = %q, want the disputed payment intent", e.Ref)
	}
}

// An event we do not act on is acknowledged rather than refused. Refusing it
// would make Stripe retry it for three days.
func TestAnEventWeDoNotHandleIsIgnoredRatherThanRefused(t *testing.T) {
	g := gateway(t)

	body := `{
  "id": "evt_1SomethingElse",
  "object": "event",
  "api_version": "2026-03-31.basil",
  "type": "customer.created",
  "data": {"object": {"id": "cus_1", "object": "customer"}}
}`

	e, err := g.Verify([]byte(body), sign(t, body, time.Now()))
	if err != nil {
		t.Fatalf("Verify refused an event it simply does not handle: %v", err)
	}

	if e.Result != paybus.ResultIgnored {
		t.Errorf("result = %q, want ignored", e.Result)
	}
	if e.ID != "evt_1SomethingElse" {
		t.Errorf("event id = %q", e.ID)
	}
}

// Metadata that is not ours, or missing, leaves the submission zero rather
// than failing. paybus acknowledges that case: an account can have other
// integrations, and a payment made by hand in the dashboard has no order.
func TestMetadataThatIsNotOursLeavesNoSubmission(t *testing.T) {
	g := gateway(t)

	for name, meta := range map[string]string{
		"absent":       `{}`,
		"another tool": `{"order_ref": "12345"}`,
		"unparseable":  `{"submission_id": "not-an-identifier"}`,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{
  "id": "evt_1NoOrder",
  "object": "event",
  "api_version": "2026-03-31.basil",
  "type": "checkout.session.completed",
  "data": {"object": {"id": "cs_1", "object": "checkout.session",
    "payment_status": "paid", "payment_intent": "pi_1", "metadata": ` + meta + `}}
}`

			e, err := g.Verify([]byte(body), sign(t, body, time.Now()))
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}

			if !e.SubmissionID.Zero() {
				t.Errorf("submission = %q, want nothing", e.SubmissionID)
			}
		})
	}
}

// The decision recorded in this package's comment, asserted rather than
// described.
//
// stripe-go refuses an event whose API version is from a different release
// train than the SDK, and that check is switched off here. With it on, an
// account whose API version does not match this binary's SDK would have *every
// payment silently stop being confirmed* -- money taken, no record of the
// sale, and a log full of messages about release trains. This test is what
// stops somebody switching it back on for tidiness.
func TestAnOldAPIVersionDoesNotStopAPaymentBeingConfirmed(t *testing.T) {
	g := gateway(t)

	// completed() stamps 2019-05-16, which is years before this SDK.
	body := completed(types.NewID().String(), "paid")

	e, err := g.Verify([]byte(body), sign(t, body, time.Now()))
	if err != nil {
		t.Fatalf("an event stamped with an old API version was refused, which would mean no order is ever marked paid: %v", err)
	}

	if e.Result != paybus.ResultPaid {
		t.Errorf("result = %q, want paid", e.Result)
	}
}

// Both credentials are required, and they are different credentials.
func TestAGatewayNeedsBothCredentials(t *testing.T) {
	if _, err := stripepay.New("", secret); err == nil {
		t.Error("New accepted an empty API key")
	}

	if _, err := stripepay.New("sk_test_x", ""); err == nil {
		t.Error("New accepted an empty webhook secret, which would mean a public URL that verifies nothing")
	}
}

// The decline reason is read for one purpose: telling one unlucky buyer apart
// from a list of stolen cards being worked through. It decides nothing -- the
// payment failed either way -- so what matters is that the more specific of
// Stripe's two codes is the one that reaches the log.
func TestTheDeclineReasonIsRead(t *testing.T) {
	g := gateway(t)

	failure := func(t *testing.T, errorBlock string) paybus.Event {
		t.Helper()

		body := `{
  "id": "evt_1Decline` + string(rune('A'+len(errorBlock)%26)) + `",
  "object": "event",
  "api_version": "2026-03-31.basil",
  "type": "payment_intent.payment_failed",
  "data": {
    "object": {
      "id": "pi_3DeclineExample",
      "object": "payment_intent",
      "currency": "usd",
      "metadata": {"submission_id": "` + types.NewID().String() + `"}` + errorBlock + `
    }
  }
}`

		e, err := g.Verify([]byte(body), sign(t, body, time.Now()))
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}

		return e
	}

	// The issuer's answer, which is the one worth having: a run of
	// generic_decline is what card testing looks like from this side.
	both := failure(t, `,
      "last_payment_error": {"code": "card_declined", "decline_code": "generic_decline"}`)
	if both.Decline != "generic_decline" {
		t.Errorf("decline = %q, want the issuer's own code", both.Decline)
	}

	// Stripe's category, when the issuer gave nothing more specific.
	only := failure(t, `,
      "last_payment_error": {"code": "expired_card"}`)
	if only.Decline != "expired_card" {
		t.Errorf("decline = %q, want the code Stripe did send", only.Decline)
	}

	// And a failure with no reason at all is still a failure. An absent block
	// is a fact rather than a parse error.
	none := failure(t, "")
	if none.Decline != "" {
		t.Errorf("decline = %q, want empty", none.Decline)
	}
	if none.Result != paybus.ResultFailed {
		t.Errorf("result = %q, want failed whatever the reason", none.Result)
	}
}

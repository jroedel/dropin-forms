package muxer_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/notify/notifybus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/mail"
)

// Who is told about a submission, and when, through the surfaces as they are
// mounted.
//
// The timing is the part worth asserting here. notifybus's own tests cover the
// wording and the recipient list; what these cover is that a receipt goes out
// when the money arrives and not when somebody is handed a payment page, which
// is a property of two request paths rather than of the message.

// An order on its way to Stripe is not an order. A "thank you for your order"
// arriving while somebody is still typing their card number either reads as a
// receipt for something they have not paid for, or as a reason to stop.
func TestNobodyIsToldAboutAnOrderThatIsStillOnItsWayToStripe(t *testing.T) {
	k := newTill(t)

	sub := k.order(t, nil)

	if sub.Status.Settled() {
		t.Fatalf("the fixture is %q; this test is about an order awaiting payment", sub.Status)
	}

	if len(k.sent.Sent) != 0 {
		t.Fatalf("%d messages went out for an unpaid order: %v", len(k.sent.Sent), subjects(k.sent.Sent))
	}
}

// And when Stripe says the money arrived, everybody hears.
func TestTheWebhookIsWhatTellsPeopleAnOrderIsPaid(t *testing.T) {
	k := newTill(t)

	sub := k.order(t, nil)

	if w := k.deliver(t, paidEvent("evt_paid_1", sub.ID), time.Now()); w.Code != http.StatusOK {
		t.Fatalf("the webhook = %d:\n%s", w.Code, w.Body.String())
	}

	if len(k.sent.Sent) != 2 {
		t.Fatalf("%d messages after the payment, want the buyer and the office: %v",
			len(k.sent.Sent), subjects(k.sent.Sent))
	}

	var buyer, office bool

	for _, m := range k.sent.Sent {
		switch m.To {
		case "maria@example.org":
			buyer = true

			if !strings.Contains(m.Subject, "payment is confirmed") {
				t.Errorf("the buyer's subject is %q", m.Subject)
			}

		case "office@schoenstatt.test":
			office = true

			if !strings.Contains(m.Text, "(paid)") {
				t.Errorf("the office was not told the money arrived:\n%s", m.Text)
			}
			if !strings.Contains(m.Text, sub.ID.String()) {
				t.Errorf("the office message does not link to the submission:\n%s", m.Text)
			}

		default:
			t.Errorf("an unexpected recipient: %s", m.To)
		}
	}

	if !buyer || !office {
		t.Errorf("buyer told: %v, office told: %v", buyer, office)
	}
}

// Stripe retries until it is acknowledged, and being told the same thing twice
// is the ordinary case. A second receipt for one payment is not.
func TestARetriedPaymentNotificationDoesNotSendASecondReceipt(t *testing.T) {
	k := newTill(t)

	sub := k.order(t, nil)

	for i := range 3 {
		if w := k.deliver(t, paidEvent("evt_paid_2", sub.ID), time.Now()); w.Code != http.StatusOK {
			t.Fatalf("delivery %d = %d:\n%s", i+1, w.Code, w.Body.String())
		}
	}

	if len(k.sent.Sent) != 2 {
		t.Errorf("%d messages after three deliveries of one event, want 2: %v",
			len(k.sent.Sent), subjects(k.sent.Sent))
	}
}

// A payment that failed is not a payment. The person is at Stripe and has been
// told there by the only party that knows what went wrong with their card.
func TestAFailedPaymentTellsNobodyByMail(t *testing.T) {
	k := newTill(t)

	sub := k.order(t, nil)

	body := `{
  "id": "evt_failed_1",
  "object": "event",
  "api_version": "2026-03-31.basil",
  "type": "payment_intent.payment_failed",
  "data": {
    "object": {
      "id": "pi_test_failed",
      "object": "payment_intent",
      "currency": "usd",
      "last_payment_error": {"code": "card_declined", "decline_code": "generic_decline"},
      "metadata": {"submission_id": "` + sub.ID.String() + `", "form": "` + theForm + `"}
    }
  }
}`

	if w := k.deliver(t, body, time.Now()); w.Code != http.StatusOK {
		t.Fatalf("the webhook = %d:\n%s", w.Code, w.Body.String())
	}

	if len(k.sent.Sent) != 0 {
		t.Errorf("%d messages went out for a declined card: %v", len(k.sent.Sent), subjects(k.sent.Sent))
	}
}

// --- a form that sells nothing -------------------------------------------------

// freeForms is the real definition with the price taken off it, because the
// only form this service ships sells lunch and the other path has to be
// exercised too: a submission with nothing to pay is finished when it is
// stored, and the message goes out from the POST rather than from a webhook
// that will never arrive.
type freeForms struct {
	inner embedapp.Forms
}

func (f freeForms) ByID(slug types.Slug) (formbus.Form, error) {
	form, err := f.inner.ByID(slug)

	form.Items = nil
	form.PaymentRequired = false
	form.PaymentNote = ""
	form.MaxPerOrder = 0

	return form, err
}

func TestASubmissionWithNothingToPayIsAnnouncedStraightAway(t *testing.T) {
	cfg := newConfig(t, nil, nil)
	cfg.Embed.Now = func() time.Time { return beforeTheFeast }
	cfg.Embed.Forms = freeForms{inner: cfg.Embed.Forms}

	subs, ok := cfg.Embed.Submissions.(*submissionbus.Business)
	if !ok {
		t.Fatalf("the test config holds a %T rather than the submission domain", cfg.Embed.Submissions)
	}

	sent := &mail.Recorder{}

	notifier, err := notifybus.NewBusiness(notifybus.Config{
		Log:          cfg.Log,
		Mail:         sent,
		Forms:        cfg.Embed.Forms,
		Submissions:  subs,
		Grants:       cfg.Access,
		Accounts:     cfg.Users,
		Office:       "office@schoenstatt.test",
		AdminBaseURL: "https://forms.test",
	})
	if err != nil {
		t.Fatalf("notifybus.NewBusiness: %v", err)
	}

	cfg.Notify = notifier

	h := embedOf(t, cfg)

	page := getPage(t, h, "/f/"+theForm)
	if page.Code != http.StatusOK {
		t.Fatalf("the blank form = %d", page.Code)
	}

	w := postForm(t, h, url.Values{
		embedapp.GrantField: {grantIn(t, page.Body.String())},
		"name":              {"Maria O'Neill"},
		"email":             {"maria@example.org"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("the submission = %d:\n%s", w.Code, short(w.Body.String()))
	}

	if len(sent.Sent) != 2 {
		t.Fatalf("%d messages, want the submitter and the office: %v", len(sent.Sent), subjects(sent.Sent))
	}

	for _, m := range sent.Sent {
		// Nothing was bought, so nothing about this may read as a receipt.
		if strings.Contains(m.Subject, "payment") || strings.Contains(m.Text, "paid") {
			t.Errorf("a form that sells nothing produced a message about payment:\n%s\n%s", m.Subject, m.Text)
		}
	}
}

func subjects(sent []mail.Message) []string {
	out := make([]string, 0, len(sent))
	for _, m := range sent {
		out = append(out, m.To+": "+m.Subject)
	}

	return out
}

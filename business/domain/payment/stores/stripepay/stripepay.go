// Package stripepay is the Stripe implementation of paybus.Gateway.
//
// It sits under stores/ although Stripe is not storage, and that is a
// deliberate reading of the layering rather than an accident: this is where a
// domain reaches something outside the process, the interface it satisfies is
// declared by the bus above it, and nothing else in the service imports
// stripe-go. Every Stripe-shaped fact -- what a parameter is called, what an
// event's JSON looks like, which SDK version we are pinned to -- stops here.
//
// # Why the event's JSON is parsed by hand
//
// stripe-go refuses an event whose API version is from a different release
// train than the SDK, with a long error telling you to recreate the webhook
// endpoint. That check is switched off here, and the fields we need are read
// out of the raw JSON into a type declared below.
//
// The reason is what the alternative costs. A Stripe account has an API
// version; a webhook endpoint created at another one delivers events stamped
// with it; and with the check on, *every payment in the service silently stops
// being confirmed* while the log fills with a message about release trains.
// That is the worst failure this service has available to it -- money taken
// and no record of the sale -- traded against the risk that a field is
// renamed. The four fields read below (the object's id, its metadata, its
// payment intent and its amount) have been stable across every Stripe API
// version since 2019, and naming them here means a rename is a compile error
// in one file rather than a silent nil in a generated struct.
//
// The signature check is emphatically *not* hand-written. That is the part
// worth having a library for, and it is the SDK's constant-time comparison
// that is called below.
package stripepay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/stripe/stripe-go/v83"
	"github.com/stripe/stripe-go/v83/webhook"

	"github.com/jroedel/dropin-forms/business/domain/payment/paybus"
	"github.com/jroedel/dropin-forms/business/types"
)

// tolerance is how stale a signed timestamp may be.
//
// Five minutes, which is Stripe's own default and their documented
// recommendation. It is a replay window: a body and its signature stay valid
// together for this long, so a captured delivery can be sent again inside it.
// Shorter starts refusing legitimate deliveries when a clock drifts, and this
// process has no NTP of its own.
const tolerance = 5 * time.Minute

// The metadata keys we set on a session and read back off an event. Stripe's
// metadata is the only thing that travels from creating a payment to being
// told about it, so these two strings are the entire join between a Stripe
// event and a submission in this database.
const (
	metaSubmission = "submission_id"
	metaForm       = "form"
)

// The event types this service acts on. Anything else is acknowledged and
// ignored -- see paybus.ResultIgnored.
const (
	kindCompleted = "checkout.session.completed"
	kindFailed    = "payment_intent.payment_failed"
	kindDisputed  = "charge.dispute.created"

	// kindAsyncFailed is the delayed counterpart of kindCompleted, for a
	// payment method that settles after the session does. Handled because a
	// form that takes cards today may take a bank debit tomorrow, and the
	// failure would otherwise be invisible.
	kindAsyncFailed = "checkout.session.async_payment_failed"
)

// Gateway talks to Stripe.
type Gateway struct {
	client *stripe.Client

	// secret is the webhook signing secret, which is a different credential
	// from the API key and is per endpoint. Getting the two confused produces
	// a signature that never verifies, so they are named apart here.
	secret string
}

// New constructs one.
//
// Both credentials are required. A gateway with an empty webhook secret would
// verify nothing, and a verification that always passes is worse than no
// endpoint at all -- it is an unauthenticated route that marks orders paid.
func New(apiKey, webhookSecret string) (*Gateway, error) {
	switch {
	case apiKey == "":
		return nil, errors.New("a payment gateway needs a Stripe secret key")
	case webhookSecret == "":
		return nil, errors.New("a payment gateway needs the webhook signing secret; without it nothing could tell a real notification from a forged one")
	}

	return &Gateway{client: stripe.NewClient(apiKey), secret: webhookSecret}, nil
}

// Checkout creates the hosted payment session.
func (g *Gateway) Checkout(ctx context.Context, o paybus.Order) (paybus.Handoff, error) {
	params := &stripe.CheckoutSessionCreateParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModePayment)),
		SuccessURL: stripe.String(o.SuccessURL),
		CancelURL:  stripe.String(o.CancelURL),

		// The metadata is the join, and it goes on the session and on the
		// payment intent both. checkout.session.completed carries the
		// session's; payment_intent.payment_failed carries the intent's, and
		// nothing in that event mentions the session at all -- so setting it
		// in one place would make one of the two events unattributable.
		Metadata: map[string]string{
			metaSubmission: o.SubmissionID.String(),
			metaForm:       o.Form.String(),
		},

		// ClientReferenceID shows in the Stripe dashboard's own search, which
		// is where somebody looks when a person emails asking about a payment.
		ClientReferenceID: stripe.String(o.SubmissionID.String()),
	}

	// The idempotency key dedupes *our own retry of this one API call* -- a
	// timeout where the session may or may not have been created. It dedupes
	// nothing a person does: what stops a second order is the single-use
	// grant, which was already spent before this call was reached.
	params.IdempotencyKey = stripe.String("checkout:" + o.SubmissionID.String())

	if e := o.Email.String(); e != "" {
		params.CustomerEmail = stripe.String(e)
		params.PaymentIntentData = &stripe.CheckoutSessionCreatePaymentIntentDataParams{
			ReceiptEmail: stripe.String(e),
			Description:  stripe.String(o.Form.String()),
			Metadata: map[string]string{
				metaSubmission: o.SubmissionID.String(),
				metaForm:       o.Form.String(),
			},
		}
	} else {
		params.PaymentIntentData = &stripe.CheckoutSessionCreatePaymentIntentDataParams{
			Description: stripe.String(o.Form.String()),
			Metadata: map[string]string{
				metaSubmission: o.SubmissionID.String(),
				metaForm:       o.Form.String(),
			},
		}
	}

	for _, l := range o.Lines {
		params.LineItems = append(params.LineItems, &stripe.CheckoutSessionCreateLineItemParams{
			Quantity: stripe.Int64(int64(l.Qty)),
			PriceData: &stripe.CheckoutSessionCreateLineItemPriceDataParams{
				Currency:   stripe.String(o.Currency),
				UnitAmount: stripe.Int64(int64(l.Unit)),

				// An inline product rather than a Price object in the Stripe
				// account. The price lives in the form definition, which is
				// the single source of truth this whole service is built
				// around -- a Price in Stripe would be a second copy of it,
				// free to disagree, and changing a ticket price would mean
				// editing two places and hoping.
				ProductData: &stripe.CheckoutSessionCreateLineItemPriceDataProductDataParams{
					Name: stripe.String(l.Label),
				},
			},
		})
	}

	s, err := g.client.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return paybus.Handoff{}, fmt.Errorf("creating a Stripe Checkout session: %w", err)
	}

	return paybus.Handoff{Ref: s.ID, URL: s.URL}, nil
}

// object is the part of an event's data we read.
//
// Declared here rather than taken from a generated type, for the reason in the
// package comment. Every field is one whose name is part of Stripe's public
// API and has not changed in years, and a nil pointer is a fact rather than a
// failure -- a dispute carries no payment intent, a failed intent carries no
// amount_total.
type object struct {
	ID       string            `json:"id"`
	Metadata map[string]string `json:"metadata"`

	// PaymentIntent is a string on a session and absent on an intent, where
	// the intent *is* the object and its own id is in ID. Read as a
	// json.RawMessage because Stripe expands it into an object when asked to,
	// and a string field would then fail to unmarshal the whole event.
	PaymentIntent json.RawMessage `json:"payment_intent"`

	AmountTotal    int64  `json:"amount_total"`
	AmountReceived int64  `json:"amount_received"`
	Currency       string `json:"currency"`

	// PaymentStatus distinguishes a completed session that has been paid from
	// one whose payment method settles later. "paid" and "no_payment_required"
	// are money; "unpaid" is a session that completed and has not been paid
	// for, which must not become a paid submission.
	PaymentStatus string `json:"payment_status"`

	// LastPaymentError is why an intent failed, and it is read for one
	// purpose: telling an ordinary decline apart from a wave of them. A run of
	// generic_decline on a public form is what card testing looks like from
	// this side, and it is the thing section 7.6 asks to be alerted on.
	//
	// A pointer because it is absent on every event that is not a failure, and
	// absent is a fact rather than an empty string. Nothing decides anything
	// from it: it is logged, and the payment failed whatever the code says.
	LastPaymentError *struct {
		Code        string `json:"code"`
		DeclineCode string `json:"decline_code"`
	} `json:"last_payment_error"`
}

// decline is the most specific reason Stripe gave, or the empty string.
//
// decline_code is the issuer's answer -- generic_decline, insufficient_funds,
// lost_card -- and code is Stripe's own category, usually card_declined. The
// narrower one first, because "card_declined" on every line tells nobody
// whether they are watching a wave of stolen cards or a bad afternoon.
func (o object) decline() string {
	if o.LastPaymentError == nil {
		return ""
	}

	if o.LastPaymentError.DeclineCode != "" {
		return o.LastPaymentError.DeclineCode
	}

	return o.LastPaymentError.Code
}

// ref is Stripe's identifier for the payment, preferring the payment intent
// over the session: the intent is what a refund, a dispute and the dashboard's
// own search all key on.
func (o object) ref() string {
	if len(o.PaymentIntent) == 0 {
		return o.ID
	}

	var id string
	if err := json.Unmarshal(o.PaymentIntent, &id); err == nil && id != "" {
		return id
	}

	// Expanded into an object, which we never ask for but which a webhook
	// endpoint configured by hand can be set to send.
	var expanded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(o.PaymentIntent, &expanded); err == nil && expanded.ID != "" {
		return expanded.ID
	}

	return o.ID
}

// Verify checks the signature over the unmodified body and says what it means.
func (g *Gateway) Verify(payload []byte, signature string) (paybus.Event, error) {
	e, err := webhook.ConstructEventWithOptions(payload, signature, g.secret, webhook.ConstructEventOptions{
		Tolerance: tolerance,

		// See the package comment. This is the trade that keeps payments
		// being confirmed when an account's API version and this SDK's
		// release train differ, and it is safe only because the fields read
		// below are named here rather than deserialised into a generated
		// type.
		IgnoreAPIVersionMismatch: true,
	})
	if err != nil {
		// Not wrapped with anything of our own. The caller turns any error
		// here into paybus.ErrRefused, and the SDK's message is the useful
		// part -- it distinguishes a bad signature from a stale timestamp.
		return paybus.Event{}, err
	}

	out := paybus.Event{
		ID:     e.ID,
		Kind:   string(e.Type),
		Result: paybus.ResultIgnored,
	}

	if e.Data == nil || len(e.Data.Raw) == 0 {
		// An event with no object. Nothing to act on, and acknowledged rather
		// than refused, because a retry would deliver the same empty thing.
		return out, nil
	}

	var obj object
	if err := json.Unmarshal(e.Data.Raw, &obj); err != nil {
		return paybus.Event{}, fmt.Errorf("reading the object out of a %s event: %w", e.Type, err)
	}

	out.Ref = obj.ref()
	out.Currency = obj.Currency
	out.Total = types.Money(obj.AmountTotal)

	if id, ok := obj.Metadata[metaSubmission]; ok {
		parsed, err := types.ParseID(id)
		if err != nil {
			// Somebody else's metadata, or ours from a much older version.
			// Left zero, which the bus treats as "no order behind this" and
			// acknowledges.
			return out, nil
		}

		out.SubmissionID = parsed
	}

	switch out.Kind {
	case kindCompleted:
		// A completed session is not necessarily a paid one. A delayed
		// payment method completes the session and settles afterwards, and
		// marking that paid would put a lunch on the list that nobody has
		// paid for.
		switch obj.PaymentStatus {
		case "paid", "no_payment_required":
			out.Result = paybus.ResultPaid
		default:
			// Left ignored on purpose rather than called a failure: the async
			// event that follows says which it became.
			out.Result = paybus.ResultIgnored
		}

	case kindFailed, kindAsyncFailed:
		out.Result = paybus.ResultFailed
		out.Decline = obj.decline()

	case kindDisputed:
		out.Result = paybus.ResultDisputed
	}

	return out, nil
}

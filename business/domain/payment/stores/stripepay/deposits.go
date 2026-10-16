package stripepay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/stripe/stripe-go/v83"

	"github.com/jroedel/dropin-forms/business/domain/deposit/depositbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// Deposits reads Stripe's payouts to the bank and what each one held. It is
// the Stripe implementation of depositbus.Ledger.
//
// Here rather than in a package under the deposit domain because this package
// is the one place stripe-go is imported, and that is worth more than the
// symmetry: the SDK version, its pagination and its error type stop here.
//
// The responses are read into the SDK's own structs, unlike the webhook's
// events. The package comment's reason for hand-reading an event is that an
// event arrives stamped with whatever API version its endpoint was created at;
// a response to a request this binary makes is at the version the SDK pins,
// so the structs fit it by construction.
type Deposits struct {
	client *stripe.Client
}

// NewDeposits constructs one. The options are for a test pointing the client
// at a fake Stripe; the service passes none.
func NewDeposits(apiKey string, opts ...stripe.ClientOption) (*Deposits, error) {
	if apiKey == "" {
		return nil, errors.New("reading deposits needs a Stripe key")
	}

	return &Deposits{client: stripe.NewClient(apiKey, opts...)}, nil
}

// pageSize is Stripe's largest page. A deposit of a busy week can hold a few
// hundred lines, and every page is a request against the rate limit.
const pageSize = 100

// Payouts lists the payouts created at or after since.
func (d *Deposits) Payouts(ctx context.Context, since time.Time) ([]depositbus.Payout, error) {
	params := &stripe.PayoutListParams{}
	params.Limit = stripe.Int64(pageSize)

	if !since.IsZero() {
		params.CreatedRange = &stripe.RangeQueryParams{GreaterThanOrEqual: since.Unix()}
	}

	var out []depositbus.Payout

	for p, err := range d.client.V1Payouts.List(ctx, params) {
		if err != nil {
			return nil, refusal(err, "Payouts")
		}

		out = append(out, depositbus.Payout{
			ID:          p.ID,
			Amount:      types.Money(p.Amount),
			Currency:    string(p.Currency),
			Created:     time.Unix(p.Created, 0).UTC(),
			ArrivalDate: time.Unix(p.ArrivalDate, 0).UTC(),
			Automatic:   p.Automatic,
			Paid:        p.Status == stripe.PayoutStatusPaid,
			Reconciled:  p.ReconciliationStatus == stripe.PayoutReconciliationStatusCompleted,
		})
	}

	return out, nil
}

// Lines lists what one automatic payout held, leaving out the payout's own
// line.
//
// The source of each line is expanded in the same request, because the
// source -- the charge, the refund, the dispute -- is what names the payment
// the line came from, and fetching each one separately would be a request per
// line.
func (d *Deposits) Lines(ctx context.Context, payout string) ([]depositbus.Line, error) {
	params := &stripe.BalanceTransactionListParams{Payout: stripe.String(payout)}
	params.Limit = stripe.Int64(pageSize)
	params.AddExpand("data.source")

	var out []depositbus.Line

	for t, err := range d.client.V1BalanceTransactions.List(ctx, params) {
		if err != nil {
			return nil, refusal(err, "Balance and Balance transaction sources")
		}

		// The payout's own line moves the whole deposit out to the bank. It
		// is the deposit, not a part of it, and the parts add up to it only
		// without it.
		if t.Type == stripe.BalanceTransactionTypePayout && t.Source != nil && t.Source.ID == payout {
			continue
		}

		out = append(out, depositbus.Line{
			ID:          t.ID,
			Kind:        kindOf(t),
			Type:        string(t.Type),
			Description: t.Description,
			Amount:      types.Money(t.Amount),
			Fee:         types.Money(t.Fee),
			Net:         types.Money(t.Net),
			Created:     time.Unix(t.Created, 0).UTC(),
			PaymentRef:  paymentOf(t.Source),
		})
	}

	return out, nil
}

// kindOf sorts a line into the four kinds the page shows.
//
// By Stripe's reporting category rather than by its transaction type. The
// category is what Stripe's own reconciliation reports group by and exists for
// exactly this; the type has dozens of values, several of them older names for
// the same thing (charge and payment, refund and payment_refund).
func kindOf(t *stripe.BalanceTransaction) depositbus.Kind {
	switch t.ReportingCategory {
	case stripe.BalanceTransactionReportingCategoryCharge:
		return depositbus.KindSale
	case stripe.BalanceTransactionReportingCategoryRefund:
		return depositbus.KindRefund
	case stripe.BalanceTransactionReportingCategoryDispute, stripe.BalanceTransactionReportingCategoryDisputeReversal:
		return depositbus.KindDispute
	default:
		return depositbus.KindOther
	}
}

// paymentOf is the payment intent a line came from, or empty.
//
// A charge, a refund and a dispute each name the intent, and the intent is
// what this service recorded on the submission when the payment was
// confirmed. A source that is not expanded, or is none of those three, names
// no payment we could have taken.
func paymentOf(s *stripe.BalanceTransactionSource) string {
	if s == nil {
		return ""
	}

	var pi *stripe.PaymentIntent

	switch {
	case s.Charge != nil:
		pi = s.Charge.PaymentIntent
	case s.Refund != nil:
		pi = s.Refund.PaymentIntent
	case s.Dispute != nil:
		pi = s.Dispute.PaymentIntent
	}

	if pi == nil {
		return ""
	}

	return pi.ID
}

// refusal turns Stripe's answer to a key without a permission into a sentence
// that says what to do, and leaves every other error as it was.
//
// Stripe answers 403 when a restricted key lacks a permission, and its own
// message names the permission in its internal spelling. Both go in the log:
// the Dashboard's name from here, for the person editing the key, and
// Stripe's, in case the two ever disagree.
func refusal(err error, permission string) error {
	se, ok := errors.AsType[*stripe.Error](err)
	if !ok || se.HTTPStatusCode != http.StatusForbidden {
		return fmt.Errorf("reading from Stripe: %w", err)
	}

	return fmt.Errorf("the Stripe key may not read deposits; give it %s: Read on the API keys page of the Stripe dashboard (Stripe said: %s)", permission, se.Msg)
}

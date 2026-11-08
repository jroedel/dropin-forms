package submissionbus

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/types"
)

// ErrNotRefundable is a refund the submission cannot take: it was never paid
// for, nothing is left to give back, or the amount is more than is left.
var ErrNotRefundable = errors.New("that submission cannot be refunded that way")

// Refund is somebody in the office giving money back, and perhaps cancelling
// the registration with it.
type Refund struct {
	ID   types.ID
	Form types.Slug
	By   types.ID

	// Amount is how much to give back now, in minor units. Zero with Cancel
	// is a cancellation alone -- for a payment already refunded in Stripe's
	// dashboard that should stop counting here.
	Amount types.Money

	// Cancel says the person is not coming: see [StatusCancelled].
	Cancel bool

	// Note is what the person is told with it. Optional.
	Note string
}

// RefundMade is one refund as it was made, from the refund form: what Stripe
// called it, how much, who, and when.
type RefundMade struct {
	RefundID     string
	SubmissionID types.ID
	Form         types.Slug
	Amount       types.Money
	By           types.ID
	At           time.Time
	Note         string
	Cancelled    bool
}

// CanRefund is the check before anybody asks Stripe for anything: whether
// this refund is one the submission can take. It answers the submission as it
// stands, for the caller to make the refund against.
//
// Separate from [Business.RecordRefund] because the money moves between the
// two, at Stripe, and this package does not call Stripe. The order is: ask
// here, refund there, record here. A refund that Stripe made and that could
// not then be recorded is caught up by Stripe's own notification -- see
// [Business.SyncRefunded].
//
// It answers [ErrNotFound] for a submission on another form, or hidden, and
// [ErrNotRefundable] for everything else it refuses.
func (b *Business) CanRefund(ctx context.Context, r Refund) (Submission, error) {
	s, err := b.store.ByID(ctx, r.ID)
	if err != nil {
		return Submission{}, err
	}

	if s.Form != r.Form {
		return Submission{}, ErrNotFound
	}

	_, hidden, err := b.store.HidingOf(ctx, r.ID)
	switch {
	case err != nil:
		return Submission{}, fmt.Errorf("reading whether it is hidden: %w", err)
	case hidden:
		return Submission{}, ErrNotFound
	}

	left := s.Kept()

	switch {
	case s.Status != StatusPaid && s.Status != StatusCancelled:
		return s, fmt.Errorf("%w: it has not been paid for", ErrNotRefundable)
	case s.PaymentRef == "" && r.Amount > 0:
		return s, fmt.Errorf("%w: there is no payment on record to refund", ErrNotRefundable)
	case r.Amount < 0:
		return s, fmt.Errorf("%w: the amount is negative", ErrNotRefundable)
	case r.Amount > left:
		return s, fmt.Errorf("%w: %s is more than the %s left", ErrNotRefundable, r.Amount, left)
	case r.Amount == 0 && !r.Cancel:
		return s, fmt.Errorf("%w: it would give nothing back and change nothing", ErrNotRefundable)
	case r.Amount == 0 && s.Status == StatusCancelled:
		return s, fmt.Errorf("%w: it is already cancelled", ErrNotRefundable)
	}

	return s, nil
}

// RecordRefund writes down a refund Stripe has made, from the refund form, and
// cancels the registration when it was asked to. before is the submission as
// [Business.CanRefund] answered it; made.RefundID is empty for a cancellation
// that gave nothing back.
//
// One transaction: the refund's own row, and the submission's running total
// as the larger of what is stored and what this refund brings it to. On
// condition that the status is still before's, so two people refunding from
// two tabs are told [ErrChangedMeanwhile] rather than both cancelling.
func (b *Business) RecordRefund(ctx context.Context, before Submission, made RefundMade) (Submission, error) {
	after := before
	after.Refunded = max(before.Refunded, before.Refunded+made.Amount)
	after.UpdatedAt = made.At.UTC()

	if made.Cancelled {
		after.Status = StatusCancelled
	}

	made.Note = strings.TrimSpace(made.Note)

	if err := b.store.Refund(ctx, before, after, made); err != nil {
		return Submission{}, err
	}

	b.log.Info("refunded",
		"submission_id", before.ID.String(), "form", before.Form.String(),
		"amount", made.Amount.String(), "refunded", after.Refunded.String(),
		"cancelled", made.Cancelled, "refund", made.RefundID)

	return after, nil
}

// SyncRefunded records the running total of refunds Stripe reports for a
// payment, from its notification: refunds made on the refund form, arriving
// again, and refunds made in Stripe's dashboard, arriving for the first time.
// The larger of the two figures is kept, so neither path counts a refund the
// other already did. It never cancels anything -- whether somebody is still
// coming is not something Stripe knows.
//
// A reference that matches no submission is not an error: the payment may be
// another service's on the same Stripe account.
func (b *Business) SyncRefunded(ctx context.Context, now time.Time, ref string, cumulative types.Money) error {
	s, err := b.store.ByPaymentRef(ctx, ref)
	switch {
	case errors.Is(err, ErrNotFound):
		b.log.Info("a refund arrived for a payment that is not one of ours", "ref", ref)

		return nil
	case err != nil:
		return fmt.Errorf("finding the refunded payment: %w", err)
	}

	if cumulative <= s.Refunded {
		return nil
	}

	if err := b.store.SetRefunded(ctx, s.ID, cumulative, now.UTC()); err != nil {
		return fmt.Errorf("recording the refund: %w", err)
	}

	b.log.Info("a refund was recorded from Stripe",
		"submission_id", s.ID.String(), "form", s.Form.String(), "refunded", cumulative.String())

	return nil
}

// RefundsOf is the refunds made from the refund form for one submission,
// oldest first. Refunds made in Stripe's dashboard are not here -- they are
// in [Submission.Refunded] and nowhere else, because nothing here made them.
func (b *Business) RefundsOf(ctx context.Context, id types.ID) ([]RefundMade, error) {
	out, err := b.store.RefundsOf(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("listing the refunds: %w", err)
	}

	slices.SortStableFunc(out, func(x, y RefundMade) int { return x.At.Compare(y.At) })

	return out, nil
}

// Cancelled lists a form's cancelled submissions, newest first, leaving out
// hidden ones: the results page's way back to them, as [Business.Declined] is
// for declined ones.
func (b *Business) Cancelled(ctx context.Context, form types.Slug) ([]Submission, error) {
	out, err := b.store.ByForm(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("listing the cancelled submissions: %w", err)
	}

	return slices.DeleteFunc(out, func(s Submission) bool { return s.Status != StatusCancelled }), nil
}

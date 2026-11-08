package submissionbus

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// Offer is somebody in the office giving a place to a submission on the
// waiting list.
type Offer struct {
	ID   types.ID
	Form types.Slug
	By   types.ID

	// Claims is the places the submission's answers take, from
	// [formbus.Form.Claims] against the definition as it stands now: the
	// form's own place and any limited option it chose. All of them are
	// counted, because a waiting submission held none of them.
	Claims []formbus.Claim
}

// ErrOfferLapsed is a place offered from the waiting list that is no longer
// held: its hold ran out, it was paid for, or it was never offered.
var ErrOfferLapsed = errors.New("that place is no longer held")

// Offer moves a submission from the waiting list into the form, returning it
// as it was and as it now is.
//
// Into received, on a form that takes no money, which is the whole of it: the
// person is in. A form that also waits for approval keeps the submission
// awaiting a decision, as it would have been had it arrived with room.
//
// Into offered, on one that does, with the place held for [OfferHold] while
// the person is sent a link to pay for it. The place is taken now, not when
// they pay, so that two people are never offered the same bed.
//
// Who is offered a place is the office's choice and not the order of the
// list. The list is shown oldest first, and that is the usual answer, but a
// couple who must come together or a bed kept for a visiting priest are
// reasons nobody can write into a rule.
//
// It answers [ErrNotFound] for a submission on another form, or hidden;
// [ErrNotOfferable] for one not on the waiting list; [ErrFull] or
// [OptionFull] when the place or the slot it chose is not free; and
// [ErrChangedMeanwhile] when somebody else offered it a place first.
func (b *Business) Offer(ctx context.Context, now time.Time, o Offer) (Submission, Submission, error) {
	before, err := b.store.ByID(ctx, o.ID)
	if err != nil {
		return Submission{}, Submission{}, err
	}

	if before.Form != o.Form {
		return Submission{}, Submission{}, ErrNotFound
	}

	_, hidden, err := b.store.HidingOf(ctx, o.ID)
	switch {
	case err != nil:
		return Submission{}, Submission{}, fmt.Errorf("reading whether it is hidden: %w", err)
	case hidden:
		return Submission{}, Submission{}, ErrNotFound
	}

	if before.Status != StatusWaiting {
		return before, before, ErrNotOfferable
	}

	after := before
	after.Status = StatusReceived
	after.UpdatedAt = now.UTC()

	if before.Answers.Total > 0 {
		after.Status = StatusOffered
		after.OfferExpiresAt = now.UTC().Add(OfferHold)
	}

	if err := b.store.Promote(ctx, before, after, o.Claims); err != nil {
		return Submission{}, Submission{}, err
	}

	b.log.Info("a place was offered from the waiting list",
		"submission_id", o.ID.String(), "form", o.Form.String(), "by", o.By.String())

	return before, after, nil
}

// Hold keeps an offered place for somebody who has just been sent to pay for
// it, until at least until -- the end of the payment page they are about to
// be given.
//
// Without it a page opened in the last hour of a hold would outlive the hold:
// the sweep would give the place away while the person was typing their card
// number, and the payment, when it landed, would overfill the form. A hold is
// only ever lengthened here, never shortened.
//
// It answers [ErrNotFound] for a submission on another form, or hidden, and
// [ErrOfferLapsed] for one whose hold has ended, or that is not an offer.
func (b *Business) Hold(ctx context.Context, now time.Time, form types.Slug, id types.ID, until time.Time) (Submission, error) {
	s, err := b.store.ByID(ctx, id)
	if err != nil {
		return Submission{}, err
	}

	if s.Form != form {
		return Submission{}, ErrNotFound
	}

	_, hidden, err := b.store.HidingOf(ctx, id)
	switch {
	case err != nil:
		return Submission{}, fmt.Errorf("reading whether it is hidden: %w", err)
	case hidden:
		return Submission{}, ErrNotFound
	}

	if s.Status != StatusOffered || !now.Before(s.OfferExpiresAt) {
		return s, ErrOfferLapsed
	}

	if err := b.store.Hold(ctx, id, now.UTC(), until.UTC()); err != nil {
		return s, err
	}

	if until.After(s.OfferExpiresAt) {
		s.OfferExpiresAt = until.UTC()
	}

	return s, nil
}

// ForPayment reads the submission a payment link names, whatever has become of
// it -- the page says whether the place is still held, paid for, or gone. It
// answers [ErrNotFound] for one on another form, or hidden: hiding withdraws a
// payment link as it withdraws an answer link.
func (b *Business) ForPayment(ctx context.Context, form types.Slug, id types.ID) (Submission, error) {
	s, err := b.store.ByID(ctx, id)
	if err != nil {
		return Submission{}, err
	}

	if s.Form != form {
		return Submission{}, ErrNotFound
	}

	_, hidden, err := b.store.HidingOf(ctx, id)
	switch {
	case err != nil:
		return Submission{}, fmt.Errorf("reading whether it is hidden: %w", err)
	case hidden:
		return Submission{}, ErrNotFound
	}

	return s, nil
}

// Offered is a form's places offered from the waiting list and not yet paid
// for, oldest offer first.
func (b *Business) Offered(ctx context.Context, form types.Slug) ([]Submission, error) {
	all, err := b.store.ByForm(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("listing the offered places: %w", err)
	}

	out := slices.DeleteFunc(all, func(s Submission) bool { return s.Status != StatusOffered })

	slices.SortStableFunc(out, func(x, y Submission) int { return x.OfferExpiresAt.Compare(y.OfferExpiresAt) })

	return out, nil
}

// Waiting is a form's waiting list, oldest first -- the order people joined
// it in, and so the order the office usually offers places in. Hidden
// submissions are left out, as they are from every list.
func (b *Business) Waiting(ctx context.Context, form types.Slug) ([]Submission, error) {
	all, err := b.store.ByForm(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("listing the waiting list: %w", err)
	}

	out := slices.DeleteFunc(all, func(s Submission) bool { return s.Status != StatusWaiting })

	// ByForm is newest first; the list is read the other way.
	slices.Reverse(out)

	return out, nil
}

// Position is where a submission stands on its form's waiting list, counting
// from one, or zero when it is not on it.
func Position(list []Submission, id types.ID) int {
	return slices.IndexFunc(list, func(s Submission) bool { return s.ID == id }) + 1
}

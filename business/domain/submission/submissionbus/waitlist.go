package submissionbus

import (
	"context"
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

// Offer moves a submission from the waiting list into the form, returning it
// as it was and as it now is.
//
// Into received, on a form that takes no money, which is the whole of it: the
// person is in. A form that also waits for approval keeps the submission
// awaiting a decision, as it would have been had it arrived with room.
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

	if before.Answers.Total > 0 {
		// Unreachable while formbus refuses a waiting list on a form that
		// takes money. Refused rather than admitted as received, which would
		// be a booking nobody has paid for that every list counts as settled.
		return before, before, fmt.Errorf("%w: it has money to collect, and a paid place cannot be offered yet", ErrNotOfferable)
	}

	after := before
	after.Status = StatusReceived
	after.UpdatedAt = now.UTC()

	if err := b.store.Promote(ctx, before, after, o.Claims); err != nil {
		return Submission{}, Submission{}, err
	}

	b.log.Info("a place was offered from the waiting list",
		"submission_id", o.ID.String(), "form", o.Form.String(), "by", o.By.String())

	return before, after, nil
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

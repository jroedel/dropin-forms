package submissionbus

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// Review is where a submission stands with the office, on a form that holds
// every submission for approval -- see formbus.Form.NeedsApproval.
//
// A second axis beside [Status] rather than more statuses, and that is the
// decision this type records. Status is about money and only moves forwards;
// a review is a person's decision and can be changed. Folding the two
// together would make "approved and paid" and "awaiting and in checkout"
// statuses of their own, and every rule that asks one question of a status --
// Settled, HoldsPlace, the abandonment sweep -- would have to learn the other.
// It is also stored in a column of its own, so a binary from before it reads
// every row it can read today: an awaiting submission looks to it like a
// received one, which is the one thing a rollback gets wrong, and it gets it
// wrong towards showing a submission rather than losing it.
type Review string

const (
	// ReviewNone is a submission on a form that did not ask for approval
	// when it was made. Every submission from before approval existed is
	// one, and nothing can be decided about it.
	ReviewNone Review = ""

	// ReviewAwaiting is waiting for somebody to decide. It holds any limited
	// place it chose, and it is not yet shown on the public list.
	ReviewAwaiting Review = "awaiting"

	// ReviewApproved is confirmed, and reads everywhere as an ordinary
	// submission does.
	ReviewApproved Review = "approved"

	// ReviewDeclined is turned down. It gives back its place and leaves
	// every list but the one that asks for it, like a hidden submission --
	// and like a hidden one it is kept, so that "what happened to my
	// sign-up" has an answer.
	ReviewDeclined Review = "declined"
)

var reviews = []Review{ReviewNone, ReviewAwaiting, ReviewApproved, ReviewDeclined}

// ErrNotAReview is a review value this binary does not recognise.
var ErrNotAReview = errors.New("not a review")

// ErrNotDecidable is a decision the submission cannot take: one on a form
// that did not ask for approval, or one that would leave it where it is.
var ErrNotDecidable = errors.New("that submission cannot be decided that way")

// ParseReview reads a review, and is the only way to get one from a database.
func ParseReview(s string) (Review, error) {
	if r := Review(s); slices.Contains(reviews, r) {
		return r, nil
	}

	return "", fmt.Errorf("%w: %q", ErrNotAReview, s)
}

func (r Review) String() string { return string(r) }

// Listed reports whether a submission with this review may be shown on the
// public list beneath a form: one nobody had to approve, or one that was.
func (r Review) Listed() bool { return r == ReviewNone || r == ReviewApproved }

// ListedReviews is every review [Review.Listed] reports true for, for a store
// that writes the rule into a query -- SettledStatuses's shape.
func ListedReviews() []Review {
	return slices.DeleteFunc(slices.Clone(reviews), func(r Review) bool { return !r.Listed() })
}

// HoldsPlace reports whether this submission keeps a place in a limited
// option: its status holds one, and nobody has declined it.
func (s Submission) HoldsPlace() bool {
	return s.Status.HoldsPlace() && s.Review != ReviewDeclined
}

// Decision is somebody approving or declining one submission.
type Decision struct {
	ID   types.ID
	Form types.Slug
	To   Review
	By   types.ID

	// Note is what the person is told along with a decline. Optional, and
	// kept with the submission so the office can read later what was said.
	Note string

	// Claims is the limited places the submission's answers take, from
	// [formbus.Form.Claims]. Only counted when a declined submission is
	// approved after all -- the one decision that takes a place back, and
	// one that somebody else may have taken in the meantime.
	Claims []formbus.Claim
}

// Decide approves or declines a submission, returning it as it was and as it
// now is.
//
// What may be decided:
//   - awaiting, to approved or declined;
//   - approved, to declined -- a booking cancelled;
//   - declined, to approved -- a mind changed, if the place is still free.
//
// It answers [ErrNotFound] for a submission on another form, or hidden;
// [ErrNotDecidable] for one that was never waiting for approval, or for a
// decision that changes nothing; [OptionFull] when re-approving needs a place
// that has gone; and [ErrChangedMeanwhile] when somebody else decided it
// between it being read and this arriving.
func (b *Business) Decide(ctx context.Context, now time.Time, d Decision) (Submission, Submission, error) {
	if d.To != ReviewApproved && d.To != ReviewDeclined {
		return Submission{}, Submission{}, fmt.Errorf("a decision is to approve or decline, not %q", d.To)
	}

	before, err := b.store.ByID(ctx, d.ID)
	if err != nil {
		return Submission{}, Submission{}, err
	}

	if before.Form != d.Form {
		return Submission{}, Submission{}, ErrNotFound
	}

	_, hidden, err := b.store.HidingOf(ctx, d.ID)
	switch {
	case err != nil:
		return Submission{}, Submission{}, fmt.Errorf("reading whether it is hidden: %w", err)
	case hidden:
		return Submission{}, Submission{}, ErrNotFound
	}

	if before.Review == ReviewNone || before.Review == d.To {
		return before, before, ErrNotDecidable
	}

	after := before
	after.Review = d.To
	after.ReviewedBy = d.By
	after.ReviewedAt = now.UTC()
	after.ReviewNote = ""

	if d.To == ReviewDeclined {
		after.ReviewNote = strings.TrimSpace(d.Note)
	}

	// Only a declined submission has given its place away, so only taking
	// one back is counted.
	var claims []formbus.Claim
	if before.Review == ReviewDeclined {
		claims = d.Claims
	}

	if err := b.store.Decide(ctx, before, after, claims); err != nil {
		return Submission{}, Submission{}, err
	}

	b.log.Info("submission decided",
		"submission_id", d.ID.String(), "form", d.Form.String(),
		"from", before.Review.String(), "to", after.Review.String())

	return before, after, nil
}

// Declined lists a form's declined submissions, newest first, leaving out
// hidden ones. Every other list leaves them out; this is the one that finds
// them again, for the results page's "show the declined" box.
func (b *Business) Declined(ctx context.Context, form types.Slug) ([]Submission, error) {
	out, err := b.store.ByForm(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("listing the declined submissions: %w", err)
	}

	return slices.DeleteFunc(out, func(s Submission) bool { return s.Review != ReviewDeclined }), nil
}

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

// Revision is a set of answers a submission held and no longer does.
//
// Its own type and its own rows, for the reason [Hiding] and [Collection]
// are: a submission row says what is true now, and the history of how it got
// there is a different kind of fact. The office reading "Arriving 5 Feb" next
// to somebody's name needs the current answer; the office asking "when did he
// tell us that, and what did it say before" needs these.
type Revision struct {
	SubmissionID types.ID

	// Number counts from 1, which is what was first submitted.
	Number int

	Version  string
	Answers  formbus.Answers
	Email    types.Email
	RemoteIP string

	// From is when these became the answers, and Until is when they were
	// replaced.
	From  time.Time
	Until time.Time
}

// Edit is what the app layer supplies to change one, the counterpart of
// [New].
type Edit struct {
	ID types.ID

	// Seen is the UpdatedAt of the answers the person was shown, carried
	// through the page they edited. It is what makes two tabs safe: the
	// second to save was drawn from answers that are no longer current, and
	// saving it as-is would quietly undo whatever the first one said. So it
	// is refused with [ErrChangedMeanwhile] and the person is shown the
	// answers as they now stand.
	//
	// It comes from a hidden field and could be edited, which buys nothing:
	// the only row anybody can reach is the one their own link names.
	Seen time.Time

	Answers  formbus.Answers
	RemoteIP string
}

// Mine reads the submission an answer link names, for showing it back to the
// person who followed the link.
//
// It answers [ErrNotFound] for every reason the link should not work: no such
// submission, one on a different form from the one in the address, one that
// has been hidden, and one waiting on or holding a payment. The public surface
// says the same sentence for all of them, and a stranger probing with a link
// they edited learns nothing from which it was.
//
// Hidden is the one worth naming. Hiding a submission is how the office
// withdraws a link that has gone somewhere it should not have -- forwarded to
// a list, say -- without touching any other link or the configured secret.
func (b *Business) Mine(ctx context.Context, form types.Slug, id types.ID) (Submission, error) {
	s, err := b.store.ByID(ctx, id)
	if err != nil {
		return Submission{}, err
	}

	if s.Form != form || s.Status != StatusReceived {
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

// Change replaces a submission's answers with validated new ones, keeping
// the old ones as a [Revision], and spends the grant the change arrived on.
//
// The answers are taken as validated, as Accept takes them, and by the same
// one validator: [formbus.Form.ValidateChange], which is also what decides
// whether the form still takes changes at all. This package has no
// definitions and asks no question about dates.
//
// It returns the submission as it was and as it now is, because the caller's
// next job is telling people what changed.
//
// What it refuses:
//   - anything [Business.Mine] would not find;
//   - answers with a total, because a form whose answers can change sells
//     nothing, and formbus refuses a definition that says otherwise;
//   - answers drawn from an earlier state of the row than the current one,
//     as [ErrChangedMeanwhile] -- see [Edit.Seen];
//   - a grant already spent, as [ErrReplayed];
//   - answers identical to the current ones, as [ErrUnchanged], with nothing
//     written and the grant left unspent -- there was nothing to replay;
//   - a row changed or hidden since it was read, as [ErrChangedMeanwhile].
func (b *Business) Change(ctx context.Context, now time.Time, g formbus.Grant, e Edit) (Submission, Submission, error) {
	ans := e.Answers

	switch {
	case g.Nonce == "":
		return Submission{}, Submission{}, errors.New("a change needs the grant it arrived on")
	case ans.FormID != g.Form:
		return Submission{}, Submission{}, fmt.Errorf("the grant is for %q and the answers are for %q", g.Form, ans.FormID)
	case ans.Version != g.Version:
		return Submission{}, Submission{}, fmt.Errorf("the grant pins version %q and the answers were checked against %q", g.Version, ans.Version)
	case ans.Total != 0 || len(ans.Lines) > 0:
		return Submission{}, Submission{}, errors.New("answers that come to money cannot be changed")
	}

	before, err := b.Mine(ctx, ans.FormID, e.ID)
	if err != nil {
		return Submission{}, Submission{}, err
	}

	// Compared at the store's precision, which is the millisecond the page
	// was given.
	if before.UpdatedAt.UnixMilli() != e.Seen.UnixMilli() {
		return before, before, ErrChangedMeanwhile
	}

	if sameAnswers(before.Answers, ans) {
		return before, before, ErrUnchanged
	}

	after := before
	after.Version = ans.Version
	after.Answers = ans
	after.Email, _ = ans.SubmitterEmail()
	after.RemoteIP = e.RemoteIP
	after.UpdatedAt = now.UTC()

	spent, err := b.store.Change(ctx, before, after, g.Nonce)
	switch {
	case err != nil:
		return Submission{}, Submission{}, err
	case !spent:
		return Submission{}, Submission{}, ErrReplayed
	}

	b.log.Info("submission changed",
		"submission_id", e.ID.String(), "form", after.Form.String())

	return before, after, nil
}

// Revisions reads the answers a submission has held and no longer does,
// oldest first.
func (b *Business) Revisions(ctx context.Context, s Submission) ([]Revision, error) {
	revs, err := b.store.Revisions(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("reading the earlier answers: %w", err)
	}

	return revs, nil
}

// sameAnswers compares what was said, and nothing else.
//
// Labels are left out on purpose: a question reworded since somebody answered
// it carries its new label in the new answers, and a revision recording that
// the office fixed a typo would be a revision of nothing the person said. So
// is the version, for the same reason.
func sameAnswers(a, b formbus.Answers) bool {
	return slices.EqualFunc(a.Fields, b.Fields, func(x, y formbus.Answer) bool {
		return x.Name == y.Name && slices.Equal(x.Values, y.Values)
	}) && slices.EqualFunc(a.Lines, b.Lines, func(x, y formbus.Line) bool {
		return x.ItemID == y.ItemID && x.Qty == y.Qty
	})
}

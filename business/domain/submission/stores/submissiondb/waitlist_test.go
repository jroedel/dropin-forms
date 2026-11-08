package submissiondb_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// The form's own places are counted from the submissions that hold one, like
// an option's: not one waiting, failed, abandoned, declined or hidden.
func TestTheFormsOwnPlacesAreCountedFromWhatHoldsThem(t *testing.T) {
	_, store := open(t)

	with := func(st submissionbus.Status) submissionbus.Submission {
		s := signup(t, "a")
		s.Status = st

		return s
	}

	declined := with(submissionbus.StatusReceived)
	declined.Review = submissionbus.ReviewDeclined

	hidden := with(submissionbus.StatusReceived)

	subs := []submissionbus.Submission{
		with(submissionbus.StatusReceived),
		with(submissionbus.StatusPending),
		with(submissionbus.StatusPaid),
		with(submissionbus.StatusWaiting),
		with(submissionbus.StatusFailed),
		with(submissionbus.StatusAbandoned),
		declined,
		hidden,
	}

	for i, s := range subs {
		if _, err := store.Accept(t.Context(), s, fmt.Sprint(i), nil); err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}

	if _, _, err := store.Hide(t.Context(), submissionbus.Hiding{
		SubmissionID: hidden.ID, Form: hidden.Form, HiddenAt: now, HiddenBy: types.NewID(),
	}); err != nil {
		t.Fatalf("Hide: %v", err)
	}

	taken, err := store.Taken(t.Context(), subs[0].Form)
	if err != nil {
		t.Fatalf("Taken: %v", err)
	}

	if got := taken.Held(); got != 3 {
		t.Errorf("%d of the form's places held, want 3: received, pending and paid", got)
	}

	// And the same count refuses the fourth place.
	four := []formbus.Claim{{Limit: 3}}
	if _, err := store.Accept(t.Context(), signup(t, "b"), "next", four); !errors.Is(err, submissionbus.ErrFull) {
		t.Errorf("Accept on a full form: %v, want ErrFull", err)
	}
}

// Offering a place is conditional on the row still waiting, and counts the
// place it takes.
func TestPromoteTakesAPlaceOnceAndOnlyWhileThereIsOne(t *testing.T) {
	_, store := open(t)
	one := []formbus.Claim{{Limit: 1}}

	waiting := func() submissionbus.Submission {
		s := signup(t, "a")
		s.Status = submissionbus.StatusWaiting

		return s
	}

	first, second := waiting(), waiting()

	for i, s := range []submissionbus.Submission{first, second} {
		if _, err := store.Accept(t.Context(), s, fmt.Sprint(i), nil); err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}

	in := first
	in.Status = submissionbus.StatusReceived
	in.UpdatedAt = now

	if err := store.Promote(t.Context(), first, in, one); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	if got, err := store.ByID(t.Context(), first.ID); err != nil || got.Status != submissionbus.StatusReceived {
		t.Errorf("after Promote the row is %s, %v; want received", got.Status, err)
	}

	// The same offer again, from a second tab: it is no longer waiting.
	if err := store.Promote(t.Context(), first, in, nil); !errors.Is(err, submissionbus.ErrChangedMeanwhile) {
		t.Errorf("promoting it twice: %v, want ErrChangedMeanwhile", err)
	}

	// And the one place is gone.
	also := second
	also.Status = submissionbus.StatusReceived

	if err := store.Promote(t.Context(), second, also, one); !errors.Is(err, submissionbus.ErrFull) {
		t.Errorf("promoting into a full form: %v, want ErrFull", err)
	}

	if got, _ := store.ByID(t.Context(), second.ID); got.Status != submissionbus.StatusWaiting {
		t.Errorf("a refused Promote left the row %s, want waiting", got.Status)
	}
}

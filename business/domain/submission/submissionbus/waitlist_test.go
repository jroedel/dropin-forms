package submissionbus_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

func TestAWaitingSubmissionHoldsNothingAndIsSettledForNothing(t *testing.T) {
	if submissionbus.StatusWaiting.HoldsPlace() || submissionbus.StatusWaiting.Settled() {
		t.Error("a submission on the waiting list holds a place or reads as settled")
	}

	if _, err := submissionbus.ParseStatus("waiting"); err != nil {
		t.Errorf("ParseStatus(waiting): %v", err)
	}
}

// Two places. The third person is turned away from a form without a waiting
// list, and put on it on a form with one -- with the place they did not get
// left unclaimed.
func TestAFullFormRefusesOrWaitlists(t *testing.T) {
	two := []formbus.Claim{{Limit: 2}}

	for _, tt := range []struct {
		name     string
		waitlist bool
	}{
		{"no waiting list", false},
		{"a waiting list", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := newBusiness()
			form := mustSlug(t, "retreat")

			for i, nonce := range []string{"one", "two"} {
				a := answers(t, form, 0)
				if _, err := b.Accept(t.Context(), now, grantFor(a, nonce), submissionbus.New{Answers: a, Claims: two, Waitlist: tt.waitlist}); err != nil {
					t.Fatalf("Accept %d: %v", i+1, err)
				}
			}

			third := answers(t, form, 0)
			s, err := b.Accept(t.Context(), now, grantFor(third, "three"), submissionbus.New{Answers: third, Claims: two, Waitlist: tt.waitlist})

			switch {
			case !tt.waitlist && !errors.Is(err, submissionbus.ErrFull):
				t.Fatalf("the third Accept: %v, want ErrFull", err)
			case !tt.waitlist:
				return
			case err != nil:
				t.Fatalf("the third Accept onto the waiting list: %v", err)
			}

			if s.Status != submissionbus.StatusWaiting {
				t.Errorf("the third is %s, want waiting", s.Status)
			}

			taken, err := b.Taken(t.Context(), form)
			if err != nil {
				t.Fatalf("Taken: %v", err)
			}

			if taken.Held() != 2 {
				t.Errorf("%d places held, want the two that were given", taken.Held())
			}
		})
	}
}

// A place opens -- here, one of the two is hidden -- and the office offers it.
// The waiting submission is in; offering a second while the form is full again
// is refused.
func TestOfferingAPlaceFromTheWaitingList(t *testing.T) {
	b, store := newBusiness()
	form := mustSlug(t, "retreat")
	one := []formbus.Claim{{Limit: 1}}

	first := answers(t, form, 0)
	in, err := b.Accept(t.Context(), now, grantFor(first, "one"), submissionbus.New{Answers: first, Claims: one, Waitlist: true})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	var waiting []submissionbus.Submission

	// A minute apart, so the order they joined in is a fact rather than a
	// tie the store breaks.
	for i, nonce := range []string{"two", "three"} {
		a := answers(t, form, 0)
		at := now.Add(time.Duration(i+1) * time.Minute)

		s, err := b.Accept(t.Context(), at, grantFor(a, nonce), submissionbus.New{Answers: a, Claims: one, Waitlist: true})
		if err != nil || s.Status != submissionbus.StatusWaiting {
			t.Fatalf("Accept onto the waiting list: %v, %s", err, s.Status)
		}

		waiting = append(waiting, s)
	}

	list, err := b.Waiting(t.Context(), form)
	if err != nil {
		t.Fatalf("Waiting: %v", err)
	}

	if got := submissionbus.Position(list, waiting[0].ID); got != 1 {
		t.Errorf("the first to wait is number %d, want 1", got)
	}
	if got := submissionbus.Position(list, waiting[1].ID); got != 2 {
		t.Errorf("the second to wait is number %d, want 2", got)
	}

	// Full: the offer is refused and nothing moves.
	if _, _, err := b.Offer(t.Context(), now, submissionbus.Offer{ID: waiting[0].ID, Form: form, Claims: one}); !errors.Is(err, submissionbus.ErrFull) {
		t.Fatalf("offering a place on a full form: %v, want ErrFull", err)
	}

	store.hidden[in.ID] = submissionbus.Hiding{SubmissionID: in.ID, Form: form}

	// The office picks the second person, which is theirs to choose.
	_, after, err := b.Offer(t.Context(), now, submissionbus.Offer{ID: waiting[1].ID, Form: form, Claims: one})
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}

	if after.Status != submissionbus.StatusReceived {
		t.Errorf("after the offer it is %s, want received", after.Status)
	}

	if _, _, err := b.Offer(t.Context(), now, submissionbus.Offer{ID: waiting[0].ID, Form: form, Claims: one}); !errors.Is(err, submissionbus.ErrFull) {
		t.Errorf("a second offer for the one place: %v, want ErrFull", err)
	}

	if _, _, err := b.Offer(t.Context(), now, submissionbus.Offer{ID: waiting[1].ID, Form: form, Claims: one}); !errors.Is(err, submissionbus.ErrNotOfferable) {
		t.Errorf("offering a place to somebody already in: %v, want ErrNotOfferable", err)
	}

	if _, _, err := b.Offer(t.Context(), now, submissionbus.Offer{ID: waiting[0].ID, Form: mustSlug(t, "elsewhere"), Claims: one}); !errors.Is(err, submissionbus.ErrNotFound) {
		t.Errorf("offering a place through another form: %v, want ErrNotFound", err)
	}

	list, err = b.Waiting(t.Context(), form)
	if err != nil {
		t.Fatalf("Waiting: %v", err)
	}

	if len(list) != 1 || list[0].ID != waiting[0].ID {
		t.Errorf("the waiting list is %v, want only the first", ids(list))
	}
}

// Somebody waiting on a form that also asks for approval cannot be approved or
// declined: they have no place to approve them into.
func TestAWaitingSubmissionIsNotDecided(t *testing.T) {
	b, _ := newBusiness()
	form := mustSlug(t, "retreat")
	one := []formbus.Claim{{Limit: 1}}

	for _, nonce := range []string{"one", "two"} {
		a := answers(t, form, 0)
		if _, err := b.Accept(t.Context(), now, grantFor(a, nonce), submissionbus.New{Answers: a, Claims: one, Waitlist: true, NeedsApproval: true}); err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}

	list, err := b.Waiting(t.Context(), form)
	if err != nil || len(list) != 1 {
		t.Fatalf("Waiting: %v, %d", err, len(list))
	}

	if _, _, err := b.Decide(t.Context(), now, submissionbus.Decision{ID: list[0].ID, Form: form, To: submissionbus.ReviewApproved}); !errors.Is(err, submissionbus.ErrNotDecidable) {
		t.Errorf("approving somebody on the waiting list: %v, want ErrNotDecidable", err)
	}
}

func ids(list []submissionbus.Submission) []types.ID {
	out := make([]types.ID, 0, len(list))
	for _, s := range list {
		out = append(out, s.ID)
	}

	return slices.Clip(out)
}

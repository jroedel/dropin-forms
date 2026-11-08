package submissionbus_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// paidOne is a one-place form with a paid registration on it, of 175.00 --
// the fee and a donation -- and a waiting list.
func paidOne(t *testing.T) (*submissionbus.Business, *memStore, submissionbus.Submission) {
	t.Helper()

	b, store := newBusiness()
	form := mustSlug(t, "retreat")

	a := answers(t, form, 17500)
	s, err := b.Accept(t.Context(), now, grantFor(a, "one"), submissionbus.New{Answers: a, Claims: []formbus.Claim{{Limit: 1}}, Waitlist: true})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	if err := b.Settle(t.Context(), now, s.ID, submissionbus.StatusPaid, "pi_1"); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	paid, err := b.ByID(t.Context(), s.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	return b, store, paid
}

// refund runs the three steps the app layer does, with Stripe's part stood in
// for by a made-up refund id.
func refund(t *testing.T, b *submissionbus.Business, r submissionbus.Refund) (submissionbus.Submission, error) {
	t.Helper()

	before, err := b.CanRefund(t.Context(), r)
	if err != nil {
		return submissionbus.Submission{}, err
	}

	return b.RecordRefund(t.Context(), before, submissionbus.RefundMade{
		RefundID: "re_" + types.NewID().String(), SubmissionID: r.ID, Form: r.Form,
		Amount: r.Amount, By: r.By, At: now, Note: r.Note, Cancelled: r.Cancel,
	})
}

func TestARefundIsFullOrPartAndCancellingIsSeparate(t *testing.T) {
	for _, tt := range []struct {
		name     string
		amount   types.Money
		cancel   bool
		status   submissionbus.Status
		holds    bool
		refunded types.Money
	}{
		{"full, cancelled", 17500, true, submissionbus.StatusCancelled, false, 17500},
		{"full, kept: a place given free after all", 17500, false, submissionbus.StatusPaid, true, 17500},
		{"the donation back", 2500, false, submissionbus.StatusPaid, true, 2500},
		{"cancelled, keeping a deposit", 15000, true, submissionbus.StatusCancelled, false, 15000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, _, s := paidOne(t)

			after, err := refund(t, b, submissionbus.Refund{ID: s.ID, Form: s.Form, Amount: tt.amount, Cancel: tt.cancel})
			if err != nil {
				t.Fatalf("refund: %v", err)
			}

			if after.Status != tt.status || after.Refunded != tt.refunded {
				t.Errorf("after: %s with %s refunded, want %s with %s", after.Status, after.Refunded, tt.status, tt.refunded)
			}

			if after.Kept() != 17500-tt.refunded {
				t.Errorf("Kept = %s", after.Kept())
			}

			taken, err := b.Taken(t.Context(), s.Form)
			if err != nil {
				t.Fatalf("Taken: %v", err)
			}

			if held := taken.Held() == 1; held != tt.holds {
				t.Errorf("holds its place = %v, want %v", held, tt.holds)
			}

			listed, err := b.ByForm(t.Context(), s.Form)
			if err != nil {
				t.Fatalf("ByForm: %v", err)
			}

			if inList := len(listed) == 1; inList == tt.cancel {
				t.Errorf("in the list = %v after a refund that cancelled = %v", inList, tt.cancel)
			}
		})
	}
}

func TestARefundIsRefusedWhatItCannotTake(t *testing.T) {
	b, _, s := paidOne(t)

	if _, err := refund(t, b, submissionbus.Refund{ID: s.ID, Form: s.Form, Amount: 17501}); !errors.Is(err, submissionbus.ErrNotRefundable) {
		t.Errorf("refunding more than was paid: %v", err)
	}

	if _, err := refund(t, b, submissionbus.Refund{ID: s.ID, Form: s.Form}); !errors.Is(err, submissionbus.ErrNotRefundable) {
		t.Errorf("a refund of nothing that cancels nothing: %v", err)
	}

	if _, err := refund(t, b, submissionbus.Refund{ID: s.ID, Form: mustSlug(t, "elsewhere"), Amount: 100}); !errors.Is(err, submissionbus.ErrNotFound) {
		t.Errorf("a refund through another form: %v", err)
	}

	// Twice part of it, and then the rest is all there is.
	for range 2 {
		if _, err := refund(t, b, submissionbus.Refund{ID: s.ID, Form: s.Form, Amount: 5000}); err != nil {
			t.Fatalf("a partial refund: %v", err)
		}
	}

	if _, err := refund(t, b, submissionbus.Refund{ID: s.ID, Form: s.Form, Amount: 7501}); !errors.Is(err, submissionbus.ErrNotRefundable) {
		t.Errorf("refunding more than is left: %v", err)
	}

	// An order not paid for has nothing to give back.
	pending := answers(t, s.Form, 17500)
	p, err := b.Accept(t.Context(), now, grantFor(pending, "pending"), submissionbus.New{Answers: pending})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	if _, err := refund(t, b, submissionbus.Refund{ID: p.ID, Form: p.Form, Amount: 100, Cancel: true}); !errors.Is(err, submissionbus.ErrNotRefundable) {
		t.Errorf("refunding an unpaid order: %v", err)
	}
}

// Refunded in Stripe's dashboard, then cancelled here with nothing to give
// back.
func TestARefundMadeInStripeIsSyncedAndCanThenBeCancelled(t *testing.T) {
	b, _, s := paidOne(t)

	if err := b.SyncRefunded(t.Context(), now, "pi_1", 17500); err != nil {
		t.Fatalf("SyncRefunded: %v", err)
	}

	got, _ := b.ByID(t.Context(), s.ID)
	if got.Refunded != 17500 || got.Status != submissionbus.StatusPaid {
		t.Fatalf("after the sync: %s, %s refunded; want still paid, all refunded", got.Status, got.Refunded)
	}

	after, err := refund(t, b, submissionbus.Refund{ID: s.ID, Form: s.Form, Cancel: true})
	if err != nil {
		t.Fatalf("cancelling with nothing left to refund: %v", err)
	}

	if after.Status != submissionbus.StatusCancelled || after.Refunded != 17500 {
		t.Errorf("after: %s with %s refunded", after.Status, after.Refunded)
	}

	// Stripe saying paid again, late, changes nothing.
	if err := b.Settle(t.Context(), now.Add(time.Hour), s.ID, submissionbus.StatusPaid, "pi_1"); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	if got, _ := b.ByID(t.Context(), s.ID); got.Status != submissionbus.StatusCancelled {
		t.Errorf("a late payment notice moved a cancelled registration to %s", got.Status)
	}
}

// Whichever way a refund arrives first, it is counted once.
func TestARefundIsNeverCountedTwice(t *testing.T) {
	b, _, s := paidOne(t)

	// Stripe's notification of the page's refund arrives before the page
	// records it.
	before, err := b.CanRefund(t.Context(), submissionbus.Refund{ID: s.ID, Form: s.Form, Amount: 2500})
	if err != nil {
		t.Fatalf("CanRefund: %v", err)
	}

	if err := b.SyncRefunded(t.Context(), now, "pi_1", 2500); err != nil {
		t.Fatalf("SyncRefunded: %v", err)
	}

	after, err := b.RecordRefund(t.Context(), before, submissionbus.RefundMade{
		RefundID: "re_1", SubmissionID: s.ID, Form: s.Form, Amount: 2500, At: now,
	})
	if err != nil {
		t.Fatalf("RecordRefund: %v", err)
	}

	if after.Refunded != 2500 {
		t.Errorf("after both: %s refunded, want 25.00", after.Refunded)
	}

	// And the notification again, afterwards.
	if err := b.SyncRefunded(t.Context(), now, "pi_1", 2500); err != nil {
		t.Fatalf("SyncRefunded: %v", err)
	}

	if got, _ := b.ByID(t.Context(), s.ID); got.Refunded != 2500 {
		t.Errorf("after the repeat: %s refunded, want 25.00", got.Refunded)
	}

	// A payment that is not ours is not an error.
	if err := b.SyncRefunded(t.Context(), now, "pi_somebody_else", 100); err != nil {
		t.Errorf("a refund for another payment: %v", err)
	}
}

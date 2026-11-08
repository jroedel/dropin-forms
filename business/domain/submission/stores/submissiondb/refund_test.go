package submissiondb_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// A refund is written with its row, its running total is the larger of the
// two it is given, a cancelled registration holds no place, and a second tab
// is told somebody got there first.
func TestARefundIsRecordedOnceWithItsRowAndFreesAPlaceWhenCancelled(t *testing.T) {
	_, store := open(t)

	s := signup(t, "a")
	s.Status = submissionbus.StatusPaid
	s.PaymentRef = "pi_1"
	s.Answers.Total = 17500

	if _, err := store.Accept(t.Context(), s, "one", nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	// Stripe's notification lands first, with the total this refund makes.
	if err := store.SetRefunded(t.Context(), s.ID, 17500, now); err != nil {
		t.Fatalf("SetRefunded: %v", err)
	}

	after := s
	after.Status = submissionbus.StatusCancelled
	after.Refunded = 17500
	after.UpdatedAt = now

	by := types.NewID()

	made := submissionbus.RefundMade{
		RefundID: "re_1", SubmissionID: s.ID, Form: s.Form, Amount: 17500,
		By: by, At: now, Note: "Sorry you cannot come.", Cancelled: true,
	}

	if err := store.Refund(t.Context(), s, after, made); err != nil {
		t.Fatalf("Refund: %v", err)
	}

	got, err := store.ByID(t.Context(), s.ID)
	if err != nil || got.Status != submissionbus.StatusCancelled || got.Refunded != 17500 {
		t.Fatalf("read back %s with %s refunded, %v; want cancelled with 175.00", got.Status, got.Refunded, err)
	}

	rows, err := store.RefundsOf(t.Context(), s.ID)
	if err != nil || len(rows) != 1 || rows[0].By != by || !rows[0].Cancelled || rows[0].Note != made.Note || rows[0].Amount != 17500 {
		t.Errorf("RefundsOf = %+v, %v", rows, err)
	}

	if taken, _ := store.Taken(t.Context(), s.Form); taken.Held() != 0 {
		t.Errorf("a cancelled registration still holds its place: %d", taken.Held())
	}

	// The same refund from a second tab, drawn while it was still paid.
	made.RefundID = "re_2"
	if err := store.Refund(t.Context(), s, after, made); !errors.Is(err, submissionbus.ErrChangedMeanwhile) {
		t.Errorf("a second refund from a stale page: %v, want ErrChangedMeanwhile", err)
	}

	// The running total never goes down.
	if err := store.SetRefunded(t.Context(), s.ID, 100, now.Add(time.Minute)); err != nil {
		t.Fatalf("SetRefunded: %v", err)
	}

	if got, _ := store.ByID(t.Context(), s.ID); got.Refunded != 17500 {
		t.Errorf("a smaller total lowered it to %s", got.Refunded)
	}
}

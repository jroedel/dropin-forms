package submissionbus_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// after is what the sweep is handed: the payment window and its margin. The
// number is paybus's; this package only has to count from it.
const after = 2*time.Hour + 10*time.Minute

// An order whose payment page has closed is abandoned whether the person
// closed the tab or had a card declined, and one still inside the window is
// left alone -- they may yet come back to it.
func TestAnOrderPastTheDeadlineIsAbandonedAndOneInsideItIsNot(t *testing.T) {
	b, _ := newBusiness()
	a := answers(t, mustSlug(t, "feast-lunch-2026"), types.Money(1200))

	accept := func(nonce string, at time.Time) submissionbus.Submission {
		t.Helper()

		s, err := b.Accept(t.Context(), at, grantFor(a, nonce), submissionbus.New{Answers: a})
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}

		return s
	}

	closed := accept("closed", now)
	declined := accept("declined", now)
	recent := accept("recent", now.Add(2*time.Hour))

	if err := b.Settle(t.Context(), now.Add(time.Minute), declined.ID, submissionbus.StatusFailed, "pi_1"); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	n, err := b.Abandon(t.Context(), now.Add(3*time.Hour), after)
	if err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	if n != 2 {
		t.Errorf("Abandon marked %d, want the two past the deadline", n)
	}

	for id, want := range map[types.ID]submissionbus.Status{
		closed.ID:   submissionbus.StatusAbandoned,
		declined.ID: submissionbus.StatusAbandoned,
		recent.ID:   submissionbus.StatusPending,
	} {
		got, err := b.ByID(t.Context(), id)
		if err != nil {
			t.Fatalf("ByID: %v", err)
		}
		if got.Status != want {
			t.Errorf("Status = %q, want %q", got.Status, want)
		}
	}
}

// Abandoned is final in one direction only. Stripe will not complete a
// payment on an expired page, but if a confirmation ever does arrive the money
// is real, and the order follows it; a late failure changes nothing.
func TestAnAbandonedOrderCanStillBePaidAndNothingElse(t *testing.T) {
	b, _ := newBusiness()
	a := answers(t, mustSlug(t, "feast-lunch-2026"), types.Money(1200))

	s, err := b.Accept(t.Context(), now, grantFor(a, "nonce"), submissionbus.New{Answers: a})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	later := now.Add(3 * time.Hour)

	if _, err := b.Abandon(t.Context(), later, after); err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	status := func() submissionbus.Status {
		t.Helper()

		got, err := b.ByID(t.Context(), s.ID)
		if err != nil {
			t.Fatalf("ByID: %v", err)
		}

		return got.Status
	}

	if err := b.Settle(t.Context(), later, s.ID, submissionbus.StatusFailed, "pi_1"); err != nil {
		t.Fatalf("a late failure: %v", err)
	}
	if got := status(); got != submissionbus.StatusAbandoned {
		t.Errorf("after a late failure the order is %q, want it still abandoned", got)
	}

	if err := b.Settle(t.Context(), later, s.ID, submissionbus.StatusPaid, "cs_1"); err != nil {
		t.Fatalf("a late payment: %v", err)
	}
	if got := status(); got != submissionbus.StatusPaid {
		t.Errorf("after a late payment the order is %q, want paid", got)
	}
}

// ByForm is what every list, total, download and feed reads, so it is the
// settled submissions alone; Unsettled is the rest, for an administrator.
func TestByFormIsTheSettledAndUnsettledIsTheRest(t *testing.T) {
	b, _ := newBusiness()
	form := mustSlug(t, "feast-lunch-2026")

	free := answers(t, form, 0)
	owed := answers(t, form, types.Money(1200))

	received, err := b.Accept(t.Context(), now, grantFor(free, "free"), submissionbus.New{Answers: free})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	pending, err := b.Accept(t.Context(), now, grantFor(owed, "owed"), submissionbus.New{Answers: owed})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	listed, err := b.ByForm(t.Context(), form)
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != received.ID {
		t.Errorf("ByForm = %d rows, want only the one with nothing to pay", len(listed))
	}

	unpaid, err := b.Unsettled(t.Context(), form)
	if err != nil {
		t.Fatalf("Unsettled: %v", err)
	}
	if len(unpaid) != 1 || unpaid[0].ID != pending.ID {
		t.Errorf("Unsettled = %d rows, want only the one still owing", len(unpaid))
	}
}

// The one thing the unpaid-orders email caught that nothing else would: a
// payment page that has stopped working looks like a quiet day. Several
// abandoned and none paid is a warning in the log, and one paid is not.
func TestAFormWhoseOrdersAreAllAbandonedIsAWarning(t *testing.T) {
	for _, tc := range []struct {
		name string
		paid bool
		warn bool
	}{
		{"none paid", false, true},
		{"one paid", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder

			store := newMemStore()
			b := submissionbus.NewBusiness(slog.New(slog.NewTextHandler(&out, nil)), store)
			a := answers(t, mustSlug(t, "feast-lunch-2026"), types.Money(1200))

			for i := range 3 {
				if _, err := b.Accept(t.Context(), now, grantFor(a, string(rune('a'+i))), submissionbus.New{Answers: a}); err != nil {
					t.Fatalf("Accept: %v", err)
				}
			}

			if tc.paid {
				s, err := b.Accept(t.Context(), now, grantFor(a, "paid"), submissionbus.New{Answers: a})
				if err != nil {
					t.Fatalf("Accept: %v", err)
				}

				if err := b.Settle(t.Context(), now.Add(time.Minute), s.ID, submissionbus.StatusPaid, "cs_1"); err != nil {
					t.Fatalf("Settle: %v", err)
				}
			}

			if _, err := b.Abandon(t.Context(), now.Add(3*time.Hour), after); err != nil {
				t.Fatalf("Abandon: %v", err)
			}

			warned := strings.Contains(out.String(), "level=WARN")
			if warned != tc.warn {
				t.Errorf("warned = %v, want %v; the log said:\n%s", warned, tc.warn, out.String())
			}
		})
	}
}

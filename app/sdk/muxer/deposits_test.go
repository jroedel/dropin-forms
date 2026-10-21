package muxer_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/deposit/depositbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// The deposits page, through the mounted admin surface: who reaches it, and
// that what it shows is the deposit as the bank received it with this form's
// share broken out.

const depositsPage = "/forms/" + theForm + "/deposits"

// fakeLedger is Stripe as far as deposits go, filled by a test before Sync.
type fakeLedger struct {
	payouts []depositbus.Payout
	lines   map[string][]depositbus.Line
}

func (l *fakeLedger) Payouts(context.Context, time.Time) ([]depositbus.Payout, error) {
	return l.payouts, nil
}

func (l *fakeLedger) Lines(_ context.Context, payout string) ([]depositbus.Line, error) {
	return l.lines[payout], nil
}

// paidOrder seeds an order on the feast form and confirms its payment under
// its own reference, as the webhook would.
func paidOrder(t *testing.T, a harness, name, ref string) submissionbus.Submission {
	t.Helper()

	sub := order(t, a, name, 3, submissionbus.StatusPending)

	if err := a.orders.Settle(t.Context(), time.Now(), sub.ID, submissionbus.StatusPaid, ref); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	return sub
}

// deposited puts one deposit in the fake Stripe holding Maria's order and a
// payment from outside the service, and reads it.
func deposited(t *testing.T, a harness) {
	t.Helper()

	arrived := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	lines := []depositbus.Line{
		{ID: "txn_maria", Kind: depositbus.KindSale, Type: "charge", Amount: 3600, Fee: 134, Net: 3466,
			Created: arrived.Add(-72 * time.Hour), PaymentRef: "pi_maria"},
		{ID: "txn_elsewhere", Kind: depositbus.KindSale, Type: "charge", Amount: 1000, Fee: 59, Net: 941,
			Created: arrived.Add(-72 * time.Hour), PaymentRef: "pi_from_the_dashboard"},
	}

	a.ledger.payouts = []depositbus.Payout{{
		ID: "po_october", Amount: 4407, Currency: "usd", Created: arrived.Add(-48 * time.Hour), ArrivalDate: arrived,
		Automatic: true, Paid: true, Reconciled: true,
		Destination: "ba_chase", Bank: "JPMORGAN CHASE BANK, NA", Last4: "2013",
		TraceID: "021000029876543", TraceStatus: depositbus.TraceSupported,
	}}
	a.ledger.lines = map[string][]depositbus.Line{"po_october": lines}

	if err := a.deposits.Sync(t.Context(), time.Now()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

// The gate is results, and everything above it on the ladder passes it.
func TestTheDepositsPageIsBehindResults(t *testing.T) {
	cases := []struct {
		role string
		who  func(*testing.T, harness, string) userbus.User
	}{
		{"results", reader},
		{"door", volunteer},
		{"admin", formAdmin},
	}

	for _, c := range cases {
		t.Run(c.role, func(t *testing.T) {
			a := newAdmin(t, "")
			cookie := sessionFor(t, a, c.who(t, a, "somebody@schoenstatt.test"))

			for _, path := range []string{depositsPage, depositsPage + ".csv"} {
				if w := a.get(t, path, cookie); w.Code != http.StatusOK {
					t.Errorf("a %s holder GET %s = %d, want 200:\n%s", c.role, path, w.Code, w.Body)
				}
			}
		})
	}

	t.Run("a grant on another form", func(t *testing.T) {
		a := newAdmin(t, "")

		u, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{Email: mustEmail(t, "elsewhere@schoenstatt.test"), Name: "Elsewhere"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := a.access.Grant(t.Context(), time.Now(), types.ID{}, u.ID, mustSlug(t, "some-other-form"), accessbus.RoleAdmin); err != nil {
			t.Fatalf("Grant: %v", err)
		}

		cookie := sessionFor(t, a, u)

		for _, path := range []string{depositsPage, depositsPage + ".csv"} {
			if w := a.get(t, path, cookie); w.Code != http.StatusForbidden {
				t.Errorf("GET %s with a grant on another form = %d, want 403", path, w.Code)
			}
		}
	})

	t.Run("signed out", func(t *testing.T) {
		a := newAdmin(t, "")

		w := a.get(t, depositsPage, "")
		if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/signin") {
			t.Errorf("signed out GET %s = %d to %q, want a redirect to /signin", depositsPage, w.Code, w.Header().Get("Location"))
		}
	})
}

func TestTheDepositsPageShowsTheBankLineAndThisFormsShare(t *testing.T) {
	a := newAdmin(t, "")
	cookie := sessionFor(t, a, reader(t, a, "accountant@schoenstatt.test"))

	paidOrder(t, a, "Maria O'Neill", "pi_maria")
	paidOrder(t, a, "Joe Waiting", "pi_joe")
	deposited(t, a)

	w := a.get(t, depositsPage, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d:\n%s", depositsPage, w.Code, w.Body)
	}

	body := w.Body.String()

	for _, want := range []string{
		"October 2026",
		"$44.07", // the deposit: the number on the bank statement
		"$34.66", // this form's net
		"−$1.34", // Stripe's fee on it, with the sign where a ledger puts it
		"$9.41",  // the payment from outside the service
		"Not from a form",
		"Maria O&#39;Neill",
		"Paid, not yet deposited", // Joe's money is still in Stripe's balance
		"Joe Waiting",
		"Checked with Stripe at",
		"to ••••2013", // which account, in the line somebody scans
		"JPMORGAN CHASE BANK, NA ••••2013", // and in full when opened
		"021000029876543",                  // what the bank can search for
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}

	w = a.get(t, depositsPage+".csv", cookie)
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("GET .csv = %d %q", w.Code, w.Header().Get("Content-Type"))
	}

	rows := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(rows) != 2 {
		t.Fatalf("the export has %d rows, want a header and Maria's line:\n%s", len(rows), w.Body)
	}

	for _, want := range []string{"2026-10-14", "po_october", "44.07", "021000029876543", "••••2013", "sale", "Maria O'Neill", "36.00", "1.34", "34.66", "pi_maria", "txn_maria"} {
		if !strings.Contains(rows[1], want) {
			t.Errorf("the export row %q does not contain %q", rows[1], want)
		}
	}

	// The payment that came from outside the service is not this form's line,
	// and nobody's name is attached to it.
	if strings.Contains(w.Body.String(), "pi_from_the_dashboard") {
		t.Error("the export includes a line that is not this form's")
	}
}

// The results page links to the deposits for a form that sells.
func TestTheResultsPageLinksToTheDeposits(t *testing.T) {
	a := newAdmin(t, "")
	cookie := sessionFor(t, a, reader(t, a, "accountant@schoenstatt.test"))

	w := a.get(t, "/forms/"+theForm+"/submissions", cookie)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `href="`+depositsPage+`"`) {
		t.Errorf("GET submissions = %d, and does not link to the deposits:\n%s", w.Code, w.Body)
	}
}

package depositbus_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/deposit/depositbus"
	"github.com/jroedel/dropin-forms/business/domain/deposit/stores/depositdb"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// The rules, against the real store and a fake Stripe. The store is real
// because half of what is worth testing here is that a payout is kept once and
// read back whole, and a fake store would only test the fake.

var now = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

// ledger is a fake Stripe: the payouts it reports and the lines of each.
type ledger struct {
	payouts []depositbus.Payout
	lines   map[string][]depositbus.Line
	fail    map[string]error

	// asked records each payout whose lines were requested, to show a kept
	// payout is not read again.
	asked []string
	since []time.Time
}

func (l *ledger) Payouts(_ context.Context, since time.Time) ([]depositbus.Payout, error) {
	l.since = append(l.since, since)
	return l.payouts, nil
}

func (l *ledger) Lines(_ context.Context, payout string) ([]depositbus.Line, error) {
	l.asked = append(l.asked, payout)
	if err := l.fail[payout]; err != nil {
		return nil, err
	}

	return slices.Clone(l.lines[payout]), nil
}

// orders is a fake submission store, keyed by payment reference.
type orders map[string]submissionbus.Submission

func (o orders) ByPaymentRef(_ context.Context, ref string) (submissionbus.Submission, error) {
	if s, ok := o[ref]; ok {
		return s, nil
	}

	return submissionbus.Submission{}, submissionbus.ErrNotFound
}

func (o orders) Paid(_ context.Context, form types.Slug) ([]submissionbus.Submission, error) {
	var out []submissionbus.Submission
	for _, s := range o {
		if s.Form == form && s.Status == submissionbus.StatusPaid {
			out = append(out, s)
		}
	}

	return out, nil
}

func open(t *testing.T) *depositdb.Store {
	t.Helper()

	db, err := sqldb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	if err := sqldb.Init(t.Context(), db); err != nil {
		t.Fatalf("sqldb.Init: %v", err)
	}
	if err := depositdb.Init(t.Context(), db); err != nil {
		t.Fatalf("depositdb.Init: %v", err)
	}

	return depositdb.NewStore(db)
}

func slug(t *testing.T, s string) types.Slug {
	t.Helper()

	out, err := types.ParseSlug(s)
	if err != nil {
		t.Fatalf("ParseSlug(%q): %v", s, err)
	}

	return out
}

func order(t *testing.T, form, ref string) submissionbus.Submission {
	t.Helper()

	return submissionbus.Submission{ID: types.NewID(), Form: slug(t, form), Status: submissionbus.StatusPaid, PaymentRef: ref}
}

// sale is a card payment of amount with Stripe's usual fee on it.
func sale(id, ref string, amount types.Money, at time.Time) depositbus.Line {
	fee := amount*29/1000 + 30
	return depositbus.Line{ID: id, Kind: depositbus.KindSale, Type: "charge", Amount: amount, Fee: fee, Net: amount - fee, Created: at, PaymentRef: ref}
}

func refund(id, ref string, amount types.Money, at time.Time) depositbus.Line {
	return depositbus.Line{ID: id, Kind: depositbus.KindRefund, Type: "refund", Amount: -amount, Net: -amount, Created: at, PaymentRef: ref}
}

func payout(id string, lines []depositbus.Line, arrival time.Time) depositbus.Payout {
	return depositbus.Payout{
		ID: id, Amount: depositbus.Sum(lines), Currency: "usd",
		Created: arrival.Add(-48 * time.Hour), ArrivalDate: arrival,
		Automatic: true, Paid: true, Reconciled: true,
	}
}

func business(l *ledger, s *depositdb.Store, o orders) *depositbus.Business {
	return depositbus.NewBusiness(slog.New(slog.NewTextHandler(io.Discard, nil)), l, s, o)
}

func TestADepositIsSplitBetweenThisFormOtherFormsAndTheRest(t *testing.T) {
	store := open(t)

	lunch, retreat := slug(t, "feast-lunch"), slug(t, "retreat")
	maria, joe, ann := order(t, "feast-lunch", "pi_maria"), order(t, "feast-lunch", "pi_joe"), order(t, "retreat", "pi_ann")

	day := now.Add(-72 * time.Hour)
	lines := []depositbus.Line{
		sale("txn_1", "pi_maria", 4500, day),
		sale("txn_2", "pi_joe", 1500, day.Add(time.Hour)),
		refund("txn_3", "pi_joe", 1500, day.Add(2*time.Hour)),
		sale("txn_4", "pi_ann", 20000, day),
		sale("txn_5", "pi_from_the_dashboard", 5000, day),
		{ID: "txn_6", Kind: depositbus.KindOther, Type: "stripe_fee", Amount: -200, Net: -200, Created: day},
	}

	l := &ledger{payouts: []depositbus.Payout{payout("po_1", lines, now)}, lines: map[string][]depositbus.Line{"po_1": lines}}
	b := business(l, store, orders{"pi_maria": maria, "pi_joe": joe, "pi_ann": ann})

	if err := b.Sync(t.Context(), now); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	shares, err := b.ForForm(t.Context(), lunch)
	if err != nil {
		t.Fatalf("ForForm: %v", err)
	}

	if len(shares) != 1 {
		t.Fatalf("ForForm = %d deposits, want 1", len(shares))
	}

	s := shares[0]
	want := depositbus.Sum(lines[:3])

	switch {
	case s.Sales != 6000:
		t.Errorf("Sales = %v, want 60.00", s.Sales)
	case s.Refunds != -1500:
		t.Errorf("Refunds = %v, want -15.00", s.Refunds)
	case s.Fees != lines[0].Fee+lines[1].Fee:
		t.Errorf("Fees = %v, want %v", s.Fees, lines[0].Fee+lines[1].Fee)
	case s.Net != want:
		t.Errorf("Net = %v, want %v", s.Net, want)
	case s.OtherForms != lines[3].Net:
		t.Errorf("OtherForms = %v, want %v", s.OtherForms, lines[3].Net)
	case s.NotFromAForm != lines[4].Net+lines[5].Net:
		t.Errorf("NotFromAForm = %v, want %v", s.NotFromAForm, lines[4].Net+lines[5].Net)
	}

	// The point of showing all three: together they are the bank line.
	if got := s.Net + s.OtherForms + s.NotFromAForm; got != s.Payout.Amount || s.Payout.Mismatch {
		t.Errorf("the share adds up to %v, the deposit is %v (mismatch %v)", got, s.Payout.Amount, s.Payout.Mismatch)
	}

	if len(s.Lines) != 3 || s.Lines[0].SubmissionID != maria.ID || s.Lines[2].Kind != depositbus.KindRefund {
		t.Errorf("Lines = %+v, want Maria's sale, Joe's sale and Joe's refund in that order", s.Lines)
	}

	// The other form sees the same deposit from its side.
	other, err := b.ForForm(t.Context(), retreat)
	if err != nil || len(other) != 1 || other[0].Net != lines[3].Net || other[0].OtherForms != want {
		t.Errorf("ForForm(retreat) = %+v, %v", other, err)
	}
}

func TestAKeptDepositIsNotReadAgain(t *testing.T) {
	store := open(t)

	lines := []depositbus.Line{sale("txn_1", "pi_1", 1000, now)}
	l := &ledger{payouts: []depositbus.Payout{payout("po_1", lines, now)}, lines: map[string][]depositbus.Line{"po_1": lines}}
	b := business(l, store, orders{})

	for range 2 {
		if err := b.Sync(t.Context(), now); err != nil {
			t.Fatalf("Sync: %v", err)
		}
	}

	if !slices.Equal(l.asked, []string{"po_1"}) {
		t.Errorf("lines were read for %v, want po_1 once", l.asked)
	}

	// The second sync looks back from the newest kept payout rather than
	// listing every payout there has ever been.
	if len(l.since) != 2 || !l.since[0].IsZero() || l.since[1].IsZero() || !l.since[1].Before(now) {
		t.Errorf("since = %v, want everything first and then a bounded window", l.since)
	}
}

func TestADepositNotYetReadyIsLeftForNextTime(t *testing.T) {
	store := open(t)

	lines := []depositbus.Line{sale("txn_1", "pi_1", 1000, now)}

	onItsWay := payout("po_transit", lines, now)
	onItsWay.Paid = false

	unworked := payout("po_unreconciled", lines, now)
	unworked.Reconciled = false

	l := &ledger{payouts: []depositbus.Payout{onItsWay, unworked}, lines: map[string][]depositbus.Line{}}
	b := business(l, store, orders{})

	if err := b.Sync(t.Context(), now); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if len(l.asked) != 0 {
		t.Errorf("lines were read for %v, want none", l.asked)
	}

	for _, id := range []string{"po_transit", "po_unreconciled"} {
		if has, err := store.Has(t.Context(), id); err != nil || has {
			t.Errorf("Has(%s) = %v, %v; want it left for a later sync", id, has, err)
		}
	}
}

func TestAManualPayoutIsKeptWithNoLines(t *testing.T) {
	store := open(t)

	p := depositbus.Payout{ID: "po_manual", Amount: 50000, Currency: "usd", Created: now, ArrivalDate: now, Paid: true}
	l := &ledger{payouts: []depositbus.Payout{p}}
	b := business(l, store, orders{})

	if err := b.Sync(t.Context(), now); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if len(l.asked) != 0 {
		t.Errorf("lines were read for a manual payout: %v", l.asked)
	}

	if has, err := store.Has(t.Context(), "po_manual"); err != nil || !has {
		t.Errorf("Has = %v, %v; want the manual payout kept", has, err)
	}
}

func TestADepositThatDoesNotAddUpIsMarked(t *testing.T) {
	store := open(t)

	lines := []depositbus.Line{sale("txn_1", "pi_maria", 4500, now)}
	p := payout("po_1", lines, now)
	p.Amount += 100

	maria := order(t, "feast-lunch", "pi_maria")
	b := business(&ledger{payouts: []depositbus.Payout{p}, lines: map[string][]depositbus.Line{"po_1": lines}}, store, orders{"pi_maria": maria})

	if err := b.Sync(t.Context(), now); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	shares, err := b.ForForm(t.Context(), maria.Form)
	if err != nil || len(shares) != 1 || !shares[0].Payout.Mismatch {
		t.Errorf("ForForm = %+v, %v; want the deposit marked as not adding up", shares, err)
	}
}

func TestOneDepositThatCannotBeReadDoesNotStopTheOthers(t *testing.T) {
	store := open(t)

	good := []depositbus.Line{sale("txn_1", "pi_1", 1000, now)}
	broken := errors.New("Stripe is having a moment")

	l := &ledger{
		payouts: []depositbus.Payout{payout("po_bad", good, now), payout("po_good", good, now)},
		lines:   map[string][]depositbus.Line{"po_good": good},
		fail:    map[string]error{"po_bad": broken},
	}
	b := business(l, store, orders{})

	if err := b.Sync(t.Context(), now); !errors.Is(err, broken) {
		t.Errorf("Sync = %v, want the failure reported", err)
	}

	if has, _ := store.Has(t.Context(), "po_good"); !has {
		t.Error("the readable deposit was not kept")
	}
	if has, _ := store.Has(t.Context(), "po_bad"); has {
		t.Error("the unreadable deposit was kept")
	}

	// And a sync that failed does not claim to have checked.
	if _, ok := b.Synced(); ok {
		t.Error("Synced reports a sync that failed")
	}
}

func TestALinePaidForBeforeItsConfirmationArrivedIsTracedLater(t *testing.T) {
	store := open(t)

	lines := []depositbus.Line{sale("txn_1", "pi_late", 1000, now)}
	l := &ledger{payouts: []depositbus.Payout{payout("po_1", lines, now)}, lines: map[string][]depositbus.Line{"po_1": lines}}

	known := orders{}
	b := business(l, store, known)

	if err := b.Sync(t.Context(), now); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// The confirmation arrives a day late, after the deposit was kept.
	late := order(t, "feast-lunch", "pi_late")
	known["pi_late"] = late

	if err := b.Sync(t.Context(), now.Add(time.Hour)); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	shares, err := b.ForForm(t.Context(), late.Form)
	if err != nil || len(shares) != 1 || len(shares[0].Lines) != 1 || shares[0].Lines[0].SubmissionID != late.ID {
		t.Errorf("ForForm = %+v, %v; want the line traced to the late order", shares, err)
	}

	if at, ok := b.Synced(); !ok || !at.Equal(now.Add(time.Hour)) {
		t.Errorf("Synced = %v, %v", at, ok)
	}
}

func TestUndepositedIsPaidOrdersNotYetInAnyDeposit(t *testing.T) {
	store := open(t)

	in, waiting := order(t, "feast-lunch", "pi_in"), order(t, "feast-lunch", "pi_waiting")
	lines := []depositbus.Line{sale("txn_1", "pi_in", 1000, now)}

	b := business(&ledger{payouts: []depositbus.Payout{payout("po_1", lines, now)}, lines: map[string][]depositbus.Line{"po_1": lines}},
		store, orders{"pi_in": in, "pi_waiting": waiting})

	if err := b.Sync(t.Context(), now); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	got, err := b.Undeposited(t.Context(), in.Form)
	if err != nil {
		t.Fatalf("Undeposited: %v", err)
	}

	if len(got) != 1 || got[0].ID != waiting.ID {
		t.Errorf("Undeposited = %+v, want only the order still waiting", got)
	}
}

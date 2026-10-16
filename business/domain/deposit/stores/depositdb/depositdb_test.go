package depositdb_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/deposit/depositbus"
	"github.com/jroedel/dropin-forms/business/domain/deposit/stores/depositdb"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// What the rules in depositbus cannot see from above: that the schema is the
// one this binary reads, that a payout is kept once, and that every field of a
// line comes back as it went in.

var now = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

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

	// Twice, because it runs at every startup.
	for range 2 {
		if err := depositdb.Init(t.Context(), db); err != nil {
			t.Fatalf("depositdb.Init: %v", err)
		}
	}

	if err := sqldb.CheckSchema(t.Context(), db, depositdb.Expected); err != nil {
		t.Fatalf("CheckSchema: %v", err)
	}

	return depositdb.NewStore(db)
}

func TestAPayoutAndItsLinesRoundTrip(t *testing.T) {
	store := open(t)

	form, err := types.ParseSlug("feast-lunch")
	if err != nil {
		t.Fatal(err)
	}

	id := types.NewID()

	p := depositbus.Payout{
		ID: "po_1", Amount: 4339, Currency: "usd", Created: now.Add(-48 * time.Hour), ArrivalDate: now,
		Automatic: true, Paid: true, Reconciled: true, FetchedAt: now,
	}
	l := depositbus.Line{
		ID: "txn_1", Payout: "po_1", Kind: depositbus.KindSale, Type: "charge", Description: "feast-lunch",
		Amount: 4500, Fee: 161, Net: 4339, Created: now.Add(-72 * time.Hour),
		PaymentRef: "pi_1", Form: form, SubmissionID: id,
	}

	if err := store.Save(t.Context(), p, []depositbus.Line{l}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A second save of the same payout -- two syncs overlapping -- is not an
	// error and does not double the lines.
	if err := store.Save(t.Context(), p, []depositbus.Line{l}); err != nil {
		t.Fatalf("Save again: %v", err)
	}

	payouts, lines, err := store.ForForm(t.Context(), form)
	if err != nil {
		t.Fatalf("ForForm: %v", err)
	}

	if len(payouts) != 1 || payouts[0] != p {
		t.Errorf("payout = %+v, want %+v", payouts, p)
	}

	if got := lines["po_1"]; len(got) != 1 || got[0] != l {
		t.Errorf("lines = %+v, want [%+v]", got, l)
	}

	latest, ok, err := store.Latest(t.Context())
	if err != nil || !ok || !latest.Equal(p.Created) {
		t.Errorf("Latest = %v, %v, %v", latest, ok, err)
	}

	deposited, err := store.Deposited(t.Context(), form)
	if err != nil || !deposited["pi_1"] || len(deposited) != 1 {
		t.Errorf("Deposited = %v, %v", deposited, err)
	}
}

func TestAnEmptyStoreHasNoLatest(t *testing.T) {
	store := open(t)

	if _, ok, err := store.Latest(t.Context()); err != nil || ok {
		t.Errorf("Latest = %v, %v; want none", ok, err)
	}
}

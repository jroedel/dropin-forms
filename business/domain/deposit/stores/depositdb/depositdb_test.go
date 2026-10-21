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

// A database from before destinations and traces were recorded: Init adds the
// columns to the rows already there, and those rows are the undescribed ones a
// sync goes back for.
func TestAnOlderDatabaseGainsTheTraceColumns(t *testing.T) {
	db, err := sqldb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	// The payouts table as the first release created it, with one deposit in
	// it.
	const old = `
CREATE TABLE payouts (
    id            TEXT    PRIMARY KEY,
    amount        INTEGER NOT NULL,
    currency      TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    arrival_date  INTEGER NOT NULL,
    automatic     INTEGER NOT NULL,
    mismatch      INTEGER NOT NULL,
    fetched_at    INTEGER NOT NULL
) STRICT;
INSERT INTO payouts VALUES ('po_old', 35708, 'usd', 1760700000000, 1760832000000, 1, 0, 1760900000000);`

	if _, err := db.ExecContext(t.Context(), old); err != nil {
		t.Fatalf("the old schema: %v", err)
	}

	for range 2 {
		if err := depositdb.Init(t.Context(), db); err != nil {
			t.Fatalf("depositdb.Init on an older database: %v", err)
		}
	}

	if err := sqldb.CheckSchema(t.Context(), db, depositdb.Expected); err != nil {
		t.Fatalf("CheckSchema: %v", err)
	}

	store := depositdb.NewStore(db)
	created := time.UnixMilli(1760700000000).UTC()

	at, ok, err := store.Undescribed(t.Context(), created.Add(-time.Hour))
	if err != nil || !ok || !at.Equal(created) {
		t.Fatalf("Undescribed = %v, %v, %v; want the old deposit", at, ok, err)
	}

	// Outside the window it is no longer asked about.
	if _, ok, _ := store.Undescribed(t.Context(), created.Add(time.Hour)); ok {
		t.Error("Undescribed reached back before since")
	}

	// A pending trace records the destination and leaves it undescribed.
	pending := depositbus.Payout{ID: "po_old", Destination: "ba_chase", Bank: "CHASE", Last4: "2013", TraceStatus: depositbus.TracePending}
	if err := store.Describe(t.Context(), pending); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, ok, _ := store.Undescribed(t.Context(), time.Time{}); !ok {
		t.Error("a pending trace counts as described")
	}

	traced := pending
	traced.TraceStatus, traced.TraceID = depositbus.TraceSupported, "021000029876543"
	if err := store.Describe(t.Context(), traced); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, ok, _ := store.Undescribed(t.Context(), time.Time{}); ok {
		t.Error("a traced deposit is still undescribed")
	}

	// And once described it is not overwritten by a later, emptier answer.
	if err := store.Describe(t.Context(), depositbus.Payout{ID: "po_old"}); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	var trace, last4 string
	if err := db.QueryRowContext(t.Context(), `SELECT trace_id, last4 FROM payouts WHERE id = 'po_old'`).Scan(&trace, &last4); err != nil {
		t.Fatal(err)
	}
	if trace != "021000029876543" || last4 != "2013" {
		t.Errorf("after a later empty answer: trace %q, last4 %q", trace, last4)
	}
}

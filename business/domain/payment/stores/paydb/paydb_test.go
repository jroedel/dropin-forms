package paydb_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/payment/stores/paydb"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// The dedupe, against the real database, because what makes it work is a
// primary key conflict and no fake reproduces that.

func store(t *testing.T) *paydb.Store {
	t.Helper()

	db, err := sqldb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	if err := sqldb.Init(t.Context(), db); err != nil {
		t.Fatalf("sqldb.Init: %v", err)
	}
	if err := paydb.Init(t.Context(), db); err != nil {
		t.Fatalf("paydb.Init: %v", err)
	}

	// Init is run at every startup, so it has to be idempotent.
	if err := paydb.Init(t.Context(), db); err != nil {
		t.Fatalf("paydb.Init is not idempotent: %v", err)
	}

	// And the columns this binary reads have to be the ones it created.
	expected := sqldb.Expected{}
	for table, columns := range sqldb.Infrastructure {
		expected[table] = columns
	}
	for table, columns := range paydb.Expected {
		expected[table] = columns
	}

	if err := sqldb.CheckSchema(t.Context(), db, expected); err != nil {
		t.Fatalf("CheckSchema: %v", err)
	}

	return paydb.NewStore(db)
}

func TestAnEventIsRecordedOnceAndOnlyOnce(t *testing.T) {
	s := store(t)
	now := time.Now()

	fresh, err := s.Record(t.Context(), "evt_1", "checkout.session.completed", now)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !fresh {
		t.Fatal("the first delivery was reported as already handled")
	}

	// The second is the whole point. The conflict is the check, so this is not
	// an error -- it is the answer.
	fresh, err = s.Record(t.Context(), "evt_1", "checkout.session.completed", now)
	if err != nil {
		t.Fatalf("a repeated delivery returned an error rather than false: %v", err)
	}
	if fresh {
		t.Error("the same event was recorded twice")
	}

	// A different event is unaffected, including one of the same kind.
	fresh, err = s.Record(t.Context(), "evt_2", "checkout.session.completed", now)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !fresh {
		t.Error("a different event was mistaken for one already handled")
	}
}

func TestPruningForgetsOnlyTheOldOnes(t *testing.T) {
	s := store(t)
	now := time.Now()

	old := now.Add(-40 * 24 * time.Hour)

	if _, err := s.Record(t.Context(), "evt_old", "checkout.session.completed", old); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := s.Record(t.Context(), "evt_new", "checkout.session.completed", now); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if err := s.Prune(t.Context(), now.Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	// The old one is forgotten, so its identifier is free again. That is the
	// consequence of pruning and is why the retention is generous: Stripe
	// retries for about three days, and forgetting too early means acting on
	// the same event twice.
	fresh, err := s.Record(t.Context(), "evt_old", "checkout.session.completed", now)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !fresh {
		t.Error("an event older than the retention was still remembered")
	}

	// The recent one is not.
	fresh, err = s.Record(t.Context(), "evt_new", "checkout.session.completed", now)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if fresh {
		t.Error("pruning forgot an event inside the retention window")
	}
}

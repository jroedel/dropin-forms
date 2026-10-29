package submissiondb_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/stores/submissiondb"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

func awaiting(t *testing.T, slot string) submissionbus.Submission {
	t.Helper()

	s := signup(t, slot)
	s.Review = submissionbus.ReviewAwaiting

	return s
}

func decided(s submissionbus.Submission, to submissionbus.Review, note string) submissionbus.Submission {
	s.Review = to
	s.ReviewedBy = types.NewID()
	s.ReviewedAt = now.Add(time.Hour)
	s.ReviewNote = note

	return s
}

func TestADecisionRoundTripsAndAnUndecidedOneReadsAsZero(t *testing.T) {
	_, store := open(t)

	s := awaiting(t, "a")
	if _, err := store.Accept(t.Context(), s, "one", nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	got, err := store.ByID(t.Context(), s.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if got.Review != submissionbus.ReviewAwaiting || !got.ReviewedAt.IsZero() || !got.ReviewedBy.Zero() {
		t.Errorf("an undecided submission read back as %q by %v at %v", got.Review, got.ReviewedBy, got.ReviewedAt)
	}

	no := decided(got, submissionbus.ReviewDeclined, "That week is full.")
	if err := store.Decide(t.Context(), got, no, nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	got, err = store.ByID(t.Context(), s.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if got.Review != no.Review || got.ReviewedBy != no.ReviewedBy || !got.ReviewedAt.Equal(no.ReviewedAt) || got.ReviewNote != no.ReviewNote {
		t.Errorf("the decision read back as %+v", got)
	}
}

func TestADecisionAgainstAStaleReadWritesNothing(t *testing.T) {
	_, store := open(t)

	s := awaiting(t, "a")
	if _, err := store.Accept(t.Context(), s, "one", nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	if err := store.Decide(t.Context(), s, decided(s, submissionbus.ReviewApproved, ""), nil); err != nil {
		t.Fatalf("the first Decide: %v", err)
	}

	// A second tab, still showing it as awaiting.
	err := store.Decide(t.Context(), s, decided(s, submissionbus.ReviewDeclined, ""), nil)
	if !errors.Is(err, submissionbus.ErrChangedMeanwhile) {
		t.Errorf("deciding from a stale read: %v, want ErrChangedMeanwhile", err)
	}
}

func TestAwaitingHoldsAPlaceDeclinedFreesItAndOnlyApprovedIsListed(t *testing.T) {
	_, store := open(t)

	waiting := awaiting(t, "a")
	turnedDown := awaiting(t, "b")
	yes := awaiting(t, "c")

	for i, s := range []submissionbus.Submission{waiting, turnedDown, yes} {
		if _, err := store.Accept(t.Context(), s, string(rune('a'+i)), nil); err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}

	if err := store.Decide(t.Context(), turnedDown, decided(turnedDown, submissionbus.ReviewDeclined, ""), nil); err != nil {
		t.Fatalf("declining: %v", err)
	}

	if err := store.Decide(t.Context(), yes, decided(yes, submissionbus.ReviewApproved, ""), nil); err != nil {
		t.Fatalf("approving: %v", err)
	}

	taken, err := store.Taken(t.Context(), waiting.Form)
	if err != nil {
		t.Fatalf("Taken: %v", err)
	}

	if taken.Count("slot", "a") != 1 || taken.Count("slot", "b") != 0 || taken.Count("slot", "c") != 1 {
		t.Errorf("Taken = %v; want the awaiting and approved holding, the declined not", taken)
	}

	listed, err := store.Settled(t.Context(), waiting.Form, 50)
	if err != nil {
		t.Fatalf("Settled: %v", err)
	}

	if len(listed) != 1 || listed[0].ID != yes.ID {
		t.Errorf("the public list has %d submissions; want only the approved one", len(listed))
	}
}

func TestReApprovingIntoATakenSlotIsRefused(t *testing.T) {
	_, store := open(t)
	slot := []formbus.Claim{{Field: "slot", Value: "a", Limit: 1}}

	first := awaiting(t, "a")
	if _, err := store.Accept(t.Context(), first, "one", slot); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	no := decided(first, submissionbus.ReviewDeclined, "")
	if err := store.Decide(t.Context(), first, no, nil); err != nil {
		t.Fatalf("declining: %v", err)
	}

	// The slot it gave back is taken by somebody else.
	if _, err := store.Accept(t.Context(), awaiting(t, "a"), "two", slot); err != nil {
		t.Fatalf("the second Accept, into the freed slot: %v", err)
	}

	err := store.Decide(t.Context(), no, decided(no, submissionbus.ReviewApproved, ""), slot)
	if _, ok := errors.AsType[submissionbus.OptionFull](err); !ok {
		t.Errorf("re-approving into a slot somebody else now holds: %v, want OptionFull", err)
	}
}

func TestAChangeCarriesTheReviewAndRefusesOneDecidedMeanwhile(t *testing.T) {
	_, store := open(t)

	s := awaiting(t, "a")
	if _, err := store.Accept(t.Context(), s, "one", nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	yes := decided(s, submissionbus.ReviewApproved, "")
	if err := store.Decide(t.Context(), s, yes, nil); err != nil {
		t.Fatalf("approving: %v", err)
	}

	// Changed back to waiting, as submissionbus does for a change to
	// approved answers.
	after := renamed(yes, "Maria Elena", now.Add(2*time.Hour))
	after.Review = submissionbus.ReviewAwaiting
	after.ReviewedBy, after.ReviewedAt = types.ID{}, time.Time{}

	if ok, err := store.Change(t.Context(), yes, after, "change", nil); err != nil || !ok {
		t.Fatalf("Change = %v, %v", ok, err)
	}

	got, err := store.ByID(t.Context(), s.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if got.Review != submissionbus.ReviewAwaiting {
		t.Errorf("after the change the review is %q, want awaiting", got.Review)
	}

	// A change drawn from a page that showed it before it was approved is
	// refused rather than written over the decision.
	stale := renamed(s, "Someone else", now.Add(3*time.Hour))
	if _, err := store.Change(t.Context(), s, stale, "stale", nil); !errors.Is(err, submissionbus.ErrChangedMeanwhile) {
		t.Errorf("a change from before the decision: %v, want ErrChangedMeanwhile", err)
	}
}

// A database made before reviews existed gains the columns at the next
// startup, and every row in it reads as one nobody was asked to approve.
func TestAnOlderDatabaseGainsTheReviewColumns(t *testing.T) {
	db, err := sqldb.Open(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	if err := sqldb.Init(t.Context(), db); err != nil {
		t.Fatalf("sqldb.Init: %v", err)
	}

	const before = `
CREATE TABLE submissions (
    id TEXT PRIMARY KEY, form_slug TEXT NOT NULL, version TEXT NOT NULL, status TEXT NOT NULL,
    answers TEXT NOT NULL, email TEXT NOT NULL, total INTEGER NOT NULL, currency TEXT NOT NULL,
    remote_ip TEXT NOT NULL, payment_ref TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
) STRICT;
INSERT INTO submissions VALUES (?, 'old-form', 'abc123def456', 'received', '{"v":1}', '', 0, 'usd', '', '', 1, 1);`

	id := types.NewID()
	if _, err := db.ExecContext(t.Context(), before, id.String()); err != nil {
		t.Fatalf("making the old table: %v", err)
	}

	if err := submissiondb.Init(t.Context(), db); err != nil {
		t.Fatalf("Init over the old table: %v", err)
	}

	if err := sqldb.CheckSchema(t.Context(), db, submissiondb.Expected); err != nil {
		t.Errorf("CheckSchema after the upgrade: %v", err)
	}

	got, err := submissiondb.NewStore(db).ByID(t.Context(), id)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if got.Review != submissionbus.ReviewNone || !got.ReviewedAt.IsZero() {
		t.Errorf("an old row reads as review %q at %v, want none", got.Review, got.ReviewedAt)
	}
}

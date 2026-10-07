package submissiondb_test

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// changeable is the sample with nothing for sale, which is the only kind of
// submission whose answers can change.
func changeable(t *testing.T) submissionbus.Submission {
	t.Helper()

	s := sample(t)
	s.Status = submissionbus.StatusReceived
	s.Answers.Lines = nil
	s.Answers.Total = 0

	return s
}

// renamed is s as it would be after its name answer was changed at at.
func renamed(s submissionbus.Submission, name string, at time.Time) submissionbus.Submission {
	s.Answers.Fields = slices.Clone(s.Answers.Fields)
	s.Answers.Fields[0].Values = []string{name}
	s.Version = "fedcba987654"
	s.Answers.Version = s.Version
	s.RemoteIP = "198.51.100.4"
	s.UpdatedAt = at

	return s
}

func TestAChangeReplacesTheRowAndKeepsTheOldAnswers(t *testing.T) {
	_, store := open(t)
	ctx := t.Context()

	first := changeable(t)
	if _, err := store.Accept(ctx, first, "nonce-1"); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	second := renamed(first, "Fr. Hector", now.Add(time.Hour))
	if ok, err := store.Change(ctx, first, second, "nonce-2"); err != nil || !ok {
		t.Fatalf("Change: %v, %v", ok, err)
	}

	third := renamed(second, "Fr. Héctor", now.Add(2*time.Hour))
	if ok, err := store.Change(ctx, second, third, "nonce-3"); err != nil || !ok {
		t.Fatalf("second Change: %v, %v", ok, err)
	}

	got, err := store.ByID(ctx, first.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if v := got.Answers.Fields[0].Value(); v != "Fr. Héctor" || got.Version != "fedcba987654" || !got.UpdatedAt.Equal(third.UpdatedAt) {
		t.Errorf("the row is %q at %s, %v; want the latest answers", v, got.Version, got.UpdatedAt)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("created_at moved to %v", got.CreatedAt)
	}

	revs, err := store.Revisions(ctx, got)
	if err != nil {
		t.Fatalf("Revisions: %v", err)
	}
	if len(revs) != 2 {
		t.Fatalf("%d revisions, want 2", len(revs))
	}

	want := []struct {
		name     string
		version  string
		from, to time.Time
	}{
		{"Maria O'Neill", "abc123def456", first.UpdatedAt, second.UpdatedAt},
		{"Fr. Hector", "fedcba987654", second.UpdatedAt, third.UpdatedAt},
	}

	for i, w := range want {
		r := revs[i]
		if r.Number != i+1 || r.Answers.Fields[0].Value() != w.name || r.Version != w.version ||
			!r.From.Equal(w.from) || !r.Until.Equal(w.to) {
			t.Errorf("revision %d is #%d %q %s %v-%v; want #%d %q %s %v-%v",
				i, r.Number, r.Answers.Fields[0].Value(), r.Version, r.From, r.Until,
				i+1, w.name, w.version, w.from, w.to)
		}
	}

	if got := revs[0].Email.String(); got != "maria@example.org" {
		t.Errorf("revision address is %q", got)
	}
}

func TestAChangeAgainstAStaleReadWritesNothing(t *testing.T) {
	_, store := open(t)
	ctx := t.Context()

	first := changeable(t)
	if _, err := store.Accept(ctx, first, "nonce-1"); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	won := renamed(first, "From the first tab", now.Add(time.Hour))
	if _, err := store.Change(ctx, first, won, "nonce-2"); err != nil {
		t.Fatalf("Change: %v", err)
	}

	// The second tab read the row before the first tab saved.
	lost := renamed(first, "From the second tab", now.Add(time.Hour+time.Second))

	_, err := store.Change(ctx, first, lost, "nonce-3")
	if !errors.Is(err, submissionbus.ErrChangedMeanwhile) {
		t.Fatalf("Change: %v, want ErrChangedMeanwhile", err)
	}

	got, _ := store.ByID(ctx, first.ID)
	if v := got.Answers.Fields[0].Value(); v != "From the first tab" {
		t.Errorf("the row says %q", v)
	}

	revs, _ := store.Revisions(ctx, got)
	if len(revs) != 1 {
		t.Errorf("%d revisions; the losing change must write none", len(revs))
	}

	// And its grant was not spent, since nothing was written: the
	// transaction rolled back as a whole.
	if ok, err := store.Change(ctx, got, renamed(got, "Again", now.Add(2*time.Hour)), "nonce-3"); err != nil || !ok {
		t.Errorf("the nonce of a rolled-back change: %v, %v; want it still spendable", ok, err)
	}
}

func TestAHiddenSubmissionCannotBeChanged(t *testing.T) {
	_, store := open(t)
	ctx := t.Context()

	first := changeable(t)
	if _, err := store.Accept(ctx, first, "nonce-1"); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	if _, _, err := store.Hide(ctx, submissionbus.Hiding{
		SubmissionID: first.ID, Form: first.Form, HiddenAt: now, HiddenBy: types.NewID(),
	}); err != nil {
		t.Fatalf("Hide: %v", err)
	}

	_, err := store.Change(ctx, first, renamed(first, "x", now.Add(time.Hour)), "nonce-2")
	if !errors.Is(err, submissionbus.ErrChangedMeanwhile) {
		t.Fatalf("Change: %v, want ErrChangedMeanwhile", err)
	}
}

func TestAReplayedChangeIsRefused(t *testing.T) {
	_, store := open(t)
	ctx := t.Context()

	first := changeable(t)
	if _, err := store.Accept(ctx, first, "nonce-1"); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	ok, err := store.Change(ctx, first, renamed(first, "x", now.Add(time.Hour)), "nonce-1")
	if err != nil || ok {
		t.Fatalf("Change with a spent nonce: %v, %v; want false and no error", ok, err)
	}
}

func TestConcurrentChangesProduceOneWinner(t *testing.T) {
	_, store := open(t)
	ctx := t.Context()

	first := changeable(t)
	if _, err := store.Accept(ctx, first, "nonce-1"); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	const n = 8

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)

	for i := range n {
		wg.Go(func() {
			next := renamed(first, "tab", now.Add(time.Duration(i+1)*time.Second))
			ok, err := store.Change(ctx, first, next, "nonce-c"+string(rune('a'+i)))

			mu.Lock()
			defer mu.Unlock()

			switch {
			case err == nil && ok:
				wins++
			case errors.Is(err, submissionbus.ErrChangedMeanwhile):
			default:
				t.Errorf("Change: %v, %v", ok, err)
			}
		})
	}

	wg.Wait()

	if wins != 1 {
		t.Fatalf("%d changes won, want exactly 1", wins)
	}

	got, _ := store.ByID(ctx, first.ID)
	revs, _ := store.Revisions(ctx, got)
	if len(revs) != 1 {
		t.Errorf("%d revisions, want 1", len(revs))
	}
}

func TestByEmailFindsOneAddressOnOneFormAndSkipsTheHidden(t *testing.T) {
	_, store := open(t)
	ctx := t.Context()

	mine := changeable(t)
	if _, err := store.Accept(ctx, mine, "n1"); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	hidden := changeable(t)
	hidden.ID = types.NewID()
	if _, err := store.Accept(ctx, hidden, "n2"); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, _, err := store.Hide(ctx, submissionbus.Hiding{SubmissionID: hidden.ID, Form: hidden.Form, HiddenAt: now, HiddenBy: types.NewID()}); err != nil {
		t.Fatalf("Hide: %v", err)
	}

	other := changeable(t)
	other.ID = types.NewID()
	other.Email = mustEmail(t, "someone@example.org")
	if _, err := store.Accept(ctx, other, "n3"); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	got, err := store.ByEmail(ctx, mine.Form, mine.Email)
	if err != nil {
		t.Fatalf("ByEmail: %v", err)
	}
	if len(got) != 1 || got[0].ID != mine.ID {
		t.Errorf("ByEmail = %d rows, want only the visible one from this address", len(got))
	}

	if got, _ := store.ByEmail(ctx, mustSlug(t, "elsewhere"), mine.Email); len(got) != 0 {
		t.Errorf("ByEmail on another form = %d rows", len(got))
	}
}

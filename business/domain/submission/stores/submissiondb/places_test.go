package submissiondb_test

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// signup is a free submission that takes a time slot and, beside it, any
// number of extra blessings -- one limited field of each shape.
func signup(t *testing.T, slot string, also ...string) submissionbus.Submission {
	t.Helper()

	s := changeable(t)
	s.ID = types.NewID()
	s.Answers.Fields = []formbus.Answer{
		{Name: "name", Label: "Your name", Kind: formbus.KindText, Values: []string{"Maria"}},
		{Name: "slot", Label: "Time slot", Kind: formbus.KindRadio, Values: []string{slot}},
	}

	if len(also) > 0 {
		s.Answers.Fields = append(s.Answers.Fields,
			formbus.Answer{Name: "also", Label: "Also bless", Kind: formbus.KindChoices, Values: also})
	}

	return s
}

func TestTakenCountsThePlacesHeld(t *testing.T) {
	_, store := open(t)

	with := func(s submissionbus.Submission, st submissionbus.Status) submissionbus.Submission {
		s.Status = st
		return s
	}

	hidden := signup(t, "b")
	elsewhere := signup(t, "a")
	elsewhere.Form = mustSlug(t, "another-form")

	subs := []submissionbus.Submission{
		with(signup(t, "a", "car", "garden"), submissionbus.StatusReceived),
		with(signup(t, "a", "car"), submissionbus.StatusPending), // waiting for its payment: holds
		with(signup(t, "b"), submissionbus.StatusPaid),
		with(signup(t, "a"), submissionbus.StatusFailed),    // holds nothing
		with(signup(t, "a"), submissionbus.StatusAbandoned), // holds nothing
		hidden,
		elsewhere,
	}

	for i, s := range subs {
		if _, err := store.Accept(t.Context(), s, fmt.Sprint(i), nil); err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}

	if _, _, err := store.Hide(t.Context(), submissionbus.Hiding{
		SubmissionID: hidden.ID, Form: hidden.Form, HiddenAt: now, HiddenBy: types.NewID(),
	}); err != nil {
		t.Fatalf("Hide: %v", err)
	}

	taken, err := store.Taken(t.Context(), mustSlug(t, "feast-lunch-2026"))
	if err != nil {
		t.Fatalf("Taken: %v", err)
	}

	for _, tt := range []struct {
		field, value string
		want         int
	}{
		{"slot", "a", 2},
		{"slot", "b", 1}, // the hidden one has given its place back
		{"also", "car", 2},
		{"also", "garden", 1},
	} {
		if got := taken.Count(tt.field, tt.value); got != tt.want {
			t.Errorf("Taken %s=%s = %d, want %d", tt.field, tt.value, got, tt.want)
		}
	}
}

func TestTheLastPlaceIsRefusedAndNothingIsWritten(t *testing.T) {
	_, store := open(t)
	limit := func(value string) []formbus.Claim {
		return []formbus.Claim{{Field: "slot", Value: value, Limit: 1}}
	}

	first := signup(t, "a")
	if ok, err := store.Accept(t.Context(), first, "one", limit("a")); err != nil || !ok {
		t.Fatalf("the first Accept = %v, %v", ok, err)
	}

	second := signup(t, "a")
	_, err := store.Accept(t.Context(), second, "two", limit("a"))

	if full, ok := errors.AsType[submissionbus.OptionFull](err); !ok || full.Value != "a" {
		t.Fatalf("the second Accept: %v, want OptionFull for a", err)
	}

	got, err := store.ByForm(t.Context(), first.Form)
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}

	if len(got) != 1 {
		t.Errorf("%d submissions stored, want only the one that got the place", len(got))
	}

	// The grant was not spent: the person is shown the form again and picks
	// another slot with the same one.
	other := signup(t, "b")
	if ok, err := store.Accept(t.Context(), other, "two", limit("b")); err != nil || !ok {
		t.Errorf("choosing another slot with the same grant = %v, %v; a refused claim must spend nothing", ok, err)
	}
}

func TestAChangeKeepsItsOwnPlaceButCannotTakeSomebodyElses(t *testing.T) {
	_, store := open(t)
	limit := func(value string) []formbus.Claim {
		return []formbus.Claim{{Field: "slot", Value: value, Limit: 1}}
	}

	mine := signup(t, "a")
	theirs := signup(t, "b")

	for i, s := range []submissionbus.Submission{mine, theirs} {
		if _, err := store.Accept(t.Context(), s, fmt.Sprint(i), limit(s.Answers.Fields[1].Value())); err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}

	kept := renamed(mine, "Maria Elena", now.Add(time.Hour))
	if ok, err := store.Change(t.Context(), mine, kept, "keep", limit("a")); err != nil || !ok {
		t.Fatalf("changing a name and keeping one's own slot = %v, %v", ok, err)
	}

	moved := renamed(kept, "Maria Elena", now.Add(2*time.Hour))
	moved.Answers.Fields = slices.Clone(moved.Answers.Fields)
	moved.Answers.Fields[1].Values = []string{"b"}

	_, err := store.Change(t.Context(), kept, moved, "move", limit("b"))
	if _, ok := errors.AsType[submissionbus.OptionFull](err); !ok {
		t.Errorf("moving into a slot somebody else holds: %v, want OptionFull", err)
	}
}

func TestConcurrentClaimsOnTheLastPlaceProduceOneWinner(t *testing.T) {
	_, store := open(t)
	claims := []formbus.Claim{{Field: "slot", Value: "a", Limit: 1}}

	const racers = 8

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		won  int
		full int
	)

	for i := range racers {
		wg.Go(func() {
			_, err := store.Accept(t.Context(), signup(t, "a"), fmt.Sprint(i), claims)

			mu.Lock()
			defer mu.Unlock()

			switch _, isFull := errors.AsType[submissionbus.OptionFull](err); {
			case err == nil:
				won++
			case isFull:
				full++
			default:
				t.Errorf("Accept: %v", err)
			}
		})
	}

	wg.Wait()

	if won != 1 || full != racers-1 {
		t.Errorf("%d took the place and %d were told it was full; want 1 and %d", won, full, racers-1)
	}
}

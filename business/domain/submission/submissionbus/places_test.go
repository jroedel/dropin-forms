package submissionbus_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
)

func TestAWaitingOrderHoldsAPlaceAndAnAbandonedOneDoesNot(t *testing.T) {
	got := submissionbus.PlaceHoldingStatuses()
	want := []submissionbus.Status{submissionbus.StatusReceived, submissionbus.StatusPending, submissionbus.StatusPaid}

	for _, st := range want {
		if !slices.Contains(got, st) {
			t.Errorf("%s does not hold a place, so the last slot could be sold twice", st)
		}
	}

	for _, st := range []submissionbus.Status{submissionbus.StatusFailed, submissionbus.StatusAbandoned} {
		if slices.Contains(got, st) {
			t.Errorf("%s holds a place, so a checkout nobody finished keeps a slot from everyone", st)
		}
	}
}

func TestTheLastPlaceGoesToOnePerson(t *testing.T) {
	b, _ := newBusiness()
	form := mustSlug(t, "visits")
	slot := []formbus.Claim{{Field: "name", Value: "Maria", Limit: 1}}

	first := answers(t, form, 0)
	if _, err := b.Accept(t.Context(), now, grantFor(first, "one"), submissionbus.New{Answers: first, Claims: slot}); err != nil {
		t.Fatalf("the first Accept: %v", err)
	}

	second := answers(t, form, 0)
	_, err := b.Accept(t.Context(), now, grantFor(second, "two"), submissionbus.New{Answers: second, Claims: slot})

	full, ok := errors.AsType[submissionbus.OptionFull](err)
	if !ok {
		t.Fatalf("the second Accept: %v, want OptionFull", err)
	}

	if full.Field != "name" || full.Value != "Maria" {
		t.Errorf("OptionFull names %q of %q, want the option that was full", full.Value, full.Field)
	}

	taken, err := b.Taken(t.Context(), form)
	if err != nil {
		t.Fatalf("Taken: %v", err)
	}

	if n := taken.Count("name", "Maria"); n != 1 {
		t.Errorf("Taken counts %d, want the one submission that was accepted", n)
	}
}

func TestKeepingYourOwnPlaceIsNotRefused(t *testing.T) {
	b, _ := newBusiness()
	form := mustSlug(t, "visits")
	s := accepted(t, b, form)
	slot := []formbus.Claim{{Field: "name", Value: "Maria", Limit: 1}}

	a := answers(t, form, 0)
	a.Fields = append(a.Fields, formbus.Answer{Name: "note", Label: "Note", Kind: formbus.KindText, Values: []string{"ring twice"}})

	_, _, err := b.Change(t.Context(), now, grantFor(a, "change"), submissionbus.Edit{
		ID: s.ID, Seen: s.UpdatedAt, Answers: a, Claims: slot,
	})
	if err != nil {
		t.Errorf("Change keeping the same place: %v; a person's own place must not count against them", err)
	}
}

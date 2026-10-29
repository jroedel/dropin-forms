package submissionbus_test

import (
	"errors"
	"testing"

	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

func waiting(t *testing.T, b *submissionbus.Business, nonce string) submissionbus.Submission {
	t.Helper()

	a := answers(t, mustSlug(t, "visits"), 0)

	s, err := b.Accept(t.Context(), now, grantFor(a, nonce), submissionbus.New{Answers: a, NeedsApproval: true})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	if s.Review != submissionbus.ReviewAwaiting {
		t.Fatalf("a submission to a form that asks for approval is %q, want awaiting", s.Review)
	}

	return s
}

func TestWhatMayBeDecided(t *testing.T) {
	b, _ := newBusiness()
	s := waiting(t, b, "one")
	by := types.NewID()

	decide := func(to submissionbus.Review) error {
		_, _, err := b.Decide(t.Context(), now, submissionbus.Decision{ID: s.ID, Form: s.Form, To: to, By: by, Note: " Full that week. "})
		return err
	}

	steps := []struct {
		to   submissionbus.Review
		want error
	}{
		{submissionbus.ReviewApproved, nil},
		{submissionbus.ReviewApproved, submissionbus.ErrNotDecidable}, // already approved
		{submissionbus.ReviewDeclined, nil},                           // a booking cancelled
		{submissionbus.ReviewApproved, nil},                           // a mind changed
	}

	for i, st := range steps {
		if err := decide(st.to); !errors.Is(err, st.want) {
			t.Fatalf("step %d, to %s: %v, want %v", i+1, st.to, err, st.want)
		}
	}

	got, err := b.ByID(t.Context(), s.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if got.Review != submissionbus.ReviewApproved || got.ReviewedBy != by || got.ReviewNote != "" {
		t.Errorf("after approving again it reads %q by %v with note %q", got.Review, got.ReviewedBy, got.ReviewNote)
	}

	if err := decide(submissionbus.ReviewAwaiting); err == nil {
		t.Error("a decision back to awaiting was accepted")
	}
}

func TestADeclineKeepsItsNoteAndLeavesTheLists(t *testing.T) {
	b, _ := newBusiness()
	s := waiting(t, b, "one")

	_, after, err := b.Decide(t.Context(), now, submissionbus.Decision{
		ID: s.ID, Form: s.Form, To: submissionbus.ReviewDeclined, By: types.NewID(), Note: "  Full that week.  ",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if after.ReviewNote != "Full that week." {
		t.Errorf("the note is %q", after.ReviewNote)
	}

	listed, err := b.ByForm(t.Context(), s.Form)
	if err != nil || len(listed) != 0 {
		t.Errorf("ByForm = %d, %v; a declined submission must leave the list", len(listed), err)
	}

	declined, err := b.Declined(t.Context(), s.Form)
	if err != nil || len(declined) != 1 {
		t.Errorf("Declined = %d, %v; want the one declined", len(declined), err)
	}

	if _, err := b.Mine(t.Context(), s.Form, s.ID); !errors.Is(err, submissionbus.ErrNotFound) {
		t.Errorf("Mine on a declined submission: %v, want ErrNotFound", err)
	}
}

func TestOnlyASubmissionThatWasAskedCanBeDecided(t *testing.T) {
	b, store := newBusiness()
	plain := accepted(t, b, mustSlug(t, "visits"))
	s := waiting(t, b, "two")

	cases := []struct {
		name string
		d    submissionbus.Decision
		want error
	}{
		{"one on a form that did not ask", submissionbus.Decision{ID: plain.ID, Form: plain.Form, To: submissionbus.ReviewApproved}, submissionbus.ErrNotDecidable},
		{"one named under another form", submissionbus.Decision{ID: s.ID, Form: mustSlug(t, "other"), To: submissionbus.ReviewApproved}, submissionbus.ErrNotFound},
	}

	for _, tt := range cases {
		if _, _, err := b.Decide(t.Context(), now, tt.d); !errors.Is(err, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, err, tt.want)
		}
	}

	store.hidden[s.ID] = submissionbus.Hiding{SubmissionID: s.ID}

	if _, _, err := b.Decide(t.Context(), now, submissionbus.Decision{ID: s.ID, Form: s.Form, To: submissionbus.ReviewApproved}); !errors.Is(err, submissionbus.ErrNotFound) {
		t.Errorf("a hidden one: %v, want ErrNotFound", err)
	}
}

func TestChangingApprovedAnswersSendsThemBackToWaiting(t *testing.T) {
	b, _ := newBusiness()
	s := waiting(t, b, "one")

	_, yes, err := b.Decide(t.Context(), now, submissionbus.Decision{ID: s.ID, Form: s.Form, To: submissionbus.ReviewApproved, By: types.NewID()})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}

	_, after, err := b.Change(t.Context(), now, grantFor(withName(s.Answers, "Maria Elena"), "change"), submissionbus.Edit{
		ID: s.ID, Seen: yes.UpdatedAt, Answers: withName(s.Answers, "Maria Elena"), NeedsApproval: true,
	})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}

	if after.Review != submissionbus.ReviewAwaiting || !after.ReviewedAt.IsZero() {
		t.Errorf("after a change the review is %q decided at %v; want awaiting, undecided", after.Review, after.ReviewedAt)
	}
}

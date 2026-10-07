package submissionbus_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

func (m *memStore) Change(_ context.Context, before, after submissionbus.Submission, nonce string) (bool, error) {
	if m.fail != nil {
		return false, m.fail
	}

	if _, spent := m.nonces[nonce]; spent {
		return false, nil
	}

	cur, ok := m.subs[before.ID]
	if _, hidden := m.hidden[before.ID]; !ok || hidden || !cur.UpdatedAt.Equal(before.UpdatedAt) {
		return false, submissionbus.ErrChangedMeanwhile
	}

	m.nonces[nonce] = after.UpdatedAt
	m.subs[after.ID] = after
	m.revs[before.ID] = append(m.revs[before.ID], submissionbus.Revision{
		SubmissionID: before.ID,
		Number:       len(m.revs[before.ID]) + 1,
		Version:      before.Version,
		Answers:      before.Answers,
		Email:        before.Email,
		RemoteIP:     before.RemoteIP,
		From:         before.UpdatedAt,
		Until:        after.UpdatedAt,
	})

	return true, nil
}

func (m *memStore) Revisions(_ context.Context, s submissionbus.Submission) ([]submissionbus.Revision, error) {
	return slices.Clone(m.revs[s.ID]), nil
}

// accepted stores one free submission and returns it.
func accepted(t *testing.T, b *submissionbus.Business, form types.Slug) submissionbus.Submission {
	t.Helper()

	a := answers(t, form, 0)

	s, err := b.Accept(t.Context(), now, grantFor(a, "first"), submissionbus.New{Answers: a})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	return s
}

// withName is the same answers with a different name in them.
func withName(a formbus.Answers, name string) formbus.Answers {
	a.Fields = slices.Clone(a.Fields)
	a.Fields[0].Values = []string{name}

	return a
}

func TestAChangeKeepsWhatItReplaced(t *testing.T) {
	b, store := newBusiness()
	form := mustSlug(t, "ordination")
	s := accepted(t, b, form)

	later := now.Add(48 * time.Hour)
	next := withName(s.Answers, "Fr. Hector")

	before, after, err := b.Change(t.Context(), later, grantFor(next, "second"), submissionbus.Edit{
		ID: s.ID, Seen: s.UpdatedAt, Answers: next, RemoteIP: "203.0.113.9",
	})
	if err != nil {
		t.Fatalf("Change: %v", err)
	}

	if got := before.Answers.Fields[0].Value(); got != "Maria" {
		t.Errorf("before says %q, want what was first sent", got)
	}
	if got := after.Answers.Fields[0].Value(); got != "Fr. Hector" {
		t.Errorf("after says %q, want the new answer", got)
	}
	if !after.CreatedAt.Equal(s.CreatedAt) || !after.UpdatedAt.Equal(later.UTC()) {
		t.Errorf("times are %v / %v; created must not move and updated must be the change", after.CreatedAt, after.UpdatedAt)
	}
	if after.Status != submissionbus.StatusReceived {
		t.Errorf("status is %s; a change is not a status", after.Status)
	}

	revs, err := b.Revisions(t.Context(), after)
	if err != nil {
		t.Fatalf("Revisions: %v", err)
	}
	if len(revs) != 1 || revs[0].Number != 1 || revs[0].Answers.Fields[0].Value() != "Maria" {
		t.Fatalf("revisions are %+v, want the first answers as revision 1", revs)
	}
	if !revs[0].Until.Equal(later.UTC()) {
		t.Errorf("revision ends %v, want the moment it was replaced", revs[0].Until)
	}

	if got := store.subs[s.ID].Answers.Fields[0].Value(); got != "Fr. Hector" {
		t.Errorf("the stored row says %q, want the current answer", got)
	}
}

func TestAChangeIsRefused(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, b *submissionbus.Business, store *memStore, s submissionbus.Submission) (formbus.Answers, formbus.Grant, types.ID)
		want  error
	}{
		{
			name: "when nothing is different, as an outcome",
			setup: func(t *testing.T, b *submissionbus.Business, store *memStore, s submissionbus.Submission) (formbus.Answers, formbus.Grant, types.ID) {
				return s.Answers, grantFor(s.Answers, "second"), s.ID
			},
			want: submissionbus.ErrUnchanged,
		},
		{
			name: "when the grant was already spent",
			setup: func(t *testing.T, b *submissionbus.Business, store *memStore, s submissionbus.Submission) (formbus.Answers, formbus.Grant, types.ID) {
				next := withName(s.Answers, "Someone else")
				return next, grantFor(next, "first"), s.ID
			},
			want: submissionbus.ErrReplayed,
		},
		{
			name: "when the submission is hidden, which is how one link is withdrawn",
			setup: func(t *testing.T, b *submissionbus.Business, store *memStore, s submissionbus.Submission) (formbus.Answers, formbus.Grant, types.ID) {
				if _, _, err := b.Hide(t.Context(), now, s.ID, types.NewID()); err != nil {
					t.Fatalf("Hide: %v", err)
				}
				next := withName(s.Answers, "Someone else")
				return next, grantFor(next, "second"), s.ID
			},
			want: submissionbus.ErrNotFound,
		},
		{
			name: "when the link was for another form",
			setup: func(t *testing.T, b *submissionbus.Business, store *memStore, s submissionbus.Submission) (formbus.Answers, formbus.Grant, types.ID) {
				next := withName(s.Answers, "Someone else")
				next.FormID = mustSlug(t, "a-different-form")
				return next, grantFor(next, "second"), s.ID
			},
			want: submissionbus.ErrNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, store := newBusiness()
			s := accepted(t, b, mustSlug(t, "ordination"))

			next, g, id := tc.setup(t, b, store, s)

			_, _, err := b.Change(t.Context(), now.Add(time.Hour), g, submissionbus.Edit{ID: id, Seen: s.UpdatedAt, Answers: next})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Change: %v, want %v", err, tc.want)
			}

			if len(store.revs[s.ID]) != 0 {
				t.Errorf("a refused change wrote a revision: %+v", store.revs[s.ID])
			}
		})
	}
}

func TestAnswersThatComeToMoneyCannotBeChanged(t *testing.T) {
	b, _ := newBusiness()
	s := accepted(t, b, mustSlug(t, "ordination"))

	next := withName(s.Answers, "Someone else")
	next.Total = types.Money(500)

	_, _, err := b.Change(t.Context(), now, grantFor(next, "second"), submissionbus.Edit{ID: s.ID, Seen: s.UpdatedAt, Answers: next})
	if err == nil || !strings.Contains(err.Error(), "money") {
		t.Fatalf("Change: %v, want a refusal about money", err)
	}
}

func TestMineFindsOnlyAReceivedSubmissionOnItsOwnForm(t *testing.T) {
	b, _ := newBusiness()
	form := mustSlug(t, "ordination")
	s := accepted(t, b, form)

	if _, err := b.Mine(t.Context(), form, s.ID); err != nil {
		t.Fatalf("Mine on its own form: %v", err)
	}

	if _, err := b.Mine(t.Context(), mustSlug(t, "elsewhere"), s.ID); !errors.Is(err, submissionbus.ErrNotFound) {
		t.Errorf("Mine on another form: %v, want ErrNotFound", err)
	}

	paid := answers(t, form, types.Money(500))
	order, err := b.Accept(t.Context(), now, grantFor(paid, "order"), submissionbus.New{Answers: paid})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	if _, err := b.Mine(t.Context(), form, order.ID); !errors.Is(err, submissionbus.ErrNotFound) {
		t.Errorf("Mine on a pending order: %v, want ErrNotFound", err)
	}
}

func TestAChangeDrawnFromOlderAnswersIsRefused(t *testing.T) {
	b, store := newBusiness()
	s := accepted(t, b, mustSlug(t, "ordination"))

	first := withName(s.Answers, "From the first tab")
	if _, _, err := b.Change(t.Context(), now.Add(time.Minute), grantFor(first, "second"),
		submissionbus.Edit{ID: s.ID, Seen: s.UpdatedAt, Answers: first}); err != nil {
		t.Fatalf("first Change: %v", err)
	}

	// The second tab was opened before the first saved, so it saw s.
	second := withName(s.Answers, "From the second tab")

	_, _, err := b.Change(t.Context(), now.Add(2*time.Minute), grantFor(second, "third"),
		submissionbus.Edit{ID: s.ID, Seen: s.UpdatedAt, Answers: second})
	if !errors.Is(err, submissionbus.ErrChangedMeanwhile) {
		t.Fatalf("second Change: %v, want ErrChangedMeanwhile", err)
	}

	if got := store.subs[s.ID].Answers.Fields[0].Value(); got != "From the first tab" {
		t.Errorf("the row says %q", got)
	}
}

func TestAChangeLosesToOneThatLandedFirst(t *testing.T) {
	b, store := newBusiness()
	s := accepted(t, b, mustSlug(t, "ordination"))

	// Somebody else's change lands between this one's read and its write.
	moved := store.subs[s.ID]
	moved.UpdatedAt = moved.UpdatedAt.Add(time.Second)

	racing := submissionbus.NewBusiness(discard(), &racingStore{memStore: store, moved: moved})
	next := withName(s.Answers, "Someone else")

	_, _, err := racing.Change(t.Context(), now.Add(time.Hour), grantFor(next, "second"), submissionbus.Edit{ID: s.ID, Seen: s.UpdatedAt, Answers: next})
	if !errors.Is(err, submissionbus.ErrChangedMeanwhile) {
		t.Fatalf("Change: %v, want ErrChangedMeanwhile", err)
	}

	if got := store.subs[s.ID].Answers.Fields[0].Value(); got != "Maria" {
		t.Errorf("the row says %q; the losing change must write nothing", got)
	}
}

// racingStore answers reads with the row as Mine first saw it and writes as
// if somebody else had changed it in between.
type racingStore struct {
	*memStore
	moved submissionbus.Submission
}

func (r *racingStore) Change(ctx context.Context, before, after submissionbus.Submission, nonce string) (bool, error) {
	r.memStore.subs[r.moved.ID] = r.moved

	return r.memStore.Change(ctx, before, after, nonce)
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

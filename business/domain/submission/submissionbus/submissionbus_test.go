package submissionbus_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

var now = time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

// memStore is a submissionbus.Storer in a map.
//
// The nonce set is the interesting part: Accept here refuses a nonce it has
// already seen, which is the same contract the SQLite store gets from a
// primary key. A double double that accepted twice would let a replay bug
// pass.
type memStore struct {
	subs   map[types.ID]submissionbus.Submission
	nonces map[string]time.Time
	handed map[types.ID]submissionbus.Collection

	// counted is how many times the day's submissions have been counted, so
	// that a test can assert an uncapped form does not pay for the query.
	counted int

	fail error
}

func newMemStore() *memStore {
	return &memStore{
		subs:   map[types.ID]submissionbus.Submission{},
		nonces: map[string]time.Time{},
		handed: map[types.ID]submissionbus.Collection{},
	}
}

func (m *memStore) Accept(_ context.Context, s submissionbus.Submission, nonce string) (bool, error) {
	if m.fail != nil {
		return false, m.fail
	}

	if _, spent := m.nonces[nonce]; spent {
		return false, nil
	}

	m.nonces[nonce] = s.CreatedAt
	m.subs[s.ID] = s

	return true, nil
}

func (m *memStore) ByID(_ context.Context, id types.ID) (submissionbus.Submission, error) {
	if m.fail != nil {
		return submissionbus.Submission{}, m.fail
	}

	s, ok := m.subs[id]
	if !ok {
		return submissionbus.Submission{}, submissionbus.ErrNotFound
	}

	return s, nil
}

func (m *memStore) ByForm(_ context.Context, form types.Slug) ([]submissionbus.Submission, error) {
	if m.fail != nil {
		return nil, m.fail
	}

	var out []submissionbus.Submission
	for _, s := range m.subs {
		if s.Form == form {
			out = append(out, s)
		}
	}

	return out, nil
}

func (m *memStore) Unpaid(_ context.Context, from, before time.Time) ([]submissionbus.Submission, error) {
	if m.fail != nil {
		return nil, m.fail
	}

	var out []submissionbus.Submission
	for _, s := range m.subs {
		if s.Status != submissionbus.StatusPending {
			continue
		}

		if s.CreatedAt.Before(from) || !s.CreatedAt.Before(before) {
			continue
		}

		out = append(out, s)
	}

	return out, nil
}

func (m *memStore) CountSince(_ context.Context, form types.Slug, since time.Time) (int, error) {
	m.counted++

	if m.fail != nil {
		return 0, m.fail
	}

	var n int
	for _, s := range m.subs {
		if s.Form == form && !s.CreatedAt.Before(since) {
			n++
		}
	}

	return n, nil
}

func (m *memStore) SetStatus(_ context.Context, id types.ID, to submissionbus.Status, ref string, at time.Time) error {
	if m.fail != nil {
		return m.fail
	}

	s, ok := m.subs[id]
	if !ok {
		return submissionbus.ErrNotFound
	}

	s.Status = to
	s.PaymentRef = ref
	s.UpdatedAt = at
	m.subs[id] = s

	return nil
}

func (m *memStore) PruneNonces(_ context.Context, before time.Time) error {
	if m.fail != nil {
		return m.fail
	}

	for nonce, at := range m.nonces {
		if at.Before(before) {
			delete(m.nonces, nonce)
		}
	}

	return nil
}

func newBusiness() (*submissionbus.Business, *memStore) {
	store := newMemStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	return submissionbus.NewBusiness(log, store), store
}

func mustSlug(t *testing.T, s string) types.Slug {
	t.Helper()

	slug, err := types.ParseSlug(s)
	if err != nil {
		t.Fatalf("ParseSlug(%q): %v", s, err)
	}

	return slug
}

// answers is what the validator would have returned: a form, a version, and
// whatever total the caller wants to test with.
func answers(t *testing.T, form types.Slug, total types.Money) formbus.Answers {
	t.Helper()

	return formbus.Answers{
		FormID:   form,
		Version:  "abc123def456",
		Currency: "usd",
		Fields: []formbus.Answer{
			{Name: "name", Label: "Your name", Kind: formbus.KindText, Values: []string{"Maria"}},
			{Name: "email", Label: "Email", Kind: formbus.KindEmail, Values: []string{"maria@example.org"}},
		},
		Total: total,
	}
}

func grantFor(a formbus.Answers, nonce string) formbus.Grant {
	return formbus.Grant{
		Form:      a.FormID,
		Version:   a.Version,
		Nonce:     nonce,
		ExpiresAt: now.Add(time.Hour),
	}
}

func TestSomethingWithATotalIsPendingAndSomethingWithoutIsComplete(t *testing.T) {
	cases := []struct {
		total types.Money
		want  submissionbus.Status
	}{
		{0, submissionbus.StatusReceived},
		{types.Money(1), submissionbus.StatusPending},
		{types.Money(1200), submissionbus.StatusPending},
	}

	for i, c := range cases {
		b, _ := newBusiness()
		a := answers(t, mustSlug(t, "feast-lunch-2026"), c.total)

		s, err := b.Accept(t.Context(), now, grantFor(a, "nonce"), submissionbus.New{Answers: a})
		if err != nil {
			t.Fatalf("case %d: Accept: %v", i, err)
		}

		if s.Status != c.want {
			t.Errorf("a total of %s gave status %q, want %q", c.total, s.Status, c.want)
		}

		// Paid reports whether there is money to collect, which is about the
		// total rather than about the status.
		if got := s.Paid(); got != (c.total > 0) {
			t.Errorf("a total of %s: Paid() = %v", c.total, got)
		}
	}
}

func TestTheSubmitterAddressIsLiftedOutOfTheAnswers(t *testing.T) {
	b, _ := newBusiness()
	a := answers(t, mustSlug(t, "feast-lunch-2026"), 0)

	s, err := b.Accept(t.Context(), now, grantFor(a, "nonce"), submissionbus.New{Answers: a})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	if s.Email.String() != "maria@example.org" {
		t.Errorf("Email = %q", s.Email)
	}

	// A form that asks for no address stores none, and that is not a failure.
	bare := answers(t, mustSlug(t, "guestbook"), 0)
	bare.Fields = slices.Delete(slices.Clone(bare.Fields), 1, 2)

	s, err = b.Accept(t.Context(), now, grantFor(bare, "another"), submissionbus.New{Answers: bare})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !s.Email.Zero() {
		t.Errorf("Email = %q, want the zero address", s.Email)
	}
}

func TestEachSubmissionGetsItsOwnIdentity(t *testing.T) {
	b, _ := newBusiness()
	a := answers(t, mustSlug(t, "feast-lunch-2026"), 0)

	seen := map[types.ID]bool{}
	for i := range 50 {
		s, err := b.Accept(t.Context(), now, grantFor(a, string(rune('a'+i))), submissionbus.New{Answers: a})
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}

		if seen[s.ID] {
			t.Fatalf("submission id %q was issued twice", s.ID)
		}

		seen[s.ID] = true
	}
}

func TestAReplayedGrantIsErrReplayed(t *testing.T) {
	b, store := newBusiness()
	a := answers(t, mustSlug(t, "feast-lunch-2026"), types.Money(1200))
	g := grantFor(a, "one-grant")

	if _, err := b.Accept(t.Context(), now, g, submissionbus.New{Answers: a}); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	_, err := b.Accept(t.Context(), now, g, submissionbus.New{Answers: a})
	if !errors.Is(err, submissionbus.ErrReplayed) {
		t.Errorf("the second Accept = %v, want ErrReplayed", err)
	}

	if len(store.subs) != 1 {
		t.Errorf("%d submissions stored, want 1", len(store.subs))
	}
}

// The grant and the answers arrive from different places -- one from a hidden
// field, one from the validator -- and a mix-up between them is what the
// grant's encoding exists to prevent. Cheap to check here as well.
func TestTheGrantAndTheAnswersMustAgree(t *testing.T) {
	b, _ := newBusiness()
	a := answers(t, mustSlug(t, "feast-lunch-2026"), 0)

	wrongForm := grantFor(a, "nonce")
	wrongForm.Form = mustSlug(t, "fall-retreat")

	if _, err := b.Accept(t.Context(), now, wrongForm, submissionbus.New{Answers: a}); err == nil {
		t.Error("accepted answers for one form on a grant for another")
	}

	wrongVersion := grantFor(a, "nonce")
	wrongVersion.Version = "999999999999"

	if _, err := b.Accept(t.Context(), now, wrongVersion, submissionbus.New{Answers: a}); err == nil {
		t.Error("accepted answers checked against one version on a grant pinning another")
	}
}

func TestAcceptRefusesWhatCannotBeASubmission(t *testing.T) {
	b, _ := newBusiness()
	a := answers(t, mustSlug(t, "feast-lunch-2026"), 0)

	if _, err := b.Accept(t.Context(), now, formbus.Grant{}, submissionbus.New{Answers: a}); err == nil {
		t.Error("accepted a submission with no grant")
	}

	noForm := a
	noForm.FormID = types.Slug{}

	if _, err := b.Accept(t.Context(), now, grantFor(a, "n"), submissionbus.New{Answers: noForm}); err == nil {
		t.Error("accepted a submission with no form")
	}

	noVersion := a
	noVersion.Version = ""

	if _, err := b.Accept(t.Context(), now, grantFor(a, "n"), submissionbus.New{Answers: noVersion}); err == nil {
		t.Error("accepted a submission with no version")
	}
}

func TestStatusParsingAndSettling(t *testing.T) {
	for _, s := range []submissionbus.Status{
		submissionbus.StatusReceived, submissionbus.StatusPending,
		submissionbus.StatusPaid, submissionbus.StatusFailed,
	} {
		got, err := submissionbus.ParseStatus(s.String())
		if err != nil || got != s {
			t.Errorf("ParseStatus(%q) = %q, %v", s, got, err)
		}
	}

	for _, s := range []string{"", "Paid", "refunded", "pending "} {
		if _, err := submissionbus.ParseStatus(s); !errors.Is(err, submissionbus.ErrNotAStatus) {
			t.Errorf("ParseStatus(%q) = %v, want ErrNotAStatus", s, err)
		}
	}

	// Settled is what stops anything collecting money twice.
	if !submissionbus.StatusPaid.Settled() || !submissionbus.StatusReceived.Settled() {
		t.Error("paid and received should be settled")
	}
	if submissionbus.StatusPending.Settled() || submissionbus.StatusFailed.Settled() {
		t.Error("pending and failed should not be settled")
	}
}

func TestSettleMovesForwardsOnly(t *testing.T) {
	b, _ := newBusiness()
	a := answers(t, mustSlug(t, "feast-lunch-2026"), types.Money(1200))

	s, err := b.Accept(t.Context(), now, grantFor(a, "nonce"), submissionbus.New{Answers: a})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	later := now.Add(time.Minute)

	if err := b.Settle(t.Context(), later, s.ID, submissionbus.StatusPaid, "cs_test_1"); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	got, err := b.ByID(t.Context(), s.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if got.Status != submissionbus.StatusPaid || got.PaymentRef != "cs_test_1" {
		t.Fatalf("after settling: %q %q", got.Status, got.PaymentRef)
	}

	// Stripe retries until acknowledged, so the same event arriving again is
	// the ordinary case and not an anomaly.
	if err := b.Settle(t.Context(), later, s.ID, submissionbus.StatusPaid, "cs_test_1"); err != nil {
		t.Errorf("settling twice: %v", err)
	}

	// Webhook delivery order is not guaranteed, so a failure arriving after a
	// success must not undo it.
	if err := b.Settle(t.Context(), later, s.ID, submissionbus.StatusFailed, "cs_test_1"); err != nil {
		t.Errorf("a late failure: %v", err)
	}

	got, err = b.ByID(t.Context(), s.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if got.Status != submissionbus.StatusPaid {
		t.Errorf("Status = %q, want it to have stayed paid", got.Status)
	}
}

func TestSettleRefusesAStatusItDoesNotKnow(t *testing.T) {
	b, _ := newBusiness()

	err := b.Settle(t.Context(), now, types.NewID(), submissionbus.Status("refunded"), "")
	if !errors.Is(err, submissionbus.ErrNotAStatus) {
		t.Errorf("Settle with an unknown status = %v, want ErrNotAStatus", err)
	}
}

func TestPruningKeepsNoncesForLongerThanAGrantCanLive(t *testing.T) {
	b, store := newBusiness()
	a := answers(t, mustSlug(t, "feast-lunch-2026"), 0)

	// Spent now, pruned as of now: a grant minted a moment ago could still be
	// in a browser, so its nonce has to be remembered.
	if _, err := b.Accept(t.Context(), now, grantFor(a, "fresh"), submissionbus.New{Answers: a}); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	if err := b.PruneNonces(t.Context(), now); err != nil {
		t.Fatalf("PruneNonces: %v", err)
	}

	if _, spent := store.nonces["fresh"]; !spent {
		t.Error("a nonce spent moments ago was forgotten")
	}

	// A day and a half later it cannot be, since a grant lives two hours.
	if err := b.PruneNonces(t.Context(), now.Add(36*time.Hour)); err != nil {
		t.Fatalf("PruneNonces: %v", err)
	}

	if _, spent := store.nonces["fresh"]; spent {
		t.Error("a nonce older than any possible grant was kept")
	}
}

func TestAnUnreadableStoreIsAnError(t *testing.T) {
	b, store := newBusiness()
	store.fail = errors.New("the disk is on fire")

	a := answers(t, mustSlug(t, "feast-lunch-2026"), 0)

	_, err := b.Accept(t.Context(), now, grantFor(a, "nonce"), submissionbus.New{Answers: a})
	if err == nil {
		t.Fatal("Accept returned no error when the store failed")
	}

	// And it is not mistaken for a replay, which the app layer answers by
	// re-rendering as though the person had done something ordinary.
	if errors.Is(err, submissionbus.ErrReplayed) {
		t.Error("a store failure was reported as a replayed grant")
	}
}

// --- the daily cap ------------------------------------------------------------

// The cap is an abuse control, so what matters is that it holds and that it
// says so in a way the app layer can answer with a sentence rather than an
// error page.
func TestAFormStopsTakingSubmissionsAtItsDailyCap(t *testing.T) {
	b, _ := newBusiness()
	form := mustSlug(t, "feast-lunch-2026")

	for i := range 2 {
		a := answers(t, form, 0)

		if _, err := b.Accept(t.Context(), now, grantFor(a, "nonce-"+string(rune('a'+i))), submissionbus.New{
			Answers:  a,
			DailyCap: 2,
		}); err != nil {
			t.Fatalf("submission %d under the cap: %v", i+1, err)
		}
	}

	a := answers(t, form, 0)

	_, err := b.Accept(t.Context(), now, grantFor(a, "nonce-c"), submissionbus.New{Answers: a, DailyCap: 2})

	if !errors.Is(err, submissionbus.ErrDailyCap) {
		t.Fatalf("the third submission against a cap of 2: %v, want ErrDailyCap", err)
	}
}

// Yesterday's submissions are not today's. The window is a trailing day rather
// than a calendar one, so that the rule needs no opinion about which midnight.
func TestTheCapOnlyCountsTheLastDay(t *testing.T) {
	b, store := newBusiness()
	form := mustSlug(t, "feast-lunch-2026")

	old := answers(t, form, 0)
	if _, err := b.Accept(t.Context(), now.Add(-48*time.Hour), grantFor(old, "old"), submissionbus.New{Answers: old}); err != nil {
		t.Fatalf("the submission from two days ago: %v", err)
	}

	fresh := answers(t, form, 0)
	if _, err := b.Accept(t.Context(), now, grantFor(fresh, "fresh"), submissionbus.New{Answers: fresh, DailyCap: 1}); err != nil {
		t.Fatalf("a submission today, with two days between it and the last one: %v", err)
	}

	if got := len(store.subs); got != 2 {
		t.Errorf("%d submissions stored, want both", got)
	}
}

// A form that sets no cap is the ordinary case, and it must not pay for a
// count it will not read. Every submission on the shrine's form goes through
// this path.
func TestNoCapMeansNoCounting(t *testing.T) {
	b, store := newBusiness()
	a := answers(t, mustSlug(t, "feast-lunch-2026"), 0)

	if _, err := b.Accept(t.Context(), now, grantFor(a, "nonce"), submissionbus.New{Answers: a}); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	if store.counted != 0 {
		t.Errorf("the day's submissions were counted %d times for a form with no cap", store.counted)
	}
}

// The will-call table's three operations, on the same fake. Collect is
// insert-if-absent, which is the property the real store gets from a primary
// key and this one has to spell out.

func (m *memStore) Collect(_ context.Context, c submissionbus.Collection) (submissionbus.Collection, bool, error) {
	if m.fail != nil {
		return submissionbus.Collection{}, false, m.fail
	}

	if won, already := m.handed[c.SubmissionID]; already {
		return won, false, nil
	}

	m.handed[c.SubmissionID] = c

	return c, true, nil
}

func (m *memStore) Uncollect(_ context.Context, id types.ID) error {
	if m.fail != nil {
		return m.fail
	}

	delete(m.handed, id)

	return nil
}

func (m *memStore) CollectionsForForm(_ context.Context, form types.Slug) (map[types.ID]submissionbus.Collection, error) {
	if m.fail != nil {
		return nil, m.fail
	}

	out := map[types.ID]submissionbus.Collection{}

	for id, c := range m.handed {
		if c.Form == form {
			out[id] = c
		}
	}

	return out, nil
}

// The will-call table's rules. Three of them, and the first is the one the
// page exists to enforce.

// An order that still owes money gets no tokens. The volunteer cannot be
// expected to notice while a queue waits, so the refusal is the domain's.
func TestAnUnpaidOrderCannotBeCollected(t *testing.T) {
	b, store := newBusiness()

	pending := seed(t, store, submissionbus.StatusPending)

	_, _, err := b.Collect(t.Context(), now, pending, types.NewID())
	if !errors.Is(err, submissionbus.ErrNotCollectable) {
		t.Errorf("collecting a pending order = %v, want ErrNotCollectable", err)
	}

	handed, err := b.Collected(t.Context(), mustSlug(t, "supper-2026"))
	if err != nil {
		t.Fatalf("Collected: %v", err)
	}

	if len(handed) != 0 {
		t.Errorf("a refused collection was recorded anyway: %v", handed)
	}
}

// Paid, and received on a form that charges nothing, are both collectable.
// Settled is the existing word for "final" and is reused rather than restated.
func TestASettledOrderCanBeCollected(t *testing.T) {
	for _, status := range []submissionbus.Status{submissionbus.StatusPaid, submissionbus.StatusReceived} {
		t.Run(status.String(), func(t *testing.T) {
			b, store := newBusiness()

			id := seed(t, store, status)
			by := types.NewID()

			c, wrote, err := b.Collect(t.Context(), now, id, by)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}

			if !wrote {
				t.Error("the first collection reported that somebody else got there first")
			}

			if c.CollectedBy != by || !c.CollectedAt.Equal(now.UTC()) {
				t.Errorf("collection = %+v, want it recorded against %s at %v", c, by, now.UTC())
			}
		})
	}
}

// Two volunteers tapping the same order is the ordinary case on a table worked
// by two people. The second is told who got there first rather than shown a
// failure, and nothing is overwritten.
func TestCollectingTwiceReportsWhoGotThereFirst(t *testing.T) {
	b, store := newBusiness()

	id := seed(t, store, submissionbus.StatusPaid)

	first := types.NewID()
	second := types.NewID()

	if _, wrote, err := b.Collect(t.Context(), now, id, first); err != nil || !wrote {
		t.Fatalf("the first collection: wrote %v, err %v", wrote, err)
	}

	later := now.Add(3 * time.Minute)

	c, wrote, err := b.Collect(t.Context(), later, id, second)
	if err != nil {
		t.Fatalf("the second collection: %v", err)
	}

	if wrote {
		t.Error("the second collection overwrote the first")
	}

	if c.CollectedBy != first {
		t.Errorf("collected by %s, want the one who got there first, %s", c.CollectedBy, first)
	}

	if !c.CollectedAt.Equal(now.UTC()) {
		t.Errorf("collected at %v, want the first time, %v", c.CollectedAt, now.UTC())
	}
}

// Undoing is not restricted to whoever marked it: the two people working the
// table are working one queue, and a correction only its author could make is
// one that waits for them to come back.
func TestAnybodyAtTheTableCanUndo(t *testing.T) {
	b, store := newBusiness()

	id := seed(t, store, submissionbus.StatusPaid)

	if _, _, err := b.Collect(t.Context(), now, id, types.NewID()); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if err := b.Uncollect(t.Context(), id); err != nil {
		t.Fatalf("Uncollect: %v", err)
	}

	handed, err := b.Collected(t.Context(), mustSlug(t, "supper-2026"))
	if err != nil {
		t.Fatalf("Collected: %v", err)
	}

	if len(handed) != 0 {
		t.Errorf("the collection is still there: %v", handed)
	}

	// And it can be handed over again afterwards, which is the point of
	// undoing rather than of a separate "wrongly collected" state.
	if _, wrote, err := b.Collect(t.Context(), now, id, types.NewID()); err != nil || !wrote {
		t.Errorf("collecting again after an undo: wrote %v, err %v", wrote, err)
	}
}

// seed puts one submission in the store at a given status and returns its id.
func seed(t *testing.T, store *memStore, status submissionbus.Status) types.ID {
	t.Helper()

	id := types.NewID()

	store.subs[id] = submissionbus.Submission{
		ID:        id,
		Form:      mustSlug(t, "supper-2026"),
		Status:    status,
		CreatedAt: now,
		UpdatedAt: now,
	}

	return id
}

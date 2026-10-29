package submissiondb_test

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/stores/submissiondb"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

var now = time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

func open(t *testing.T) (*sql.DB, *submissiondb.Store) {
	t.Helper()

	db, err := sqldb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	if err := sqldb.Init(t.Context(), db); err != nil {
		t.Fatalf("sqldb.Init: %v", err)
	}
	if err := submissiondb.Init(t.Context(), db); err != nil {
		t.Fatalf("submissiondb.Init: %v", err)
	}

	return db, submissiondb.NewStore(db)
}

func mustSlug(t *testing.T, s string) types.Slug {
	t.Helper()

	slug, err := types.ParseSlug(s)
	if err != nil {
		t.Fatalf("ParseSlug(%q): %v", s, err)
	}

	return slug
}

func mustEmail(t *testing.T, s string) types.Email {
	t.Helper()

	e, err := types.ParseEmail(s)
	if err != nil {
		t.Fatalf("ParseEmail(%q): %v", s, err)
	}

	return e
}

// sample is a submission with something of every shape in it: several fields,
// a multi-valued one, a priced line, an address and a total.
func sample(t *testing.T) submissionbus.Submission {
	t.Helper()

	form := mustSlug(t, "feast-lunch-2026")

	return submissionbus.Submission{
		ID:      types.NewID(),
		Form:    form,
		Version: "abc123def456",
		Status:  submissionbus.StatusPending,
		Answers: formbus.Answers{
			FormID:   form,
			Version:  "abc123def456",
			Currency: "usd",
			Fields: []formbus.Answer{
				{Name: "name", Label: "Your name", Kind: formbus.KindText, Values: []string{"Maria O'Neill"}},
				{Name: "email", Label: "Email", Kind: formbus.KindEmail, Values: []string{"maria@example.org"}},
				{Name: "dietary", Label: "Dietary needs", Kind: formbus.KindChoices, Values: []string{"vegetarian", "no-nuts"}},
			},
			Lines: []formbus.Line{
				{ItemID: "ticket", Label: "Lunch ticket", Price: types.Money(1200), Qty: 3, Amount: types.Money(3600)},
			},
			Total: types.Money(3600),
		},
		Email:     mustEmail(t, "maria@example.org"),
		RemoteIP:  "203.0.113.7",
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func TestSchemaMatchesWhatTheStoreReads(t *testing.T) {
	db, _ := open(t)

	if err := sqldb.CheckSchema(t.Context(), db, submissiondb.Expected); err != nil {
		t.Errorf("CheckSchema: %v", err)
	}
}

func TestASubmissionRoundTrips(t *testing.T) {
	_, store := open(t)
	want := sample(t)

	spent, err := store.Accept(t.Context(), want, "nonce-one", nil)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !spent {
		t.Fatal("Accept reported the grant as already spent")
	}

	got, err := store.ByID(t.Context(), want.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if got.ID != want.ID || got.Form != want.Form || got.Version != want.Version {
		t.Errorf("identity read back as %q %q %q", got.ID, got.Form, got.Version)
	}
	if got.Status != submissionbus.StatusPending {
		t.Errorf("Status = %q", got.Status)
	}
	if got.Email != want.Email {
		t.Errorf("Email = %q, want %q", got.Email, want.Email)
	}
	if got.RemoteIP != want.RemoteIP {
		t.Errorf("RemoteIP = %q", got.RemoteIP)
	}
	if !got.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, now)
	}

	// The answers: the form name, version, currency and total come from their
	// own columns, and everything else out of the document.
	a := got.Answers

	if a.FormID != want.Form || a.Version != want.Version || a.Currency != "usd" {
		t.Errorf("answers header = %+v", a)
	}
	if a.Total != types.Money(3600) {
		t.Errorf("Total = %s, want 36.00", a.Total)
	}
	if len(a.Fields) != 3 {
		t.Fatalf("%d fields, want 3", len(a.Fields))
	}

	// Order is the definition's, and it survives the round trip -- a CSV
	// export's column order depends on it.
	for i, name := range []string{"name", "email", "dietary"} {
		if a.Fields[i].Name != name {
			t.Errorf("field %d is %q, want %q", i, a.Fields[i].Name, name)
		}
	}

	// A multi-valued answer keeps all of its values, and its kind.
	dietary, ok := a.Field("dietary")
	if !ok {
		t.Fatal("the choices field is missing")
	}
	if len(dietary.Values) != 2 || dietary.Values[0] != "vegetarian" || dietary.Values[1] != "no-nuts" {
		t.Errorf("Values = %q", dietary.Values)
	}
	if dietary.Kind != formbus.KindChoices {
		t.Errorf("Kind = %q", dietary.Kind)
	}

	// An apostrophe in a name survives, which is the thing a hand-built SQL
	// string would have broken.
	if name, _ := a.Field("name"); name.Value() != "Maria O'Neill" {
		t.Errorf("name = %q", name.Value())
	}

	if len(a.Lines) != 1 {
		t.Fatalf("%d lines, want 1", len(a.Lines))
	}

	l := a.Lines[0]
	if l.ItemID != "ticket" || l.Qty != 3 || l.Price != types.Money(1200) || l.Amount != types.Money(3600) {
		t.Errorf("line = %+v", l)
	}
}

func TestASubmissionWithNothingInItRoundTrips(t *testing.T) {
	_, store := open(t)

	form := mustSlug(t, "guestbook")

	// No address, no lines, no total: a form that asks one optional question
	// and sells nothing.
	bare := submissionbus.Submission{
		ID:      types.NewID(),
		Form:    form,
		Version: "000000000000",
		Status:  submissionbus.StatusReceived,
		Answers: formbus.Answers{
			FormID: form, Version: "000000000000", Currency: "usd",
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if _, err := store.Accept(t.Context(), bare, "nonce-bare", nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	got, err := store.ByID(t.Context(), bare.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if !got.Email.Zero() {
		t.Errorf("Email = %q, want the zero address", got.Email)
	}
	if got.PaymentRef != "" {
		t.Errorf("PaymentRef = %q, want empty", got.PaymentRef)
	}
	if len(got.Answers.Fields) != 0 || len(got.Answers.Lines) != 0 {
		t.Errorf("answers = %+v, want empty", got.Answers)
	}
	if got.Answers.Total != 0 {
		t.Errorf("Total = %s, want zero", got.Answers.Total)
	}
}

// The single-use property. The second attempt with the same nonce writes
// nothing at all -- not the nonce, and not the submission behind it.
func TestAReplayedGrantIsRefusedAndWritesNothing(t *testing.T) {
	_, store := open(t)

	first := sample(t)
	if spent, err := store.Accept(t.Context(), first, "one-grant", nil); err != nil || !spent {
		t.Fatalf("the first Accept: spent = %v, err = %v", spent, err)
	}

	second := sample(t)

	spent, err := store.Accept(t.Context(), second, "one-grant", nil)
	if err != nil {
		t.Fatalf("the second Accept: %v", err)
	}
	if spent {
		t.Error("the same grant was spent twice")
	}

	if _, err := store.ByID(t.Context(), second.ID); !errors.Is(err, submissionbus.ErrNotFound) {
		t.Errorf("the replayed submission was written anyway: %v", err)
	}

	all, err := store.ByForm(t.Context(), first.Form)
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("%d submissions, want 1", len(all))
	}
}

// Two POSTs carrying one grant, at once. This is the case a SELECT-then-INSERT
// would let through, and the reason the nonce insert is the claim.
func TestConcurrentReplaysProduceExactlyOneSubmission(t *testing.T) {
	_, store := open(t)

	const tries = 8

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		accepts int
		fails   []error
	)

	for range tries {
		sub := sample(t)

		wg.Go(func() {
			spent, err := store.Accept(t.Context(), sub, "the-one-nonce", nil)

			mu.Lock()
			defer mu.Unlock()

			switch {
			case err != nil:
				fails = append(fails, err)
			case spent:
				accepts++
			}
		})
	}

	wg.Wait()

	for _, err := range fails {
		t.Errorf("Accept: %v", err)
	}

	if accepts != 1 {
		t.Errorf("%d of %d concurrent attempts were accepted, want exactly 1", accepts, tries)
	}

	all, err := store.ByForm(t.Context(), mustSlug(t, "feast-lunch-2026"))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("%d submissions stored, want 1", len(all))
	}
}

func TestByFormIsNewestFirstAndScoped(t *testing.T) {
	_, store := open(t)

	lunch := mustSlug(t, "feast-lunch-2026")
	retreat := mustSlug(t, "fall-retreat")

	for i, at := range []time.Time{now, now.Add(time.Hour), now.Add(-time.Hour)} {
		sub := sample(t)
		sub.CreatedAt = at
		sub.UpdatedAt = at

		if _, err := store.Accept(t.Context(), sub, string(rune('a'+i)), nil); err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}

	other := sample(t)
	other.Form = retreat
	other.Answers.FormID = retreat

	if _, err := store.Accept(t.Context(), other, "other", nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	got, err := store.ByForm(t.Context(), lunch)
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("%d submissions for the lunch form, want 3", len(got))
	}

	for i := 1; i < len(got); i++ {
		if got[i].CreatedAt.After(got[i-1].CreatedAt) {
			t.Errorf("submission %d is newer than the one before it", i)
		}
	}

	if only, err := store.ByForm(t.Context(), retreat); err != nil || len(only) != 1 {
		t.Errorf("the other form has %d submissions, err = %v", len(only), err)
	}

	if none, err := store.ByForm(t.Context(), mustSlug(t, "nothing-here")); err != nil || len(none) != 0 {
		t.Errorf("an unknown form has %d submissions, err = %v", len(none), err)
	}
}

// Submissions made in the same millisecond come back in the order they
// arrived, newest first, every time. Ten of them, because with the random
// identifier this used to break ties by, ten in a row in the right order by
// chance is one in 3.6 million.
func TestATieIsBrokenByArrival(t *testing.T) {
	_, store := open(t)

	var arrived []types.ID

	for i := range 10 {
		sub := sample(t)

		if _, err := store.Accept(t.Context(), sub, fmt.Sprintf("nonce-%d", i), nil); err != nil {
			t.Fatalf("Accept: %v", err)
		}

		arrived = append(arrived, sub.ID)
	}

	got, err := store.ByForm(t.Context(), mustSlug(t, "feast-lunch-2026"))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}

	slices.Reverse(arrived)

	for i, s := range got {
		if s.ID != arrived[i] {
			t.Fatalf("position %d is %s, want %s: a tie is not in the order of arrival", i, s.ID, arrived[i])
		}
	}
}

// Settled is what the public list beneath a form reads, so what it must not
// do is show an order nobody paid for, or read past the number asked for.
func TestSettledIsNewestFirstBoundedAndSkipsTheUnpaid(t *testing.T) {
	_, store := open(t)

	lunch := mustSlug(t, "feast-lunch-2026")

	statuses := []submissionbus.Status{
		submissionbus.StatusReceived, // oldest
		submissionbus.StatusPending,
		submissionbus.StatusPaid,
		submissionbus.StatusFailed,
		submissionbus.StatusReceived, // newest
	}

	for i, st := range statuses {
		sub := sample(t)
		sub.Status = st
		sub.CreatedAt = now.Add(time.Duration(i) * time.Minute)
		sub.UpdatedAt = sub.CreatedAt

		if _, err := store.Accept(t.Context(), sub, string(rune('a'+i)), nil); err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}

	got, err := store.Settled(t.Context(), lunch, 10)
	if err != nil {
		t.Fatalf("Settled: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("%d settled submissions, want 3", len(got))
	}
	for i, s := range got {
		if !s.Status.Settled() {
			t.Errorf("submission %d is %s, which is not settled", i, s.Status)
		}
		if i > 0 && s.CreatedAt.After(got[i-1].CreatedAt) {
			t.Errorf("submission %d is newer than the one before it", i)
		}
	}

	two, err := store.Settled(t.Context(), lunch, 2)
	if err != nil {
		t.Fatalf("Settled: %v", err)
	}
	if len(two) != 2 || !two[0].CreatedAt.Equal(now.Add(4*time.Minute)) || two[1].Status != submissionbus.StatusPaid {
		t.Errorf("Settled with a limit of 2 = %d rows, want the newest received and then the paid one", len(two))
	}
}

func TestSetStatusRecordsThePayment(t *testing.T) {
	_, store := open(t)
	sub := sample(t)

	if _, err := store.Accept(t.Context(), sub, "nonce", nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	later := now.Add(time.Minute)

	err := store.SetStatus(t.Context(), sub.ID, submissionbus.StatusPaid, "cs_test_123", later)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	got, err := store.ByID(t.Context(), sub.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if got.Status != submissionbus.StatusPaid {
		t.Errorf("Status = %q", got.Status)
	}
	if got.PaymentRef != "cs_test_123" {
		t.Errorf("PaymentRef = %q", got.PaymentRef)
	}
	if !got.UpdatedAt.Equal(later) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, later)
	}

	// The answers are untouched by a status change.
	if got.Answers.Total != types.Money(3600) || len(got.Answers.Fields) != 3 {
		t.Errorf("the answers changed: %+v", got.Answers)
	}

	if err := store.SetStatus(t.Context(), types.NewID(), submissionbus.StatusPaid, "x", later); !errors.Is(err, submissionbus.ErrNotFound) {
		t.Errorf("SetStatus on a missing submission = %v, want ErrNotFound", err)
	}
}

func TestAPaymentIsTracedBackToItsSubmissionHiddenOrNot(t *testing.T) {
	_, store := open(t)

	paid, hidden, pending, other := sample(t), sample(t), sample(t), sample(t)
	other.Form = mustSlug(t, "retreat-2026")
	other.Answers.FormID = other.Form

	for i, sub := range []submissionbus.Submission{paid, hidden, pending, other} {
		if _, err := store.Accept(t.Context(), sub, fmt.Sprintf("nonce-%d", i), nil); err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}

	for _, s := range []struct {
		id  types.ID
		ref string
	}{{paid.ID, "pi_paid"}, {hidden.ID, "pi_hidden"}, {other.ID, "pi_other"}} {
		if err := store.SetStatus(t.Context(), s.id, submissionbus.StatusPaid, s.ref, now); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
	}

	if _, _, err := store.Hide(t.Context(), submissionbus.Hiding{SubmissionID: hidden.ID, Form: hidden.Form, HiddenAt: now, HiddenBy: types.NewID()}); err != nil {
		t.Fatalf("Hide: %v", err)
	}

	for ref, want := range map[string]types.ID{"pi_paid": paid.ID, "pi_hidden": hidden.ID, "pi_other": other.ID} {
		got, err := store.ByPaymentRef(t.Context(), ref)
		if err != nil {
			t.Fatalf("ByPaymentRef(%q): %v", ref, err)
		}
		if got.ID != want {
			t.Errorf("ByPaymentRef(%q) = %s, want %s", ref, got.ID, want)
		}
	}

	// The empty reference every unpaid row carries must never match one of
	// them, or every unattributed deposit line would land on an arbitrary
	// pending order.
	for _, ref := range []string{"", "pi_nobody"} {
		if _, err := store.ByPaymentRef(t.Context(), ref); !errors.Is(err, submissionbus.ErrNotFound) {
			t.Errorf("ByPaymentRef(%q) = %v, want ErrNotFound", ref, err)
		}
	}

	got, err := store.Paid(t.Context(), paid.Form)
	if err != nil {
		t.Fatalf("Paid: %v", err)
	}

	ids := []types.ID{}
	for _, s := range got {
		ids = append(ids, s.ID)
	}

	if len(ids) != 2 || !slices.Contains(ids, paid.ID) || !slices.Contains(ids, hidden.ID) {
		t.Errorf("Paid = %v, want the paid and the hidden paid order and nothing else", ids)
	}
}

func TestPruningForgetsOldNoncesAndNotRecentOnes(t *testing.T) {
	_, store := open(t)

	old := sample(t)
	old.CreatedAt = now.Add(-48 * time.Hour)
	old.UpdatedAt = old.CreatedAt

	if _, err := store.Accept(t.Context(), old, "old-nonce", nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	recent := sample(t)
	if _, err := store.Accept(t.Context(), recent, "recent-nonce", nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	if err := store.PruneNonces(t.Context(), now.Add(-24*time.Hour)); err != nil {
		t.Fatalf("PruneNonces: %v", err)
	}

	// The old nonce is forgotten, so it would be accepted again -- which is
	// safe only because no grant carrying it can still be valid.
	replay := sample(t)
	if spent, err := store.Accept(t.Context(), replay, "old-nonce", nil); err != nil || !spent {
		t.Errorf("a pruned nonce was not reusable: spent = %v, err = %v", spent, err)
	}

	// The recent one is still remembered.
	again := sample(t)
	if spent, err := store.Accept(t.Context(), again, "recent-nonce", nil); err != nil || spent {
		t.Errorf("a recent nonce was pruned: spent = %v, err = %v", spent, err)
	}

	// Pruning nonces does not touch submissions.
	all, err := store.ByForm(t.Context(), old.Form)
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("%d submissions after pruning, want 3", len(all))
	}
}

// A document written by a newer binary must not read back as a partial
// submission. The case is a release rolled back, and a half-read record of
// what somebody agreed to is worse than a refusal.
func TestADocumentThisBinaryCannotReadIsAnError(t *testing.T) {
	db, store := open(t)

	id := types.NewID()

	_, err := db.ExecContext(t.Context(), `
INSERT INTO submissions
    (id, form_slug, version, status, answers, email, total, currency, remote_ip, payment_ref, created_at, updated_at)
VALUES (?, 'feast-lunch-2026', 'v', 'received', ?, '', 0, 'usd', '', '', ?, ?)`,
		id.String(), `{"v":99,"fields":[]}`, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		t.Fatalf("inserting by hand: %v", err)
	}

	if _, err := store.ByID(t.Context(), id); err == nil {
		t.Error("a document from a newer format was read as valid")
	}
}

func TestAStatusThisBinaryCannotReadIsAnError(t *testing.T) {
	db, store := open(t)

	id := types.NewID()

	_, err := db.ExecContext(t.Context(), `
INSERT INTO submissions
    (id, form_slug, version, status, answers, email, total, currency, remote_ip, payment_ref, created_at, updated_at)
VALUES (?, 'feast-lunch-2026', 'v', 'refunded', ?, '', 0, 'usd', '', '', ?, ?)`,
		id.String(), `{"v":1}`, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		t.Fatalf("inserting by hand: %v", err)
	}

	_, err = store.ByID(t.Context(), id)
	if err == nil {
		t.Fatal("a status this binary does not know was read as valid")
	}
	if errors.Is(err, submissionbus.ErrNotFound) {
		t.Error("an unreadable row reported itself as no row at all")
	}
}

// CountSince is what a form's daily cap is measured against, so it has to
// count one form's recent rows and nothing else's.
func TestCountSinceCountsOneFormsRecentRows(t *testing.T) {
	_, store := open(t)

	write := func(form types.Slug, at time.Time) {
		t.Helper()

		s := sample(t)
		s.ID = types.NewID()
		s.Form = form
		s.Answers.FormID = form
		s.CreatedAt = at
		s.UpdatedAt = at

		ok, err := store.Accept(t.Context(), s, s.ID.String(), nil)
		if err != nil || !ok {
			t.Fatalf("Accept: ok=%v err=%v", ok, err)
		}
	}

	feast := mustSlug(t, "feast-lunch-2026")
	other := mustSlug(t, "another-form")

	write(feast, now)
	write(feast, now.Add(-time.Hour))
	write(feast, now.Add(-48*time.Hour)) // too old
	write(other, now)                    // another form

	got, err := store.CountSince(t.Context(), feast, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("CountSince: %v", err)
	}

	if got != 2 {
		t.Errorf("CountSince = %d, want 2: the two from the last day on this form", got)
	}

	// A form nobody has submitted is zero rather than an error, because that
	// is the answer on the morning a form goes live.
	none, err := store.CountSince(t.Context(), mustSlug(t, "no-such-form"), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("CountSince on an unused form: %v", err)
	}
	if none != 0 {
		t.Errorf("CountSince on an unused form = %d, want 0", none)
	}
}

func TestAbandonClosesOnlyOpenOrdersPastTheDeadline(t *testing.T) {
	_, store := open(t)

	write := func(status submissionbus.Status, at time.Time) types.ID {
		t.Helper()

		s := sample(t)
		s.ID = types.NewID()
		s.Status = status
		s.CreatedAt = at
		s.UpdatedAt = at

		ok, err := store.Accept(t.Context(), s, s.ID.String(), nil)
		if err != nil || !ok {
			t.Fatalf("Accept: ok=%v err=%v", ok, err)
		}

		return s.ID
	}

	// Past the deadline, both the ways an order can still be open: a closed
	// tab and a declined card. The second is hidden, and is closed anyway --
	// hiding an order says nothing about whether it was paid.
	closed := write(submissionbus.StatusPending, now.Add(-3*time.Hour))
	declined := write(submissionbus.StatusFailed, now.Add(-3*time.Hour))

	if _, _, err := store.Hide(t.Context(), submissionbus.Hiding{
		SubmissionID: declined, Form: sample(t).Form, HiddenAt: now, HiddenBy: types.NewID(),
	}); err != nil {
		t.Fatalf("Hide: %v", err)
	}

	// Inside it: somebody who may still come back to the tab.
	open := write(submissionbus.StatusPending, now.Add(-10*time.Minute))

	// Settled, in both the ways a submission can be. A payment confirmed
	// while the sweep ran is this case, and must not be overwritten.
	paid := write(submissionbus.StatusPaid, now.Add(-3*time.Hour))
	received := write(submissionbus.StatusReceived, now.Add(-3*time.Hour))

	forms, err := store.Abandon(t.Context(), now.Add(-2*time.Hour), now)
	if err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	if len(forms) != 2 || forms[0] != sample(t).Form || forms[1] != sample(t).Form {
		t.Errorf("Abandon reported %v, want the form twice", forms)
	}

	want := map[types.ID]submissionbus.Status{
		closed:   submissionbus.StatusAbandoned,
		declined: submissionbus.StatusAbandoned,
		open:     submissionbus.StatusPending,
		paid:     submissionbus.StatusPaid,
		received: submissionbus.StatusReceived,
	}

	for id, status := range want {
		got, err := store.ByID(t.Context(), id)
		if err != nil {
			t.Fatalf("ByID: %v", err)
		}

		if got.Status != status {
			t.Errorf("a %s order is now %s", status, got.Status)
		}
	}

	// Twice is nothing: the statement only touches what is still open.
	again, err := store.Abandon(t.Context(), now.Add(-2*time.Hour), now)
	if err != nil {
		t.Fatalf("the second Abandon: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("the second Abandon changed %d rows", len(again))
	}

	paidCount, abandoned, err := store.Outcomes(t.Context(), sample(t).Form, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("Outcomes: %v", err)
	}
	if paidCount != 1 || abandoned != 2 {
		t.Errorf("Outcomes = %d paid, %d abandoned; want 1 and 2", paidCount, abandoned)
	}
}

// The collections table. What is worth asserting here rather than in the
// business layer is the once-only property, because it is a primary key doing
// the work rather than a check anybody performs.

// Two volunteers tapping the same order from two phones. One row is written
// and the one that lost is told who won -- the same shape as a replayed grant.
func TestOnlyOneCollectionIsEverRecorded(t *testing.T) {
	_, store := open(t)

	sub := sample(t)
	if _, err := store.Accept(t.Context(), sub, "a-nonce", nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	first := types.NewID()
	second := types.NewID()

	won, wrote, err := store.Collect(t.Context(), submissionbus.Collection{
		SubmissionID: sub.ID, Form: sub.Form, CollectedAt: now, CollectedBy: first,
	})
	if err != nil || !wrote {
		t.Fatalf("the first collection: wrote %v, err %v", wrote, err)
	}

	if won.CollectedBy != first {
		t.Errorf("the first collection was recorded against %s, want %s", won.CollectedBy, first)
	}

	later := now.Add(2 * time.Minute)

	won, wrote, err = store.Collect(t.Context(), submissionbus.Collection{
		SubmissionID: sub.ID, Form: sub.Form, CollectedAt: later, CollectedBy: second,
	})
	if err != nil {
		t.Fatalf("the second collection: %v", err)
	}

	if wrote {
		t.Error("the second collection wrote a row")
	}

	if won.CollectedBy != first || !won.CollectedAt.Equal(now.UTC()) {
		t.Errorf("the losing caller was told %+v, want the first collection back", won)
	}
}

// The same thing under real concurrency, the way the spent-grant test does it:
// one submission, many goroutines, exactly one winner.
func TestConcurrentCollectionsProduceOneWinner(t *testing.T) {
	_, store := open(t)

	sub := sample(t)
	if _, err := store.Accept(t.Context(), sub, "a-nonce", nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	const hands = 8

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		wins   int
		errors []error
	)

	for range hands {
		wg.Go(func() {
			_, wrote, err := store.Collect(t.Context(), submissionbus.Collection{
				SubmissionID: sub.ID, Form: sub.Form, CollectedAt: now, CollectedBy: types.NewID(),
			})

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				errors = append(errors, err)

				return
			}

			if wrote {
				wins++
			}
		})
	}

	wg.Wait()

	if len(errors) > 0 {
		t.Fatalf("%d of %d collections failed: %v", len(errors), hands, errors)
	}

	if wins != 1 {
		t.Errorf("%d of %d collections reported writing the row, want exactly 1", wins, hands)
	}
}

func TestCollectionsAreReadBackByForm(t *testing.T) {
	_, store := open(t)

	mine := sample(t)
	if _, err := store.Accept(t.Context(), mine, "nonce-one", nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	other := sample(t)
	other.Form = mustSlug(t, "supper-2026")
	other.Answers.FormID = other.Form

	if _, err := store.Accept(t.Context(), other, "nonce-two", nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	by := types.NewID()

	for _, sub := range []submissionbus.Submission{mine, other} {
		if _, _, err := store.Collect(t.Context(), submissionbus.Collection{
			SubmissionID: sub.ID, Form: sub.Form, CollectedAt: now, CollectedBy: by,
		}); err != nil {
			t.Fatalf("Collect: %v", err)
		}
	}

	handed, err := store.CollectionsForForm(t.Context(), mine.Form)
	if err != nil {
		t.Fatalf("CollectionsForForm: %v", err)
	}

	if len(handed) != 1 {
		t.Fatalf("read back %d collections for one form, want 1", len(handed))
	}

	c, ok := handed[mine.ID]
	if !ok {
		t.Fatalf("the collection is not keyed by its submission: %v", handed)
	}

	if c.CollectedBy != by || !c.CollectedAt.Equal(now.UTC()) {
		t.Errorf("collection = %+v, want it against %s at %v", c, by, now.UTC())
	}
}

func TestUncollectLetsAnOrderBeHandedOverAgain(t *testing.T) {
	_, store := open(t)

	sub := sample(t)
	if _, err := store.Accept(t.Context(), sub, "a-nonce", nil); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	c := submissionbus.Collection{
		SubmissionID: sub.ID, Form: sub.Form, CollectedAt: now, CollectedBy: types.NewID(),
	}

	if _, _, err := store.Collect(t.Context(), c); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if err := store.Uncollect(t.Context(), sub.ID); err != nil {
		t.Fatalf("Uncollect: %v", err)
	}

	if _, wrote, err := store.Collect(t.Context(), c); err != nil || !wrote {
		t.Errorf("collecting after an undo: wrote %v, err %v", wrote, err)
	}

	// Removing nothing is not an error, for the reason the method's comment
	// gives: the caller wanted the mark gone and it is gone.
	if err := store.Uncollect(t.Context(), types.NewID()); err != nil {
		t.Errorf("Uncollect on an order nobody collected = %v, want nil", err)
	}
}

// A hidden submission leaves every listing query -- ByForm and Settled --
// and stays readable by id, counted by CountSince, and listed by
// Hidden. Unhiding puts it back.
func TestAHiddenSubmissionLeavesEveryListAndComesBack(t *testing.T) {
	_, store := open(t)

	lunch := mustSlug(t, "feast-lunch-2026")

	keep := sample(t)
	keep.Status = submissionbus.StatusPaid

	test := sample(t)
	test.Status = submissionbus.StatusPaid
	test.CreatedAt = now.Add(time.Minute)

	unpaid := sample(t)
	unpaid.CreatedAt = now.Add(2 * time.Minute)

	for i, s := range []submissionbus.Submission{keep, test, unpaid} {
		if _, err := store.Accept(t.Context(), s, string(rune('a'+i)), nil); err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}

	by := types.NewID()

	for _, id := range []types.ID{test.ID, unpaid.ID} {
		h, wrote, err := store.Hide(t.Context(), submissionbus.Hiding{SubmissionID: id, Form: lunch, HiddenAt: now, HiddenBy: by})
		if err != nil || !wrote || h.HiddenBy != by {
			t.Fatalf("Hide = %+v, %v, %v", h, wrote, err)
		}
	}

	// Twice is once, and says who got there first.
	if h, wrote, err := store.Hide(t.Context(), submissionbus.Hiding{SubmissionID: test.ID, Form: lunch, HiddenAt: now, HiddenBy: types.NewID()}); err != nil || wrote || h.HiddenBy != by {
		t.Errorf("hiding twice = %+v, %v, %v; want the first hiding and no write", h, wrote, err)
	}

	listed, err := store.ByForm(t.Context(), lunch)
	if err != nil || len(listed) != 1 || listed[0].ID != keep.ID {
		t.Errorf("ByForm = %d rows, %v; want only the one not hidden", len(listed), err)
	}

	if settled, err := store.Settled(t.Context(), lunch, 10); err != nil || len(settled) != 1 {
		t.Errorf("Settled = %d rows, %v; want only the one not hidden", len(settled), err)
	}

	if n, err := store.CountSince(t.Context(), lunch, now.Add(-time.Hour)); err != nil || n != 3 {
		t.Errorf("CountSince = %d, %v; want all three -- the daily cap counts what was hidden", n, err)
	}

	if _, err := store.ByID(t.Context(), test.ID); err != nil {
		t.Errorf("a hidden submission cannot be read by id: %v", err)
	}

	if hidden, err := store.Hidden(t.Context(), lunch); err != nil || len(hidden) != 2 || hidden[0].ID != unpaid.ID {
		t.Errorf("Hidden = %d rows, %v; want both, newest first", len(hidden), err)
	}

	if h, ok, err := store.HidingOf(t.Context(), test.ID); err != nil || !ok || !h.HiddenAt.Equal(now) {
		t.Errorf("HidingOf = %+v, %v, %v", h, ok, err)
	}

	if err := store.Unhide(t.Context(), test.ID); err != nil {
		t.Fatalf("Unhide: %v", err)
	}

	if listed, _ := store.ByForm(t.Context(), lunch); len(listed) != 2 {
		t.Errorf("after unhiding, ByForm = %d rows, want 2", len(listed))
	}

	if _, ok, _ := store.HidingOf(t.Context(), test.ID); ok {
		t.Error("the hiding is still there after Unhide")
	}

	if err := store.Unhide(t.Context(), test.ID); err != nil {
		t.Errorf("unhiding what is not hidden = %v, want nothing", err)
	}
}

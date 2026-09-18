package paybus_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formtoml"
	"github.com/jroedel/dropin-forms/business/domain/payment/paybus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/forms"
)

// The rules about money, with no network and no database.
//
// Two of these are the reason this package exists at all: that the lines sent
// to Stripe add up to the total the validator derived, and that a webhook
// settles a submission *before* it records the event. Both are invisible in
// normal operation and expensive when wrong.

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- the fakes --------------------------------------------------------------

// gateway is a Stripe that does what the test says.
type gateway struct {
	handoff paybus.Handoff
	event   paybus.Event
	err     error

	// orders records what it was asked to charge, which is what the balance
	// assertions read.
	orders []paybus.Order
}

func (g *gateway) Checkout(_ context.Context, o paybus.Order) (paybus.Handoff, error) {
	g.orders = append(g.orders, o)

	if g.err != nil {
		return paybus.Handoff{}, g.err
	}

	// Keyed on Ref rather than URL, so that a test can deliberately hand back
	// a session with no address -- which is the one case the default below
	// would otherwise hide.
	if g.handoff.Ref == "" {
		return paybus.Handoff{Ref: "cs_test_1", URL: "https://checkout.stripe.com/c/pay/cs_test_1"}, nil
	}

	return g.handoff, nil
}

func (g *gateway) Verify(_ []byte, _ string) (paybus.Event, error) {
	if g.err != nil {
		return paybus.Event{}, g.err
	}

	return g.event, nil
}

// seen is the event ledger.
type seen struct {
	ids map[string]bool
	err error
}

func newSeen() *seen { return &seen{ids: map[string]bool{}} }

func (s *seen) Record(_ context.Context, id string, _ string, _ time.Time) (bool, error) {
	if s.err != nil {
		return false, s.err
	}

	if s.ids[id] {
		return false, nil
	}

	s.ids[id] = true

	return true, nil
}

func (s *seen) Prune(context.Context, time.Time) error { return nil }

// settler records what happened to a submission, and in what order relative to
// the ledger above -- which is the thing one of these tests is about.
type settler struct {
	status map[types.ID]submissionbus.Status
	refs   map[types.ID]string
	calls  int
	err    error
}

func newSettler() *settler {
	return &settler{status: map[types.ID]submissionbus.Status{}, refs: map[types.ID]string{}}
}

func (s *settler) Settle(_ context.Context, _ time.Time, id types.ID, to submissionbus.Status, ref string) error {
	s.calls++

	if s.err != nil {
		return s.err
	}

	// Forwards only, like the real one: a paid submission does not become
	// pending again because a webhook arrived out of order.
	if was := s.status[id]; was.Settled() {
		return nil
	}

	s.status[id] = to
	s.refs[id] = ref

	return nil
}

// --- the form and a submission to it ----------------------------------------

// theForm is the real shipped definition, so that what these tests price is
// what the shrine will charge.
func theForm(t *testing.T) formbus.Form {
	t.Helper()

	store, err := formtoml.Load(forms.FS)
	if err != nil {
		t.Fatalf("loading the definitions: %v", err)
	}

	slug, err := types.ParseSlug("feast-lunch-2026")
	if err != nil {
		t.Fatalf("ParseSlug: %v", err)
	}

	f, err := store.ByID(slug)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	return f
}

// beforeTheFeast keeps the form open whatever today is.
var beforeTheFeast = time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

// order validates real answers and wraps them in a stored submission, which is
// what the app layer hands to OrderFor.
func submissionOf(t *testing.T, f formbus.Form, in formbus.Values) submissionbus.Submission {
	t.Helper()

	ans, err := f.Validate(beforeTheFeast, in)
	if err != nil {
		t.Fatalf("Validate refused the fixture: %v", err)
	}

	email, _ := ans.SubmitterEmail()

	return submissionbus.Submission{
		ID:      types.NewID(),
		Form:    f.ID,
		Version: ans.Version,
		Status:  submissionbus.StatusPending,
		Answers: ans,
		Email:   email,
	}
}

// --- starting a payment -----------------------------------------------------

// The assertion this package was written for: the itemisation a person reads
// on Stripe's page adds up to the total the validator derived.
//
// A donation is the interesting case, because it is an amount *field* rather
// than a priced item -- the validator adds the two together, and a line list
// built from Answers.Lines alone would charge for the tickets and silently
// drop the gift.
func TestTheLinesSentToStripeAddUpToTheDerivedTotal(t *testing.T) {
	f := theForm(t)

	sub := submissionOf(t, f, formbus.Values{
		"name":                          {"Maria O'Neill"},
		"email":                         {"maria@example.org"},
		"donation":                      {"5.00"},
		formbus.QuantityField("ticket"): {"2"},
	})

	// Two tickets at twelve, plus a five dollar gift.
	if sub.Answers.Total != 2900 {
		t.Fatalf("the validator derived %s, want $29.00", sub.Answers.Total)
	}

	o := paybus.OrderFor(f, sub)

	var sum types.Money
	for _, l := range o.Lines {
		sum += l.Amount()
	}

	if sum != sub.Answers.Total {
		t.Errorf("the lines come to %s and the total is %s", sum, sub.Answers.Total)
	}

	if len(o.Lines) != 2 {
		t.Fatalf("got %d lines, want the tickets and the donation: %+v", len(o.Lines), o.Lines)
	}

	// Labelled from the definition, never from anything somebody typed: this
	// is what a person reads on the payment page.
	if o.Lines[0].Label != "Lunch ticket" || o.Lines[0].Qty != 2 || o.Lines[0].Unit != 1200 {
		t.Errorf("the ticket line is %+v", o.Lines[0])
	}
	if o.Lines[1].Label != "Donation for the Shrine" || o.Lines[1].Qty != 1 || o.Lines[1].Unit != 500 {
		t.Errorf("the donation line is %+v", o.Lines[1])
	}

	// The return addresses carry the marker and nothing else. No identifier
	// and no amount, because a browser can edit them and nothing should be
	// decided by what comes back.
	if o.SuccessURL == "" || o.CancelURL == "" {
		t.Fatal("the order has nowhere to send the browser afterwards")
	}
	if o.SuccessURL == o.CancelURL {
		t.Error("paying and cancelling land in the same place")
	}
	if got := o.SuccessURL; !strings.Contains(got, f.ReturnURL) || !strings.Contains(got, paybus.ReturnMarker) {
		t.Errorf("the success URL is %q, want the form's own return_url plus the marker", got)
	}

	// And a donation with no tickets prices on its own.
	only := submissionOf(t, f, formbus.Values{
		"name":     {"A Donor"},
		"email":    {"donor@example.org"},
		"donation": {"25.00"},
	})

	if lines := paybus.OrderFor(f, only).Lines; len(lines) != 1 || lines[0].Unit != 2500 {
		t.Errorf("a donation on its own priced as %+v", lines)
	}
}

// And Start actually refuses when they disagree, which is what makes the
// property above load-bearing rather than incidental.
func TestStartRefusesAnOrderThatDoesNotAddUp(t *testing.T) {
	g := &gateway{}
	b := paybus.NewBusiness(quiet(), g, newSeen(), newSettler())

	base := paybus.Order{
		SubmissionID: types.NewID(),
		Form:         mustSlug(t, "feast-lunch-2026"),
		Currency:     "usd",
		SuccessURL:   "https://example.test/lunch?dropin=x",
		CancelURL:    "https://example.test/lunch?dropin=y",
		Lines:        []paybus.Line{{Label: "Lunch ticket", Qty: 2, Unit: 1200}},
	}

	good := base
	good.Total = 2400

	if _, err := b.Start(t.Context(), good); err != nil {
		t.Fatalf("Start refused a balanced order: %v", err)
	}

	// One cent out, which is what a rounding bug looks like.
	off := base
	off.Total = 2401

	_, err := b.Start(t.Context(), off)
	if !errors.Is(err, paybus.ErrUnbalanced) {
		t.Errorf("a one-cent discrepancy gave %v, want ErrUnbalanced", err)
	}

	// A dropped line, which is what forgetting the donation looked like.
	dropped := base
	dropped.Total = 2900

	if _, err := b.Start(t.Context(), dropped); !errors.Is(err, paybus.ErrUnbalanced) {
		t.Errorf("a dropped line gave %v, want ErrUnbalanced", err)
	}

	// Nothing reached Stripe in either failing case.
	if len(g.orders) != 1 {
		t.Errorf("Stripe was called %d times, want only for the balanced order", len(g.orders))
	}
}

func TestStartRefusesOrdersThatCannotBePaid(t *testing.T) {
	g := &gateway{}
	b := paybus.NewBusiness(quiet(), g, newSeen(), newSettler())

	ok := paybus.Order{
		SubmissionID: types.NewID(),
		Currency:     "usd",
		SuccessURL:   "https://example.test/a",
		CancelURL:    "https://example.test/b",
		Lines:        []paybus.Line{{Label: "Lunch ticket", Qty: 1, Unit: 1200}},
		Total:        1200,
	}

	nothing := ok
	nothing.Lines = nil
	nothing.Total = 0

	if _, err := b.Start(t.Context(), nothing); !errors.Is(err, paybus.ErrNothingToPay) {
		t.Errorf("a zero total gave %v, want ErrNothingToPay", err)
	}

	unnamed := ok
	unnamed.Lines = []paybus.Line{{Label: "", Qty: 1, Unit: 1200}}

	if _, err := b.Start(t.Context(), unnamed); err == nil {
		t.Error("Start accepted a line with no label, which is a blank row on a payment page")
	}

	noReturn := ok
	noReturn.SuccessURL = ""

	if _, err := b.Start(t.Context(), noReturn); err == nil {
		t.Error("Start accepted an order with nowhere to send the browser afterwards")
	}

	noSubmission := ok
	noSubmission.SubmissionID = types.ID{}

	if _, err := b.Start(t.Context(), noSubmission); err == nil {
		t.Error("Start accepted a payment for no submission, which nothing could ever attribute")
	}

	// And a gateway that says no is an error rather than a handoff to nowhere.
	failing := &gateway{err: errors.New("Stripe is unreachable")}
	fb := paybus.NewBusiness(quiet(), failing, newSeen(), newSettler())

	if _, err := fb.Start(t.Context(), ok); err == nil {
		t.Error("Start returned a handoff although the gateway failed")
	}

	// A gateway that returns success and no URL is refused too: a button with
	// no address is worse than an error somebody can act on.
	empty := &gateway{handoff: paybus.Handoff{Ref: "cs_1"}}
	eb := paybus.NewBusiness(quiet(), empty, newSeen(), newSettler())

	if _, err := eb.Start(t.Context(), ok); err == nil {
		t.Error("Start accepted a payment session with no address")
	}
}

// --- confirming one ---------------------------------------------------------

func TestAPaidNotificationSettlesTheSubmission(t *testing.T) {
	id := types.NewID()

	g := &gateway{event: paybus.Event{
		ID:           "evt_1",
		Kind:         "checkout.session.completed",
		Result:       paybus.ResultPaid,
		SubmissionID: id,
		Ref:          "pi_1",
	}}

	ledger, subs := newSeen(), newSettler()
	b := paybus.NewBusiness(quiet(), g, ledger, subs)

	e, err := b.Fulfil(t.Context(), time.Now(), []byte(`{}`), "sig")
	if err != nil {
		t.Fatalf("Fulfil: %v", err)
	}

	if e.Result != paybus.ResultPaid {
		t.Errorf("result = %q", e.Result)
	}
	if subs.status[id] != submissionbus.StatusPaid {
		t.Errorf("the submission is %q, want paid", subs.status[id])
	}
	if subs.refs[id] != "pi_1" {
		t.Errorf("the payment reference is %q, want pi_1", subs.refs[id])
	}
	if !ledger.ids["evt_1"] {
		t.Error("the event was not recorded, so a retry would repeat it")
	}
}

// Stripe retries until acknowledged, so the same event arriving twice is the
// ordinary case. It must not double-settle, and it must not be reported as a
// failure -- anything but an acknowledgement asks Stripe to keep trying.
func TestTheSameNotificationTwiceIsRecognised(t *testing.T) {
	id := types.NewID()

	g := &gateway{event: paybus.Event{
		ID: "evt_1", Kind: "checkout.session.completed",
		Result: paybus.ResultPaid, SubmissionID: id, Ref: "pi_1",
	}}

	ledger, subs := newSeen(), newSettler()
	b := paybus.NewBusiness(quiet(), g, ledger, subs)

	if _, err := b.Fulfil(t.Context(), time.Now(), []byte(`{}`), "sig"); err != nil {
		t.Fatalf("the first delivery: %v", err)
	}

	_, err := b.Fulfil(t.Context(), time.Now(), []byte(`{}`), "sig")
	if !errors.Is(err, paybus.ErrSeen) {
		t.Errorf("the second delivery gave %v, want ErrSeen", err)
	}

	if subs.status[id] != submissionbus.StatusPaid {
		t.Errorf("the submission is %q after two deliveries", subs.status[id])
	}
}

// The ordering, which is the thing in this package most likely to be
// "tidied" into a bug.
//
// Settle first, record second. If recording fails, the effect has already
// happened and the only cost of not writing it down is that a retry repeats a
// no-op -- so Fulfil must return success. Returning an error there would make
// Stripe retry an event that was already acted on, for three days, over a
// bookkeeping row.
func TestAFailureToRecordDoesNotUndoOrRetryAPaymentAlreadySettled(t *testing.T) {
	id := types.NewID()

	g := &gateway{event: paybus.Event{
		ID: "evt_1", Kind: "checkout.session.completed",
		Result: paybus.ResultPaid, SubmissionID: id, Ref: "pi_1",
	}}

	ledger := newSeen()
	ledger.err = errors.New("the ledger is unwritable")

	subs := newSettler()
	b := paybus.NewBusiness(quiet(), g, ledger, subs)

	if _, err := b.Fulfil(t.Context(), time.Now(), []byte(`{}`), "sig"); err != nil {
		t.Fatalf("Fulfil reported a failure although the payment was recorded: %v", err)
	}

	if subs.status[id] != submissionbus.StatusPaid {
		t.Errorf("the submission is %q, want paid: the settle must come first", subs.status[id])
	}
}

// And the other way round: when settling fails, that *is* an error, because
// the retry is what recovers the payment.
func TestAFailureToSettleIsReportedSoStripeRetries(t *testing.T) {
	g := &gateway{event: paybus.Event{
		ID: "evt_1", Kind: "checkout.session.completed",
		Result: paybus.ResultPaid, SubmissionID: types.NewID(), Ref: "pi_1",
	}}

	ledger := newSeen()
	subs := newSettler()
	subs.err = errors.New("the database is unreachable")

	b := paybus.NewBusiness(quiet(), g, ledger, subs)

	if _, err := b.Fulfil(t.Context(), time.Now(), []byte(`{}`), "sig"); err == nil {
		t.Fatal("Fulfil acknowledged an event it could not act on, which loses the payment")
	}

	// Nothing recorded, so the retry is not deduplicated away.
	if len(ledger.ids) != 0 {
		t.Error("the event was recorded although it was not acted on, so the retry would be ignored")
	}
}

func TestAFailedPaymentLeavesTheSubmissionRecoverable(t *testing.T) {
	id := types.NewID()

	g := &gateway{event: paybus.Event{
		ID: "evt_1", Kind: "payment_intent.payment_failed",
		Result: paybus.ResultFailed, SubmissionID: id, Ref: "pi_1",
	}}

	subs := newSettler()
	b := paybus.NewBusiness(quiet(), g, newSeen(), subs)

	if _, err := b.Fulfil(t.Context(), time.Now(), []byte(`{}`), "sig"); err != nil {
		t.Fatalf("Fulfil: %v", err)
	}

	if subs.status[id] != submissionbus.StatusFailed {
		t.Errorf("the submission is %q, want failed", subs.status[id])
	}

	// Failed is not settled, so a second attempt on Stripe's page can still
	// pay for it. Somebody whose card was declined very often tries another.
	if subs.status[id].Settled() {
		t.Error("a declined card left the submission terminal, so paying afterwards would be ignored")
	}

	g.event = paybus.Event{
		ID: "evt_2", Kind: "checkout.session.completed",
		Result: paybus.ResultPaid, SubmissionID: id, Ref: "pi_2",
	}

	if _, err := b.Fulfil(t.Context(), time.Now(), []byte(`{}`), "sig"); err != nil {
		t.Fatalf("the retry: %v", err)
	}

	if subs.status[id] != submissionbus.StatusPaid {
		t.Errorf("after a successful retry the submission is %q, want paid", subs.status[id])
	}
}

// A body Stripe did not sign changes nothing.
func TestAnUnverifiedNotificationChangesNothing(t *testing.T) {
	g := &gateway{err: errors.New("no valid signature")}

	ledger, subs := newSeen(), newSettler()
	b := paybus.NewBusiness(quiet(), g, ledger, subs)

	_, err := b.Fulfil(t.Context(), time.Now(), []byte(`{"amount_total": 1}`), "")
	if !errors.Is(err, paybus.ErrRefused) {
		t.Fatalf("an unsigned body gave %v, want ErrRefused", err)
	}

	if subs.calls != 0 {
		t.Error("an unverified notification reached the submission domain")
	}
	if len(ledger.ids) != 0 {
		t.Error("an unverified notification was recorded, which would swallow the real one if it shared an id")
	}
}

// A dispute has no submission on it and must still be written down.
func TestADisputeIsAcknowledgedAndNotSwallowed(t *testing.T) {
	g := &gateway{event: paybus.Event{
		ID: "evt_1", Kind: "charge.dispute.created",
		Result: paybus.ResultDisputed, Ref: "pi_1", Total: 2900,
	}}

	ledger, subs := newSeen(), newSettler()
	b := paybus.NewBusiness(quiet(), g, ledger, subs)

	e, err := b.Fulfil(t.Context(), time.Now(), []byte(`{}`), "sig")
	if err != nil {
		t.Fatalf("Fulfil: %v", err)
	}

	// Still reported as a dispute rather than folded into "ignored", which is
	// what would happen if the zero submission identifier were checked first.
	if e.Result != paybus.ResultDisputed {
		t.Errorf("result = %q, want disputed", e.Result)
	}

	if !ledger.ids["evt_1"] {
		t.Error("the dispute was not recorded, so every retry would log it again")
	}

	// Nothing is reversed. What to do about a chargeback is a decision for a
	// person.
	if subs.calls != 0 {
		t.Error("a dispute changed a submission on its own")
	}
}

// An event with no order behind it: a payment made by hand in the Stripe
// dashboard, or another integration on the same account.
func TestAnEventWithNoOrderIsAcknowledged(t *testing.T) {
	g := &gateway{event: paybus.Event{
		ID: "evt_1", Kind: "checkout.session.completed",
		Result: paybus.ResultPaid, Ref: "pi_1",
	}}

	ledger, subs := newSeen(), newSettler()
	b := paybus.NewBusiness(quiet(), g, ledger, subs)

	e, err := b.Fulfil(t.Context(), time.Now(), []byte(`{}`), "sig")
	if err != nil {
		t.Fatalf("Fulfil refused an event about somebody else's payment: %v", err)
	}

	if e.Result != paybus.ResultIgnored {
		t.Errorf("result = %q, want ignored", e.Result)
	}
	if subs.calls != 0 {
		t.Error("an event naming no submission reached the submission domain")
	}
}

// An event kind we do not handle is acknowledged and not written down.
// Recording every event an account emits is how a table becomes the largest
// one in the database.
func TestAnIgnoredEventIsNotRecorded(t *testing.T) {
	g := &gateway{event: paybus.Event{
		ID: "evt_1", Kind: "customer.created", Result: paybus.ResultIgnored,
	}}

	ledger, subs := newSeen(), newSettler()
	b := paybus.NewBusiness(quiet(), g, ledger, subs)

	if _, err := b.Fulfil(t.Context(), time.Now(), []byte(`{}`), "sig"); err != nil {
		t.Fatalf("Fulfil: %v", err)
	}

	if len(ledger.ids) != 0 {
		t.Error("an event we do not act on was recorded")
	}
	if subs.calls != 0 {
		t.Error("an event we do not act on reached the submission domain")
	}
}

func mustSlug(t *testing.T, s string) types.Slug {
	t.Helper()

	slug, err := types.ParseSlug(s)
	if err != nil {
		t.Fatalf("ParseSlug(%q): %v", s, err)
	}

	return slug
}

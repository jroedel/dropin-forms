// Package submissionbus holds what somebody filled in, once it has been
// accepted.
//
// A submission is immutable apart from its status. What was answered, what was
// ordered, what it came to and which version of the definition decided all of
// that are written once and never edited -- a record of what a person agreed
// to is not a working document. Only the payment status moves, and only
// forwards.
//
// # Accepting a submission is one transaction with the grant it arrived on
//
// [Business.Accept] writes the submission and spends the grant's nonce
// together, and reports [ErrReplayed] when the nonce had already been spent.
// That is the whole of the single-use property, and it has to be one statement
// pair in one transaction: a SELECT to see whether the nonce is free, followed
// by an INSERT, lets two simultaneous POSTs carrying the same grant both
// observe a free nonce and both succeed. On a form selling tickets that is a
// double charge.
//
// The transaction must not be held across a call to Stripe. SQLite has one
// writer, and a network call inside the write lock stalls every other
// submission for as long as the other end takes to answer. So the ordering is:
// accept the submission, commit, then create the payment -- which is also why
// a submission exists in a pending state before any money is involved.
package submissionbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// Status is where a submission has got to.
type Status string

const (
	// StatusReceived is a submission with nothing to pay. It is complete on
	// arrival, and it is the only terminal status a form that sells nothing
	// ever produces.
	StatusReceived Status = "received"

	// StatusPending is accepted, stored, and waiting for money. A pending
	// submission is a real submission: the person filled the form in and the
	// answers are kept whether or not they go on to pay, because "who started
	// and did not finish" is a question somebody selling lunches will ask.
	StatusPending Status = "pending"

	// StatusPaid is confirmed by Stripe's webhook, which is the only thing
	// that may set it. A success redirect is a browser saying something
	// happened, and a browser is not the authority on whether it did.
	StatusPaid Status = "paid"

	// StatusFailed is a payment Stripe told us would not happen.
	StatusFailed Status = "failed"
)

// statuses is every Status, in the order a submission moves through them.
var statuses = []Status{StatusReceived, StatusPending, StatusPaid, StatusFailed}

// The errors this package returns.
var (
	// ErrReplayed is a grant nonce that had already been spent. It means the
	// same grant arrived twice -- a double-clicked button, a refreshed POST,
	// or a deliberate replay -- and the app layer answers it the same way as
	// any other stale grant: re-render with a fresh one.
	ErrReplayed = errors.New("that submission has already been received")

	// ErrNotFound is a lookup by identifier that matched nothing.
	ErrNotFound = errors.New("no such submission")

	// ErrNotAStatus is a status value this binary does not recognise.
	ErrNotAStatus = errors.New("not a submission status")

	// ErrNotCollectable is an order that cannot be handed over, because it has
	// not been paid for. It is not a fault in the request: the volunteer
	// tapped a real button on a real order, and the answer is that this one
	// owes money, which is a sentence for a person rather than an error.
	ErrNotCollectable = errors.New("that order has not been paid for")

	// ErrDailyCap is a form that has taken as many submissions today as its
	// author said it should. Not a fault in the submission: the person who
	// meets it filled everything in correctly and is being turned away by a
	// number somebody else set, which is why the app layer answers it with the
	// form and a sentence rather than with an error page.
	ErrDailyCap = errors.New("that form has taken as many submissions as it can today")
)

// ParseStatus reads a status, and is the only way to get one from a database.
func ParseStatus(s string) (Status, error) {
	if st := Status(s); slices.Contains(statuses, st) {
		return st, nil
	}

	return "", fmt.Errorf("%w: %q is not one of %v", ErrNotAStatus, s, statuses)
}

func (s Status) String() string { return string(s) }

// Settled reports whether this status is final, so that nothing tries to
// collect money for it twice.
func (s Status) Settled() bool { return s == StatusReceived || s == StatusPaid }

// Submission is an accepted submission.
type Submission struct {
	ID   types.ID
	Form types.Slug

	// Version is the fingerprint of the definition that accepted this. Stored
	// so that a row read back next year can be read against the rules that
	// applied to it rather than against whatever the form says by then.
	Version string

	Status Status

	// Answers is what was filled in and what it came to, exactly as the
	// validator derived it.
	Answers formbus.Answers

	// Email is the submitter's address, lifted out of the answers so that
	// sending a receipt and handing an address to Stripe do not each have to
	// know which field it was. Zero when the form did not ask for one.
	Email types.Email

	// RemoteIP is the visitor's address, kept because Stripe names the end
	// user's IP and email as the factors its own fraud controls depend on, and
	// this surface has no cookie and therefore no other per-browser anchor at
	// all.
	RemoteIP string

	// PaymentRef is Stripe's identifier for the payment, once there is one.
	// Empty on a form that sells nothing, and empty until the payment step
	// fills it.
	PaymentRef string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Paid reports whether there is money to collect.
func (s Submission) Paid() bool { return s.Answers.Total > 0 }

// Collection is an order whose tokens were handed over at the will-call table.
//
// Its own type and its own row rather than a field on [Submission], because a
// submission is immutable apart from its status -- see the package comment --
// and this is not a payment status. It is a fact about a morning: who stood at
// the table, and when.
type Collection struct {
	SubmissionID types.ID
	Form         types.Slug
	CollectedAt  time.Time

	// CollectedBy is the account that recorded it. It is the reason this is a
	// row rather than a flag: the alternative to giving each volunteer an
	// account was one shared between them, which loses exactly this, on the
	// one surface where knowing who handed over what is the point.
	CollectedBy types.ID
}

// New is what the app layer supplies to accept one.
type New struct {
	Answers  formbus.Answers
	RemoteIP string

	// DailyCap is the form's own ceiling, copied from the definition by
	// whoever read it. Zero or less means no cap.
	//
	// Passed in rather than read here, because this package has no
	// definitions: it is handed validated answers and the rules that produced
	// them. The cap is the one rule the validator cannot enforce, since
	// counting what has already been stored is a question for storage.
	DailyCap int
}

// Storer is what this package needs from storage.
type Storer interface {
	// Accept writes the submission and spends the nonce in one transaction,
	// reporting false when the nonce had already been spent.
	//
	// The bool rather than an error for that case is the same shape userbus
	// uses for its three single-use claims, and for the same reason: "somebody
	// else got there first" is an outcome rather than a fault.
	Accept(ctx context.Context, s Submission, nonce string) (bool, error)

	ByID(ctx context.Context, id types.ID) (Submission, error)
	ByForm(ctx context.Context, form types.Slug) ([]Submission, error)

	// CountSince is how many submissions a form has taken since an instant,
	// which is what a daily cap is measured against.
	CountSince(ctx context.Context, form types.Slug, since time.Time) (int, error)

	// Unpaid lists submissions still waiting for money that were made in the
	// window [from, before), oldest first.
	Unpaid(ctx context.Context, from, before time.Time) ([]Submission, error)

	// SetStatus moves a submission forward, recording Stripe's reference.
	SetStatus(ctx context.Context, id types.ID, to Status, ref string, at time.Time) error

	// PruneNonces forgets spent nonces older than the grants that carried
	// them could possibly be. On a timer, never per request.
	PruneNonces(ctx context.Context, before time.Time) error

	// Collect records a collection, reporting false when there already was
	// one -- and returning the row that won either way, so the caller can say
	// who got there first.
	//
	// The bool rather than an error is the shape Accept uses above, for the
	// same reason: two volunteers tapping the same order from two phones is an
	// outcome, not a fault.
	Collect(ctx context.Context, c Collection) (Collection, bool, error)

	// Uncollect removes one.
	Uncollect(ctx context.Context, id types.ID) error

	// CollectionsForForm reads every collection on a form, keyed by
	// submission, in one query.
	CollectionsForForm(ctx context.Context, form types.Slug) (map[types.ID]Collection, error)
}

// Business is the set of operations on submissions.
type Business struct {
	log   *slog.Logger
	store Storer
}

// NewBusiness constructs one.
func NewBusiness(log *slog.Logger, store Storer) *Business {
	return &Business{log: log, store: store}
}

// Accept stores a validated submission and spends the grant it arrived on.
//
// The answers are taken as already validated, and that is the contract: this
// package does not re-derive a total or re-check a bound, because there would
// then be two implementations of every rule and the interesting question would
// be which one was wrong. [formbus.Form.Validate] is the only thing that
// decides whether a submission is acceptable, and it returns the total it
// derived from the definition.
//
// What this does decide is the status, because that is a fact about the
// submission rather than about the form's rules: something with a total is
// pending until money arrives, and something with no total is complete.
func (b *Business) Accept(ctx context.Context, now time.Time, g formbus.Grant, ns New) (Submission, error) {
	switch {
	case g.Nonce == "":
		return Submission{}, errors.New("a submission needs the grant it arrived on")
	case ns.Answers.FormID.Zero():
		return Submission{}, errors.New("a submission needs a form")
	case ns.Answers.Version == "":
		return Submission{}, errors.New("a submission needs the version it was checked against")
	case ns.Answers.FormID != g.Form:
		// Unreachable through the app layer, which redeems the grant against
		// the same form it validated with. Checked because the two arriving
		// from different places is precisely the mix-up the grant's
		// length-prefixed encoding exists to prevent, and a check here costs
		// nothing.
		return Submission{}, fmt.Errorf("the grant is for %q and the answers are for %q", g.Form, ns.Answers.FormID)
	case ns.Answers.Version != g.Version:
		return Submission{}, fmt.Errorf("the grant pins version %q and the answers were checked against %q", g.Version, ns.Answers.Version)
	}

	// The cap, before anything is written.
	//
	// A trailing day rather than a calendar one. "Since midnight" would have
	// to answer midnight where, and an intake control has no business holding
	// an opinion about the reader's timezone -- whereas "in the last
	// twenty-four hours" means the same thing everywhere and needs no
	// configuration.
	//
	// Counted before the insert rather than inside its transaction, so two
	// submissions arriving in the same instant can both see the same count and
	// both be accepted. That is deliberate: this is a bound on a day's damage,
	// not an inventory, and one over the line costs a row while a count inside
	// the write transaction would put a second statement in the path of every
	// submission on a database with one writer.
	if ns.DailyCap > 0 {
		taken, err := b.store.CountSince(ctx, ns.Answers.FormID, now.Add(-24*time.Hour))
		if err != nil {
			return Submission{}, fmt.Errorf("counting today's submissions: %w", err)
		}

		if taken >= ns.DailyCap {
			b.log.Warn("a form has reached its daily cap and is turning submissions away",
				"form", ns.Answers.FormID.String(), "cap", ns.DailyCap, "taken", taken)

			return Submission{}, ErrDailyCap
		}
	}

	status := StatusReceived
	if ns.Answers.Total > 0 {
		status = StatusPending
	}

	s := Submission{
		ID:        types.NewID(),
		Form:      ns.Answers.FormID,
		Version:   ns.Answers.Version,
		Status:    status,
		Answers:   ns.Answers,
		RemoteIP:  ns.RemoteIP,
		CreatedAt: now.UTC(),
		UpdatedAt: now.UTC(),
	}

	s.Email, _ = ns.Answers.SubmitterEmail()

	spent, err := b.store.Accept(ctx, s, g.Nonce)
	if err != nil {
		return Submission{}, fmt.Errorf("storing the submission: %w", err)
	}

	if !spent {
		// The grant had already been used. Nothing was written, which is the
		// point of doing both in one transaction.
		return Submission{}, ErrReplayed
	}

	b.log.Info("submission accepted",
		"submission_id", s.ID.String(), "form", s.Form.String(),
		"status", s.Status, "total", s.Answers.Total.String())

	return s, nil
}

// ByID reads one submission.
func (b *Business) ByID(ctx context.Context, id types.ID) (Submission, error) {
	s, err := b.store.ByID(ctx, id)
	if err != nil {
		return Submission{}, fmt.Errorf("reading the submission: %w", err)
	}

	return s, nil
}

// ByForm lists a form's submissions, newest first.
func (b *Business) ByForm(ctx context.Context, form types.Slug) ([]Submission, error) {
	out, err := b.store.ByForm(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("listing the submissions: %w", err)
	}

	return out, nil
}

// unpaidWindow is how far back [Business.Unpaid] will ever look.
//
// A floor rather than a policy about how long an order stays interesting. What
// it protects against is the day somebody restores a database without the
// record of which unpaid orders have already been reported: without a bound,
// the next sweep would mail the office about every order anybody has ever
// abandoned, which is the kind of message that gets notifications switched off
// for good.
const unpaidWindow = 30 * 24 * time.Hour

// Unpaid lists submissions that were started and never paid for, oldest first.
//
// grace is how long an order is given before it counts: somebody handed a
// payment page four minutes ago is typing a card number, not gone. What the
// number should be is a judgement about people rather than about storage,
// which is why it is an argument and not a constant here.
func (b *Business) Unpaid(ctx context.Context, now time.Time, grace time.Duration) ([]Submission, error) {
	out, err := b.store.Unpaid(ctx, now.Add(-unpaidWindow), now.Add(-grace))
	if err != nil {
		return nil, fmt.Errorf("listing the unpaid submissions: %w", err)
	}

	return out, nil
}

// Settle records what Stripe said about a payment.
//
// Forwards only. A paid submission does not become pending again because a
// webhook arrived out of order, and Stripe's delivery order is explicitly not
// guaranteed -- so the guard is here rather than in the handler that happens
// to receive the events.
func (b *Business) Settle(ctx context.Context, now time.Time, id types.ID, to Status, ref string) error {
	if _, err := ParseStatus(to.String()); err != nil {
		return err
	}

	s, err := b.store.ByID(ctx, id)
	if err != nil {
		return fmt.Errorf("reading the submission: %w", err)
	}

	if s.Status == to {
		// Already there. Stripe retries a webhook until it is acknowledged, so
		// this is the ordinary case rather than an anomaly.
		return nil
	}

	if s.Status.Settled() {
		b.log.Warn("ignoring a status change to a settled submission",
			"submission_id", id.String(), "from", s.Status, "to", to)

		return nil
	}

	if err := b.store.SetStatus(ctx, id, to, ref, now.UTC()); err != nil {
		return fmt.Errorf("recording the payment: %w", err)
	}

	b.log.Info("submission settled",
		"submission_id", id.String(), "from", s.Status, "to", to)

	return nil
}

// PruneNonces forgets spent grant nonces that no grant could still be carrying.
//
// The retention is the grant's own lifetime with a wide margin, because the
// cost of keeping a nonce too long is a row and the cost of forgetting one too
// early is a replay that succeeds.
func (b *Business) PruneNonces(ctx context.Context, now time.Time) error {
	if err := b.store.PruneNonces(ctx, now.Add(-24*time.Hour)); err != nil {
		return fmt.Errorf("pruning the spent grants: %w", err)
	}

	return nil
}

// Collect records that an order's tokens were handed over.
//
// # Only a settled order
//
// An order that still owes money is refused with [ErrNotCollectable], and that
// refusal is the rule this method exists for. The will-call table hands over
// one token per ticket bought, and "bought" means Stripe said so -- a pending
// order is somebody who reached the checkout page and closed it. Handing that
// person a plate is the mistake the list is there to prevent, and it is not one
// the volunteer can be expected to catch while a queue waits.
//
// [Status.Settled] is the existing word for this and is reused rather than
// restated: a form that sells nothing produces StatusReceived, which is as
// final as StatusPaid, and a definition like that should still be collectable
// if anybody ever runs a free sign-up through the same table.
//
// # Collecting twice
//
// Not an error. The second caller is told which of them recorded it and when,
// and the returned bool says whether this call was the one that wrote. Two
// volunteers working one queue will tap the same order, and a page that
// answered that with a failure would be a page they learn to ignore.
func (b *Business) Collect(ctx context.Context, now time.Time, id, by types.ID) (Collection, bool, error) {
	s, err := b.store.ByID(ctx, id)
	if err != nil {
		return Collection{}, false, fmt.Errorf("reading the submission: %w", err)
	}

	if !s.Status.Settled() {
		return Collection{}, false, fmt.Errorf("%w: %s is %s", ErrNotCollectable, id, s.Status)
	}

	c, wrote, err := b.store.Collect(ctx, Collection{
		SubmissionID: s.ID,
		Form:         s.Form,
		CollectedAt:  now.UTC(),
		CollectedBy:  by,
	})
	if err != nil {
		return Collection{}, false, fmt.Errorf("recording the collection: %w", err)
	}

	if wrote {
		b.log.Info("order collected",
			"submission_id", id.String(), "form", s.Form.String(), "by", by.String())
	}

	return c, wrote, nil
}

// Uncollect takes the mark off again.
//
// It exists because the mis-tap is certain: a phone, a queue, a list of names
// that look alike. Undoing it has to be one button and not a conversation with
// whoever administers the service.
//
// Deliberately not restricted to whoever marked it. The two people working the
// table are working one queue, and a correction that only its author could make
// would be a correction that waits for them to come back from the car park.
// Who marked it is still recorded, and the page shows it.
func (b *Business) Uncollect(ctx context.Context, id types.ID) error {
	if err := b.store.Uncollect(ctx, id); err != nil {
		return fmt.Errorf("removing the collection: %w", err)
	}

	b.log.Info("collection undone", "submission_id", id.String())

	return nil
}

// Collected is every collection on a form, keyed by submission.
func (b *Business) Collected(ctx context.Context, form types.Slug) (map[types.ID]Collection, error) {
	out, err := b.store.CollectionsForForm(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("reading the collections on %s: %w", form, err)
	}

	return out, nil
}

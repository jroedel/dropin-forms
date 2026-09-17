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

// New is what the app layer supplies to accept one.
type New struct {
	Answers  formbus.Answers
	RemoteIP string
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

	// SetStatus moves a submission forward, recording Stripe's reference.
	SetStatus(ctx context.Context, id types.ID, to Status, ref string, at time.Time) error

	// PruneNonces forgets spent nonces older than the grants that carried
	// them could possibly be. On a timer, never per request.
	PruneNonces(ctx context.Context, before time.Time) error
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

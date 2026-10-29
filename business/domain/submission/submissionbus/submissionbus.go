// Package submissionbus holds what somebody filled in, once it has been
// accepted.
//
// A submission is immutable apart from its status. What was answered, what was
// ordered, what it came to and which version of the definition decided all of
// that are written once and never edited -- a record of what a person agreed
// to is not a working document. Only the payment status moves, and only
// forwards.
//
// One exception, and it is narrow. On a form that says so, and that sells
// nothing, the person who answered may change their answers until a date the
// form states, through the link in every message we send them. Even then
// nothing is overwritten: the answers being replaced are kept as a
// [Revision], so the record of what somebody said, and when, is all still
// there. change.go has the rules.
//
// The other thing that moves is the [Review], on a form that holds its
// submissions for approval: awaiting, then approved or declined by somebody
// in the office. It is not a status and does not touch the money; review.go
// says why it is a second axis rather than more statuses.
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
	"strings"
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

	// StatusAbandoned is an order whose payment page has closed without being
	// paid. It is the end of the road and it is meant to read like one: the
	// person was sent to Stripe, and the page Stripe gave them has expired, so
	// there is nothing left that could take their money for this order.
	//
	// It exists because "pending" said the opposite. A row marked pending a
	// week later reads as something still in progress, and people asked what
	// it was waiting for; the answer was nothing. Set by [Business.Abandon] on
	// a timer, never by a webhook -- Stripe sends nothing when somebody closes
	// a tab, and the deadline is one we chose, so the clock is the authority.
	StatusAbandoned Status = "abandoned"
)

// statuses is every Status, in the order a submission moves through them.
var statuses = []Status{StatusReceived, StatusPending, StatusPaid, StatusFailed, StatusAbandoned}

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

	// ErrChangedMeanwhile is a change to answers that were changed, or
	// hidden, after they were read and before the change arrived: two tabs
	// open on the same link. Nothing was written. The app layer shows the
	// answers as they now stand and lets the person decide again.
	ErrChangedMeanwhile = errors.New("those answers were changed somewhere else in the meantime")

	// ErrUnchanged is a change that changed nothing. An outcome rather than a
	// fault -- somebody pressed Save to be sure -- and nothing is written,
	// so there is no revision and no message about it.
	ErrUnchanged = errors.New("nothing in those answers was different")
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

// SettledStatuses is every status [Status.Settled] reports true for, for a
// store that has to write the same rule into a query.
func SettledStatuses() []Status {
	return slices.DeleteFunc(slices.Clone(statuses), func(s Status) bool { return !s.Settled() })
}

// HoldsPlace reports whether a submission with this status keeps a place in an
// option that only so many may take -- a time slot, say.
//
// Wider than [Status.Settled] by one: an order waiting for its payment holds
// its place. Otherwise two people could both be sent to pay for the last slot
// and both payments would land, with nothing here able to refund either.
// Holding it costs at most the checkout window: an order not paid by then is
// abandoned, and an abandoned or failed order holds nothing. A declined one
// holds nothing either -- see [Submission.HoldsPlace], which is the rule;
// this is its half that is about money.
func (s Status) HoldsPlace() bool { return s.Settled() || s == StatusPending }

// PlaceHoldingStatuses is every status [Status.HoldsPlace] reports true for,
// for a store that counts places in a query -- SettledStatuses's shape.
func PlaceHoldingStatuses() []Status {
	return slices.DeleteFunc(slices.Clone(statuses), func(s Status) bool { return !s.HoldsPlace() })
}

// OptionFull is a submission refused because an option it chose has no place
// left: somebody else took the last one between the page being drawn and the
// answers arriving. Not a fault in the submission, so the app layer answers it
// with the form, the person's other answers still in it, and a sentence beside
// the question that has to be answered again.
type OptionFull struct {
	Field string
	Value string
}

func (e OptionFull) Error() string {
	return fmt.Sprintf("the option %q of %q has no places left", e.Value, e.Field)
}

// Submission is an accepted submission.
type Submission struct {
	ID   types.ID
	Form types.Slug

	// Version is the fingerprint of the definition that accepted this. Stored
	// so that a row read back next year can be read against the rules that
	// applied to it rather than against whatever the form says by then.
	Version string

	Status Status

	// Review is where it stands with the office, on a form that asks for
	// approval, and ReviewNone on every other. ReviewedBy and ReviewedAt are
	// who last decided it and when, zero until somebody has; ReviewNote is
	// what a decline said.
	Review     Review
	ReviewedBy types.ID
	ReviewedAt time.Time
	ReviewNote string

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

// Hiding is a submission taken out of every list: a test, a duplicate, a
// joke. Its own row, for the reason [Collection] is -- a submission is
// immutable apart from its status, and this is not a status. It is somebody
// deciding a row should not be counted.
//
// Hidden rather than deleted, and that is the decision this type exists to
// record. A submission can be a real card payment, and deleting it would leave
// Stripe holding a charge that nothing here can explain. A mis-click would be
// permanent. And a hidden row keeps saying who hid it and when, which is the
// answer to "where did Maria's order go" that a deleted one cannot give.
//
// What a hidden submission is left out of is every reader that counts or
// lists: ByForm, the public list beneath a form, the unpaid-order sweep, and
// through them the admin list and its totals, the CSV, the index counts and
// the will-call table. What it is not left out of is CountSince, the daily
// cap: that is an abuse control, and a script's submissions do not stop
// counting because somebody tidied them away.
type Hiding struct {
	SubmissionID types.ID
	Form         types.Slug
	HiddenAt     time.Time
	HiddenBy     types.ID
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

	// Claims is the limited places these answers take, from
	// [formbus.Form.Claims], passed in for the reason DailyCap is. Unlike the
	// cap, each one is counted inside the transaction that saves the
	// submission: see [Storer.Accept].
	Claims []formbus.Claim

	// NeedsApproval is the form's own setting, copied in for the same reason:
	// the submission is stored awaiting a decision.
	NeedsApproval bool
}

// Storer is what this package needs from storage.
type Storer interface {
	// Accept writes the submission and spends the nonce in one transaction,
	// reporting false when the nonce had already been spent.
	//
	// The bool rather than an error for that case is the same shape userbus
	// uses for its three single-use claims, and for the same reason: "somebody
	// else got there first" is an outcome rather than a fault.
	//
	// Each claim is counted against the places already held, inside the same
	// transaction and before anything is written, and a full one is refused
	// with [OptionFull]. Inside, unlike the daily cap, because this is an
	// inventory: two people taking the last slot in the same instant is the
	// case it exists for.
	Accept(ctx context.Context, s Submission, nonce string, claims []formbus.Claim) (bool, error)

	// Taken counts the places held in each of a form's options, by the
	// submissions [Submission.HoldsPlace] names, leaving out hidden ones.
	Taken(ctx context.Context, form types.Slug) (formbus.Taken, error)

	// Decide writes after's review over before's, in one transaction, on
	// condition that the stored review is still before's and the row is not
	// hidden -- ErrChangedMeanwhile otherwise. Claims are counted as Accept
	// counts them, with the row itself left out.
	Decide(ctx context.Context, before, after Submission, claims []formbus.Claim) error

	ByID(ctx context.Context, id types.ID) (Submission, error)
	ByForm(ctx context.Context, form types.Slug) ([]Submission, error)

	// Settled lists at most limit of a form's settled submissions, newest
	// first. Bounded in the query rather than by the caller, because the
	// caller is a public page and reading every row a form has ever taken to
	// show fifty of them is a cost any stranger can make us pay per reload.
	Settled(ctx context.Context, form types.Slug, limit int) ([]Submission, error)

	// CountSince is how many submissions a form has taken since an instant,
	// which is what a daily cap is measured against.
	CountSince(ctx context.Context, form types.Slug, since time.Time) (int, error)

	// Abandon marks every order still waiting for money that was made before
	// an instant as abandoned, in one statement, and reports the form of each
	// one it changed. One statement rather than a read and a write, so that a
	// payment confirmed between the two cannot be overwritten.
	Abandon(ctx context.Context, before, at time.Time) ([]types.Slug, error)

	// Outcomes counts a form's paid and abandoned submissions made since an
	// instant, hidden ones included.
	Outcomes(ctx context.Context, form types.Slug, since time.Time) (paid, abandoned int, err error)

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

	// Hide records a hiding, reporting false when there already was one and
	// returning the row that stands either way -- Collect's shape.
	Hide(ctx context.Context, h Hiding) (Hiding, bool, error)

	// Unhide removes one. Removing nothing is not an error.
	Unhide(ctx context.Context, id types.ID) error

	// Hidden lists a form's hidden submissions, newest first. Every other
	// list leaves them out; this is the one that finds them again.
	Hidden(ctx context.Context, form types.Slug) ([]Submission, error)

	// HidingOf reads one submission's hiding, and whether there is one.
	HidingOf(ctx context.Context, id types.ID) (Hiding, bool, error)

	// Change replaces before's answers with after's, keeps before's as a
	// revision and spends the nonce, in one transaction. It reports false
	// when the nonce had already been spent, Accept's shape, and
	// ErrChangedMeanwhile when the stored row is no longer before -- changed
	// or hidden since it was read. Claims are counted as Accept counts them,
	// except that the row being changed holds no place against itself.
	Change(ctx context.Context, before, after Submission, nonce string, claims []formbus.Claim) (bool, error)

	// Revisions reads the answers a submission has held and no longer does,
	// oldest first.
	Revisions(ctx context.Context, sub Submission) ([]Revision, error)

	// ByEmail lists a form's submissions from one address, not hidden,
	// newest first.
	ByEmail(ctx context.Context, form types.Slug, email types.Email) ([]Submission, error)

	// ChangeCounts is how many times each of a form's submissions has been
	// changed, absent for none.
	ChangeCounts(ctx context.Context, form types.Slug) (map[types.ID]int, error)

	// ByPaymentRef reads the submission a Stripe payment reference belongs
	// to, hidden or not, and ErrNotFound when none does.
	ByPaymentRef(ctx context.Context, ref string) (Submission, error)

	// Paid lists a form's paid submissions that carry a payment reference,
	// hidden ones included, newest first.
	Paid(ctx context.Context, form types.Slug) ([]Submission, error)
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

	if ns.NeedsApproval {
		s.Review = ReviewAwaiting
	}

	s.Email, _ = ns.Answers.SubmitterEmail()

	spent, err := b.store.Accept(ctx, s, g.Nonce, ns.Claims)
	if full, ok := errors.AsType[OptionFull](err); ok {
		b.log.Info("a submission chose an option with no places left",
			"form", s.Form.String(), "field", full.Field, "option", full.Value)

		return Submission{}, full
	}

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

// ByForm lists a form's settled submissions, newest first, leaving out any
// that have been hidden -- see [Hiding] for which readers that reaches.
//
// Settled, which is the same line [Business.Recent] draws and for a wider
// reason. An order nobody paid for is not a submission anybody needs to read:
// the list, its totals, the download, the spreadsheet feed and the API all
// come through here, and every one of them was being asked what a row of
// somebody who closed the payment page meant. Leaving them out in one place
// means a page added next year cannot forget to. [Business.Unsettled] is the
// way back to them, for the one reader who asks.
//
// A declined submission is left out for the same reason, and [Business.Declined]
// is its way back. One awaiting approval is kept: it is a real request that
// somebody has to act on, and the list is where they act on it.
func (b *Business) ByForm(ctx context.Context, form types.Slug) ([]Submission, error) {
	out, err := b.store.ByForm(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("listing the submissions: %w", err)
	}

	return slices.DeleteFunc(out, func(s Submission) bool {
		return !s.Status.Settled() || s.Review == ReviewDeclined
	}), nil
}

// Unsettled lists a form's submissions that were started and not paid for,
// newest first, leaving out any that have been hidden: the ones still in
// checkout, the ones whose card was declined, and the abandoned.
//
// For an administrator looking at how a form is doing -- how many people
// reached the payment page and stopped is a question about the form, not about
// any one order. Nothing else should read it.
func (b *Business) Unsettled(ctx context.Context, form types.Slug) ([]Submission, error) {
	out, err := b.store.ByForm(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("listing the unpaid submissions: %w", err)
	}

	return slices.DeleteFunc(out, func(s Submission) bool { return s.Status.Settled() }), nil
}

// ByPaymentRef reads the submission a Stripe payment reference belongs to.
//
// This is how a line in a bank deposit is traced back to an order: the
// reference recorded when the payment was confirmed is the one Stripe puts on
// the charge, the refund and the dispute alike. Hidden submissions are found,
// because hiding an order does not take its money out of the deposit.
func (b *Business) ByPaymentRef(ctx context.Context, ref string) (Submission, error) {
	if ref == "" {
		return Submission{}, ErrNotFound
	}

	s, err := b.store.ByPaymentRef(ctx, ref)
	if err != nil {
		return Submission{}, fmt.Errorf("reading the submission for a payment: %w", err)
	}

	return s, nil
}

// Paid lists a form's paid submissions, hidden ones included, newest first.
//
// Only those with a payment reference, which is every order paid through
// Stripe: what reads this is looking for money that has not reached the bank
// yet, and an order with no reference was never going to.
func (b *Business) Paid(ctx context.Context, form types.Slug) ([]Submission, error) {
	out, err := b.store.Paid(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("listing the paid submissions: %w", err)
	}

	return out, nil
}

// Recent lists at most limit of a form's settled submissions, newest first,
// for the list a form shows of its earlier answers beneath itself.
//
// Settled, and that is the rule this exists to state. The list is read on the
// public page as "these people are down for this", and an order that reached
// the checkout page and was closed is somebody who is not: printing their name
// under a sign-up form would be telling the next person a place is taken that
// is not. A free submission is settled on arrival, so on a form that sells
// nothing this is simply every submission.
//
// On a form that asks for approval, only what was approved: one still
// awaiting a decision is not yet somebody who is down for this, and one
// declined is somebody who is not. See [Review.Listed].
func (b *Business) Recent(ctx context.Context, form types.Slug, limit int) ([]Submission, error) {
	if limit <= 0 {
		return nil, nil
	}

	out, err := b.store.Settled(ctx, form, limit)
	if err != nil {
		return nil, fmt.Errorf("listing the recent submissions: %w", err)
	}

	return out, nil
}

// Taken counts the places held in each limited option of a form, for drawing
// the form with its full options marked and for the tally the results page
// shows. The count that decides whether a submission is accepted is a second
// one, made inside the transaction that saves it; this one is only ever a
// picture of a moment, and a page drawn from it can be out of date by the time
// it is submitted.
func (b *Business) Taken(ctx context.Context, form types.Slug) (formbus.Taken, error) {
	t, err := b.store.Taken(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("counting the places taken: %w", err)
	}

	return t, nil
}

// abandonAlarm is how many orders on one form may be abandoned in
// abandonLookback, with none paid, before the log says something.
//
// This is what is left of the unpaid-orders email. That email was noise to the
// people it went to -- somebody who closed a payment page is not news -- but
// it was also the only thing that would notice a payment step that had broken
// entirely, which looks exactly like a quiet day: no orders paid, and nobody
// told. Three abandoned and none paid in a day is not what a working form
// does, and a log line costs nobody's attention until somebody goes looking.
const (
	abandonAlarm    = 3
	abandonLookback = 24 * time.Hour
)

// Abandon marks every order still unpaid after the given time as abandoned,
// pending and declined alike, and reports how many it marked.
//
// after is how long the payment page stays open, with whatever margin the
// caller wants on top of it. It is passed in rather than known here because
// the deadline is set where the payment page is made, and the two must be the
// same number; see paybus.AbandonAfter.
//
// Called on a timer. A declined card is included because past the deadline it
// is in exactly the position of a closed tab: the page that could have taken a
// second card has gone.
func (b *Business) Abandon(ctx context.Context, now time.Time, after time.Duration) (int, error) {
	forms, err := b.store.Abandon(ctx, now.Add(-after), now.UTC())
	if err != nil {
		return 0, fmt.Errorf("marking the expired orders abandoned: %w", err)
	}

	if len(forms) == 0 {
		return 0, nil
	}

	b.log.Info("orders abandoned", "count", len(forms))

	// Asked only of the forms this pass touched, so the line is repeated at
	// most once per newly abandoned order rather than on every tick for as
	// long as the condition holds.
	slices.SortFunc(forms, func(x, y types.Slug) int { return strings.Compare(x.String(), y.String()) })

	for _, form := range slices.Compact(forms) {
		paid, abandoned, err := b.store.Outcomes(ctx, form, now.Add(-abandonLookback))
		if err != nil {
			b.log.Error("whether a form's payments are working could not be read",
				"form", form.String(), "error", err)

			continue
		}

		if paid == 0 && abandoned >= abandonAlarm {
			b.log.Warn("orders on this form are being abandoned and none have been paid; check that its payment page opens and takes a card",
				"form", form.String(), "abandoned", abandoned, "paid", paid, "since", abandonLookback.String())
		}
	}

	return len(forms), nil
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

	// An abandoned order may still be paid and may not become anything else.
	// Stripe will not complete a payment on a page that has expired, so the
	// first should never happen -- but if a confirmation does arrive, the money
	// is real and the status follows it. A late failure says nothing new.
	if s.Status == StatusAbandoned && to != StatusPaid {
		b.log.Warn("ignoring a status change to an abandoned submission",
			"submission_id", id.String(), "to", to)

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

// Hide takes a submission out of every list, recording who did it.
//
// Twice is once: the second caller is told who got there first, as Collect
// tells a second volunteer. Any status may be hidden, a paid one included --
// a test run with a real card is exactly the row somebody wants gone -- and
// hiding refunds nothing, which the page that offers it says.
func (b *Business) Hide(ctx context.Context, now time.Time, id, by types.ID) (Hiding, bool, error) {
	s, err := b.store.ByID(ctx, id)
	if err != nil {
		return Hiding{}, false, fmt.Errorf("reading the submission: %w", err)
	}

	h, wrote, err := b.store.Hide(ctx, Hiding{
		SubmissionID: s.ID,
		Form:         s.Form,
		HiddenAt:     now.UTC(),
		HiddenBy:     by,
	})
	if err != nil {
		return Hiding{}, false, fmt.Errorf("hiding the submission: %w", err)
	}

	if wrote {
		b.log.Info("submission hidden",
			"submission_id", id.String(), "form", s.Form.String(), "status", s.Status.String(), "by", by.String())
	}

	return h, wrote, nil
}

// Unhide puts a submission back in every list.
func (b *Business) Unhide(ctx context.Context, id, by types.ID) error {
	if err := b.store.Unhide(ctx, id); err != nil {
		return fmt.Errorf("unhiding the submission: %w", err)
	}

	b.log.Info("submission unhidden", "submission_id", id.String(), "by", by.String())

	return nil
}

// Hidden lists a form's hidden submissions, newest first.
func (b *Business) Hidden(ctx context.Context, form types.Slug) ([]Submission, error) {
	out, err := b.store.Hidden(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("listing the hidden submissions: %w", err)
	}

	return out, nil
}

// HidingOf reads one submission's hiding, and whether it has one.
func (b *Business) HidingOf(ctx context.Context, id types.ID) (Hiding, bool, error) {
	h, ok, err := b.store.HidingOf(ctx, id)
	if err != nil {
		return Hiding{}, false, fmt.Errorf("reading whether the submission is hidden: %w", err)
	}

	return h, ok, nil
}

// Package depositbus reconciles Stripe's deposits to the bank against the
// forms whose payments are in them.
//
// Stripe does not send each payment to the bank. It collects them in a
// balance, takes its fees out, and pays the balance into the checking account
// on a schedule -- one deposit, one line on the bank statement, holding
// payments from every form that sold anything that week, refunds taken back
// out, and possibly money that never came through this service at all. The
// person keeping the books has the bank line and needs the sales behind it.
//
// Stripe can list exactly which payments, refunds, disputes and fees make up a
// deposit it scheduled itself (an *automatic* payout); it cannot for one
// somebody started by hand, because a manual payout is just an amount. Each
// line of an automatic payout carries the reference of the payment it came
// from, and that reference is what this service recorded on the submission
// when the payment was confirmed. That reference is the whole join.
//
// # Pulled, not pushed
//
// Deposits are read from Stripe on a timer rather than delivered by webhook.
// A webhook would need a fifth event on the endpoint, and the four that are
// there already are the ones whose absence fails silently; a fifth is one more
// thing to forget when an endpoint is recreated. A deposit is also not
// urgent: it reaches the bank days after the payments in it, and an hour's lag
// in seeing it here costs nothing.
//
// # Kept, not fetched per page
//
// A paid deposit never changes, so each one is read from Stripe once and kept.
// The page that shows a form's share of every deposit would otherwise have to
// read every deposit there has ever been, with every line, on every view --
// slow, and a cost any reload can make us pay against Stripe's rate limit.
// Nothing prunes the kept rows. They are the ledger the page is read from.
package depositbus

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// Kind is what a line of a deposit is, in the words somebody keeping the books
// uses rather than in Stripe's. Stripe has dozens of transaction types; the
// page needs four.
type Kind string

// The kinds.
const (
	// KindSale is a payment taken: money in.
	KindSale Kind = "sale"

	// KindRefund is money given back. Negative.
	KindRefund Kind = "refund"

	// KindDispute is money taken back by a cardholder's bank, or returned when
	// a dispute is won. Its fee is Stripe's dispute fee.
	KindDispute Kind = "dispute"

	// KindOther is everything else: Stripe's own account fees, adjustments,
	// anything this service did not cause. Kept so that a deposit's lines
	// still add up to the deposit.
	KindOther Kind = "other"
)

// Payout is one deposit to the bank.
type Payout struct {
	// ID is Stripe's, po_….
	ID string

	// Amount is what reached the bank: the number on the statement.
	Amount   types.Money
	Currency string

	Created time.Time

	// ArrivalDate is when Stripe expected it in the account, which is the
	// date the bank statement shows it under.
	ArrivalDate time.Time

	// Automatic is false for a payout somebody started by hand. Stripe cannot
	// say what is in one of those, so it is kept with no lines.
	Automatic bool

	// Paid and Reconciled are as Stripe reported them when it was read. Only
	// a paid payout is kept, and an automatic one only once Stripe has
	// finished working out its lines; neither is stored, because a kept
	// payout is both by construction.
	Paid       bool
	Reconciled bool

	// Mismatch is set when the lines Stripe gave do not add up to the amount.
	// That should not happen, and when it does the page says so rather than
	// showing a breakdown that quietly fails to match the bank.
	Mismatch bool

	// FetchedAt is when it was read from Stripe.
	FetchedAt time.Time

	// Destination is Stripe's identifier for where the money went, and Bank
	// and Last4 are that account as a person recognises it: "CHASE ••••2013".
	// Kept because "the deposit is not in the account" is the first question
	// anybody reconciling asks, and the answer is sometimes that it went to a
	// different account. Destination alone is still worth showing when it is
	// not a bank account at all, or the key may not read the account.
	Destination string
	Bank        string
	Last4       string

	// TraceID is the bank's own reference for the transfer, which the bank
	// can search for when a deposit cannot be found on the statement by its
	// amount. Stripe only knows it some time after paying -- TraceStatus
	// says whether it is pending, supported (TraceID is set) or unsupported
	// by the receiving bank.
	TraceID     string
	TraceStatus string
}

// The trace statuses Stripe reports.
const (
	TracePending     = "pending"
	TraceSupported   = "supported"
	TraceUnsupported = "unsupported"
)

// Described reports whether everything Stripe will ever say about where a
// payout went is known: its destination, and a trace that is no longer
// pending.
func (p Payout) Described() bool {
	return p.Destination != "" && p.TraceStatus != "" && p.TraceStatus != TracePending
}

// Line is one movement of money inside a deposit.
type Line struct {
	// ID is Stripe's balance transaction, txn_….
	ID     string
	Payout string

	Kind Kind

	// Type is Stripe's own word for it, kept for the export: somebody
	// checking a line against the Stripe dashboard will look for that word.
	Type        string
	Description string

	// Amount is before fees, Fee is what Stripe kept, and Net is what reached
	// the deposit. Amount and Net are negative for money going out.
	Amount types.Money
	Fee    types.Money
	Net    types.Money

	Created time.Time

	// PaymentRef is the payment the line came from, pi_…, or empty when it
	// came from none (Stripe's own fees).
	PaymentRef string

	// Form and SubmissionID say which order it was, and are zero when the
	// payment was not taken by this service.
	Form         types.Slug
	SubmissionID types.ID
}

// Ledger is Stripe, as far as deposits go.
type Ledger interface {
	// Payouts lists payouts created at or after since, any status. The zero
	// time means every payout there has ever been.
	Payouts(ctx context.Context, since time.Time) ([]Payout, error)

	// Lines lists every movement in an automatic payout except the payout's
	// own: the line that moves the total out to the bank is the deposit
	// itself, not a part of it.
	Lines(ctx context.Context, payout string) ([]Line, error)
}

// Storer is what this package needs from storage.
type Storer interface {
	// Has reports whether a payout is already kept.
	Has(ctx context.Context, id string) (bool, error)

	// Save keeps a payout and its lines together, in one transaction, so a
	// payout is never kept with half its lines. Saving one already kept is
	// not an error and changes nothing.
	Save(ctx context.Context, p Payout, lines []Line) error

	// Latest is when the newest kept payout was created, and false when none
	// is kept.
	Latest(ctx context.Context) (time.Time, bool, error)

	// Undescribed is when the oldest kept payout created after since that is
	// not yet [Payout.Described] was created, and false when there is none.
	Undescribed(ctx context.Context, since time.Time) (time.Time, bool, error)

	// Describe records where a kept payout went and its trace, if the kept
	// row is not yet described. A payout already described is left alone.
	Describe(ctx context.Context, p Payout) error

	// Unattributed lists the lines that name a payment and no form.
	Unattributed(ctx context.Context) ([]Line, error)

	// Attribute records which order a line was.
	Attribute(ctx context.Context, line string, form types.Slug, submission types.ID) error

	// ForForm lists the payouts holding any line of a form's, newest arrival
	// first, and every line of each of them -- the whole deposit, not just
	// the form's share, because the share is shown beside the total.
	ForForm(ctx context.Context, form types.Slug) ([]Payout, map[string][]Line, error)

	// Deposited is the set of payment references of a form's sales that are
	// in a kept payout.
	Deposited(ctx context.Context, form types.Slug) (map[string]bool, error)
}

// Submissions is what reconciling needs to know about orders.
type Submissions interface {
	ByPaymentRef(ctx context.Context, ref string) (submissionbus.Submission, error)
	Paid(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error)
}

// lookback is how far before the newest kept payout each sync looks again.
//
// A payout is created days before it is paid, and Stripe works out its lines
// a little after that, so a payout created before the newest kept one may not
// have been ready when that one was. Thirty days is far longer than any of
// that takes; the cost of looking further back is a list of payouts that are
// already kept and are skipped by identifier.
const lookback = 30 * 24 * time.Hour

// describeFor is how long after it was created a payout's trace is still
// asked about.
//
// Stripe says a trace can stay pending for up to ten days after the payout
// arrives. Sixty is far past that; a payout still pending then is one the
// receiving bank will never report, and asking about it forever would keep
// every sync listing two months of payouts it already has.
const describeFor = 60 * 24 * time.Hour

// Business is the set of operations on deposits.
type Business struct {
	log         *slog.Logger
	ledger      Ledger
	store       Storer
	submissions Submissions

	// synced is when the last sync finished without an error. In memory
	// rather than stored, because a sync runs at every startup: after a
	// restart the page says "not yet" for the seconds that takes, which is
	// true.
	mu     sync.Mutex
	synced time.Time
}

// NewBusiness constructs one.
func NewBusiness(log *slog.Logger, ledger Ledger, store Storer, submissions Submissions) *Business {
	return &Business{log: log, ledger: ledger, store: store, submissions: submissions}
}

// Synced is when deposits were last read from Stripe successfully, and false
// when they have not been since this process started.
func (b *Business) Synced() (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.synced, !b.synced.IsZero()
}

// Sync reads every deposit Stripe has paid that is not kept yet, and keeps it.
//
// One payout that cannot be read does not stop the others: each is its own
// transaction, the failure is collected, and the next sync tries it again
// because it is still not kept. The error returned joins them all.
func (b *Business) Sync(ctx context.Context, now time.Time) error {
	var since time.Time

	latest, ok, err := b.store.Latest(ctx)
	if err != nil {
		return fmt.Errorf("reading the newest kept deposit: %w", err)
	}
	if ok {
		since = latest.Add(-lookback)
	}

	// Further back, if a kept payout is still waiting for its trace -- or was
	// kept before this service recorded traces at all. The list Stripe
	// returns is what describes it, so it has to reach that far.
	oldest, ok, err := b.store.Undescribed(ctx, now.Add(-describeFor))
	if err != nil {
		return fmt.Errorf("reading which deposits still lack a trace: %w", err)
	}
	if ok && oldest.Before(since) {
		since = oldest
	}

	payouts, err := b.ledger.Payouts(ctx, since)
	if err != nil {
		return fmt.Errorf("listing the deposits: %w", err)
	}

	var errs []error

	for _, p := range payouts {
		kept, err := b.keep(ctx, now, p)
		if err != nil {
			errs = append(errs, fmt.Errorf("deposit %s: %w", p.ID, err))
			continue
		}

		if kept {
			b.log.Info("deposit read from Stripe",
				"payout", p.ID, "amount", p.Amount.String(), "currency", p.Currency,
				"arrival", p.ArrivalDate.Format(time.DateOnly), "automatic", p.Automatic,
				"to", p.Bank+" "+p.Last4, "destination", p.Destination, "trace", p.TraceStatus)
		}
	}

	if err := b.reattribute(ctx); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	b.mu.Lock()
	b.synced = now
	b.mu.Unlock()

	return nil
}

// keep reads one payout's lines and keeps it, reporting whether it did.
func (b *Business) keep(ctx context.Context, now time.Time, p Payout) (bool, error) {
	// Not paid yet, or paid and then failed: neither is money in the bank.
	// A payout that is still on its way is looked at again next time.
	if !p.Paid {
		return false, nil
	}

	// Stripe works out an automatic payout's lines after paying it. Until it
	// has, a list would come back short, and keeping a short list would keep
	// it forever.
	if p.Automatic && !p.Reconciled {
		return false, nil
	}

	has, err := b.store.Has(ctx, p.ID)
	if err != nil {
		return false, err
	}
	if has {
		// Kept already, and its lines never change. Its trace can: Stripe
		// learns it after paying, so a payout kept with its trace pending --
		// or kept before traces were recorded -- is described here on a
		// later sync. The store writes only to a row not yet described, so
		// this is a no-op for the rest.
		return false, b.store.Describe(ctx, p)
	}

	var lines []Line
	if p.Automatic {
		lines, err = b.ledger.Lines(ctx, p.ID)
		if err != nil {
			return false, err
		}
	}

	for i := range lines {
		lines[i].Payout = p.ID

		if err := b.attribute(ctx, &lines[i]); err != nil {
			return false, err
		}
	}

	p.Mismatch = p.Automatic && Sum(lines) != p.Amount
	p.FetchedAt = now.UTC()

	if p.Mismatch {
		b.log.Error("a deposit's lines do not add up to it",
			"payout", p.ID, "amount", p.Amount.String(), "lines", Sum(lines).String(),
			"consequence", "the deposits page shows it with a warning instead of a breakdown that matches the bank")
	}

	if err := b.store.Save(ctx, p, lines); err != nil {
		return false, err
	}

	return true, nil
}

// attribute fills in which order a line was, when it was one of ours.
func (b *Business) attribute(ctx context.Context, l *Line) error {
	if l.PaymentRef == "" {
		return nil
	}

	sub, err := b.submissions.ByPaymentRef(ctx, l.PaymentRef)

	switch {
	case errors.Is(err, submissionbus.ErrNotFound):
		// Not taken by this service: a payment made in the Stripe dashboard,
		// or another integration on the same account. Shown on the page as
		// such, not an error.
		return nil
	case err != nil:
		return err
	}

	l.Form = sub.Form
	l.SubmissionID = sub.ID

	return nil
}

// reattribute tries again for kept lines that name a payment and no order.
//
// The case it is for: a payment confirmation that reached this service late
// -- a webhook that failed for a day and was retried -- after the deposit
// holding it had already been kept. Without this the line would say "not from
// a form" forever. It is one indexed lookup per such line, and on an account
// used only by this service there are none.
func (b *Business) reattribute(ctx context.Context) error {
	lines, err := b.store.Unattributed(ctx)
	if err != nil {
		return fmt.Errorf("listing the deposit lines with no order: %w", err)
	}

	for _, l := range lines {
		if err := b.attribute(ctx, &l); err != nil {
			return fmt.Errorf("tracing deposit line %s: %w", l.ID, err)
		}

		if l.Form.Zero() {
			continue
		}

		if err := b.store.Attribute(ctx, l.ID, l.Form, l.SubmissionID); err != nil {
			return fmt.Errorf("recording deposit line %s: %w", l.ID, err)
		}
	}

	return nil
}

// Sum is the net of some lines: what they put into a deposit together.
//
// A plain sum rather than [types.Money.Add]. Add guards the ceiling on what
// one order may cost, and a deposit is not an order -- a large one is not
// wrong.
func Sum(lines []Line) types.Money {
	var n types.Money
	for _, l := range lines {
		n += l.Net
	}

	return n
}

// Share is one deposit as one form sees it.
type Share struct {
	Payout Payout

	// The form's lines by kind, before fees. Refunds and Disputes are
	// usually negative.
	Sales    types.Money
	Refunds  types.Money
	Disputes types.Money
	Other    types.Money

	// Fees is what Stripe kept from the form's lines, as a positive number.
	Fees types.Money

	// Net is what the form's lines put into the deposit.
	Net types.Money

	// OtherForms is the net of every other form's lines in the same deposit,
	// and NotFromAForm the net of the rest: Stripe's own fees, payments taken
	// outside this service. The three add up to the deposit, which is the
	// number on the bank statement, unless Payout.Mismatch says otherwise.
	OtherForms   types.Money
	NotFromAForm types.Money

	// Lines is the form's lines, oldest first.
	Lines []Line
}

// ForForm is every kept deposit holding any of a form's money, newest first,
// each with the form's share of it.
func (b *Business) ForForm(ctx context.Context, form types.Slug) ([]Share, error) {
	payouts, lines, err := b.store.ForForm(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("reading the deposits: %w", err)
	}

	out := make([]Share, 0, len(payouts))

	for _, p := range payouts {
		s := Share{Payout: p}

		for _, l := range lines[p.ID] {
			switch {
			case l.Form == form:
				s.Lines = append(s.Lines, l)
				s.Fees += l.Fee
				s.Net += l.Net

				switch l.Kind {
				case KindSale:
					s.Sales += l.Amount
				case KindRefund:
					s.Refunds += l.Amount
				case KindDispute:
					s.Disputes += l.Amount
				default:
					s.Other += l.Amount
				}

			case !l.Form.Zero():
				s.OtherForms += l.Net

			default:
				s.NotFromAForm += l.Net
			}
		}

		slices.SortStableFunc(s.Lines, func(a, b Line) int { return a.Created.Compare(b.Created) })

		out = append(out, s)
	}

	slices.SortStableFunc(out, func(a, b Share) int {
		return cmp.Or(b.Payout.ArrivalDate.Compare(a.Payout.ArrivalDate), cmp.Compare(b.Payout.ID, a.Payout.ID))
	})

	return out, nil
}

// Undeposited lists a form's paid orders whose payment is in no kept deposit
// yet, newest first.
//
// This is the usual answer to "why does the bank not match": the money is in
// Stripe's balance, waiting for the next payout, or in a payout that has not
// arrived. It is also where a payment shows that will never be deposited --
// one refunded in full before the payout, say -- which is why the page lists
// these by name rather than only counting them.
func (b *Business) Undeposited(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error) {
	paid, err := b.submissions.Paid(ctx, form)
	if err != nil {
		return nil, err
	}

	deposited, err := b.store.Deposited(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("reading which payments are deposited: %w", err)
	}

	return slices.DeleteFunc(paid, func(s submissionbus.Submission) bool { return deposited[s.PaymentRef] }), nil
}

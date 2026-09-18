// Package paybus is how money is asked for and how it is confirmed.
//
// Two operations, and they are deliberately far apart. [Business.Start] runs
// inside the request that submitted the form and produces a URL to send
// somebody to. [Business.Fulfil] runs much later, in a request from Stripe
// that has nothing to do with the person, and is the only thing that may
// decide a submission was paid for. A browser arriving back at a success URL
// is a browser saying something happened, and a browser is not the authority
// on whether it did.
//
// # Why this package depends on the submission domain
//
// A payment is a payment *for* a submission: there is no order without one,
// and the identifier in Stripe's metadata is a submission identifier. The
// dependency is real and it runs one way, so it is written as an import rather
// than hidden behind an adapter that would only move the same knowledge into
// whatever built it.
//
// # The order of the two writes in Fulfil
//
// Settle the submission first, record the event second. It is the opposite of
// what feels careful, and the reason is what each failure costs:
//
//   - Record first, then crash: Stripe retries, the event is recognised as
//     already seen, nothing is settled, and somebody has paid for a lunch that
//     the office has no record of selling. The failure is silent and permanent.
//   - Settle first, then crash: Stripe retries and settles again. Settle is
//     forwards-only and returns without writing when the status is already
//     where it is going, so the second attempt is a no-op.
//
// One of those is recoverable by doing nothing and the other is not, so the
// dedupe is protection against *repeated effects*, not the thing standing
// between us and a lost payment.
package paybus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// The errors this package returns.
var (
	// ErrRefused is a webhook whose signature did not verify, or whose
	// timestamp is outside the tolerance. It is the one error here that must
	// never be answered with a retryable status: a body Stripe did not sign is
	// not a delivery Stripe will retry.
	ErrRefused = errors.New("that payment notification is not from Stripe")

	// ErrSeen is an event that has already been acted on. Not a fault: Stripe
	// retries until acknowledged, and being told the same thing twice is the
	// ordinary case rather than an anomaly.
	ErrSeen = errors.New("that payment notification has already been handled")

	// ErrNothingToPay is an order whose total is zero. A Checkout session for
	// nothing is a page somebody cannot pay, so this is refused here rather
	// than discovered by them.
	ErrNothingToPay = errors.New("there is nothing to pay for")

	// ErrUnbalanced is an order whose lines do not sum to the total the
	// validator derived. See Business.Start.
	ErrUnbalanced = errors.New("the lines do not add up to the total")
)

// Line is one priced line of an order, as it will appear on Stripe's page.
//
// The label is what the person sees there, so it comes from the form
// definition rather than from anything they typed.
type Line struct {
	Label string
	Qty   int
	Unit  types.Money
}

// Amount is what this line comes to.
func (l Line) Amount() types.Money { return l.Unit * types.Money(l.Qty) }

// Order is what to charge for, assembled by whoever accepted the submission.
type Order struct {
	SubmissionID types.ID
	Form         types.Slug

	// Currency is the form's, lowercase, as Stripe wants it.
	Currency string

	// Email is where Stripe sends its own receipt, and is one of the two
	// factors Stripe names as what its automated fraud controls depend on.
	// The other is the end user's IP address, and with Checkout that costs
	// nothing to provide because the person's browser reaches Stripe directly
	// -- Stripe sees the address itself. On the in-page Payment Element it
	// would have to be passed, which is one more reason that is Track B.
	Email types.Email

	Lines []Line

	// Total is what the validator derived from the pinned definition. It is
	// passed in rather than recomputed here, and then checked against the
	// lines -- see Business.Start.
	Total types.Money

	// Return is where Stripe sends the browser afterwards. Both are absolute
	// URLs on the site that hosts the form, not on this service: the person
	// ends up back where they started, and the in-line confirmation appears in
	// the frame there.
	SuccessURL string
	CancelURL  string
}

// Handoff is where to send somebody, and what Stripe called it.
type Handoff struct {
	// Ref is Stripe's identifier for the session. Stored on the submission so
	// that a row in the admin UI can be matched against a row in the Stripe
	// dashboard, which is the first thing anybody wants when a payment is in
	// question.
	Ref string

	// URL is Stripe's hosted page. It goes into a link the person clicks
	// rather than a redirect: a real click is a user activation, which is what
	// gets a top-level navigation out of an iframe past every popup blocker,
	// and it is honest that they are leaving the site.
	URL string
}

// Result is what Stripe told us about a payment.
type Result string

const (
	// ResultPaid is money collected.
	ResultPaid Result = "paid"

	// ResultFailed is a payment Stripe says will not happen. Not terminal for
	// the submission: somebody whose card was declined very often tries
	// another one, and the submission is still pending until it is paid.
	ResultFailed Result = "failed"

	// ResultDisputed is a chargeback. Nothing here acts on it beyond writing
	// it down loudly, because what to do about a disputed lunch ticket is a
	// decision for a person.
	ResultDisputed Result = "disputed"

	// ResultIgnored is an event we are not interested in. Stripe delivers
	// whatever the account is subscribed to, and acknowledging an event we do
	// not handle is correct: refusing it would make Stripe retry it for days.
	ResultIgnored Result = "ignored"
)

// Event is a webhook, verified and reduced to what this service acts on.
type Event struct {
	// ID is Stripe's event identifier, which is what dedupe is keyed on.
	ID   string
	Kind string

	Result Result

	// SubmissionID is read out of the metadata we set when the session was
	// created. Zero on an event that carries none, which is not an error --
	// an account can have other integrations, and a test event has no order
	// behind it at all.
	SubmissionID types.ID

	// Ref is Stripe's identifier for the payment itself, recorded on the
	// submission.
	Ref string

	// Total and Currency are what Stripe says was charged, kept so that a
	// mismatch against what we asked for can be logged. Deliberately not used
	// to *decide* anything: the amount that matters is the one the validator
	// derived, and it is already stored.
	Total    types.Money
	Currency string
}

// Gateway is the payment processor.
//
// An interface, and the reason is not testability alone: everything above this
// line is a rule about money that has to hold whoever is processing it, and
// the rules are worth being able to read without a network client in the way.
// The implementation is in stores/stripepay.
type Gateway interface {
	// Checkout creates the hosted payment session.
	Checkout(ctx context.Context, o Order) (Handoff, error)

	// Verify checks a webhook's signature over the *unmodified* body and
	// returns what it says. It must be given the bytes exactly as they
	// arrived: the signature covers them, so anything that has re-encoded a
	// form or re-marshalled a JSON document has already destroyed it.
	//
	// No clock is passed, unlike everything else in this package. Stripe's
	// signature covers a timestamp and the tolerance check is part of the
	// constant-time verification in their SDK, which reads the system clock
	// itself -- so handing one in would be a parameter the only real
	// implementation ignores. A test of the tolerance builds a signature with
	// a stale timestamp instead of moving a clock.
	Verify(payload []byte, signature string) (Event, error)
}

// Seen records which events have been acted on.
type Seen interface {
	// Record writes an event identifier, reporting false when it was already
	// there. The conflict is the check, in one statement, because two
	// deliveries of one event can be in flight at the same time and a
	// read-then-write would let both through.
	Record(ctx context.Context, id string, kind string, at time.Time) (bool, error)

	// Prune forgets events older than any retry could still be for.
	Prune(ctx context.Context, before time.Time) error
}

// Submissions is what fulfilment does to a submission.
type Submissions interface {
	Settle(ctx context.Context, now time.Time, id types.ID, to submissionbus.Status, ref string) error
}

// Business is the set of operations on payments.
type Business struct {
	log         *slog.Logger
	gateway     Gateway
	seen        Seen
	submissions Submissions
}

// NewBusiness constructs one.
func NewBusiness(log *slog.Logger, gateway Gateway, seen Seen, submissions Submissions) *Business {
	return &Business{log: log, gateway: gateway, seen: seen, submissions: submissions}
}

// Start creates the hosted payment session for an accepted submission.
//
// The check worth the name here is that the lines add up to the total. The
// validator derived the total from the pinned definition, and the lines are
// assembled separately to be sent to Stripe as the things a person will see
// itemised on the payment page -- two derivations of one number, which is the
// shape this whole service tries not to have. Rather than pick one and hope,
// this refuses to charge anything when they disagree.
//
// Without it, a dropped line or a mis-added donation means the confirmation
// page says one figure and the card is charged another, and the person with
// the receipt is the one who finds out.
func (b *Business) Start(ctx context.Context, o Order) (Handoff, error) {
	switch {
	case o.SubmissionID.Zero():
		return Handoff{}, errors.New("a payment needs the submission it is for")
	case o.Currency == "":
		return Handoff{}, errors.New("a payment needs a currency")
	case o.SuccessURL == "" || o.CancelURL == "":
		return Handoff{}, errors.New("a payment needs somewhere to send the browser afterwards")
	case o.Total <= 0:
		return Handoff{}, ErrNothingToPay
	}

	var sum types.Money

	for _, l := range o.Lines {
		switch {
		case l.Qty <= 0:
			return Handoff{}, fmt.Errorf("the line %q has a quantity of %d", l.Label, l.Qty)
		case l.Unit < 0:
			return Handoff{}, fmt.Errorf("the line %q has a negative price", l.Label)
		case l.Label == "":
			// Stripe shows this to the person paying, and an unnamed line on
			// a payment page is how somebody decides not to pay.
			return Handoff{}, errors.New("every line needs a label; it is what somebody reads on the payment page")
		}

		sum += l.Amount()
	}

	if sum != o.Total {
		return Handoff{}, fmt.Errorf("%w: the lines come to %s and the total is %s", ErrUnbalanced, sum, o.Total)
	}

	h, err := b.gateway.Checkout(ctx, o)
	if err != nil {
		return Handoff{}, fmt.Errorf("creating the payment session: %w", err)
	}

	if h.URL == "" {
		return Handoff{}, errors.New("the payment session has no address to send anybody to")
	}

	b.log.Info("payment session created",
		"submission_id", o.SubmissionID.String(), "form", o.Form.String(),
		"total", o.Total.String(), "session", h.Ref)

	return h, nil
}

// Fulfil verifies a webhook and acts on it, returning what it was.
//
// payload must be the body exactly as it arrived. Every caller of this is one
// handler, and that handler must not sit behind anything that reads the body
// -- which is why the webhook has a chain of its own.
func (b *Business) Fulfil(ctx context.Context, now time.Time, payload []byte, signature string) (Event, error) {
	e, err := b.gateway.Verify(payload, signature)
	if err != nil {
		// Logged at Info rather than Error. An unsigned POST to a public
		// webhook URL is somebody scanning, which is a fact about the internet
		// rather than a fault in this service -- but the count matters, so it
		// is one line each.
		b.log.Info("a payment notification was refused",
			"bytes", len(payload), "reason", err)

		return Event{}, fmt.Errorf("%w: %w", ErrRefused, err)
	}

	if e.Result == ResultIgnored {
		// Not recorded. Nothing happened, so there is nothing for a second
		// delivery to repeat, and writing a row for every event the account
		// happens to emit is how a table becomes the largest one in the
		// database.
		b.log.Debug("a payment notification was not one we act on", "event", e.ID, "kind", e.Kind)

		return e, nil
	}

	// A dispute is handled before the check below, because it is the one event
	// that legitimately carries no submission identifier: our metadata is on
	// the payment intent, and a dispute's metadata is the dispute's own. Its
	// payment reference is what somebody needs in order to find it, and that
	// is always there -- so this is written down loudly rather than falling
	// into the "names no submission" case and being swallowed.
	//
	// Attributing a dispute to a submission would mean a lookup by payment
	// reference, which is worth doing and is not what stands between a
	// chargeback and somebody noticing it. The log line is.
	if e.Result == ResultDisputed {
		// Error rather than Warn, so it reaches whatever is watching: a
		// dispute has a deadline to answer in the Stripe dashboard, and a
		// missed one is the money plus a fee.
		b.log.Error("a payment was disputed and needs answering in Stripe",
			"event", e.ID, "ref", e.Ref,
			"submission_id", e.SubmissionID.String(), "total", e.Total.String())

		if _, err := b.seen.Record(ctx, e.ID, e.Kind, now); err != nil {
			b.log.Error("a disputed payment was logged but not recorded",
				"event", e.ID, "error", err)
		}

		return e, nil
	}

	if e.SubmissionID.Zero() {
		// A real event with no order behind it: a payment made in the Stripe
		// dashboard, or another integration on the same account. Acknowledged,
		// because refusing it makes Stripe retry it for days.
		b.log.Warn("a payment notification names no submission",
			"event", e.ID, "kind", e.Kind, "ref", e.Ref)

		e.Result = ResultIgnored

		return e, nil
	}

	// Settle first, record second. The reasoning is in the package comment,
	// and it is the one thing here that must not be reordered for tidiness.
	switch e.Result {
	case ResultPaid:
		if err := b.submissions.Settle(ctx, now, e.SubmissionID, submissionbus.StatusPaid, e.Ref); err != nil {
			return Event{}, fmt.Errorf("recording a payment: %w", err)
		}

	case ResultFailed:
		if err := b.submissions.Settle(ctx, now, e.SubmissionID, submissionbus.StatusFailed, e.Ref); err != nil {
			return Event{}, fmt.Errorf("recording a failed payment: %w", err)
		}

		// No arm for ResultDisputed: it returned above, before the
		// submission identifier was required. Nothing here reverses a
		// submission, because whether to is a decision for a person.
	}

	fresh, err := b.seen.Record(ctx, e.ID, e.Kind, now)
	if err != nil {
		// The effect has already happened, and the only cost of failing to
		// record it is that a retry repeats a no-op. So this is not returned
		// as a failure: doing so would make Stripe retry an event that was
		// already acted on, forever, over a bookkeeping row.
		b.log.Error("a payment notification was acted on but not recorded",
			"event", e.ID, "submission_id", e.SubmissionID.String(), "error", err)

		return e, nil
	}

	if !fresh {
		b.log.Info("a payment notification arrived twice",
			"event", e.ID, "kind", e.Kind, "submission_id", e.SubmissionID.String())

		return e, ErrSeen
	}

	b.log.Info("payment notification handled",
		"event", e.ID, "kind", e.Kind,
		"submission_id", e.SubmissionID.String(), "result", e.Result, "ref", e.Ref)

	return e, nil
}

// Forget prunes the recorded events.
//
// The retention is generous on purpose: Stripe retries a failed delivery for
// up to three days, and the cost of keeping a row too long is a row, while the
// cost of forgetting one too early is acting on the same event twice.
func (b *Business) Forget(ctx context.Context, now time.Time) error {
	if err := b.seen.Prune(ctx, now.Add(-30*24*time.Hour)); err != nil {
		return fmt.Errorf("pruning the handled notifications: %w", err)
	}

	return nil
}

// OrderFor assembles what to charge from a stored submission.
//
// This is the second derivation of the total, and it is deliberate. The
// validator produced [formbus.Answers.Total] from the pinned definition; this
// walks the same answers and produces the itemised lines a person will read on
// Stripe's page. [Business.Start] then refuses to charge anything unless the
// two agree.
//
// That is the opposite of this service's usual rule, which is that a number
// must be derived in exactly one place. The exception is earned here: Stripe
// needs the lines whether or not we want them, the person paying needs to see
// what they are paying for, and an itemisation that does not add up to the
// total is a bug that is invisible until somebody reads their receipt. Two
// derivations that must agree catch it; one derivation cannot.
//
// The reason the lines are not simply [formbus.Answers.Lines] is that they are
// not all of it. Answers.Lines is the priced items; a donation is an amount
// *field*, and the validator adds the two together. A line list built from
// Answers.Lines alone would charge for the tickets and quietly drop the gift.
func OrderFor(f formbus.Form, sub submissionbus.Submission) Order {
	o := Order{
		SubmissionID: sub.ID,
		Form:         sub.Form,
		Currency:     sub.Answers.Currency,
		Email:        sub.Email,
		Total:        sub.Answers.Total,
	}

	for _, l := range sub.Answers.Lines {
		o.Lines = append(o.Lines, Line{Label: l.Label, Qty: l.Qty, Unit: l.Price})
	}

	for _, a := range sub.Answers.Fields {
		if a.Kind != formbus.KindAmount || len(a.Values) == 0 {
			continue
		}

		// Re-parsed with the same strict parser the validator used, so the two
		// cannot disagree about what "5.00" means. An unparseable value here
		// is unreachable -- it would have been a violation and there would be
		// no submission -- and is skipped rather than guessed at, which makes
		// it show up as an unbalanced order rather than as a wrong charge.
		amount, err := types.ParseMoney(a.Values[0])
		if err != nil || amount == 0 {
			continue
		}

		o.Lines = append(o.Lines, Line{Label: a.Label, Qty: 1, Unit: amount})
	}

	// The return addresses are the form's own, with a marker so that the page
	// hosting the form can tell a browser coming back from Stripe from one
	// arriving for the first time. Safe to append with a '?': Form.Check
	// refuses a return_url that already carries a query string, precisely so
	// that this is a concatenation rather than a URL rewrite.
	if f.ReturnURL != "" {
		o.SuccessURL = f.ReturnURL + "?" + ReturnMarker + "=" + sub.Form.String() + "&" + ReturnState + "=" + StatePaid
		o.CancelURL = f.ReturnURL + "?" + ReturnMarker + "=" + sub.Form.String() + "&" + ReturnState + "=" + StateCancelled
	}

	return o
}

// The query parameters Stripe sends the browser back with.
//
// Read by the script on the hosting page, which passes them into the frame.
// They say which form and what happened, and nothing more: no identifier, no
// amount, nothing worth guessing. What a person is actually told about their
// own order comes from the submission, and the authority for "this is paid" is
// the webhook rather than anything in a URL a browser can edit.
const (
	ReturnMarker = "dropin"
	ReturnState  = "dropin_state"
)

// The two things that can have happened by the time the browser is back.
//
// Named rather than written twice, because they are written in three places
// that have to agree and only two of them are Go: this package builds the
// URLs, the embedded form's own page decides what to say from them, and
// embed.js on the hosting page carries them from one to the other. A typo in
// any of them is a visitor who paid and is told nothing.
//
// Neither is evidence. StatePaid means Stripe sent the browser to the success
// address, which is a browser repeating something rather than Stripe telling
// us anything -- anybody can type it. It decides the wording on a page and
// nothing else; the authority for "this is paid" is the webhook signature.
const (
	StatePaid      = "paid"
	StateCancelled = "cancelled"
)

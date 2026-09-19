// Package notifybus tells people about a submission: the person who made it,
// and the people who have to act on it.
//
// # Why this is a domain package and not part of either app
//
// Two entirely different requests produce the same message. A form that sells
// nothing is finished the moment it is stored, in a POST from a stranger's
// browser; a form that sells is finished when Stripe says the money arrived,
// in a request nobody is waiting on. An App package may not import another App
// package, and it should not: what to say and who to say it to is the same
// question in both cases, and answering it twice is how the office comes to be
// told one thing about a free form and another about a paid one.
//
// # Nothing here fails a request
//
// Every method returns nothing. That is not laziness about errors, it is the
// only honest signature: by the time any of this runs the submission is stored
// and, on the paid path, the money is collected. There is no caller anywhere
// that should answer differently because a message did not go out, and one
// that could would eventually turn a broken relay into a refused submission or
// a Stripe retry storm. So a failure is a log line -- a loud one, because
// silence here means the office never hears about an order.
//
// # What it sends, and what it deliberately does not
//
// One message to the submitter, and one to each person in the office. Never
// one message addressed to several people: foundation/mail takes a single
// recipient by design, because a list of addresses on one envelope is how
// everybody learns who else is on it.
//
// Nothing is sent at the time for a pending submission. Somebody who has just
// been handed a payment page has not finished, and an email saying "we have
// your order" arriving while they are still typing their card number would
// either read as a receipt or as a reason to stop.
//
// But an order that stays pending is one nobody hears about at all, and that
// was a real gap rather than a considered silence: Stripe sends no webhook for
// a checkout somebody simply closed, so an abandoned order sat in the
// management app and waited to be noticed. [Business.ReportUnpaid] closes it,
// on a timer rather than on a request -- one message to the office listing
// what has been sitting unpaid, each order reported once. The submitter is
// told nothing, ever: they did not pay, they may have meant not to, and
// chasing them is not this service's business.
package notifybus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/mail"
)

// sendBudget bounds how long one submission's messages may take.
//
// A relay that has stopped answering must not hold a request open: on the
// embed surface somebody is watching a spinner, and on the webhook Stripe is
// waiting to be told the payment was recorded. The budget covers the whole
// batch rather than each message, because four slow messages are as bad as one
// stuck one.
//
// Exceeding it costs the messages that had not gone yet, and nothing else. On
// the webhook path in particular the payment is already settled and Stripe's
// retry is answered with "already handled" -- so a slow relay can delay an
// acknowledgement and can never lose a payment or send a receipt twice.
const sendBudget = 15 * time.Second

// Forms is where definitions come from.
type Forms interface {
	ByID(slug types.Slug) (formbus.Form, error)
}

// Submissions is the one thing this package reads about a submission.
//
// It re-reads rather than taking a value, even from the caller that has just
// written one. A notification is a statement about what is stored, and the
// paid path has no submission in hand at all -- it has an identifier out of
// Stripe's metadata. One lookup by primary key is a small price for both
// callers saying the same thing.
type Submissions interface {
	ByID(ctx context.Context, id types.ID) (submissionbus.Submission, error)

	// Unpaid is every order started at least grace ago and still waiting for
	// money, oldest first. What counts as long enough is passed in rather than
	// decided there, because it is a judgement about people.
	Unpaid(ctx context.Context, now time.Time, grace time.Duration) ([]submissionbus.Submission, error)
}

// Grants answers who may read a form's submissions, which is most of who
// should be told about one.
type Grants interface {
	ForForm(ctx context.Context, form types.Slug) ([]accessbus.Grant, error)
}

// Accounts turns the identifier on a grant into an address.
type Accounts interface {
	ByID(ctx context.Context, id types.ID) (userbus.User, error)
}

// Mutes is who has asked not to be emailed about which form.
//
// Mutes rather than subscriptions, and the inversion is the point: a row means
// silence, so somebody newly given the job of reading a form's submissions
// hears about them without doing anything. A table of subscriptions would mean
// a new grant arrives silent, and nobody would find out until an order went
// unnoticed.
type Mutes interface {
	Muted(ctx context.Context, userID types.ID, form types.Slug) (bool, error)
	MutedForForm(ctx context.Context, form types.Slug) ([]types.ID, error)
	MutedForUser(ctx context.Context, userID types.ID) ([]types.Slug, error)
	Mute(ctx context.Context, userID types.ID, form types.Slug, at time.Time) error
	Unmute(ctx context.Context, userID types.ID, form types.Slug) error
}

// Reports is the record of which unpaid orders the office has already been
// told about.
//
// It exists for one reason: without it the hourly sweep would report the same
// abandoned order every hour until somebody deleted it, and there is nothing
// to delete -- an unpaid order stays unpaid forever. The claim is a single
// statement rather than a read and a write, so a restart between the two
// cannot turn one message into a message an hour.
type Reports interface {
	ClaimUnpaidNotice(ctx context.Context, id types.ID, at time.Time) (bool, error)
	ForgetUnpaidNotices(ctx context.Context, before time.Time) error
}

// Config is what this package needs.
type Config struct {
	Log         *slog.Logger
	Mail        mail.Sender
	Forms       Forms
	Submissions Submissions
	Grants      Grants
	Accounts    Accounts

	// Mutes is who has turned email about a form off. Optional: without it
	// everybody who holds the form is told, which is what this service did
	// before there was anything to turn off, and the page that offers the
	// choice is simply not mounted.
	Mutes Mutes

	// Reports is where it is written down that an unpaid order has been
	// reported. Optional, and without it the sweep does nothing at all --
	// which is the right degradation: a sweep with no memory would mail the
	// office about the same orders every hour, and that is worse than the
	// silence it was meant to fix.
	Reports Reports

	// MuteKey signs the unsubscribe link in each notification. A zero key
	// leaves the link out and the message otherwise unchanged -- the
	// management app is then the only way to change the preference, which is
	// a degradation rather than a failure.
	MuteKey MuteKey

	// Office is the address every notification goes to regardless of who holds
	// a grant, from mail.notify in the configuration. Optional: a form's own
	// notify list and its results holders are the other two ways somebody
	// hears about a submission, and an installation may reasonably use only
	// those.
	Office string

	// AdminBaseURL is where the link in a notification points. Without it the
	// message still says everything it has to say and simply carries no link,
	// which is better than a link to nowhere.
	AdminBaseURL string

	// Now is the clock, injected so a test does not have to be run at a
	// particular time. Nil means time.Now.
	Now func() time.Time
}

// Business sends the notifications.
type Business struct {
	cfg Config
}

// NewBusiness constructs one.
//
// It refuses a config it cannot work with, rather than discovering it on the
// first submission: this runs at the end of the one path in the service that
// takes somebody's money, and a nil dereference there is a 500 in front of a
// person who has just paid.
func NewBusiness(cfg Config) (*Business, error) {
	switch {
	case cfg.Log == nil:
		return nil, errNoLog
	case cfg.Mail == nil:
		return nil, errNoMail
	case cfg.Forms == nil || cfg.Submissions == nil:
		return nil, errNoSource
	}

	return &Business{cfg: cfg}, nil
}

var (
	errNoLog    = fmt.Errorf("notifications need a logger; a failed send has nowhere else to be recorded")
	errNoMail   = fmt.Errorf("notifications need somewhere to send mail, even if that is a recorder")
	errNoSource = fmt.Errorf("notifications need the form definitions and the submissions they are about")
)

// Received tells everybody about a submission with nothing to pay.
func (b *Business) Received(ctx context.Context, id types.ID) {
	b.tell(ctx, id, false)
}

// Paid tells everybody about a submission Stripe has confirmed.
//
// Called from the webhook, after the submission has been settled, and never
// from the browser coming back from Stripe: a browser saying it paid is a
// browser repeating something, and a receipt is not a thing to send on that.
func (b *Business) Paid(ctx context.Context, id types.ID) {
	b.tell(ctx, id, true)
}

// tell is both of the above, because they differ in four sentences and not in
// any of the work.
func (b *Business) tell(ctx context.Context, id types.ID, paid bool) {
	// Detached from the caller's context and given a budget of its own. A
	// visitor closing the tab must not cancel the message that tells the
	// office they ordered something, and a relay that has stopped answering
	// must not hold the request open indefinitely.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendBudget)
	defer cancel()

	sub, err := b.cfg.Submissions.ByID(ctx, id)
	if err != nil {
		b.cfg.Log.Error("nobody could be told about a submission, because it could not be read",
			"submission_id", id.String(), "error", err)

		return
	}

	f, err := b.cfg.Forms.ByID(sub.Form)
	if err != nil {
		b.cfg.Log.Error("nobody could be told about a submission, because its form could not be read",
			"submission_id", id.String(), "form", sub.Form.String(), "error", err)

		return
	}

	if to := sub.Email.String(); to != "" {
		b.send(ctx, to, b.forSubmitter(f, sub, paid), "submitter", sub)
	}

	office := b.office(ctx, f)

	for _, to := range office {
		// Built per recipient rather than once, because the last line differs:
		// somebody who holds the form is told how to stop, and an address that
		// comes from a configuration file is told where the decision lives. An
		// unsubscribe link that does nothing is worse than none.
		b.send(ctx, to.address, b.forOffice(f, sub, paid, to), "office", sub)
	}

	if len(office) == 0 {
		// Worth a line of its own. A service that stores orders nobody is told
		// about is the failure this step exists to fix, and the way back into
		// it is quiet: an empty notify list and a form whose grants were all
		// revoked.
		b.cfg.Log.Warn("a submission was recorded and nobody was notified; check mail.notify, the form's notify list, and who holds results on it",
			"submission_id", sub.ID.String(), "form", sub.Form.String())
	}
}

// unpaidGrace is how long an order is given before the office is told it was
// started and not paid for.
//
// An hour. Somebody handed a payment page four minutes ago is typing a card
// number; somebody who was going to finish has finished. Longer would be safer
// against reporting an order that is about to be paid, and the cost of that
// mistake is small -- a message saying an order is unpaid, followed by the
// ordinary one saying it was paid, which reads as an update rather than as a
// contradiction. The cost in the other direction is a broken payment step
// nobody notices until somebody thinks to look at the list.
const unpaidGrace = time.Hour

// noticeRetention is how long the record of a reported order is kept.
//
// Longer than the window submissionbus.Unpaid looks back over, and that
// relationship is the whole of the number: forget a notice while the order it
// names is still inside that window and the next sweep reports it again.
const noticeRetention = 90 * 24 * time.Hour

// sweepBudget bounds one pass of ReportUnpaid.
//
// Wider than sendBudget because nobody is waiting: this runs on a timer rather
// than inside a request, and the thing it must not do is outlive the process
// it belongs to. It still has a bound, because a relay that accepts
// connections and never answers would otherwise leave this goroutine wedged
// until shutdown, and the next tick would start a second one.
const sweepBudget = 2 * time.Minute

// ReportUnpaid tells the office about orders that were started and never paid
// for.
//
// Called on a timer. Nothing returns an error, for the reason the rest of this
// package does not: there is no caller that should behave differently because
// a message did not go out, and a failure is a loud log line.
//
// Each order is reported once and then never again, which is what the claim in
// [Reports] is for. Note the order of operations below -- recipients, then the
// claim, then the send. Claiming before knowing there is anybody to tell would
// silently spend the one notice an order gets on a service with an empty
// notification list, and the day somebody was finally added they would hear
// nothing about any of it.
func (b *Business) ReportUnpaid(ctx context.Context, now time.Time) {
	if b.cfg.Reports == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sweepBudget)
	defer cancel()

	subs, err := b.cfg.Submissions.Unpaid(ctx, now, unpaidGrace)
	if err != nil {
		b.cfg.Log.Error("the unpaid orders could not be listed, so nobody was told about them", "error", err)

		return
	}

	if len(subs) == 0 {
		return
	}

	// Grouped by form, because the message is per form: it names the form in
	// its subject, it links into that form's submissions, and the people it
	// goes to are that form's. Insertion order is kept so the whole sweep
	// stays oldest-first.
	byForm := map[types.Slug][]submissionbus.Submission{}
	var forms []types.Slug

	for _, sub := range subs {
		if _, seen := byForm[sub.Form]; !seen {
			forms = append(forms, sub.Form)
		}

		byForm[sub.Form] = append(byForm[sub.Form], sub)
	}

	for _, slug := range forms {
		b.reportForm(ctx, now, slug, byForm[slug])
	}
}

// reportForm is one form's unpaid orders, in one message per recipient.
func (b *Business) reportForm(ctx context.Context, now time.Time, slug types.Slug, subs []submissionbus.Submission) {
	f, err := b.cfg.Forms.ByID(slug)
	if err != nil {
		// A form renamed away with orders still pending against it. Logged
		// once per sweep rather than claimed and dropped, so that restoring
		// the definition also restores the notice.
		b.cfg.Log.Error("unpaid orders could not be reported, because their form could not be read",
			"form", slug.String(), "unpaid", len(subs), "error", err)

		return
	}

	office := b.office(ctx, f)
	if len(office) == 0 {
		b.cfg.Log.Warn("orders were started and not paid for, and there is nobody to tell; check mail.notify, the form's notify list, and who holds results on it",
			"form", slug.String(), "unpaid", len(subs))

		return
	}

	// Claimed one at a time, keeping only the ones this sweep is the first to
	// see. A claim that fails is skipped rather than fatal: the alternative is
	// one unreadable row stopping the whole report.
	fresh := make([]submissionbus.Submission, 0, len(subs))

	for _, sub := range subs {
		mine, err := b.cfg.Reports.ClaimUnpaidNotice(ctx, sub.ID, now)
		if err != nil {
			b.cfg.Log.Error("an unpaid order could not be claimed for reporting",
				"submission_id", sub.ID.String(), "form", slug.String(), "error", err)

			continue
		}

		if mine {
			fresh = append(fresh, sub)
		}
	}

	if len(fresh) == 0 {
		return
	}

	for _, to := range office {
		m := b.forUnpaid(f, fresh, to)
		m.To = to.address

		if err := b.cfg.Mail.Send(ctx, m); err != nil {
			b.cfg.Log.Error("a report of unpaid orders could not be sent",
				"form", slug.String(), "unpaid", len(fresh), "error", err)

			continue
		}
	}

	b.cfg.Log.Info("unpaid orders reported",
		"form", slug.String(), "unpaid", len(fresh), "recipients", len(office))
}

// ForgetReports drops the record of orders reported long enough ago that
// nothing will look at them again.
//
// On a timer, beside the other prunes, and never per request.
func (b *Business) ForgetReports(ctx context.Context, now time.Time) error {
	if b.cfg.Reports == nil {
		return nil
	}

	if err := b.cfg.Reports.ForgetUnpaidNotices(ctx, now.Add(-noticeRetention)); err != nil {
		return fmt.Errorf("forgetting the reported orders: %w", err)
	}

	return nil
}

// send is one message, and a failure is a log line.
func (b *Business) send(ctx context.Context, to string, m mail.Message, who string, sub submissionbus.Submission) {
	m.To = to

	if err := b.cfg.Mail.Send(ctx, m); err != nil {
		b.cfg.Log.Error("a submission notification could not be sent",
			"recipient", who, "submission_id", sub.ID.String(),
			"form", sub.Form.String(), "error", err)

		return
	}

	b.cfg.Log.Info("submission notification sent",
		"recipient", who, "submission_id", sub.ID.String(), "form", sub.Form.String())
}

// recipient is one address in the office list, and why it is there.
//
// The why is not bookkeeping: it decides the last line of the message. An
// account that holds the form gets a link that turns these off, and an address
// that comes from a file gets a sentence saying where that decision is made
// instead -- because offering somebody a button that cannot work is worse than
// offering nothing.
type recipient struct {
	address string

	// userID is zero for an address that came from configuration rather than
	// from an account, which is exactly the case with nobody to unsubscribe.
	userID types.ID
}

// office is everybody who should hear about a submission to this form, in one
// deduplicated list.
//
// Three sources, and they are three because each answers a different question.
// The configured address is "where does this installation's post go"; the
// definition's notify list is "who else cares about this particular form" --
// the kitchen, for a lunch -- and the results holders are "who has been given
// the job of reading these", which is the list that stays right when somebody
// leaves.
//
// Only the third can be turned off. The other two are decisions somebody made
// in a file about an address rather than about themselves, and an unsubscribe
// link on a shared office mailbox is a way for one person to switch off
// everybody else's copy.
func (b *Business) office(ctx context.Context, f formbus.Form) []recipient {
	seen := make(map[string]bool)
	var out []recipient

	add := func(raw string, userID types.ID) {
		address := strings.TrimSpace(raw)
		if address == "" {
			return
		}

		// Deduplicated case-insensitively, because the same person written
		// two ways in two places is one person and two messages.
		key := strings.ToLower(address)
		if seen[key] {
			return
		}

		seen[key] = true
		out = append(out, recipient{address: address, userID: userID})
	}

	add(b.cfg.Office, types.ID{})

	for _, address := range f.Notify {
		add(address, types.ID{})
	}

	if b.cfg.Grants == nil || b.cfg.Accounts == nil {
		return out
	}

	grants, err := b.cfg.Grants.ForForm(ctx, f.ID)
	if err != nil {
		// The configured address and the form's own list still go out. Losing
		// the grant lookup should cost the notifications it names and not the
		// ones it does not.
		b.cfg.Log.Error("the accounts holding this form could not be read, so some notifications were not sent",
			"form", f.ID.String(), "error", err)

		return out
	}

	quiet := b.mutedFor(ctx, f.ID)

	for _, g := range grants {
		// Anything that can read the submissions, which is what the role
		// means. Asking through Includes rather than comparing to results is
		// what keeps "admin implies results" in one place.
		if !g.Role.Includes(accessbus.RoleResults) {
			continue
		}

		if quiet[g.UserID] {
			// They asked not to be told about this form. Nothing is logged:
			// this is the ordinary case for anybody who has used the link, and
			// a line per silenced recipient per submission is how a log stops
			// being read.
			continue
		}

		u, err := b.cfg.Accounts.ByID(ctx, g.UserID)
		if err != nil {
			b.cfg.Log.Error("an account holding this form could not be read, so it was not notified",
				"form", f.ID.String(), "user_id", g.UserID.String(), "error", err)

			continue
		}

		if !u.Enabled {
			// A disabled account is somebody who has left. Their grant may
			// well still be there -- revoking one is a separate action -- and
			// mail to them is mail going somewhere nobody reads.
			continue
		}

		add(u.Email.String(), u.ID)
	}

	return out
}

// mutedFor is the set of accounts that have turned this form off.
//
// One query for the whole form rather than one per grant holder: this runs
// inside the request that announces a submission, which on the paid path is
// the one Stripe is waiting on.
//
// A failure here returns an empty set, which means everybody is told. That is
// the safe direction by a wide margin: the cost of getting it wrong this way
// is somebody receiving an email they had switched off, and the cost of the
// other way is an order nobody hears about.
func (b *Business) mutedFor(ctx context.Context, form types.Slug) map[types.ID]bool {
	if b.cfg.Mutes == nil {
		return nil
	}

	ids, err := b.cfg.Mutes.MutedForForm(ctx, form)
	if err != nil {
		b.cfg.Log.Error("the notification preferences could not be read, so everybody who holds this form was told",
			"form", form.String(), "error", err)

		return nil
	}

	quiet := make(map[types.ID]bool, len(ids))
	for _, id := range ids {
		quiet[id] = true
	}

	return quiet
}

// Muted reports whether this account has turned email about this form off.
func (b *Business) Muted(ctx context.Context, userID types.ID, form types.Slug) (bool, error) {
	if b.cfg.Mutes == nil {
		return false, nil
	}

	muted, err := b.cfg.Mutes.Muted(ctx, userID, form)
	if err != nil {
		return false, fmt.Errorf("reading the notification preference: %w", err)
	}

	return muted, nil
}

// MutedForms lists the forms this account has turned email off for, so that a
// page listing forms can say which is which.
func (b *Business) MutedForms(ctx context.Context, userID types.ID) ([]types.Slug, error) {
	if b.cfg.Mutes == nil {
		return nil, nil
	}

	forms, err := b.cfg.Mutes.MutedForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("reading the notification preferences: %w", err)
	}

	return forms, nil
}

// MutedUsers lists the accounts that have turned email about one form off, so
// that the page managing who can see a form can say who hears about it.
//
// The mirror of [Business.MutedForms], and one call for a whole page rather
// than one per person, for the same reason [Business.mutedFor] is: a query per
// row is how a page that is quick with two people is slow with forty.
//
// Unlike mutedFor this returns the error rather than swallowing it. That one
// runs inside the request Stripe is waiting on, where the safe failure is to
// tell everybody; this one only decorates a listing, where the safe failure is
// to say nothing rather than to say something wrong about who is being
// emailed.
func (b *Business) MutedUsers(ctx context.Context, form types.Slug) ([]types.ID, error) {
	if b.cfg.Mutes == nil {
		return nil, nil
	}

	ids, err := b.cfg.Mutes.MutedForForm(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("reading the notification preferences: %w", err)
	}

	return ids, nil
}

// SetMuted turns email about one form off or on for one account.
//
// Both directions through one method, because the page that offers it is one
// page with one button whose label changes. Idempotent in both directions:
// asking to be told about something you are already told about is not a
// mistake worth reporting.
func (b *Business) SetMuted(ctx context.Context, now time.Time, userID types.ID, form types.Slug, muted bool) error {
	switch {
	case b.cfg.Mutes == nil:
		return errors.New("this installation has nowhere to record a notification preference")
	case userID.Zero():
		return errors.New("a notification preference needs an account")
	case form.Zero():
		return errors.New("a notification preference needs a form")
	}

	var err error

	if muted {
		err = b.cfg.Mutes.Mute(ctx, userID, form, now)
	} else {
		err = b.cfg.Mutes.Unmute(ctx, userID, form)
	}

	if err != nil {
		return fmt.Errorf("storing the notification preference: %w", err)
	}

	b.cfg.Log.Info("a notification preference changed",
		"user_id", userID.String(), "form", form.String(), "muted", muted)

	return nil
}

// ReadLink says whose preference an unsubscribe link names.
//
// The key stays in here rather than being handed to the app layer, so that
// there is one place that knows how these are signed and no handler holds a
// signing key it could mint with.
func (b *Business) ReadLink(token string) (types.ID, types.Slug, error) {
	return ReadMuteToken(b.cfg.MuteKey, token)
}

// CanUnsubscribe reports whether this installation can offer the choice at
// all, which needs both somewhere to record it and a key to sign the links.
func (b *Business) CanUnsubscribe() bool {
	return b.cfg.Mutes != nil && !b.cfg.MuteKey.Zero()
}

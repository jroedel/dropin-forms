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
// Nothing is sent for a pending submission. Somebody who has just been handed
// a payment page has not finished, and an email saying "we have your order"
// arriving while they are still typing their card number would either read as
// a receipt or as a reason to stop. The office sees pending rows in the
// management app, which is where a half-finished order belongs.
package notifybus

import (
	"context"
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

// Config is what this package needs.
type Config struct {
	Log         *slog.Logger
	Mail        mail.Sender
	Forms       Forms
	Submissions Submissions
	Grants      Grants
	Accounts    Accounts

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
		b.send(ctx, to, b.forOffice(f, sub, paid), "office", sub)
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

// office is everybody who should hear about a submission to this form, in one
// deduplicated list.
//
// Three sources, and they are three because each answers a different question.
// The configured address is "where does this installation's post go"; the
// definition's notify list is "who else cares about this particular form" --
// the kitchen, for a lunch -- and the results holders are "who has been given
// the job of reading these", which is the list that stays right when somebody
// leaves.
func (b *Business) office(ctx context.Context, f formbus.Form) []string {
	seen := make(map[string]bool)
	var out []string

	add := func(raw string) {
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
		out = append(out, address)
	}

	add(b.cfg.Office)

	for _, address := range f.Notify {
		add(address)
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

	for _, g := range grants {
		// Anything that can read the submissions, which is what the role
		// means. Asking through Includes rather than comparing to results is
		// what keeps "admin implies results" in one place.
		if !g.Role.Includes(accessbus.RoleResults) {
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

		add(u.Email.String())
	}

	return out
}

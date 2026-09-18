package notifybus

import (
	"fmt"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/foundation/mail"
)

// The messages are plain text and there is no HTML alternative.
//
// The same reasoning as the sign-in link: a message that survives every
// client, every screen reader and every spam filter is the one that has to be
// right, and a second rendering of the same facts is a second thing to keep in
// step. Bare newlines are fine -- foundation/mail turns them into CRLF, so the
// bodies below can be written the way they read.

// forSubmitter is the message to the person who filled the form in.
//
// It repeats what they sent, and that is the point of it rather than padding:
// a form in an iframe on somebody else's website leaves no trace in the
// browser afterwards, so this is the only copy of their own answers they will
// ever have. The confirmation text is the definition's own, so the words here
// are the words the page showed them.
func (b *Business) forSubmitter(f formbus.Form, sub submissionbus.Submission, paid bool) mail.Message {
	var body strings.Builder

	greeting := "We have your form."
	if paid {
		greeting = "Thank you -- your payment has gone through and we have your order."
	}

	body.WriteString(greeting)
	body.WriteString("\n\n")

	if note := strings.TrimSpace(f.Confirmation); note != "" {
		body.WriteString(note)
		body.WriteString("\n\n")
	}

	if lines := receipt(f, sub); lines != "" {
		heading := "What you ordered:\n\n"
		if paid {
			heading = "What you paid for:\n\n"
		}

		body.WriteString(heading)
		body.WriteString(lines)
		body.WriteString("\n")
	}

	if answers := answered(sub); answers != "" {
		body.WriteString("What you sent us:\n\n")
		body.WriteString(answers)
		body.WriteString("\n")
	}

	// Reply, rather than a link to anything. This address belongs to a
	// stranger who has no account here and never will, and the useful thing
	// they can do with a mistake is tell a person about it.
	body.WriteString("If anything above is wrong, reply to this message and we will put it right.\n")

	subject := f.Title + ": we have your form"
	if paid {
		subject = f.Title + ": your payment is confirmed"
	}

	return mail.Message{Subject: subject, Text: body.String()}
}

// forOffice is the message to whoever has to act on a submission.
//
// Everything needed to act is in the body, and the link is for the rest. A
// notification that says only "there is a new submission, go and look" is one
// that gets read on a phone in a car park and then forgotten.
func (b *Business) forOffice(f formbus.Form, sub submissionbus.Submission, paid bool, to recipient) mail.Message {
	var body strings.Builder

	who := sub.Email.String()
	if name, ok := sub.Answers.Field("name"); ok && name.Value() != "" {
		who = name.Value()
		if sub.Email.String() != "" {
			who += " <" + sub.Email.String() + ">"
		}
	}

	fmt.Fprintf(&body, "%s submitted %q on %s.\n\n",
		who, f.Title, b.when(sub.CreatedAt))

	if sub.Answers.Total > 0 {
		state := "not paid yet"
		if paid {
			state = "paid"
		}

		fmt.Fprintf(&body, "Total: %s (%s)\n\n", formbus.Show(sub.Answers.Total, f.Currency), state)
	}

	if lines := receipt(f, sub); lines != "" {
		body.WriteString(lines)
		body.WriteString("\n")
	}

	if answers := answered(sub); answers != "" {
		body.WriteString(answers)
		body.WriteString("\n")
	}

	if link := b.link(sub); link != "" {
		body.WriteString("Read it here, with everything else that has come in:\n")
		body.WriteString(link)
		body.WriteString("\n")
	}

	body.WriteString("\n")
	body.WriteString(b.why(f, to))

	subject := "New submission: " + f.Title
	if paid {
		subject = fmt.Sprintf("Payment received, %s: %s", formbus.Show(sub.Answers.Total, f.Currency), f.Title)
	}

	return mail.Message{Subject: subject, Text: body.String()}
}

// why is the last line of an office notification: what put this address on the
// list, and what to do about it.
//
// It is written for every recipient rather than only for the ones who can act,
// because "why am I getting this" is the question every notification email
// eventually provokes, and an address that cannot unsubscribe itself still
// needs to know who can.
func (b *Business) why(f formbus.Form, to recipient) string {
	const holds = "You are getting this because you can read submissions for this form."

	if to.userID.Zero() {
		// A configured address: mail.notify, or the form's own notify list.
		// There is nobody to unsubscribe -- it is a decision somebody made in
		// a file about an address rather than about themselves, and a link
		// here would let one person switch off a shared mailbox for everybody.
		return "This address is on the notification list for " + f.Title +
			". Whoever administers this service can change that.\n"
	}

	if !b.CanUnsubscribe() || b.cfg.AdminBaseURL == "" {
		return holds + " To stop, sign in and turn it off on the forms page.\n"
	}

	token, err := MintMuteToken(b.cfg.MuteKey, to.userID, f.ID)
	if err != nil {
		// Unreachable with a key and an account, both of which were checked
		// above. Logged rather than dropped silently, because the visible
		// symptom would be a notification that quietly stopped offering a way
		// out.
		b.cfg.Log.Error("an unsubscribe link could not be built",
			"form", f.ID.String(), "user_id", to.userID.String(), "error", err)

		return holds + " To stop, sign in and turn it off on the forms page.\n"
	}

	return holds + " To stop:\n" +
		b.cfg.AdminBaseURL + "/notifications/" + f.ID.String() + "?t=" + token + "\n"
}

// link is where the submission can be read in the management app, or empty.
func (b *Business) link(sub submissionbus.Submission) string {
	if b.cfg.AdminBaseURL == "" {
		return ""
	}

	return b.cfg.AdminBaseURL + "/forms/" + sub.Form.String() + "/submissions/" + sub.ID.String()
}

// when writes an instant the way the management app's own pages do, in the
// server's zone, so that a time in a notification and a time on the page it
// links to are the same time.
func (b *Business) when(t time.Time) string {
	return t.Local().Format("Monday 2 January 2006, 15:04")
}

// receipt is the priced lines, or empty for a form that sells nothing.
//
// Built from what was stored rather than re-derived, for the reason the
// confirmation page is: the numbers a person reads afterwards must be the ones
// that were charged, and there is exactly one record of those.
func receipt(f formbus.Form, sub submissionbus.Submission) string {
	if sub.Answers.Total == 0 {
		return ""
	}

	var b strings.Builder

	for _, l := range sub.Answers.Lines {
		fmt.Fprintf(&b, "  %d x %s -- %s\n", l.Qty, l.Label, formbus.Show(l.Amount, f.Currency))
	}

	// An amount field is not a priced line and is added into the total
	// separately -- a donation beside a ticket. Walking the answers for the
	// amounts as well is the same correction paybus.OrderFor makes, and for
	// the same reason: a receipt that lists the tickets and drops the gift is
	// a receipt that does not add up.
	for _, a := range sub.Answers.Fields {
		if a.Kind != formbus.KindAmount || a.Value() == "" {
			continue
		}

		fmt.Fprintf(&b, "  %s -- %s%s\n", a.Label, formbus.Symbol(f.Currency), a.Value())
	}

	fmt.Fprintf(&b, "  Total: %s\n", formbus.Show(sub.Answers.Total, f.Currency))

	return b.String()
}

// answered is every field that was filled in, in the definition's order.
//
// Only what was answered, because that is all the record holds: a field hidden
// by its condition or left blank is absent entirely rather than present and
// empty, which is the honest account of what somebody was asked and what they
// said.
func answered(sub submissionbus.Submission) string {
	var b strings.Builder

	for _, a := range sub.Answers.Fields {
		if len(a.Values) == 0 {
			continue
		}

		// Several values on one line for a set of checkboxes, and indented
		// under their label when the answer runs long -- a paragraph field
		// holds five hundred characters and reading it as one wrapped line
		// beside a label is unpleasant.
		value := strings.Join(a.Values, ", ")

		if strings.Contains(value, "\n") || len(value) > 60 {
			fmt.Fprintf(&b, "  %s:\n", a.Label)

			for line := range strings.SplitSeq(value, "\n") {
				fmt.Fprintf(&b, "    %s\n", line)
			}

			continue
		}

		fmt.Fprintf(&b, "  %s: %s\n", a.Label, value)
	}

	return b.String()
}

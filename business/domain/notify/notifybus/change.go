package notifybus

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/mail"
)

// The messages about answers somebody has changed through the link we sent
// them, and the message carrying that link again to somebody who has lost it.
//
// # Why Changed takes both submissions rather than an identifier
//
// Everything else here re-reads the submission it is told about, so that a
// notification is a statement about what is stored. A change is the one case
// where that cannot work: the point of the office's message is what the
// answers said *before*, and by the time this runs the stored row says only
// what they say now. So the caller, which has just had both from
// submissionbus.Change, hands both over.

func (b *Business) now() time.Time {
	if b.cfg.Now != nil {
		return b.cfg.Now()
	}

	return time.Now()
}

// Changed tells the person and the office that answers were changed.
//
// Three messages, to up to three kinds of people:
//   - the person, at the address the answers now give, with the answers as
//     they stand and the link again -- the same receipt a first answer gets;
//   - the address the answers gave before, when that changed, so that a link
//     forwarded to somebody else cannot quietly move a person's mail away from
//     them without their hearing about it;
//   - the office, with what changed, question by question, because "Fr. X
//     updated his form" is a message that sends somebody to go and look, and
//     "Arriving: 5 Feb, 14:20 (was: not answered)" is one they can act on.
func (b *Business) Changed(ctx context.Context, before, after submissionbus.Submission) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendBudget)
	defer cancel()

	f, err := b.cfg.Forms.ByID(after.Form)
	if err != nil {
		b.cfg.Log.Error("nobody could be told about a change, because its form could not be read",
			"submission_id", after.ID.String(), "form", after.Form.String(), "error", err)

		return
	}

	if to := after.Email.String(); to != "" {
		b.send(ctx, to, b.forChanged(f, after), "submitter", after)
	}

	if was := before.Email; !was.Zero() && was != after.Email {
		b.send(ctx, was.String(), b.forOldAddress(f, before, after), "previous address", after)
	}

	diff := changes(f, before.Answers, after.Answers)

	for _, to := range b.office(ctx, f) {
		b.send(ctx, to.address, b.forOfficeChange(f, after, diff, to), "office", after)
	}
}

// SendLinks mails somebody the links to every set of answers they could
// still change on a form, and does nothing at all if there are none.
//
// Nothing, rather than a message saying there is nothing: the page that asks
// for this takes any address anybody types, and a message to an address that
// never answered would make it a way to send our mail to strangers. The page
// says the same thing whichever happened.
func (b *Business) SendLinks(ctx context.Context, form types.Slug, email types.Email) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendBudget)
	defer cancel()

	f, err := b.cfg.Forms.ByID(form)
	if err != nil {
		b.cfg.Log.Error("links could not be sent, because the form could not be read",
			"form", form.String(), "error", err)

		return
	}

	subs, err := b.cfg.Submissions.Yours(ctx, form, email)
	if err != nil {
		b.cfg.Log.Error("links could not be sent, because the submissions could not be read",
			"form", form.String(), "error", err)

		return
	}

	var links []string

	for _, s := range subs {
		if link := b.answerLink(f, s); link != "" {
			links = append(links, fmt.Sprintf("Sent %s:\n%s\n", b.when(s.CreatedAt), link))
		}
	}

	if len(links) == 0 {
		b.cfg.Log.Info("a link was asked for and there was none to send", "form", form.String())

		return
	}

	m := b.forLinks(f, links)
	m.To = email.String()

	if err := b.cfg.Mail.Send(ctx, m); err != nil {
		b.cfg.Log.Error("links could not be sent", "form", form.String(), "error", err)

		return
	}

	b.cfg.Log.Info("links sent again", "form", form.String(), "count", len(links))
}

// forChanged is the person's own copy of their answers after a change.
func (b *Business) forChanged(f formbus.Form, sub submissionbus.Submission) mail.Message {
	var body strings.Builder

	body.WriteString("We have your changes.\n\n")

	if answers := answered(sub); answers != "" {
		body.WriteString("Your answers now:\n\n")
		body.WriteString(answers)
		body.WriteString("\n")
	}

	body.WriteString(b.wayBack(f, sub))

	return mail.Message{Subject: f.Title + ": your changes are saved", Text: body.String()}
}

// forOldAddress tells the address the answers used to give that they now
// give another.
//
// It carries the link, deliberately. This person had it already -- it is the
// same link, and it never changes -- and if the change was not theirs, the
// link is the quickest way to put their own address back.
func (b *Business) forOldAddress(f formbus.Form, before, after submissionbus.Submission) mail.Message {
	var body strings.Builder

	fmt.Fprintf(&body, "The email address on your answers to %q was changed to %s on %s,\n"+
		"so messages about them will go there from now on.\n\n",
		f.Title, after.Email.String(), b.when(after.UpdatedAt))

	body.WriteString("If that was you, there is nothing to do. If it was not, reply to this message and\n" +
		"we will sort it out.\n\n")

	body.WriteString(b.wayBack(f, before))

	return mail.Message{Subject: f.Title + ": your email address was changed", Text: body.String()}
}

// forOfficeChange is what the office is told about a change: who, when, and
// what is different.
func (b *Business) forOfficeChange(f formbus.Form, sub submissionbus.Submission, diff []string, to recipient) mail.Message {
	var body strings.Builder

	who := whoSent(sub)

	fmt.Fprintf(&body, "%s changed their answers to %q on %s.\n\n", who, f.Title, b.when(sub.UpdatedAt))

	body.WriteString("What changed:\n\n")
	for _, line := range diff {
		body.WriteString(line)
	}
	body.WriteString("\n")

	if answers := answered(sub); answers != "" {
		body.WriteString("Their answers now:\n\n")
		body.WriteString(answers)
		body.WriteString("\n")
	}

	if link := b.link(sub); link != "" {
		body.WriteString("Read it here, with what it said before:\n")
		body.WriteString(link)
		body.WriteString("\n")
	}

	body.WriteString("\n")
	body.WriteString(b.why(f, to))

	return mail.Message{Subject: "Changed: " + f.Title + " -- " + who, Text: body.String()}
}

// forLinks is the links again, for somebody who asked.
func (b *Business) forLinks(f formbus.Form, links []string) mail.Message {
	var body strings.Builder

	fmt.Fprintf(&body, "You asked for the link to your answers to %q.\n\n", f.Title)

	if len(links) > 1 {
		body.WriteString("This address sent the form more than once, so there is a link for each:\n\n")
	}

	for _, l := range links {
		body.WriteString(l)
		body.WriteString("\n")
	}

	fmt.Fprintf(&body, "Each works until %s. Keep them to yourself -- anyone with one can change\n"+
		"what you sent.\n\nIf you did not ask for this, you can ignore it; nothing has changed.\n",
		b.when(f.ChangeableUntil))

	return mail.Message{Subject: f.Title + ": your link", Text: body.String()}
}

// whoSent is how a submission's sender is named to the office: their name and
// address when the form asked for a name, else the address.
func whoSent(sub submissionbus.Submission) string {
	who := sub.Email.String()

	if name, ok := sub.Answers.Field("name"); ok && name.Value() != "" {
		who = name.Value()
		if sub.Email.String() != "" {
			who += " <" + sub.Email.String() + ">"
		}
	}

	if who == "" {
		return "Somebody"
	}

	return who
}

// changes is what is different between two sets of answers, one line per
// question, in the definition's order.
//
// Compared by stored values and written out readably, so a date answered the
// same way twice is no change however it is displayed. A question the
// definition no longer has is still reported if its answer changed -- the
// office should hear about it -- after every question it does have.
//
// Plain ASCII, as the rest of these messages are: "(was: ...)" rather than an
// arrow, because the message is read in every mail client there is.
func changes(f formbus.Form, before, after formbus.Answers) []string {
	var out []string

	line := func(label string, was, now formbus.Answer, hadWas, hadNow bool) {
		if hadWas == hadNow && slices.Equal(was.Values, now.Values) {
			return
		}

		say := func(a formbus.Answer, had bool) string {
			if !had || len(a.Values) == 0 {
				return "not answered"
			}

			return a.Readable()
		}

		out = append(out, fmt.Sprintf("  %s: %s (was: %s)\n", label, say(now, hadNow), say(was, hadWas)))
	}

	seen := make(map[string]bool, len(f.Fields))

	for _, fld := range f.Fields {
		seen[fld.Name] = true

		was, hadWas := before.Field(fld.Name)
		now, hadNow := after.Field(fld.Name)

		line(fld.Label, was, now, hadWas, hadNow)
	}

	for _, a := range before.Fields {
		if seen[a.Name] {
			continue
		}

		now, hadNow := after.Field(a.Name)
		line(a.Label, a, now, true, hadNow)
	}

	if len(out) == 0 {
		// Unreachable through submissionbus, which refuses a change that
		// changes nothing. Said rather than sent empty, should it ever be.
		out = append(out, "  Nothing that this form still asks.\n")
	}

	return out
}

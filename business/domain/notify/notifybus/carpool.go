package notifybus

import (
	"context"
	"strings"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/foundation/mail"
)

// Carpool tells the office something about a form's carpool that somebody has
// to act on -- carpoolbus decides what, and says it in says.
//
// The office here is the same list a submission goes to, and for the same
// reasons (see [Business.office]): the people who read this form's
// submissions are the ones organising its event, and anybody who has turned
// the form off has turned this off too. Organisers were once texted about the
// carpool instead, every placement, at a mobile number on their account; a
// stream of texts about a carpool that was working is noise, and the few
// things worth interrupting somebody for read as well in an email that links
// to the page where they can be dealt with.
func (b *Business) Carpool(ctx context.Context, f formbus.Form, subject, says string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendBudget)
	defer cancel()

	office := b.office(ctx, f)

	if len(office) == 0 {
		b.cfg.Log.Warn("there was something to tell the organisers about a carpool and nobody to tell; check mail.notify, the form's notify list, and who holds results on it",
			"form", f.ID.String())

		return
	}

	for _, to := range office {
		var body strings.Builder

		body.WriteString(strings.TrimRight(says, "\n"))
		body.WriteString("\n\n")

		if b.cfg.AdminBaseURL != "" {
			body.WriteString("The cars, and who is waiting:\n")
			body.WriteString(b.cfg.AdminBaseURL + "/forms/" + f.ID.String() + "/carpool\n\n")
		}

		body.WriteString(b.why(f, to))

		m := mail.Message{To: to.address, Subject: f.Title + " carpool: " + subject, Text: body.String()}

		if err := b.cfg.Mail.Send(ctx, m); err != nil {
			b.cfg.Log.Error("a carpool notification could not be sent", "form", f.ID.String(), "error", err)

			continue
		}

		b.cfg.Log.Info("carpool notification sent", "form", f.ID.String())
	}
}

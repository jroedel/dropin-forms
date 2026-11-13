package sheetbus

import (
	"context"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/foundation/mail"
)

// tell emails the person who connected, and nobody else: the sheet is theirs,
// and nobody else knows it exists.
//
// Fire and log, like every notification here, with a budget of its own: a
// relay that has stopped answering must not hold up the sync that noticed.
func (b *Business) tell(ctx context.Context, c Connection, subject, text string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	who, err := b.cfg.Accounts.ByID(ctx, c.User)
	if err != nil || who.Email.String() == "" {
		b.cfg.Log.Error("somebody could not be told about their Google Sheet, because their account could not be read",
			"connection", c.ID.String(), "error", err)

		return
	}

	title := c.Form.String()
	if f, err := b.cfg.Forms.ByID(c.Form); err == nil {
		title = f.Title
	}

	var body strings.Builder

	body.WriteString(text)
	body.WriteString("\n\n")

	if c.URL != "" {
		body.WriteString("The spreadsheet: " + c.URL + "\n")
	}

	if b.cfg.AdminBaseURL != "" {
		body.WriteString("Your sheet's page: " + b.cfg.AdminBaseURL + "/forms/" + c.Form.String() + "/sheet\n")
	}

	body.WriteString("\nYou are getting this because you connected a Google Sheet to these answers. Nobody else is told.\n")

	m := mail.Message{
		To:      who.Email.String(),
		Subject: title + ": " + subject,
		Text:    body.String(),
	}

	if err := b.cfg.Mail.Send(ctx, m); err != nil {
		b.cfg.Log.Error("a Google Sheet notice could not be sent", "connection", c.ID.String(), "error", err)

		return
	}

	b.cfg.Log.Info("Google Sheet notice sent", "connection", c.ID.String(), "subject", subject)
}

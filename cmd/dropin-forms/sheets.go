package main

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/jroedel/dropin-forms/app/domain/sheetapp"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/sheet/sheetbus"
	"github.com/jroedel/dropin-forms/business/domain/sheet/stores/sheetdb"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/foundation/google"
	"github.com/jroedel/dropin-forms/foundation/mail"
)

// newSheets builds the Google Sheets domain, or nil when Google is not
// configured -- the same shape newPayments has, for its reason.
func newSheets(cfg config, log *slog.Logger, db *sql.DB, forms *formbus.Business, subs *submissionbus.Business,
	access *accessbus.Business, users *userbus.Business, sender mail.Sender) (*sheetbus.Business, error) {
	g := cfg.Google
	if g.ClientID == "" {
		return nil, nil
	}

	client, err := google.New(google.Config{
		ClientID:     g.ClientID,
		ClientSecret: g.ClientSecret,
		RedirectURL:  cfg.Server.AdminBaseURL + sheetapp.CallbackPath,
	})
	if err != nil {
		return nil, err
	}

	return sheetbus.NewBusiness(sheetbus.Config{
		Log:          log,
		Store:        sheetdb.NewStore(db),
		Google:       client,
		Forms:        forms,
		Subs:         subs,
		Access:       access,
		Accounts:     users,
		Mail:         sender,
		SecretBytes:  g.tokenKey,
		Zone:         g.zone,
		ZoneName:     g.TimeZone,
		AdminBaseURL: cfg.Server.AdminBaseURL,
	})
}

// sheetsEvery is how often the Google Sheets are looked at. Two minutes:
// "kept up to date" should read as true to somebody who has just watched a
// registration arrive, and a pass where nothing changed asks Google nothing.
const sheetsEvery = 2 * time.Minute

// sheetWatch keeps the Google Sheets up to date, depositWatch's shape.
type sheetWatch struct {
	log    *slog.Logger
	sheets *sheetbus.Business
}

func (s sheetWatch) start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})

	if s.sheets == nil {
		close(done)
		return done
	}

	go func() {
		defer close(done)

		s.pass(ctx)

		ticker := time.NewTicker(sheetsEvery)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case <-ticker.C:
				s.pass(ctx)
			}
		}
	}()

	return done
}

// pass syncs every connection that is due, one at a time. A failure is the
// connection's own -- recorded on it, retried later, told to its owner when
// it lasts -- and does not stop the others.
func (s sheetWatch) pass(ctx context.Context) {
	now := time.Now()

	due, err := s.sheets.Due(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("the Google Sheets due could not be listed", "error", err,
				"consequence", "none are updated this time; the next look is in two minutes")
		}

		return
	}

	for _, c := range due {
		if ctx.Err() != nil {
			return
		}

		// Logged at debug only: sheetbus logs what went wrong, with what it
		// will do about it, and the ordinary failures are retried.
		if err := s.sheets.Pass(ctx, time.Now(), c.ID); err != nil {
			s.log.Debug("a Google Sheet pass did not complete", "connection", c.ID.String(), "error", err)
		}
	}
}

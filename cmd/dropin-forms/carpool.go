package main

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/carpool/carpoolbus"
	"github.com/jroedel/dropin-forms/business/domain/carpool/stores/carpooldb"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/notify/stores/notifydb"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/foundation/twilio"
)

// newCarpool builds the carpool domain. Always, unlike payments and sheets:
// a carpool with texting off still arranges its cars and shows them to the
// organisers, which is most of what it is for. Texting is the part that is
// switched by the configuration.
func newCarpool(cfg config, log *slog.Logger, db *sql.DB, forms *formbus.Business, subs *submissionbus.Business,
	access *accessbus.Business, users *userbus.Business) (*carpoolbus.Business, error) {
	c := carpoolbus.Config{
		Log:         log,
		Store:       carpooldb.NewStore(db),
		Submissions: subs,
		Forms:       forms,
		Grants:      access,
		Accounts:    users,
		Mutes:       notifydb.NewStore(db),
	}

	// Assigned only when there is one, so that a nil *twilio.Sender is not
	// an interface that reads as configured.
	if tw := cfg.Twilio; tw.AccountSID != "" {
		sender, err := twilio.New(twilio.Config{AccountSID: tw.AccountSID, AuthToken: tw.AuthToken, From: tw.From})
		if err != nil {
			return nil, err
		}

		c.Texter = sender
	}

	return carpoolbus.NewBusiness(c)
}

// carpoolEvery is how often every carpool is arranged. Thirty seconds: a
// rider who has just signed up should hear about their car while they still
// remember doing it, and a pass over forms whose cars have not changed reads
// two tables and sends nothing.
const carpoolEvery = 30 * time.Second

// carpoolWatch arranges the carpools, the other watches' shape.
type carpoolWatch struct {
	log     *slog.Logger
	forms   *formbus.Business
	carpool *carpoolbus.Business
}

func (c carpoolWatch) start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		defer close(done)

		c.pass(ctx)

		ticker := time.NewTicker(carpoolEvery)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case <-ticker.C:
				c.pass(ctx)
			}
		}
	}()

	return done
}

// pass arranges each form with a carpool. One that fails is logged and the
// rest still go: one form's database error must not leave another's riders
// without a car.
func (c carpoolWatch) pass(ctx context.Context) {
	for _, f := range c.forms.All() {
		if _, ok := f.CarpoolOf(); !ok {
			continue
		}

		if err := c.carpool.Arrange(ctx, f); err != nil && ctx.Err() == nil {
			c.log.Error("a carpool could not be arranged", "form", f.ID.String(), "error", err)
		}
	}
}

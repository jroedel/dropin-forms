package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/payment/paybus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
)

// abandonEvery is how often orders whose payment page has closed are marked
// abandoned.
//
// Every fifteen minutes, so an order reads as abandoned within a quarter of an
// hour of paybus.AbandonAfter. The sweep is one UPDATE that usually changes
// nothing, so running it often costs nothing; running it hourly would leave an
// order looking open for most of an hour after nobody could pay for it.
//
// A separate ticker from the housekeeping sweep rather than a fourth job on
// it. The two are on different clocks -- that one forgets rows nothing will
// read again, every six hours, and this one changes what a list says.
const abandonEvery = 15 * time.Minute

// abandonWatch marks orders abandoned once their payment page has closed.
//
// It exists because Stripe sends no webhook for a checkout somebody simply
// closed. A payment that fails produces an event and a paid one produces an
// event; a person who closes the tab produces nothing at all, so before this
// an order sat "pending" for ever, and people reading the list asked what it
// was waiting for.
type abandonWatch struct {
	log         *slog.Logger
	submissions *submissionbus.Business

	// payments is nil when Stripe is not configured. With no payment page
	// there is no deadline for one to pass, so there is nothing to sweep.
	payments *paybus.Business
}

// start runs the sweeps until ctx is done, and returns a channel closed when
// the last one has finished.
//
// The same shape as housekeeper.start, and the channel is there for the same
// reason: without it the process can return from Serve, close the database and
// leave a query in flight against it, which is harmless in effect and produces
// an alarming line in the log at exactly the moment somebody is reading the
// log to find out whether a deploy went cleanly.
func (a abandonWatch) start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})

	if a.payments == nil {
		close(done)
		return done
	}

	go func() {
		defer close(done)

		// Once at startup, before the first tick, because a deploy restarts
		// this process more often than some intervals and the first deploy of
		// this one has every order ever left pending to catch up on. Marking
		// an order twice is a no-op -- the UPDATE only touches what is still
		// open -- so a restart costs nothing.
		a.sweep(ctx)

		ticker := time.NewTicker(abandonEvery)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case <-ticker.C:
				a.sweep(ctx)
			}
		}
	}()

	return done
}

func (a abandonWatch) sweep(ctx context.Context) {
	if _, err := a.submissions.Abandon(ctx, time.Now(), paybus.AbandonAfter); err != nil {
		a.log.Error("the orders whose payment page has closed could not be marked abandoned", "error", err)
	}
}

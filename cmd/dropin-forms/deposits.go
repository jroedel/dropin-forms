package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/deposit/depositbus"
)

// depositsEvery is how often Stripe is asked for new deposits to the bank.
//
// Hourly. A deposit reaches the bank days after the payments in it, so an
// hour's lag in seeing it here costs nothing; what matters is that somebody
// reconciling the morning a deposit lands finds it already there. More often
// would spend Stripe's rate limit on a list that has not changed.
const depositsEvery = time.Hour

// depositWatch reads Stripe's deposits into the ledger the deposits page shows.
//
// Its own ticker rather than a job on the housekeeping sweep, for the reason
// abandonWatch gives: the sweep forgets rows every six hours, and this fetches
// from outside on a different clock.
type depositWatch struct {
	log      *slog.Logger
	deposits *depositbus.Business
}

// start runs the syncs until ctx is done, and returns a channel closed when
// the last one has finished -- housekeeper.start's shape, for its reason.
//
// With Stripe not configured there is nothing to read, and the channel is
// closed at once.
func (d depositWatch) start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})

	if d.deposits == nil {
		close(done)
		return done
	}

	go func() {
		defer close(done)

		// Once at startup, for abandonWatch's reason: a deploy restarts this
		// more often than the interval. A deposit already kept is skipped by
		// its identifier, so a restart costs one list request.
		d.sync(ctx)

		ticker := time.NewTicker(depositsEvery)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case <-ticker.C:
				d.sync(ctx)
			}
		}
	}()

	return done
}

// sync runs one and logs a failure. A failure does not stop the loop: the
// deposits not kept this time are still not kept next time, which is the
// retry.
func (d depositWatch) sync(ctx context.Context) {
	if err := d.deposits.Sync(ctx, time.Now()); err != nil && ctx.Err() == nil {
		d.log.Error("reading deposits from Stripe failed", "err", err,
			"consequence", "the deposits page shows what was read before; the next attempt is within the hour")
	}
}

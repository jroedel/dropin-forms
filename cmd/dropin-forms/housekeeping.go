package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/payment/paybus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
)

// housekeepingEvery is how often the three prunes run.
//
// Six hours, which is often enough that nothing accumulates for a day and rare
// enough that it is four log lines. None of the three is urgent: each forgets
// rows whose retention is already generous, and the cost of one being late is
// that a table is briefly larger than it needs to be.
const housekeepingEvery = 6 * time.Hour

// housekeeper forgets what nothing will ever read again.
//
// This exists because three Prune functions were written with the step that
// needed them and then never called, which the design document recorded as a
// known gap: submissionbus.PruneNonces, userbus.Prune and paybus.Forget all
// existed and nothing invoked any of them, so three tables grew without bound
// on a shared host with a disk quota.
//
// Each of the three decides its own retention, and deliberately so. How long a
// spent grant nonce has to be remembered is a property of how long a grant
// lives, which is formbus's business and not a number to re-derive here; the
// same goes for Stripe's three days of retries. What this type knows is when
// to ask, which is the one piece of that nobody else could own.
type housekeeper struct {
	log         *slog.Logger
	submissions *submissionbus.Business
	users       *userbus.Business

	// payments is nil when Stripe is not configured, exactly as it is
	// everywhere else in this binary. Nothing to forget, rather than a
	// special case.
	payments *paybus.Business
}

// start runs the sweeps until ctx is done, and returns a channel closed when
// the last one has finished.
//
// The channel is the point. Without it the process could return from Serve,
// close the database and leave a DELETE in flight against it -- which is
// harmless in effect and produces an alarming line in the log at exactly the
// moment somebody is reading the log to find out whether a deploy went
// cleanly.
func (h housekeeper) start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		defer close(done)

		// Once at startup, before the first tick. A process that is restarted
		// more often than the interval -- which is what a deploy does -- would
		// otherwise never sweep at all.
		h.sweep(ctx)

		ticker := time.NewTicker(housekeepingEvery)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case <-ticker.C:
				h.sweep(ctx)
			}
		}
	}()

	return done
}

// sweep runs all three, and lets each one fail on its own.
//
// No early return between them: these are three unrelated tables, and a
// failure to prune one says nothing about the others. Stopping at the first
// would mean a single stubborn error quietly switching off the housekeeping
// for everything after it in the list.
func (h housekeeper) sweep(ctx context.Context) {
	now := time.Now()
	started := now

	if err := h.submissions.PruneNonces(ctx, now); err != nil {
		h.log.Error("the spent submission grants could not be pruned", "error", err)
	}

	if err := h.users.Prune(ctx, now); err != nil {
		h.log.Error("the expired sign-in credentials could not be pruned", "error", err)
	}

	if h.payments != nil {
		if err := h.payments.Forget(ctx, now); err != nil {
			h.log.Error("the handled payment notifications could not be pruned", "error", err)
		}
	}

	h.log.Info("housekeeping done", "ms", time.Since(started).Milliseconds())
}

package main

import (
	"context"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/notify/notifybus"
)

// unpaidEvery is how often the office is told about orders that were started
// and never paid for.
//
// Hourly, which with notifybus's own grace period means an abandoned order is
// reported between one and two hours after it was started. That is the useful
// range: soon enough that a payment step which has stopped working is noticed
// the same morning, and not so soon that somebody who is still finding their
// card is counted as gone.
//
// A separate ticker from the housekeeping sweep rather than a fourth job on
// it. The two are different kinds of work -- one forgets rows nothing will
// read again, the other sends mail to people -- and folding this into a
// six-hourly prune would mean either reporting late or pruning five times more
// often than anything needs.
const unpaidEvery = time.Hour

// unpaidWatch reports orders that were started and never paid for.
//
// It exists because Stripe sends no webhook for a checkout somebody simply
// closed. A payment that fails produces an event and a paid one produces an
// event; a person who closes the tab produces nothing at all, so before this
// an abandoned order sat in the management app until somebody thought to look.
// That is a quiet failure mode by nature: a payment step that has broken
// entirely looks exactly like an afternoon when nobody bought anything.
type unpaidWatch struct {
	notify *notifybus.Business
}

// start runs the sweeps until ctx is done, and returns a channel closed when
// the last one has finished.
//
// The same shape as housekeeper.start, and the channel is there for the same
// reason: without it the process can return from Serve, close the database and
// leave a query in flight against it, which is harmless in effect and produces
// an alarming line in the log at exactly the moment somebody is reading the
// log to find out whether a deploy went cleanly.
func (u unpaidWatch) start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		defer close(done)

		// Once at startup, before the first tick, because a deploy restarts
		// this process more often than the interval and a sweep that only ever
		// runs on the hour would never run at all. Reporting the same order
		// twice is what the ledger in notifydb prevents, so a restart costs
		// nothing.
		u.notify.ReportUnpaid(ctx, time.Now())

		ticker := time.NewTicker(unpaidEvery)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case <-ticker.C:
				u.notify.ReportUnpaid(ctx, time.Now())
			}
		}
	}()

	return done
}

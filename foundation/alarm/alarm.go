// Package alarm counts how often something happens and says when it has
// happened too often.
//
// It is not a rate limiter and must not be used as one. A limiter decides
// whether to refuse the request in front of it; this decides whether somebody
// should be told, and the thing it is watching goes ahead either way. The two
// are separate on purpose: the traffic worth an alarm here -- a form taking
// submissions faster than a parish lunch could possibly sell out, a run of
// declined cards -- is traffic that might also be a real rush, and a service
// that stops selling tickets because the tickets are selling is worse than the
// problem.
//
// So the answer is a log line, and the reason it is one line rather than one
// per event is that an alarm which fires a thousand times is an alarm nobody
// reads. [Ceiling.Count] returns true exactly once per window: on the event
// that crosses it.
//
// Nothing here knows a domain word. The caller supplies the key and writes the
// sentence, because what a count means -- and what somebody should do about it
// -- is knowledge this package deliberately does not have.
package alarm

import (
	"sync"
	"time"
)

// Ceiling is a count per key per window, and a line drawn across it.
//
// The windows are fixed rather than rolling: a key's window begins with its
// first event and lasts for the whole period, and the count resets when the
// next event arrives after it has elapsed. A rolling window would need every
// timestamp kept, and the difference between the two is whether a burst
// straddling a boundary is noticed now or a few minutes later -- which does
// not matter to something whose whole output is a log line.
type Ceiling struct {
	limit  int
	period time.Duration

	mu     sync.Mutex
	counts map[string]*tally
}

type tally struct {
	n     int
	since time.Time

	// fired records that this window has already produced its one line, so
	// that everything after the crossing is counted silently.
	fired bool
}

// NewCeiling constructs one: limit events per period, per key.
//
// A limit of zero or a period of zero switches it off, which is how a caller
// keeps the call site and stops the alarm.
func NewCeiling(limit int, period time.Duration) *Ceiling {
	return &Ceiling{
		limit:  limit,
		period: period,
		counts: make(map[string]*tally),
	}
}

// Count records one event and reports whether it is the one that crossed the
// ceiling, along with how many there have now been in this window.
//
// True at most once per key per window. A caller that logs whenever this is
// true gets one line per window however far past the ceiling the count goes,
// and the count in that line says how far.
func (c *Ceiling) Count(key string, now time.Time) (bool, int) {
	if c.limit <= 0 || c.period <= 0 {
		return false, 0
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.sweep(now)

	t, ok := c.counts[key]
	if !ok || now.Sub(t.since) >= c.period {
		t = &tally{since: now}
		c.counts[key] = t
	}

	t.n++

	if t.n <= c.limit || t.fired {
		return false, t.n
	}

	t.fired = true

	return true, t.n
}

// Len reports how many windows are being held, for a test that wants to assert
// they do not accumulate.
func (c *Ceiling) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.counts)
}

// sweep forgets windows that have elapsed, and is called with the lock held.
//
// Cheap because the keys here are things this service knows about -- a form
// name, a currency, the word "declines" -- rather than anything a stranger
// chooses. A caller keying a ceiling by something attacker-supplied should use
// a limiter instead, which bounds its own map because it has to.
func (c *Ceiling) sweep(now time.Time) {
	for key, t := range c.counts {
		if now.Sub(t.since) >= c.period {
			delete(c.counts, key)
		}
	}
}

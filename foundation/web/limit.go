package web

import (
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- who is asking ------------------------------------------------------------

// ClientIP is the visitor's address, as well as this process can know it.
//
// trustProxy has to be configured rather than sniffed. This service listens on
// the loopback behind Apache, so the socket's own address is always 127.0.0.1
// and the visitor's is only in a header -- and a header is exactly as
// trustworthy as whatever is in front of it. Believing one with nothing in
// front means letting a stranger choose both the address that goes to Stripe's
// fraud checks and the bucket their requests are counted in, which is worse
// than having no address at all.
//
// The leftmost X-Forwarded-For entry is the original client and everything
// after it was added by a proxy. Reading it that way is only meaningful
// because trustProxy asserts that what is in front overwrites rather than
// appends.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			first, _, _ := strings.Cut(fwd, ",")

			return strings.TrimSpace(first)
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}

// IPBucket narrows an address to what a rate limit should count.
//
// A /64 for IPv6, the address itself for IPv4. That is not a rounding: a
// single residential IPv6 allocation is a /64 at its smallest and frequently a
// /56 or /48, so counting IPv6 per address is counting one household as
// eighteen quintillion strangers -- a limit keyed that way is not a limit. IPv4
// is the opposite case, where a whole office can share one address, so it is
// left alone rather than widened further.
//
// Anything unparseable is returned as it arrived. It is then its own bucket,
// which is the safe direction: a limit that counts something strange too
// narrowly refuses too little, and a key that cannot be parsed is not a key
// anybody can aim.
func IPBucket(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}

	if addr.Is4() || addr.Is4In6() {
		return addr.Unmap().String()
	}

	prefix, err := addr.Prefix(64)
	if err != nil {
		return ip
	}

	return prefix.String()
}

// --- token buckets ------------------------------------------------------------

// Rate is how often something may happen: Burst of them at once, and then one
// more every Every.
//
// Written as a burst and an interval rather than as "n per minute" because the
// two numbers answer different questions and a single figure hides one of
// them. The burst is how much of a hurry a legitimate person may be in -- a
// mistyped form corrected three times in ten seconds -- and the interval is
// what a machine is held to once the burst is gone.
type Rate struct {
	Burst int
	Every time.Duration
}

// Zero reports whether this rate says nothing, in which case nothing is
// limited. A zero Rate is how a caller switches a throttle off without
// removing it from a chain.
func (r Rate) Zero() bool { return r.Burst <= 0 || r.Every <= 0 }

// Limiter is a set of token buckets, one per key.
//
// Fifty lines and no dependency, which is what the design document budgeted
// for it. golang.org/x/time/rate is the obvious alternative and it solves a
// harder problem than this one: it has no eviction, so a limiter keyed by a
// stranger's IP address needs the map management written anyway, which is most
// of what is here.
//
// # What keeps the map from being the attack
//
// A limiter keyed by something an attacker chooses is a memory leak with a
// rate limit attached. Two things bound it. A bucket that has refilled to full
// is indistinguishable from one that has never been used, so it is deleted;
// that alone bounds the map at however many distinct keys arrive within one
// full refill. And if a flood of distinct keys outruns that, the
// least-recently-seen entries are evicted down to a ceiling -- which does
// weaken the limit for whoever is evicted, and is the right direction: the
// alternative is a process that runs out of memory, which refuses everybody.
type Limiter struct {
	rate    Rate
	maxKeys int

	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

// bucket is one key's allowance. tokens is fractional because the refill is
// continuous: a bucket holding two thirds of a token has to remember it, or a
// caller polling faster than the refill interval would never accumulate one.
type bucket struct {
	tokens float64
	seen   time.Time
}

// maxKeys is the default ceiling on how many keys one limiter tracks.
//
// Sized for what this service is: a form on a parish website. Twenty thousand
// distinct addresses in one refill window is already far outside anything this
// site sees, and the entries cost about a hundred bytes each.
const maxKeys = 20_000

// sweepEvery is how often full buckets are collected. On the sweep rather than
// on a timer, because a limiter with no traffic has nothing to collect and a
// goroutine per limiter would have to be stopped by somebody.
const sweepEvery = time.Minute

// NewLimiter constructs one.
func NewLimiter(rate Rate) *Limiter {
	return &Limiter{
		rate:    rate,
		maxKeys: maxKeys,
		buckets: make(map[string]*bucket),
	}
}

// Allow spends a token for key and reports whether there was one.
//
// The duration is how long until there will be, which is what a Retry-After
// header wants. It is zero when the answer is yes.
//
// now is passed in rather than read here. Everything else in this service that
// depends on the clock takes it as an argument for the same reason: a test of a
// rate limit should not take a minute to run.
func (l *Limiter) Allow(key string, now time.Time) (bool, time.Duration) {
	if l.rate.Zero() || key == "" {
		return true, 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)

	capacity := float64(l.rate.Burst)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: capacity}
		l.buckets[key] = b
	}

	// Refilled from how long it has been rather than by a ticker: one token
	// per Every, continuously, capped at the burst. math.Min rather than a
	// branch because the elapsed time can be enormous -- a bucket last seen
	// last week -- and the arithmetic has to saturate rather than overflow.
	elapsed := now.Sub(b.seen)
	if elapsed > 0 && !b.seen.IsZero() {
		b.tokens = math.Min(capacity, b.tokens+elapsed.Seconds()/l.rate.Every.Seconds())
	}

	b.seen = now

	if b.tokens < 1 {
		// Rounded up: a Retry-After of zero invites an immediate retry that
		// will also be refused, and a client that honours the header should be
		// able to trust that waiting it out works.
		wait := time.Duration((1 - b.tokens) * float64(l.rate.Every))

		return false, wait.Round(time.Second) + time.Second
	}

	b.tokens--

	return true, 0
}

// sweep forgets buckets that no longer say anything, and is called with the
// lock held.
func (l *Limiter) sweep(now time.Time) {
	overfull := len(l.buckets) > l.maxKeys

	if !overfull && now.Sub(l.swept) < sweepEvery {
		return
	}

	l.swept = now

	// A bucket at capacity is exactly what a key that has never been seen
	// would get, so deleting it changes no answer. This is the collection that
	// does the work in ordinary running.
	capacity := float64(l.rate.Burst)

	for key, b := range l.buckets {
		refilled := b.tokens + now.Sub(b.seen).Seconds()/l.rate.Every.Seconds()
		if refilled >= capacity {
			delete(l.buckets, key)
		}
	}

	if len(l.buckets) <= l.maxKeys {
		return
	}

	// Still over the ceiling, which means distinct keys are arriving faster
	// than they refill. The oldest go, because the newest are the ones a limit
	// is currently being applied to.
	keys := make([]string, 0, len(l.buckets))
	for key := range l.buckets {
		keys = append(keys, key)
	}

	slices.SortFunc(keys, func(a, b string) int {
		return l.buckets[a].seen.Compare(l.buckets[b].seen)
	})

	for _, key := range keys[:len(l.buckets)-l.maxKeys] {
		delete(l.buckets, key)
	}
}

// Len reports how many keys are being tracked, for a test that wants to assert
// the map does not grow without bound.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.buckets)
}

// --- the middleware -----------------------------------------------------------

// KeyFor decides which bucket a request is counted in.
//
// An empty string means this request is not counted at all, which is how a
// throttle is applied to one method or one route from a chain that carries
// every request.
type KeyFor func(*http.Request) string

// Throttling is one throttle: how fast, counted how.
type Throttling struct {
	Rate Rate
	Key  KeyFor

	// Log is where a refusal is recorded. One line per refused request, which
	// is deliberate even though a flood produces a flood of lines: the count
	// is the signal, and a rate limiter that hides how often it fires is a
	// setting nobody can tune.
	Log *slog.Logger

	// Refuse answers a request that has run out of tokens. Nil gets a plain
	// 429 with a sentence, which is right for a machine and wrong for a page
	// somebody is looking at -- so a surface with templates passes its own.
	//
	// Retry-After is already set when this is called, so a handler that only
	// wants to change the wording does not have to know about the header.
	Refuse http.Handler

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Throttle refuses a request that is arriving too often.
//
// It is a middleware rather than a check inside a handler because the whole
// value of it is that the handler does not run: the reason to limit the form
// POST is that the handler behind it creates objects at Stripe.
func Throttle(t Throttling) Middleware {
	if t.Rate.Zero() || t.Key == nil {
		// Nothing to enforce. Returned as a pass-through rather than as nil so
		// that a caller can build a chain without checking, and so that
		// switching a throttle off is one zero value rather than a change to
		// the chain.
		return func(next http.Handler) http.Handler { return next }
	}

	limiter := NewLimiter(t.Rate)

	now := t.Now
	if now == nil {
		now = time.Now
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := t.Key(r)

			ok, wait := limiter.Allow(key, now())
			if ok {
				next.ServeHTTP(w, r)

				return
			}

			if t.Log != nil {
				t.Log.InfoContext(r.Context(), "throttled",
					"id", RequestIDFrom(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"key", key,
					"retry_after_s", int(wait.Seconds()),
				)
			}

			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))

			if t.Refuse != nil {
				t.Refuse.ServeHTTP(w, r)

				return
			}

			http.Error(w,
				fmt.Sprintf("That came through faster than we can take it. Please wait %s and try again.", inWords(wait)),
				http.StatusTooManyRequests)
		})
	}
}

// inWords writes a wait as something a person would say, because this sentence
// is read by whoever is being refused and "wait 90s" is not English.
func inWords(d time.Duration) string {
	switch seconds := int(d.Seconds()); {
	case seconds <= 1:
		return "a moment"
	case seconds < 60:
		return fmt.Sprintf("%d seconds", seconds)
	case seconds < 120:
		return "a minute"
	default:
		return fmt.Sprintf("%d minutes", seconds/60)
	}
}

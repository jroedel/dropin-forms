package web_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/foundation/web"
)

var noon = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- the buckets -------------------------------------------------------------

func TestABurstIsSpentAndThenRefused(t *testing.T) {
	l := web.NewLimiter(web.Rate{Burst: 3, Every: 10 * time.Second})

	for i := 1; i <= 3; i++ {
		if ok, _ := l.Allow("one", noon); !ok {
			t.Fatalf("request %d of a burst of 3 was refused", i)
		}
	}

	ok, wait := l.Allow("one", noon)
	if ok {
		t.Fatal("the fourth request inside one instant was allowed")
	}

	// Rounded up, because a Retry-After that expires a moment too early is a
	// client that retries and is refused again.
	if wait < 10*time.Second {
		t.Errorf("Retry-After is %s, want at least the refill interval", wait)
	}
}

func TestTokensComeBackWithTime(t *testing.T) {
	l := web.NewLimiter(web.Rate{Burst: 2, Every: 10 * time.Second})

	l.Allow("one", noon)
	l.Allow("one", noon)

	if ok, _ := l.Allow("one", noon.Add(9*time.Second)); ok {
		t.Error("a token arrived before the interval was up")
	}

	if ok, _ := l.Allow("one", noon.Add(11*time.Second)); !ok {
		t.Error("no token after the interval, so the bucket never refills")
	}

	// And never more than the burst, however long it has been.
	for i := 1; i <= 2; i++ {
		if ok, _ := l.Allow("one", noon.Add(time.Hour)); !ok {
			t.Fatalf("request %d after an hour was refused", i)
		}
	}
	if ok, _ := l.Allow("one", noon.Add(time.Hour)); ok {
		t.Error("an hour of idleness produced more than a burst's worth of tokens")
	}
}

func TestOneKeyCannotSpendAnothers(t *testing.T) {
	l := web.NewLimiter(web.Rate{Burst: 1, Every: time.Minute})

	l.Allow("one", noon)

	if ok, _ := l.Allow("two", noon); !ok {
		t.Fatal("a second key was refused on the first key's spending")
	}
}

// A zero rate is how a caller switches a limit off without taking it out of a
// chain, so it must allow rather than refuse.
func TestAZeroRateLimitsNothing(t *testing.T) {
	for name, rate := range map[string]web.Rate{
		"no burst":    {Burst: 0, Every: time.Minute},
		"no interval": {Burst: 5, Every: 0},
	} {
		t.Run(name, func(t *testing.T) {
			l := web.NewLimiter(rate)

			for range 100 {
				if ok, _ := l.Allow("one", noon); !ok {
					t.Fatal("a switched-off limiter refused a request")
				}
			}
		})
	}
}

// An empty key is how a throttle covering a whole chain says "not this
// request" -- a GET under a limit that only counts writes.
func TestAnEmptyKeyIsNotCounted(t *testing.T) {
	l := web.NewLimiter(web.Rate{Burst: 1, Every: time.Hour})

	for range 10 {
		if ok, _ := l.Allow("", noon); !ok {
			t.Fatal("an uncounted request was refused")
		}
	}
}

// The map is keyed by something a stranger chooses, so the thing worth
// asserting is that it does not keep what it no longer needs.
func TestRefilledBucketsAreForgotten(t *testing.T) {
	l := web.NewLimiter(web.Rate{Burst: 2, Every: time.Second})

	for i := range 500 {
		l.Allow(string(rune('a'+i%26))+strings.Repeat("x", i%7)+string(rune('0'+i%10)), noon)
	}

	if l.Len() == 0 {
		t.Fatal("nothing was tracked at all, so this asserts nothing")
	}

	// A minute later every one of those has refilled to full, which makes it
	// indistinguishable from a key never seen. One more call sweeps them.
	l.Allow("last", noon.Add(time.Minute))

	if got := l.Len(); got != 1 {
		t.Errorf("%d buckets are still held after they all refilled, want 1", got)
	}
}

// --- who is asking -----------------------------------------------------------

func TestTheForwardedHeaderIsBelievedOnlyWhenSomethingIsInFront(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/f/x", nil)
	r.RemoteAddr = "127.0.0.1:5001"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")

	if got := web.ClientIP(r, true); got != "203.0.113.9" {
		t.Errorf("behind a proxy the address is %q, want the leftmost entry", got)
	}

	// With nothing in front, the header is a string a stranger sent us.
	if got := web.ClientIP(r, false); got != "127.0.0.1" {
		t.Errorf("with no proxy the address is %q, want the socket's own", got)
	}
}

func TestIPv6IsBucketedByItsPrefix(t *testing.T) {
	tests := map[string]string{
		"203.0.113.9":                            "203.0.113.9",
		"::ffff:203.0.113.9":                     "203.0.113.9",
		"2001:db8:1234:5678:abcd:ef01:2345:6789": "2001:db8:1234:5678::/64",
		"2001:db8:1234:5678:0000:0000:0000:0001": "2001:db8:1234:5678::/64",
		"not an address":                         "not an address",
		"":                                       "",
	}

	for in, want := range tests {
		if got := web.IPBucket(in); got != want {
			t.Errorf("IPBucket(%q) = %q, want %q", in, got, want)
		}
	}

	// The point of the /64: two addresses out of one household's allocation
	// have to land in one bucket, or an IPv6 visitor has as many allowances as
	// they care to use.
	a := web.IPBucket("2001:db8:1234:5678:1::1")
	b := web.IPBucket("2001:db8:1234:5678:2::2")

	if a != b {
		t.Errorf("two addresses in one /64 bucket apart: %q and %q", a, b)
	}
}

// --- the middleware -----------------------------------------------------------

func TestThrottleRefusesWithRetryAfterAndStopsTheHandler(t *testing.T) {
	var reached int

	h := web.Throttle(web.Throttling{
		Rate: web.Rate{Burst: 2, Every: 30 * time.Second},
		Key:  func(*http.Request) string { return "one" },
		Log:  discard(),
		Now:  func() time.Time { return noon },
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	}))

	for i := 1; i <= 2; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/f/x", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("request %d of the burst = %d", i, w.Code)
		}
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/f/x", nil))

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("the third request = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("a 429 with no Retry-After leaves a client guessing")
	}

	// The reason this is a middleware and not a check inside the handler: the
	// handler is what creates objects at Stripe, and it must not run.
	if reached != 2 {
		t.Errorf("the handler ran %d times, want 2", reached)
	}
}

func TestAThrottleWithNothingToEnforceIsAPassThrough(t *testing.T) {
	for name, tr := range map[string]web.Throttling{
		"no rate": {Key: func(*http.Request) string { return "one" }},
		"no key":  {Rate: web.Rate{Burst: 1, Every: time.Hour}},
	} {
		t.Run(name, func(t *testing.T) {
			h := web.Throttle(tr)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))

			for range 5 {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/f/x", nil))

				if w.Code != http.StatusOK {
					t.Fatalf("got %d, want everything through", w.Code)
				}
			}
		})
	}
}

// --- what a write may be -------------------------------------------------------

func TestOnlyAFormEncodedBodyIsAccepted(t *testing.T) {
	h := web.FormEncodedOnly()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name   string
		method string
		kind   string
		want   int
	}{
		{"a form", http.MethodPost, "application/x-www-form-urlencoded", http.StatusOK},
		{"a form with a charset", http.MethodPost, "application/x-www-form-urlencoded; charset=utf-8", http.StatusOK},
		{"the type in capitals", http.MethodPost, "Application/X-WWW-Form-Urlencoded", http.StatusOK},
		{"multipart", http.MethodPost, "multipart/form-data; boundary=x", http.StatusUnsupportedMediaType},
		{"json", http.MethodPost, "application/json", http.StatusUnsupportedMediaType},
		{"nothing at all", http.MethodPost, "", http.StatusUnsupportedMediaType},
		{"a type with the right prefix", http.MethodPost, "application/x-www-form-urlencoded-ish", http.StatusUnsupportedMediaType},
		{"unparseable", http.MethodPost, "application/x-www-form-urlencoded; =", http.StatusUnsupportedMediaType},

		// A GET has no body to refuse, and a Content-Type on one means
		// nothing.
		{"a GET with a strange type", http.MethodGet, "application/json", http.StatusOK},
		{"a GET with none", http.MethodGet, "", http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/f/x", strings.NewReader("a=b"))
			if tc.kind != "" {
				r.Header.Set("Content-Type", tc.kind)
			}

			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != tc.want {
				t.Errorf("%s = %d, want %d", tc.kind, w.Code, tc.want)
			}
		})
	}
}

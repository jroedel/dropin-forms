package muxer_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// The abuse controls of docs/design/drop-in-forms.md section 7.6, through the
// mounted surfaces.
//
// They are tested here rather than against the handlers because what is worth
// asserting about a throttle is that the handler behind it does not run: the
// reason the form POST is limited at all is that it creates objects at Stripe,
// and a limiter the handler runs behind protects nothing.

// throttled builds the embed surface with a submit allowance small enough to
// exhaust in a test, and the clock stopped before the feast.
func throttled(t *testing.T, submit web.Rate) http.Handler {
	t.Helper()

	cfg := newConfig(t, nil, nil)
	cfg.Embed.Now = func() time.Time { return beforeTheFeast }
	cfg.Embed.Limits = embedapp.Limits{Submit: submit}

	return embedOf(t, cfg)
}

// postFrom is a submission from a named address, so that a test can assert one
// visitor's spending is their own.
func postFrom(t *testing.T, h http.Handler, from string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, "/f/"+theForm, strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", web.FormEncoded)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.RemoteAddr = from

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	return w
}

func TestASubmissionFloodIsRefusedOnceTheBurstIsGone(t *testing.T) {
	h := throttled(t, web.Rate{Burst: 3, Every: time.Minute})

	// The bodies are deliberately not valid submissions. A throttled request
	// is refused before anything looks at it, so what these assert is the
	// count and nothing else.
	for i := 1; i <= 3; i++ {
		if w := postFrom(t, h, "198.51.100.7:4000", url.Values{"name": {"x"}}); w.Code == http.StatusTooManyRequests {
			t.Fatalf("submission %d of a burst of 3 was throttled", i)
		}
	}

	w := postFrom(t, h, "198.51.100.7:4000", url.Values{"name": {"x"}})

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("the fourth submission = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("the refusal carries no Retry-After, so nothing knows when to come back")
	}
}

// Keyed by address as well as by form, which is what stops one stranger
// emptying the bucket the next real buyer needs.
func TestOneVisitorCannotSpendAnothersAllowance(t *testing.T) {
	h := throttled(t, web.Rate{Burst: 1, Every: time.Minute})

	postFrom(t, h, "198.51.100.7:4000", url.Values{"name": {"x"}})

	if w := postFrom(t, h, "198.51.100.7:4000", url.Values{"name": {"x"}}); w.Code != http.StatusTooManyRequests {
		t.Fatalf("the same address twice = %d, want the second refused", w.Code)
	}

	if w := postFrom(t, h, "203.0.113.22:4000", url.Values{"name": {"x"}}); w.Code == http.StatusTooManyRequests {
		t.Error("a second visitor was refused on the first one's spending")
	}
}

// Reading is not writing. Somebody who has just been throttled out of
// submitting can still be shown the form, which is what they need in order to
// read the sentence telling them to wait.
func TestReadingAFormIsNotHeldToTheSubmitAllowance(t *testing.T) {
	h := throttled(t, web.Rate{Burst: 1, Every: time.Hour})

	postFrom(t, h, "198.51.100.7:4000", url.Values{"name": {"x"}})
	postFrom(t, h, "198.51.100.7:4000", url.Values{"name": {"x"}})

	if w := getPage(t, h, "/f/"+theForm); w.Code != http.StatusOK {
		t.Errorf("GET a form after being throttled out of posting = %d, want 200", w.Code)
	}
}

// --- what a body may be ---------------------------------------------------------

// The allowlist of one, and the route that must not be behind it.
func TestOnlyAFormEncodedSubmissionReachesTheForm(t *testing.T) {
	h, _ := embedSurface(t)

	for name, kind := range map[string]string{
		"multipart":    "multipart/form-data; boundary=zzz",
		"json":         "application/json",
		"nothing":      "",
		"plain text":   "text/plain",
		"a near match": "application/x-www-form-urlencoded-ish",
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/f/"+theForm, strings.NewReader("name=x"))
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			if kind != "" {
				r.Header.Set("Content-Type", kind)
			}

			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != http.StatusUnsupportedMediaType {
				t.Errorf("a %s body = %d, want 415", name, w.Code)
			}
		})
	}
}

// The webhook shares this listener and posts JSON, which is the whole reason
// the content-type gate is handed to embedapp for its own routes instead of
// being put on the chain. A 415 here would silently stop every payment being
// confirmed.
func TestTheWebhookIsNotBehindTheFormBodyRules(t *testing.T) {
	h, _ := embedSurface(t)

	r := httptest.NewRequest(http.MethodPost, "/stripe/webhook", strings.NewReader(`{"id":"evt_1"}`))
	r.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code == http.StatusUnsupportedMediaType {
		t.Fatal("the webhook was refused for posting JSON, which is the only thing Stripe posts")
	}
	if w.Code == http.StatusTooManyRequests {
		t.Fatal("the webhook is behind a throttle, so a retry storm from Stripe would drop payments")
	}
}

// --- the form's own daily cap ------------------------------------------------

// cappedForms is the real definitions with a cap written over them.
//
// Written over rather than loaded from a fixture, and that is an assertion in
// itself: the cap is deliberately outside the fingerprint, so a form whose cap
// has just changed still honours the grants already in people's browsers. If
// it were hashed, the submission below would be refused as stale and this test
// would fail for the wrong reason.
type cappedForms struct {
	inner embedapp.Forms
	cap   int
}

func (c cappedForms) ByID(slug types.Slug) (formbus.Form, error) {
	f, err := c.inner.ByID(slug)
	f.DailyCap = c.cap

	return f, err
}

func TestAFormAtItsDailyCapTurnsSubmissionsAway(t *testing.T) {
	cfg := newConfig(t, nil, nil)
	cfg.Embed.Now = func() time.Time { return beforeTheFeast }
	cfg.Embed.Forms = cappedForms{inner: cfg.Embed.Forms, cap: 1}

	h := embedOf(t, cfg)

	// One good submission, which is the cap.
	first := getPage(t, h, "/f/"+theForm)
	if w := postForm(t, h, filled(grantIn(t, first.Body.String()))); w.Code != http.StatusOK {
		t.Fatalf("the first submission = %d:\n%s", w.Code, short(w.Body.String()))
	}

	second := getPage(t, h, "/f/"+theForm)

	w := postForm(t, h, filled(grantIn(t, second.Body.String())))

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("the submission over the cap = %d, want 429:\n%s", w.Code, short(w.Body.String()))
	}

	body := w.Body.String()

	// A sentence saying what to do, and the form still there with the answers
	// in it. Somebody turned away by a number the shrine set has done nothing
	// wrong and should not be shown an error page.
	if !strings.Contains(body, "try again tomorrow") {
		t.Errorf("the page does not say when to come back:\n%s", short(body))
	}
	if !strings.Contains(body, "Maria O&#39;Neill") && !strings.Contains(body, "Maria O'Neill") {
		t.Errorf("the answers were not put back in the boxes:\n%s", short(body))
	}
}

// --- the hourly ceiling -------------------------------------------------------

// The ceiling refuses nothing, which is the point of it: a form selling out in
// an afternoon is the outcome this service exists for, and the most expensive
// bug available here would be a limiter that stopped it. What it does is say
// so, once, while it is still happening.
func TestAFormTakingSubmissionsTooFastSaysSoWithoutRefusingAny(t *testing.T) {
	var lines strings.Builder

	cfg := newConfig(t, nil, nil)
	cfg.Embed.Now = func() time.Time { return beforeTheFeast }
	cfg.Embed.Log = slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg.Embed.Limits = embedapp.Limits{PerFormHourly: 2}

	h := embedOf(t, cfg)

	for i := 1; i <= 4; i++ {
		page := getPage(t, h, "/f/"+theForm)

		if w := postForm(t, h, filled(grantIn(t, page.Body.String()))); w.Code != http.StatusOK {
			t.Fatalf("submission %d = %d, want every one of them accepted:\n%s", i, w.Code, short(w.Body.String()))
		}
	}

	out := lines.String()

	if got := strings.Count(out, "far faster than expected"); got != 1 {
		t.Errorf("the alarm fired %d times in one hour, want exactly 1:\n%s", got, out)
	}
}

// --- signing in ------------------------------------------------------------------

// Four ways of presenting one credential, one allowance. A limit that let
// somebody exhaust the backup codes and then start on the sign-in links would
// be four limits and no limit.
func TestSignInAttemptsAreThrottled(t *testing.T) {
	a := newAdmin(t, "")

	// The default rate is five at once, so the sixth attempt across these
	// routes is the one that is refused.
	var last *httptest.ResponseRecorder

	for range 5 {
		last = a.post(t, "/signin", url.Values{"email": {"nobody@example.test"}}, "")
		if last.Code == http.StatusTooManyRequests {
			t.Fatalf("an attempt inside the burst was refused")
		}
	}

	if w := a.post(t, "/signin/code", url.Values{"email": {"nobody@example.test"}, "code": {"aaaa-bbbb"}}, ""); w.Code != http.StatusTooManyRequests {
		t.Errorf("a backup code attempt after five sign-in requests = %d, want 429", w.Code)
	}

	// The page itself is not behind the limit: somebody who has just been
	// refused has to be able to read the form they were refused on.
	if w := a.get(t, "/signin", ""); w.Code != http.StatusOK {
		t.Errorf("GET /signin after being throttled = %d, want 200", w.Code)
	}
}

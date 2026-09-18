// Package paymentapp is the one route Stripe calls.
//
// It is the third request surface, and it has nothing in common with the other
// two. Nobody is looking at it, there is no session, there is no form, and the
// caller is not a browser. What it has instead is a signature over the exact
// bytes of the body, and that single fact decides almost every line here.
//
// # What must not be in front of this handler
//
// Anything that reads the body. The signature covers the bytes as they
// arrived, so a middleware calling r.ParseForm above this route consumes them
// and destroys the only credential this surface has. The parent project's form
// token middleware does exactly that, which is why the design document names
// it, and why this route is on its own listener rather than a path on one of
// the others.
//
// Also not in front of it: the same-origin check. Stripe sends no
// Sec-Fetch-Site and no Origin, so that gate would refuse every real delivery.
//
// # What the status code means to Stripe
//
// Stripe retries any non-2xx for about three days, with backoff, and gives up
// after that. So the codes here are chosen by what we want to happen next
// rather than by what reads best in a log:
//
//	200  handled, or deliberately ignored, or already seen. Stop sending it.
//	400  the signature did not verify. Nothing legitimate produces this, and
//	     a retry of a body Stripe did not sign would fail identically.
//	500  we could not finish. Please send it again -- this is the case the
//	     retry exists for, and answering 200 here would lose a payment.
//
// The one that is easy to get wrong is the last: a handler that logs an error
// and answers 200 has told Stripe to forget about a payment it never recorded.
package paymentapp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/payment/paybus"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// maxBody is the cap on a delivery.
//
// Generous compared with the form surface's, because an event embeds the whole
// object it is about and some of Stripe's objects are large -- a session with
// many line items, an invoice, a dispute with evidence. Too small a cap here
// is a signature that cannot verify because the body was truncated, which
// presents as a forged request and is a bad afternoon.
const maxBody = 256 << 10

// SignatureHeader is where Stripe puts the signature.
const SignatureHeader = "Stripe-Signature"

// Payments is the slice of the payment domain this app needs.
type Payments interface {
	Fulfil(ctx context.Context, now time.Time, payload []byte, signature string) (paybus.Event, error)
}

// Config is what this app needs.
type Config struct {
	Log      *slog.Logger
	Payments Payments

	// Now is injected so a test can put the clock anywhere. Nil means
	// time.Now.
	Now func() time.Time
}

type app struct {
	cfg Config
}

func (a app) now() time.Time {
	if a.cfg.Now == nil {
		return time.Now()
	}

	return a.cfg.Now()
}

// Routes mounts this app.
//
// One route, and no middleware argument, because there is nothing this route
// may sit behind. That absence is the point: a future reader adding a gate
// here should first read this package's comment.
func Routes(mux *http.ServeMux, cfg Config) {
	a := app{cfg: cfg}

	mux.HandleFunc("POST /stripe/webhook", a.webhook)
}

func (a app) webhook(w http.ResponseWriter, r *http.Request) {
	requestID := web.RequestIDFrom(r.Context())

	// Read, and read the whole thing, with no decoding of any kind. This is
	// the only place in the service that wants raw bytes rather than parsed
	// values, and the reason is that the credential is a MAC over them.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		// Almost always the cap: a body larger than maxBody, which
		// MaxBytesReader turns into an error here. Answered 400 rather than
		// 500 because a retry sends the same oversized body and would fail
		// the same way, and because the useful action is to raise the cap
		// rather than to wait.
		a.cfg.Log.Error("a payment notification could not be read",
			"request_id", requestID, "error", err)
		http.Error(w, "unreadable", http.StatusBadRequest)

		return
	}

	e, err := a.cfg.Payments.Fulfil(r.Context(), a.now(), body, r.Header.Get(SignatureHeader))

	switch {
	case errors.Is(err, paybus.ErrRefused):
		// Not logged here: paybus already wrote one line with the reason, and
		// this route is a public URL that gets scanned, so two lines per
		// probe is how a log becomes unreadable.
		http.Error(w, "that notification is not signed by Stripe", http.StatusBadRequest)

		return

	case errors.Is(err, paybus.ErrSeen):
		// Already handled. A 200, emphatically: Stripe is doing exactly what
		// it promises, and anything else asks it to keep trying.
		a.ok(w)

		return

	case err != nil:
		// The one case that must be a 500. Something went wrong on our side
		// and the retry is what recovers the payment.
		a.cfg.Log.Error("a payment notification could not be handled",
			"request_id", requestID, "error", err)
		http.Error(w, "we could not record that. Please retry.", http.StatusInternalServerError)

		return
	}

	// Logged at this layer as well as in paybus, because the request line and
	// this one are what tie a Stripe event id to a request id, and that is the
	// join somebody needs when a payment is in question.
	a.cfg.Log.Info("payment notification accepted",
		"request_id", requestID, "event", e.ID, "kind", e.Kind, "result", e.Result)

	a.ok(w)
}

// ok answers Stripe.
//
// A tiny body rather than an empty one: Stripe's dashboard shows the response,
// and something readable there is worth four bytes when somebody is looking at
// a delivery attempt trying to work out whether we received it.
func (a app) ok(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	if _, err := io.WriteString(w, "ok\n"); err != nil {
		a.cfg.Log.Warn("a payment acknowledgement could not be sent", "error", err)
	}
}

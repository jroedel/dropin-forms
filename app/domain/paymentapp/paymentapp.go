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
// and destroys the only credential this route has. The parent project's form
// token middleware does exactly that, which is why the design document names
// it.
//
// That is the *only* requirement, and it is why this route shares the embed
// listener rather than having one of its own: nothing on that chain reads a
// body. It is mounted outside that surface's same-origin gate all the same,
// and the reason is worth being precise about, because an earlier version of
// this comment got it wrong.
//
// The wrong version said the gate would refuse Stripe's delivery. It would
// not: web.sameOrigin treats a request carrying neither Sec-Fetch-Site nor
// Origin as a pass -- the hole its own comment documents -- and that is
// exactly what a server-to-server POST carries. So this route would have
// worked behind the gate, by falling through a hole.
//
// Which is not a thing to depend on. Closing that hole is a defensible change
// to make for a form POST some day, and it would silently stop every payment
// being confirmed. So muxer.Embed hands the gate to embedapp for its own write
// route and leaves this one genuinely outside it.
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
	"github.com/jroedel/dropin-forms/business/types"
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

// Notify tells the submitter and the office that a payment is confirmed.
//
// It is called from here rather than from paybus, and that is deliberate: what
// paybus decides is whether money arrived, which is a rule about payments, and
// who hears about it afterwards is not. Keeping the send in the handler also
// keeps it out of the path between settling a submission and recording the
// event, which is the one ordering in this service that must not acquire extra
// steps.
type Notify interface {
	Paid(ctx context.Context, id types.ID)
}

// Config is what this app needs.
type Config struct {
	Log      *slog.Logger
	Payments Payments

	// Notify is optional. Without it a payment is still confirmed, still
	// recorded and still visible in the management app; nobody is told by
	// mail.
	Notify Notify

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

	// Exactly one delivery of one event reaches this line: a retry of
	// something already handled comes back as ErrSeen above and is
	// acknowledged without getting here. That is what keeps a receipt from
	// being sent twice, and it is worth knowing that the guarantee comes from
	// the event ledger rather than from anything in notifybus.
	//
	// The gap in it, named rather than hidden: paybus deliberately returns
	// success when it settles a submission and then fails to *record* the
	// event, because the effect has already happened. A retry of that event is
	// therefore fresh, and this line runs a second time. The cost is a
	// duplicate receipt for a payment that did go through, which is the right
	// end of that trade -- the alternative ordering loses payments.
	if e.Result == paybus.ResultPaid && !e.SubmissionID.Zero() && a.cfg.Notify != nil {
		a.cfg.Notify.Paid(r.Context(), e.SubmissionID)
	}

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

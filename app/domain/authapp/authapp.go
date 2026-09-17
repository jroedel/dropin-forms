// Package authapp is the sign-in surface: asking for a link, redeeming one,
// backup codes, the one-time bootstrap, and the account page.
//
// # The emailed link does not sign anybody in
//
// GET /signin/link renders a page with a button. POST /signin/link redeems
// the token. That split is the single most important thing in this package and
// it is not caution for its own sake: mail scanners, corporate security
// gateways and the link previewers built into chat clients all fetch URLs
// found in messages, without anybody clicking. A single-use token redeemed on
// GET is a token spent by software before the person ever sees it -- and the
// symptom is "the link says it has already been used", every time, for
// everybody, with nothing in a log to explain it.
//
// # What the sign-in page will not tell you
//
// Asking for a link renders the same page whether or not an account exists.
// userbus enforces that by returning no error for an unknown address; this
// package holds up the other end by never varying the response, including
// when sending the mail fails. That last one is a deliberate trade: somebody
// whose relay is broken gets a page saying to check their email and no email.
// The alternative is an error that appears only for addresses that do have an
// account, which hands over exactly the list this service should not be
// publishing. The recovery paths for a broken relay are the backup codes and
// the bootstrap secret, which is why both exist.
package authapp

import (
	"context"
	"embed"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/mail"
	"github.com/jroedel/dropin-forms/foundation/web"
)

//go:embed templates
var templates embed.FS

// Templates is this app's own template directory, handed to page.NewRenderer
// by whatever builds the renderer.
var Templates = templates

// home is where signing in lands somebody who was not going anywhere in
// particular.
const home = "/account"

// SiteAdminEnsurer is the one thing this app needs from the access domain, and
// only on the bootstrap path. Named as an interface rather than taking the
// business type, because that is the whole extent of it: this app grants
// nothing else and reads no grants.
type SiteAdminEnsurer interface {
	EnsureSiteAdmin(ctx context.Context, now time.Time, userID types.ID) (bool, error)
}

// Config is what this app needs.
type Config struct {
	Log    *slog.Logger
	Users  *userbus.Business
	Access SiteAdminEnsurer
	Mail   mail.Sender
	Render *page.Renderer

	// BaseURL is this surface's own origin, used to build the link that goes
	// in an email. It cannot be taken from the request: a link built from a
	// Host header is a link an attacker can point at their own host by sending
	// one request, and the person who receives it has no way to tell.
	BaseURL string

	// Bootstrap is the one-time secret from the config file. Empty means the
	// bootstrap route is not mounted at all, which is the right answer once a
	// service has accounts.
	Bootstrap string
}

type app struct {
	cfg Config
}

// Routes mounts this app. Everything here is outside Require except the
// account page, which is why the mounting is split.
func Routes(mux *http.ServeMux, cfg Config, guard func(http.Handler) http.Handler) {
	a := app{cfg: cfg}

	mux.HandleFunc("GET /signin", a.signInForm)
	mux.HandleFunc("POST /signin", a.requestLink)
	mux.HandleFunc("GET /signin/link", a.confirmLink)
	mux.HandleFunc("POST /signin/link", a.redeemLink)
	mux.HandleFunc("GET /signin/code", a.codeForm)
	mux.HandleFunc("POST /signin/code", a.redeemCode)
	mux.HandleFunc("POST /signout", a.signOut)

	// Only when there is a secret to compare against. An unmounted route is a
	// 404, which is a better answer than a form that can never succeed.
	if cfg.Bootstrap != "" {
		mux.HandleFunc("GET /signin/bootstrap", a.bootstrapForm)
		mux.HandleFunc("POST /signin/bootstrap", a.redeemBootstrap)
	}

	mux.Handle("GET /account", guard(http.HandlerFunc(a.account)))
	mux.Handle("POST /account/backup-codes", guard(http.HandlerFunc(a.issueCodes)))
}

// signInView is the data behind the address form.
type signInView struct {
	Next          string
	Email         string
	Problem       string
	HaveBootstrap bool
}

func (a app) signInForm(w http.ResponseWriter, r *http.Request) {
	// Already signed in: there is nothing to do here, and showing the form
	// invites somebody to sign in again for no reason.
	if _, ok := mid.UserFrom(r.Context()); ok {
		http.Redirect(w, r, a.next(r.URL.Query().Get("next")), http.StatusSeeOther)

		return
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "signin", signInView{
		Next:          mid.SafeNext(r.URL.Query().Get("next")),
		HaveBootstrap: a.cfg.Bootstrap != "",
	})
}

func (a app) requestLink(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "signin", signInView{
			Problem:       "We could not read that. Please try again.",
			HaveBootstrap: a.cfg.Bootstrap != "",
		})

		return
	}

	next := mid.SafeNext(r.PostFormValue("next"))
	typed := r.PostFormValue("email")

	email, err := types.ParseEmail(typed)
	if err != nil {
		// A malformed address is the one thing this page will complain about,
		// and it reveals nothing: it is a statement about the text, not about
		// whether anybody holds it.
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "signin", signInView{
			Next:          next,
			Email:         typed,
			Problem:       "That does not look like an email address. Check for a typo.",
			HaveBootstrap: a.cfg.Bootstrap != "",
		})

		return
	}

	req, err := a.cfg.Users.RequestSignIn(r.Context(), time.Now(), email)
	if err != nil {
		a.cfg.Log.Error("a sign-in link could not be prepared",
			"request_id", web.RequestIDFrom(r.Context()), "error", err)
		a.cfg.Render.Render(w, r, http.StatusInternalServerError, "signin", signInView{
			Next:          next,
			Email:         typed,
			Problem:       "Something went wrong at our end. Please try again shortly.",
			HaveBootstrap: a.cfg.Bootstrap != "",
		})

		return
	}

	if req.Sendable() {
		a.send(r, req, next)
	}

	// The same page either way, including when the send above failed. See the
	// package comment: an error here would appear only for addresses that
	// have an account.
	a.cfg.Render.Render(w, r, http.StatusOK, "sent", struct{ Email string }{Email: email.String()})
}

// send mails the link, and treats a failure as something to record rather than
// something to report.
func (a app) send(r *http.Request, req userbus.SignInRequest, next string) {
	link := a.cfg.BaseURL + "/signin/link?t=" + url.QueryEscape(req.Secret)
	if next != "" {
		link += "&next=" + url.QueryEscape(next)
	}

	text := "Somebody asked to sign in to the Schoenstatt Austin forms admin as " +
		req.User.Email.String() + ".\r\n\r\n" +
		"Open this link and press the button to sign in:\r\n\r\n" +
		link + "\r\n\r\n" +
		"The link works once and stops working in fifteen minutes.\r\n\r\n" +
		"If this was not you, nothing has happened and you can ignore this message.\r\n"

	if err := a.cfg.Mail.Send(r.Context(), mail.Message{
		To:      req.User.Email.String(),
		Subject: "Sign in to Schoenstatt Austin forms",
		Text:    text,
	}); err != nil {
		// Loud, because this is the failure that leaves somebody staring at a
		// page telling them to check an inbox nothing will arrive in, and the
		// log is the only place it can be said.
		a.cfg.Log.Error("a sign-in link could not be sent",
			"request_id", web.RequestIDFrom(r.Context()),
			"user_id", req.User.ID.String(),
			"error", err)
	}
}

// linkView carries the token through the page that offers the button.
type linkView struct {
	Token   string
	Next    string
	Problem string
}

// confirmLink renders the page the emailed link opens. It redeems nothing.
func (a app) confirmLink(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	token := q.Get("t")
	if token == "" {
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "link", linkView{
			Problem: "That link is not complete. Please ask for a new one.",
		})

		return
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "link", linkView{
		Token: token,
		Next:  mid.SafeNext(q.Get("next")),
	})
}

func (a app) redeemLink(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "link", linkView{
			Problem: "We could not read that. Please ask for a new link.",
		})

		return
	}

	user, cookie, err := a.cfg.Users.SignIn(r.Context(), time.Now(), r.PostFormValue("token"))
	a.finish(w, r, user, cookie, err, "link", linkView{
		Next:    mid.SafeNext(r.PostFormValue("next")),
		Problem: "That link did not work. It may have been used already, or it may have expired. Please ask for a new one.",
	})
}

type codeView struct {
	Email   string
	Next    string
	Problem string
}

func (a app) codeForm(w http.ResponseWriter, r *http.Request) {
	a.cfg.Render.Render(w, r, http.StatusOK, "code", codeView{
		Next: mid.SafeNext(r.URL.Query().Get("next")),
	})
}

func (a app) redeemCode(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "code", codeView{
			Problem: "We could not read that. Please try again.",
		})

		return
	}

	next := mid.SafeNext(r.PostFormValue("next"))
	typed := r.PostFormValue("email")

	email, err := types.ParseEmail(typed)
	if err != nil {
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "code", codeView{
			Email:   typed,
			Next:    next,
			Problem: "That does not look like an email address. Check for a typo.",
		})

		return
	}

	user, cookie, err := a.cfg.Users.SignInWithBackupCode(r.Context(), time.Now(), email, r.PostFormValue("code"))
	a.finish(w, r, user, cookie, err, "code", codeView{
		Email:   typed,
		Next:    next,
		Problem: "That address and code did not work together. Check both and try again.",
	})
}

type bootstrapView struct {
	Email   string
	Next    string
	Problem string
}

func (a app) bootstrapForm(w http.ResponseWriter, r *http.Request) {
	a.cfg.Render.Render(w, r, http.StatusOK, "bootstrap", bootstrapView{})
}

func (a app) redeemBootstrap(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "bootstrap", bootstrapView{
			Problem: "We could not read that. Please try again.",
		})

		return
	}

	typed := r.PostFormValue("email")

	email, err := types.ParseEmail(typed)
	if err != nil {
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "bootstrap", bootstrapView{
			Email:   typed,
			Problem: "That does not look like an email address. Check for a typo.",
		})

		return
	}

	now := time.Now()

	user, cookie, err := a.cfg.Users.Bootstrap(
		r.Context(), now, a.cfg.Bootstrap, r.PostFormValue("secret"), email)

	// A session that can see nothing is not a bootstrap. The secret exists so
	// that somebody can get in and start granting, so making that account a
	// site-wide administrator is part of redeeming it rather than a separate
	// step somebody has to know about.
	if err == nil {
		a.makeAdmin(r, user)
	}

	a.finish(w, r, user, cookie, err, "bootstrap", bootstrapView{
		Email:   typed,
		Problem: "That did not work. The secret may be wrong, or it may already have been used.",
	})
}

// makeAdmin gives the bootstrap account site-wide admin, and never fails the
// sign-in.
//
// The order is forced: userbus.Bootstrap has already spent the one-time
// secret by the time this runs, so answering 500 here would consume the only
// way in and give nothing back. A session with no grants is recoverable -- the
// account exists, it has backup codes to issue, and somebody can fix the
// database -- while a spent secret and no session is not.
//
// EnsureSiteAdmin declining is not an error. It means the service already has
// an administrator, and a bootstrap secret that has somehow been reissued must
// not be a way to award yourself authority over a service somebody else is
// already running. The person gets their session and sees nothing.
func (a app) makeAdmin(r *http.Request, user userbus.User) {
	granted, err := a.cfg.Access.EnsureSiteAdmin(r.Context(), time.Now(), user.ID)

	switch {
	case err != nil:
		a.cfg.Log.Error("the bootstrap account could not be made an administrator, and the secret is now spent",
			"request_id", web.RequestIDFrom(r.Context()), "user_id", user.ID.String(), "error", err)

	case granted:
		a.cfg.Log.Info("the bootstrap account was made a site-wide administrator",
			"request_id", web.RequestIDFrom(r.Context()), "user_id", user.ID.String())

	default:
		a.cfg.Log.Warn("a bootstrap sign-in was redeemed on a service that already has an administrator, so no grant was made",
			"request_id", web.RequestIDFrom(r.Context()), "user_id", user.ID.String())
	}
}

// finish is the shared tail of every sign-in attempt: set the cookie and go,
// or re-render the page that was tried with its own refusal sentence.
//
// The sentence comes from the caller rather than from the error, because
// userbus returns one error for every failure on purpose. Wording it per page
// is as specific as this service is willing to be.
func (a app) finish(
	w http.ResponseWriter, r *http.Request,
	user userbus.User, cookie string, err error,
	template string, view any,
) {
	switch {
	case errors.Is(err, userbus.ErrDenied):
		// 401 rather than 200, so that a refused attempt is visible in a log
		// and to anything watching for a burst of them.
		a.cfg.Render.Render(w, r, http.StatusUnauthorized, template, view)

		return

	case err != nil:
		a.cfg.Log.Error("a sign-in attempt failed unexpectedly",
			"request_id", web.RequestIDFrom(r.Context()), "error", err)
		http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)

		return
	}

	mid.SetSession(w, cookie, time.Now().Add(14*24*time.Hour))

	a.cfg.Log.Info("signed in",
		"request_id", web.RequestIDFrom(r.Context()), "user_id", user.ID.String())

	http.Redirect(w, r, a.next(nextOf(view)), http.StatusSeeOther)
}

func (a app) signOut(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(mid.CookieName); err == nil {
		if err := a.cfg.Users.SignOut(r.Context(), c.Value); err != nil {
			a.cfg.Log.Error("a session could not be ended",
				"request_id", web.RequestIDFrom(r.Context()), "error", err)
		}
	}

	// Cleared whatever happened above. Leaving the cookie in place after
	// somebody pressed sign out is the worse failure of the two.
	mid.ClearSession(w)

	http.Redirect(w, r, "/signin", http.StatusSeeOther)
}

type accountView struct {
	Email string
	Codes []string
}

func (a app) account(w http.ResponseWriter, r *http.Request) {
	u, ok := mid.UserFrom(r.Context())
	if !ok {
		// Unreachable behind Require. Answered rather than assumed, so that a
		// route mounted without the guard fails visibly here instead of
		// panicking on a zero user.
		http.Redirect(w, r, "/signin", http.StatusSeeOther)

		return
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "account", accountView{Email: u.Email.String()})
}

func (a app) issueCodes(w http.ResponseWriter, r *http.Request) {
	u, ok := mid.UserFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/signin", http.StatusSeeOther)

		return
	}

	codes, err := a.cfg.Users.IssueBackupCodes(r.Context(), time.Now(), u.ID)
	if err != nil {
		a.cfg.Log.Error("backup codes could not be issued",
			"request_id", web.RequestIDFrom(r.Context()), "user_id", u.ID.String(), "error", err)
		http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)

		return
	}

	// Rendered rather than redirected to, because this is the only time these
	// will ever be shown. A redirect would need them in a session or a query
	// string, and both of those are places a one-time secret should not go.
	a.cfg.Render.Render(w, r, http.StatusOK, "account", accountView{
		Email: u.Email.String(),
		Codes: codes,
	})
}

// next resolves where to go after signing in.
func (a app) next(want string) string {
	if safe := mid.SafeNext(want); safe != "" {
		return safe
	}

	return home
}

// nextOf pulls the preserved destination back out of whichever view struct
// this was, so that finish can stay one function across four pages.
func nextOf(view any) string {
	switch v := view.(type) {
	case linkView:
		return v.Next
	case codeView:
		return v.Next
	case bootstrapView:
		return v.Next
	case signInView:
		return v.Next
	}

	return ""
}

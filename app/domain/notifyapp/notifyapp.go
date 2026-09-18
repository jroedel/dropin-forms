// Package notifyapp is the page that turns email about a form off and on.
//
// # Two ways in, and only one of them has a session
//
// The link at the bottom of every notification is opened from a mailbox, quite
// possibly on a device nobody has ever signed in on. Putting that page behind
// the session gate would mean a sign-in round trip to stop an email, which is
// how somebody instead writes to a person and asks them to do it -- so the
// link carries a signed token naming the account and the form, and the page
// accepts either that or an ordinary session.
//
// Both are checked here rather than by a middleware, because the two answers
// are not interchangeable for anything else: a token is proof of one
// preference belonging to one person, and nothing on this surface should be
// able to mistake it for a session.
//
// # The GET changes nothing, and that is load-bearing
//
// Mail scanners, corporate security gateways and the link previewers built
// into chat clients fetch URLs found in messages, without anybody clicking. A
// link that unsubscribed on GET would be spent by software before the person
// ever saw it, and the symptom is the worst kind: notifications that simply
// stop, for no reason anybody can see, with nothing in a log that looks wrong.
//
// So the GET renders a page with a button and the POST does the work, which is
// the same split authapp uses for the emailed sign-in link and for the same
// reason.
package notifyapp

import (
	"context"
	"embed"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formtoml"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

//go:embed templates
var templates embed.FS

// Templates is this app's own template directory, handed to page.NewRenderer
// by whatever builds the renderer.
var Templates = templates

// Path is where this app is mounted, without the form. Exported because the
// link in a notification is built in notifybus and the routes are mounted
// here, and the two have to agree.
const Path = "/notifications"

// Forms is where definitions come from: this app needs a title to put on a
// page and nothing else.
type Forms interface {
	ByID(slug types.Slug) (formbus.Form, error)
}

// Preferences is the slice of the notification domain this app needs.
type Preferences interface {
	ReadLink(token string) (types.ID, types.Slug, error)
	Muted(ctx context.Context, userID types.ID, form types.Slug) (bool, error)
	SetMuted(ctx context.Context, now time.Time, userID types.ID, form types.Slug, muted bool) error
}

// Config is what this app needs.
type Config struct {
	Log         *slog.Logger
	Forms       Forms
	Preferences Preferences
	Render      *page.Renderer

	// SignInPath is where somebody with neither a token nor a session is sent.
	// Handed in rather than written here, because which path signs somebody in
	// is the muxer's fact.
	SignInPath string

	// Now is the clock. Nil means time.Now.
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
// No guard argument, unlike every other app on this surface, and that is the
// whole shape of it rather than an omission: the route has to be reachable
// from a mailbox. What stands in for the session is the signed token, checked
// in the handler, and the package comment says why it cannot be a middleware.
func Routes(mux *http.ServeMux, cfg Config) {
	a := app{cfg: cfg}

	mux.HandleFunc("GET "+Path+"/{"+mid.FormSlugParam+"}", a.show)
	mux.HandleFunc("POST "+Path+"/{"+mid.FormSlugParam+"}", a.set)
}

// view is the page.
type view struct {
	FormID string
	Title  string

	// Muted is the current state, which decides both the sentence and the
	// button's label.
	Muted bool

	// Token is carried into the form when that is how somebody arrived, so
	// that the POST knows whose preference it is changing without a session.
	Token string

	// Done says the button has just been pressed, so the page can say what
	// happened rather than only what is true now.
	Done bool

	// Problem is a link that did not work, or a preference that could not be
	// stored.
	Problem string
}

// show renders the current state and changes nothing.
func (a app) show(w http.ResponseWriter, r *http.Request) {
	user, f, token, ok := a.who(w, r, r.URL.Query().Get("t"))
	if !ok {
		return
	}

	muted, err := a.cfg.Preferences.Muted(r.Context(), user, f.ID)
	if err != nil {
		a.oops(w, r, "a notification preference could not be read", err)

		return
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "notifications", view{
		FormID: f.ID.String(),
		Title:  f.Title,
		Muted:  muted,
		Token:  token,
	})
}

// set is the button.
func (a app) set(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.oops(w, r, "a notification preference could not be read from the request", err)

		return
	}

	user, f, token, ok := a.who(w, r, r.PostFormValue("t"))
	if !ok {
		return
	}

	// The button says what it will do, and the value says the same thing, so
	// that pressing a stale page's button twice is idempotent rather than a
	// toggle that flips back.
	muted := r.PostFormValue("muted") == "yes"

	if err := a.cfg.Preferences.SetMuted(r.Context(), a.now(), user, f.ID, muted); err != nil {
		a.cfg.Log.Error("a notification preference could not be stored",
			"request_id", web.RequestIDFrom(r.Context()),
			"form", f.ID.String(), "error", err)

		a.cfg.Render.Render(w, r, http.StatusInternalServerError, "notifications", view{
			FormID:  f.ID.String(),
			Title:   f.Title,
			Token:   token,
			Problem: "We could not save that. Please try again in a moment.",
		})

		return
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "notifications", view{
		FormID: f.ID.String(),
		Title:  f.Title,
		Muted:  muted,
		Token:  token,
		Done:   true,
	})
}

// who resolves whose preference this request is about, from the token if there
// is one and from the session otherwise.
//
// The token wins when both are present, and the case is worth naming: somebody
// signed in as one account can be holding a link belonging to a colleague who
// forwarded the email. Honouring the token is what the link says it does, and
// the alternative -- silently changing the reader's own preference instead --
// would be the one outcome nobody could explain afterwards.
//
// It also answers the request itself when there is nothing to work with, so
// that every caller either has a user and a form or has already returned.
func (a app) who(w http.ResponseWriter, r *http.Request, token string) (types.ID, formbus.Form, string, bool) {
	slug, err := types.ParseSlug(r.PathValue(mid.FormSlugParam))
	if err != nil {
		a.notFound(w, r)

		return types.ID{}, formbus.Form{}, "", false
	}

	f, err := a.cfg.Forms.ByID(slug)

	switch {
	case errors.Is(err, formtoml.ErrNotFound):
		a.notFound(w, r)

		return types.ID{}, formbus.Form{}, "", false

	case err != nil:
		a.oops(w, r, "a form could not be read", err)

		return types.ID{}, formbus.Form{}, "", false
	}

	if token != "" {
		user, named, err := a.cfg.Preferences.ReadLink(token)

		switch {
		case err != nil:
			// Logged at Info: a link that does not verify is far more likely
			// to be one mangled by a mail client than an attack, and either
			// way the page says the same thing.
			a.cfg.Log.Info("an unsubscribe link did not verify",
				"request_id", web.RequestIDFrom(r.Context()), "form", slug.String(), "reason", err)

			a.refuse(w, r, f)

			return types.ID{}, formbus.Form{}, "", false

		case named != slug:
			// The token is ours and names a different form from the one in the
			// path. Nothing legitimate produces this -- the link is built with
			// both -- so it is refused rather than resolved in favour of
			// either.
			a.cfg.Log.Warn("an unsubscribe link named a different form from the one it was used on",
				"request_id", web.RequestIDFrom(r.Context()),
				"path_form", slug.String(), "token_form", named.String())

			a.refuse(w, r, f)

			return types.ID{}, formbus.Form{}, "", false
		}

		return user, f, token, true
	}

	if u, ok := mid.UserFrom(r.Context()); ok {
		return u.ID, f, "", true
	}

	// Neither. Sign in, and come back here -- which is also the page somebody
	// lands on when a link has expired out of a mailbox into a new laptop.
	http.Redirect(w, r, a.cfg.SignInPath+"?next="+mid.SafeNext(Path+"/"+slug.String()), http.StatusSeeOther)

	return types.ID{}, formbus.Form{}, "", false
}

// refuse is the answer to a link that did not verify.
//
// It renders the page with a problem rather than a bare 403, because the
// person holding a bad link is almost always the person the link was for: a
// mail client wrapped it, or it was copied by hand and lost a character. The
// page tells them the one thing that always works, which is signing in.
func (a app) refuse(w http.ResponseWriter, r *http.Request, f formbus.Form) {
	a.cfg.Render.Render(w, r, http.StatusForbidden, "notifications", view{
		FormID: f.ID.String(),
		Title:  f.Title,
		Problem: "That link did not work. It may have been broken up by your email program. " +
			"Sign in and you can change this from the forms page.",
	})
}

// notFound is a slug that names no form.
//
// Plain text rather than a rendered page, and deliberately not submissionapp's
// own "no-form" template: two apps defining a block of the same name into one
// renderer is a collision, and reaching into another app's templates is the
// coupling the layering rule exists to prevent. Nobody arrives here from a
// link this service wrote.
func (a app) notFound(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "There is no form by that name.", http.StatusNotFound)
}

func (a app) oops(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.cfg.Log.Error(what, "request_id", web.RequestIDFrom(r.Context()), "error", err)

	http.Error(w, "Something went wrong on our end. Please try again in a moment.", http.StatusInternalServerError)
}

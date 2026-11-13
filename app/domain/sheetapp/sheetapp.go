// Package sheetapp is the page where somebody connects a Google Sheet of a
// form's answers, and the address Google sends them back to.
//
// The page shows the person's own connection and nothing else: whose sheets
// exist is nobody else's business, administrators' included (sheetbus says
// why). It is behind results, the same gate as the download, because a sheet
// is a download that keeps itself up to date.
//
// # Why Connect is a link and not a button
//
// Every other change on these pages is a POST. Connect is a GET, because what
// it does is send the browser to Google, and this site's Content-Security-
// Policy has form-action 'self': browsers apply that to where a form's POST
// is redirected as well, so a POST that answered with a redirect to
// accounts.google.com would be stopped by the browser. A GET that changes
// nothing here -- it sets a short-lived cookie and redirects -- is safe to be
// a link; what it starts cannot finish without the person agreeing on
// Google's own page, and the state it carries is signed for this person and
// form only.
package sheetapp

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
	"github.com/jroedel/dropin-forms/business/domain/sheet/sheetbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

//go:embed templates
var templates embed.FS

// Templates is this app's template directory, for page.NewRenderer.
var Templates = templates

// CallbackPath is where Google sends the browser back to. Registered, with the
// admin surface's address in front, as the OAuth client's redirect URI.
const CallbackPath = "/google/callback"

// verifierCookie holds the PKCE verifier between leaving for Google and
// coming back. __Host- for the session cookie's reasons; Lax, because the
// browser arrives back on a top-level GET from Google, which Lax allows.
const verifierCookie = "__Host-sheet-verifier"

// Forms resolves the slug in the path.
type Forms interface {
	ByID(slug types.Slug) (formbus.Form, error)
}

// Sheets is the sheet domain.
type Sheets interface {
	Begin(ctx context.Context, now time.Time, user types.ID, form types.Slug, hint string) (string, string, error)
	ReadState(now time.Time, user types.ID, state string) (types.Slug, error)
	Finish(ctx context.Context, now time.Time, user types.ID, state, code, verifier string) (sheetbus.Connection, error)
	Mine(ctx context.Context, user types.ID, form types.Slug) (sheetbus.Connection, []sheetbus.Event, error)
	Now(ctx context.Context, now time.Time, user types.ID, form types.Slug, refill bool) (sheetbus.Connection, error)
	Remake(ctx context.Context, now time.Time, user types.ID, form types.Slug) (sheetbus.Connection, error)
	Disconnect(ctx context.Context, user types.ID, form types.Slug) error
}

// Config is what this app needs.
type Config struct {
	Log    *slog.Logger
	Forms  Forms
	Sheets Sheets
	Render *page.Renderer

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

type app struct {
	cfg Config
}

func (a app) now() time.Time {
	if a.cfg.Now != nil {
		return a.cfg.Now()
	}

	return time.Now()
}

// Routes mounts this app. guard is Require and results is
// RequireFormRole(results), handed in by the muxer.
func Routes(mux *http.ServeMux, cfg Config, guard, results func(http.Handler) http.Handler) {
	a := app{cfg: cfg}

	base := "/forms/{" + mid.FormSlugParam + "}/sheet"

	mux.Handle("GET "+base, guard(results(http.HandlerFunc(a.page))))
	mux.Handle("GET "+base+"/connect", guard(results(http.HandlerFunc(a.connect))))
	mux.Handle("POST "+base+"/sync", guard(results(http.HandlerFunc(a.sync))))
	mux.Handle("POST "+base+"/refill", guard(results(http.HandlerFunc(a.refill))))
	mux.Handle("POST "+base+"/remake", guard(results(http.HandlerFunc(a.remake))))
	mux.Handle("POST "+base+"/disconnect", guard(results(http.HandlerFunc(a.disconnect))))

	// Behind Require alone: there is no form in this path for the form gate
	// to read. The form comes from the signed state, and Finish checks this
	// person may read it before anything is made.
	mux.Handle("GET "+CallbackPath, guard(http.HandlerFunc(a.callback)))
}

// The words the routes send the browser back with, each a sentence on the
// page. Fixed words rather than text in the address, so nothing a link says
// is shown as if this service had said it.
var said = map[string]struct {
	text    string
	problem bool
}{
	"connected":    {"Connected. Your spreadsheet is filled in, and from now on it is kept up to date every few minutes.", false},
	"synced":       {"Updated.", false},
	"refilled":     {"Refilled: every row and column you had deleted is back.", false},
	"remade":       {"A new spreadsheet has been made and filled in.", false},
	"disconnected": {"Disconnected. The spreadsheet is still in your Google Drive, and is no longer updated.", false},
	"declined":     {"Nothing was connected: Google says permission was not given.", true},
	"no-files":     {"Nothing was connected: on Google's page, the box allowing this service to create the spreadsheet in your Drive was not ticked. Connect again and leave it ticked.", true},
	"expired":      {"Nothing was connected: that took too long, or began in another browser. Please connect again.", true},
	"failed":       {"That did not work, and nothing was changed. Please try again in a minute.", true},
	"reconnect":    {"Google no longer accepts this connection. Connect again to carry on.", true},
	"gone":         {"The spreadsheet can't be reached. Make a new one, or disconnect.", true},
}

type pageView struct {
	Form   formbus.Form
	FormID string

	Said    string
	Problem bool

	// Connected is whether this person has a sheet of this form.
	Connected bool
	Account   string
	URL       string
	State     string
	Note      string
	Synced    string
	Events    []eventView

	Reconnect bool
	Gone      bool
	Stopped   bool
}

type eventView struct {
	When string
	Text string
}

func (a app) page(w http.ResponseWriter, r *http.Request) {
	f, me, ok := a.who(w, r)
	if !ok {
		return
	}

	view := pageView{Form: f, FormID: f.ID.String()}

	if s, ok := said[r.URL.Query().Get("sheet")]; ok {
		view.Said, view.Problem = s.text, s.problem
	}

	c, events, err := a.cfg.Sheets.Mine(r.Context(), me, f.ID)

	switch {
	case errors.Is(err, sheetbus.ErrNotFound):
		a.cfg.Render.Render(w, r, http.StatusOK, "sheet", view)

		return
	case err != nil:
		a.oops(w, r, "a Google Sheet connection could not be read", err)

		return
	}

	view.Connected = true
	view.Account = c.Account
	view.URL = c.URL
	view.Note = c.Problem
	view.Reconnect = c.State == sheetbus.StateReconnect
	view.Gone = c.State == sheetbus.StateGone
	view.Stopped = c.State == sheetbus.StateStopped

	switch c.State {
	case sheetbus.StateOK:
		view.State = "Kept up to date"
	case sheetbus.StateReconnect:
		view.State = "Waiting for you to connect again"
	case sheetbus.StateGone:
		view.State = "The spreadsheet can't be reached"
	case sheetbus.StateStopped:
		view.State = "Stopped"
	}

	if !c.SyncedAt.IsZero() {
		view.Synced = c.SyncedAt.Local().Format("2 Jan 2006, 15:04")
	}

	for _, e := range events {
		view.Events = append(view.Events, eventView{When: e.At.Local().Format("2 Jan, 15:04"), Text: e.Text})
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "sheet", view)
}

// connect sends the browser to Google's consent screen.
func (a app) connect(w http.ResponseWriter, r *http.Request) {
	f, me, ok := a.who(w, r)
	if !ok {
		return
	}

	to, verifier, err := a.cfg.Sheets.Begin(r.Context(), a.now(), me, f.ID, "")
	if err != nil {
		a.oops(w, r, "connecting a Google Sheet could not begin", err)

		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     verifierCookie,
		Value:    verifier,
		Path:     "/",
		MaxAge:   int((15 * time.Minute).Seconds()),
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	http.Redirect(w, r, to, http.StatusSeeOther)
}

// callback is Google sending the browser back.
func (a app) callback(w http.ResponseWriter, r *http.Request) {
	user, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "Google's answer was reached with no account", nil)

		return
	}

	q := r.URL.Query()
	now := a.now()

	form, err := a.cfg.Sheets.ReadState(now, user.ID, q.Get("state"))
	if err != nil {
		// Not this person's sign-in, or too old: there is no form to send
		// them back to with any confidence.
		http.Error(w, "That answer from Google does not belong to this sign-in, or took too long. Go back to the form's Google Sheet page and connect again.", http.StatusBadRequest)

		return
	}

	back := func(word string) {
		http.SetCookie(w, &http.Cookie{Name: verifierCookie, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		http.Redirect(w, r, "/forms/"+form.String()+"/sheet?sheet="+word, http.StatusSeeOther)
	}

	if q.Get("error") != "" {
		back("declined")

		return
	}

	cookie, err := r.Cookie(verifierCookie)
	if err != nil || cookie.Value == "" {
		back("expired")

		return
	}

	_, err = a.cfg.Sheets.Finish(r.Context(), now, user.ID, q.Get("state"), q.Get("code"), cookie.Value)

	switch {
	case err == nil:
		back("connected")
	case errors.Is(err, sheetbus.ErrNoFiles):
		back("no-files")
	case errors.Is(err, sheetbus.ErrBadState):
		back("expired")
	case errors.Is(err, sheetbus.ErrNoAccess):
		http.Error(w, "You no longer have access to that form's answers.", http.StatusForbidden)
	default:
		a.cfg.Log.Error("a Google Sheet could not be connected",
			"request_id", web.RequestIDFrom(r.Context()), "form", form.String(), "error", err)
		back("failed")
	}
}

func (a app) sync(w http.ResponseWriter, r *http.Request)   { a.again(w, r, false) }
func (a app) refill(w http.ResponseWriter, r *http.Request) { a.again(w, r, true) }

func (a app) again(w http.ResponseWriter, r *http.Request, refill bool) {
	f, me, ok := a.who(w, r)
	if !ok {
		return
	}

	_, err := a.cfg.Sheets.Now(r.Context(), a.now(), me, f.ID, refill)

	word := "synced"
	if refill {
		word = "refilled"
	}

	a.answer(w, r, f, err, word)
}

func (a app) remake(w http.ResponseWriter, r *http.Request) {
	f, me, ok := a.who(w, r)
	if !ok {
		return
	}

	_, err := a.cfg.Sheets.Remake(r.Context(), a.now(), me, f.ID)
	a.answer(w, r, f, err, "remade")
}

func (a app) disconnect(w http.ResponseWriter, r *http.Request) {
	f, me, ok := a.who(w, r)
	if !ok {
		return
	}

	err := a.cfg.Sheets.Disconnect(r.Context(), me, f.ID)
	if errors.Is(err, sheetbus.ErrNotFound) {
		err = nil
	}

	a.answer(w, r, f, err, "disconnected")
}

func (a app) answer(w http.ResponseWriter, r *http.Request, f formbus.Form, err error, word string) {
	switch {
	case err == nil:
	case errors.Is(err, sheetbus.ErrNotFound):
		http.Error(w, "You have no Google Sheet connected to this form.", http.StatusNotFound)

		return
	case errors.Is(err, sheetbus.ErrReconnect):
		word = "reconnect"
	case errors.Is(err, sheetbus.ErrGone):
		word = "gone"
	default:
		a.cfg.Log.Warn("a Google Sheet action did not work",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)

		word = "failed"
	}

	http.Redirect(w, r, "/forms/"+f.ID.String()+"/sheet?sheet="+word, http.StatusSeeOther)
}

// who resolves the form in the path and the person asking.
func (a app) who(w http.ResponseWriter, r *http.Request) (formbus.Form, types.ID, bool) {
	slug, err := types.ParseSlug(r.PathValue(mid.FormSlugParam))
	if err != nil {
		http.Error(w, "there is no form by that name.", http.StatusNotFound)

		return formbus.Form{}, types.ID{}, false
	}

	f, err := a.cfg.Forms.ByID(slug)

	switch {
	case errors.Is(err, formbus.ErrNotFound):
		http.Error(w, "there is no form by that name.", http.StatusNotFound)

		return formbus.Form{}, types.ID{}, false
	case err != nil:
		a.oops(w, r, "a form could not be read", err)

		return formbus.Form{}, types.ID{}, false
	}

	me, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "the Google Sheet page was reached with no account", nil)

		return formbus.Form{}, types.ID{}, false
	}

	return f, me.ID, true
}

func (a app) oops(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.cfg.Log.Error(what, "request_id", web.RequestIDFrom(r.Context()), "error", err)
	http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)
}

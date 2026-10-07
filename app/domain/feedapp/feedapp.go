// Package feedapp lets a spreadsheet read one form's submissions, and lets the
// form's administrators decide which spreadsheets may.
//
// feedbus says why there is a key at all on a service that otherwise has no
// credential but a session, and what keeps it narrow.
//
// # What guards each route
//
//	GET  /forms/{slug}/feed               Require, then RequireFormRole(admin)
//	POST /forms/{slug}/feed               the same: makes a key, shows it once
//	POST /forms/{slug}/feed/{id}/revoke   the same
//	GET  /forms/{slug}/feed.json          a key for this form, in an
//	                                      Authorization: Bearer header, and a
//	                                      throttle. No session.
//
// Admin to make or revoke a key, not results, although results is all a key
// can do: handing a standing copy of every answer to something outside this
// service is a decision about the form, and that is the administrator's.
//
// The JSON route sits on the admin listener behind the same chain as every
// other admin route. That chain refuses nothing a script's GET carries: the
// same-origin gate passes a request with neither Sec-Fetch-Site nor Origin, the
// form-encoding gate looks only at writes, and Authenticate never refuses. The
// key is checked here, and nothing a session holds is consulted at all.
//
// # What the feed holds
//
// The same rows as the CSV download: every submission that is not hidden, the
// answers as stored, one entry per question the form asks now. Oldest first,
// so that a sheet that writes them out in order keeps each person on the row
// they were on yesterday. Everything, every time, rather than what changed
// since a date: hiding a submission does not change it, so a script asking
// "what is new" would never hear that a row should go -- and at the size of a
// parish form, all of it is a few kilobytes.
package feedapp

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/feed/feedbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

//go:embed templates
var templates embed.FS

// Templates is this app's template directory, for page.NewRenderer.
var Templates = templates

// Forms resolves the slug in the path.
type Forms interface {
	ByID(slug types.Slug) (formbus.Form, error)
}

// Submissions is what the feed reads.
type Submissions interface {
	ByForm(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error)
	ChangeCounts(ctx context.Context, form types.Slug) (map[types.ID]int, error)
}

// Keys is the key domain.
type Keys interface {
	Create(ctx context.Context, now time.Time, form types.Slug, label string, by types.ID) (feedbus.Key, string, error)
	Check(ctx context.Context, now time.Time, form types.Slug, presented string) (feedbus.Key, error)
	ForForm(ctx context.Context, form types.Slug) ([]feedbus.Key, error)
	Revoke(ctx context.Context, now time.Time, form types.Slug, id, by types.ID) error
}

// Config is what this app needs.
type Config struct {
	Log         *slog.Logger
	Forms       Forms
	Submissions Submissions
	Keys        Keys
	Render      *page.Renderer

	// BaseURL is the admin surface's own address, for writing the feed's full
	// address into the script on the page. Without it the page shows the
	// path, which is still enough to work it out.
	BaseURL string

	// TrustProxy and Rate decide the throttle in front of the JSON. A zero
	// Rate gets the default; see Routes.
	TrustProxy bool
	Rate       web.Rate

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

// Routes mounts this app. guard is Require and admins is
// RequireFormRole(admin), handed in by the muxer.
func Routes(mux *http.ServeMux, cfg Config, guard, admins func(http.Handler) http.Handler) {
	a := app{cfg: cfg}

	// A sheet's script runs on a timer, every few minutes at most. Sixty in a
	// burst and one a second after is far above that and far below anything
	// that would trouble the database, and keyed by address and form so that
	// two sheets behind Google's shared addresses reading two forms do not
	// spend each other's allowance.
	if cfg.Rate.Zero() {
		cfg.Rate = web.Rate{Burst: 60, Every: time.Second}
	}

	reads := web.Throttle(web.Throttling{
		Rate: cfg.Rate,
		Key: func(r *http.Request) string {
			return "feed|" + web.IPBucket(web.ClientIP(r, cfg.TrustProxy)) + "|" + r.PathValue(mid.FormSlugParam)
		},
		Log: cfg.Log,
	})

	base := "/forms/{" + mid.FormSlugParam + "}/feed"

	mux.Handle("GET "+base, guard(admins(http.HandlerFunc(a.page))))
	mux.Handle("POST "+base, guard(admins(http.HandlerFunc(a.create))))
	mux.Handle("POST "+base+"/{id}/revoke", guard(admins(http.HandlerFunc(a.revoke))))
	mux.Handle("GET "+base+".json", reads(http.HandlerFunc(a.feed)))
}

// pageView is the page that manages a form's keys.
type pageView struct {
	Form   formbus.Form
	FormID string

	// FeedURL is the address a script reads.
	FeedURL string

	// Secret is a key just made, shown this once.
	Secret string

	Keys []keyView
}

type keyView struct {
	ID       string
	Label    string
	Made     string
	LastUsed string
	Live     bool
}

func (a app) page(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	a.render(w, r, http.StatusOK, f, "")
}

// create makes a key and shows it on the page it answers with, which is the
// only time it is ever shown.
//
// A page rather than a redirect, which every other write on this surface
// answers with, because the key has to be on the page and must not be in a
// URL. Reloading it asks the browser to send the form again, which would make
// a second key; that is a key nobody copied, and it is listed beside the
// first with a button to revoke it.
func (a app) create(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	me, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "making a feed key was reached with no account", nil)

		return
	}

	_, secret, err := a.cfg.Keys.Create(r.Context(), a.now(), f.ID, r.PostFormValue("label"), me.ID)
	if err != nil {
		a.oops(w, r, "a feed key could not be made", err)

		return
	}

	a.render(w, r, http.StatusOK, f, secret)
}

func (a app) revoke(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	me, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "revoking a feed key was reached with no account", nil)

		return
	}

	id, err := types.ParseID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "there is no such key.", http.StatusNotFound)

		return
	}

	// The form is passed down to the store's condition, so a key on another
	// form is left alone rather than revoked by somebody who edited the URL.
	if err := a.cfg.Keys.Revoke(r.Context(), a.now(), f.ID, id, me.ID); err != nil {
		a.oops(w, r, "a feed key could not be revoked", err)

		return
	}

	http.Redirect(w, r, "/forms/"+f.ID.String()+"/feed", http.StatusSeeOther)
}

func (a app) render(w http.ResponseWriter, r *http.Request, status int, f formbus.Form, secret string) {
	keys, err := a.cfg.Keys.ForForm(r.Context(), f.ID)
	if err != nil {
		a.oops(w, r, "the feed keys could not be read", err)

		return
	}

	view := pageView{
		Form:    f,
		FormID:  f.ID.String(),
		FeedURL: a.cfg.BaseURL + "/forms/" + f.ID.String() + "/feed.json",
		Secret:  secret,
	}

	for _, k := range keys {
		kv := keyView{
			ID:       k.ID.String(),
			Label:    k.Label,
			Made:     k.CreatedAt.Local().Format("2 January 2006"),
			LastUsed: "never",
			Live:     k.Live(),
		}

		if !k.LastUsedAt.IsZero() {
			kv.LastUsed = k.LastUsedAt.Local().Format("2 January 2006, 15:04")
		}

		view.Keys = append(view.Keys, kv)
	}

	a.cfg.Render.Render(w, r, status, "feed", view)
}

// The JSON. Field names are spelled out on these wire types rather than
// inherited from the domain's, because they are a contract with scripts this
// repository cannot see: renaming a Go field must not rename a column in
// somebody's spreadsheet.
type feedDoc struct {
	Form        string        `json:"form"`
	Title       string        `json:"title"`
	GeneratedAt time.Time     `json:"generated_at"`
	Fields      []fieldDoc    `json:"fields"`
	Responses   []responseDoc `json:"responses"`
}

type fieldDoc struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
	Multiple bool   `json:"multiple"`
}

type responseDoc struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Changes is how many times the person has changed these answers through
	// the link we sent them: 0 for answers as first sent.
	Changes int `json:"changes"`

	Status string `json:"status"`
	Email  string `json:"email"`

	// Answers has a key for every question the form asks now: a string, a
	// list of strings for a question that takes several, or null for one
	// not answered. Every key every time, so a script can write a column per
	// field without asking whether it is there.
	Answers map[string]any `json:"answers"`
}

// feed answers a script with the form's submissions.
func (a app) feed(w http.ResponseWriter, r *http.Request) {
	slug, err := types.ParseSlug(r.PathValue(mid.FormSlugParam))
	if err != nil {
		a.refuse(w)

		return
	}

	// The key before the form, so that a request without one learns nothing
	// about which forms exist: every refusal is the same 401.
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		a.refuse(w)

		return
	}

	if _, err := a.cfg.Keys.Check(r.Context(), a.now(), slug, strings.TrimSpace(presented)); err != nil {
		if !errors.Is(err, feedbus.ErrRefused) {
			a.cfg.Log.Error("a feed key could not be checked",
				"request_id", web.RequestIDFrom(r.Context()), "form", slug.String(), "error", err)
			a.jsonError(w, http.StatusInternalServerError, "Something went wrong at our end. Try again shortly.")

			return
		}

		a.refuse(w)

		return
	}

	f, err := a.cfg.Forms.ByID(slug)
	if err != nil {
		// A key for a form that has since been deleted.
		a.jsonError(w, http.StatusNotFound, "That form no longer exists.")

		return
	}

	subs, err := a.cfg.Submissions.ByForm(r.Context(), f.ID)
	if err != nil {
		a.cfg.Log.Error("the feed could not read the submissions",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
		a.jsonError(w, http.StatusInternalServerError, "Something went wrong at our end. Try again shortly.")

		return
	}

	counts, err := a.cfg.Submissions.ChangeCounts(r.Context(), f.ID)
	if err != nil {
		a.cfg.Log.Error("the feed could not count the changes",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
		a.jsonError(w, http.StatusInternalServerError, "Something went wrong at our end. Try again shortly.")

		return
	}

	doc := feedOf(f, subs, counts, a.now())

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if err := json.NewEncoder(w).Encode(doc); err != nil {
		a.cfg.Log.Error("the feed could not be written",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
	}
}

// feedOf lays submissions out against the definition.
func feedOf(f formbus.Form, subs []submissionbus.Submission, counts map[types.ID]int, now time.Time) feedDoc {
	doc := feedDoc{
		Form:        f.ID.String(),
		Title:       f.Title,
		GeneratedAt: now.UTC(),
		Fields:      make([]fieldDoc, 0, len(f.Fields)),
		Responses:   make([]responseDoc, 0, len(subs)),
	}

	for _, fld := range f.Questions() {
		doc.Fields = append(doc.Fields, fieldDoc{
			Name:     fld.Name,
			Label:    fld.Label,
			Kind:     string(fld.Kind),
			Multiple: fld.Kind.MultiValue(),
		})
	}

	subs = slices.Clone(subs)
	slices.SortStableFunc(subs, func(x, y submissionbus.Submission) int {
		return x.CreatedAt.Compare(y.CreatedAt)
	})

	for _, s := range subs {
		answers := make(map[string]any, len(f.Fields))

		for _, fld := range f.Questions() {
			ans, ok := s.Answers.Field(fld.Name)

			switch {
			case !ok || len(ans.Values) == 0:
				answers[fld.Name] = nil
			case fld.Kind.MultiValue():
				answers[fld.Name] = ans.Values
			default:
				answers[fld.Name] = ans.Value()
			}
		}

		doc.Responses = append(doc.Responses, responseDoc{
			ID:        s.ID.String(),
			CreatedAt: s.CreatedAt.UTC(),
			UpdatedAt: s.UpdatedAt.UTC(),
			Changes:   counts[s.ID],
			Status:    s.Status.String(),
			Email:     s.Email.String(),
			Answers:   answers,
		})
	}

	return doc
}

// refuse is every reason a key does not read a form.
func (a app) refuse(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="feed"`)
	a.jsonError(w, http.StatusUnauthorized,
		"That key does not read this form. Make one on the form's Spreadsheet page and send it as: Authorization: Bearer <key>")
}

func (a app) jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (a app) form(w http.ResponseWriter, r *http.Request) (formbus.Form, bool) {
	slug, err := types.ParseSlug(r.PathValue(mid.FormSlugParam))
	if err != nil {
		http.Error(w, "there is no form by that name.", http.StatusNotFound)

		return formbus.Form{}, false
	}

	f, err := a.cfg.Forms.ByID(slug)

	switch {
	case errors.Is(err, formbus.ErrNotFound):
		http.Error(w, "there is no form by that name.", http.StatusNotFound)

		return formbus.Form{}, false
	case err != nil:
		a.oops(w, r, "a form could not be read", err)

		return formbus.Form{}, false
	}

	return f, true
}

func (a app) oops(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.cfg.Log.Error(what, "request_id", web.RequestIDFrom(r.Context()), "error", err)
	http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)
}

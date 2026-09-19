// Package formapp is the visual builder: authoring a form in a browser.
//
// It is what makes this repository reusable rather than one-form-specific. The
// feast form ships as a file in forms/, read at startup by formtoml and
// changed by a pull request; everything authored here is a row in SQLite,
// changed by somebody in the parish office who has never seen a terminal.
// Both end up as the same [formbus.Form] and both go through Stamp and Check
// before anybody is served, which is the property that lets the two coexist.
//
// # What guards each route
//
//	GET  /build                      Require, then RequireSiteAdmin(admin)
//	GET  /build/new                  the same
//	POST /build/new                  the same
//	everything under /forms/{slug}/edit
//	                                 Require, then RequireFormRole(admin)
//
// Two gates because there are two questions. Editing a form is admin on that
// form, the same gate peopleapp sits behind and for a recognisably similar
// reason: deciding what a form asks and what it charges is not something
// whoever counts the lunches should be able to do. But making a form cannot be
// a per-form question, since the form does not exist yet and no grant on it
// can, so that one is the site-wide grant.
//
// Which is also why the builder's listing is at /build and behind the
// site-wide gate rather than folded into the /forms page everybody sees. That
// page is built from the catalogue of forms being served, and a draft is by
// definition not in it. Somebody who administers one form reaches its editor
// from the link beside it there; drafts and the New form button belong to
// whoever administers the service, which is who makes them.
//
// A consequence worth naming: a draft cannot be handed to anybody, because
// peopleapp resolves a form through the served catalogue and a draft is not in
// it. Access to a new form is given once it is live, which is the moment there
// is anything to give access to.
//
// # There is no JavaScript, and that is not a constraint being worked around
//
// The management surface's Content-Security-Policy has no script-src at all --
// see app/sdk/page.AdminPolicy, which says adding one should feel like a
// decision. So this builder is not the usual drag-and-drop canvas: it is a
// list of fields with buttons beside them, and every action is a form POST
// that re-renders the page.
//
// That turns out to cost very little and buy something real. What a drag
// handle does here is reorder a list, which two buttons do exactly as well and
// do on a phone, with a keyboard, and in a screen reader. What is genuinely
// lost is a live preview -- and the answer to that is better than a preview
// would have been: the builder links to the form's own public page, which is
// rendered by embedapp from this definition. A preview is a second renderer
// that can disagree with the first; this cannot, because it is the first.
//
// # Draft and live, and why Check is not always enforced
//
// A new form is created unpublished. While it is unpublished it is not in the
// catalogue's snapshot, the embed surface answers 404 for it, and it is
// therefore allowed to be half-finished -- which it has to be, because a form
// with no fields yet fails [formbus.Form.Check] and a builder that refused to
// save one would be a builder you could not start using.
//
// Publishing runs Check and refuses with every problem at once, which is the
// list this app puts on the page. After that each save is checked again, so a
// live form cannot be edited into an unusable state: the change is refused and
// the form keeps serving what it served before. That is why the "add a field"
// form asks for a select's options up front rather than leaving them for the
// next screen -- so that each individual edit lands somewhere valid.
//
// # Editing a live form re-renders the tabs that are open against it
//
// Every save changes the definition's fingerprint, and a submission grant is
// signed over that fingerprint, so every half-filled copy of the form already
// open in somebody's browser stops being submittable and is re-rendered at the
// new definition. That is the mechanism working rather than a cost of using
// it: it is what stops a tab opened at the old price being charged a number
// nobody agreed to. The builder says so, in a sentence, beside the button.
package formapp

import (
	"context"
	"embed"
	"log/slog"
	"net/http"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
)

//go:embed templates
var templates embed.FS

// Templates is this app's own template directory, handed to page.NewRenderer
// by whatever builds the renderer.
var Templates = templates

// Catalog is the slice of the form domain this app needs.
//
// Both halves of it, unlike every other app's Forms interface: this is the one
// package that writes a definition. ByID is the served catalogue and Draft is
// what is being edited, and the difference between them is the draft/live
// bargain in the package comment above.
type Catalog interface {
	ByID(slug types.Slug) (formbus.Form, error)
	All() []formbus.Form
	Editable(slug types.Slug) bool

	Draft(ctx context.Context, slug types.Slug) (formbus.Stored, error)
	Drafts(ctx context.Context) ([]formbus.Stored, error)
	Create(ctx context.Context, now time.Time, who types.ID, f formbus.Form) (formbus.Stored, error)
	Save(ctx context.Context, now time.Time, who types.ID, f formbus.Form, live bool) error
	Delete(ctx context.Context, slug types.Slug) error
}

// Config is what this app needs.
type Config struct {
	Log     *slog.Logger
	Catalog Catalog
	Render  *page.Renderer

	// EmbedBaseURL is the public surface's own origin, and it is what turns
	// this page into something somebody can act on: the two lines they paste
	// into their website, and the link that opens the real form.
	//
	// Optional, because it is a setting an existing installation will not have
	// until somebody adds it, and a service that refuses to start over a
	// missing preview link would be trading a working deploy for a
	// convenience. Without it the builder says which setting to add instead of
	// showing a snippet that names the wrong host -- which is the one outcome
	// worth avoiding, since a wrong host in pasted markup fails silently on
	// somebody else's website.
	EmbedBaseURL string
}

type app struct {
	cfg Config
}

// Routes mounts this app.
//
// guard is Require, admins is RequireFormRole(admin) and site is
// RequireSiteAdmin(admin). All three are handed in by the muxer rather than
// built here: a route's position in the chain is written down in one place and
// is not a decision an app package gets to make.
func Routes(mux *http.ServeMux, cfg Config, guard, admins, site func(http.Handler) http.Handler) {
	a := app{cfg: cfg}

	// Behind the site-wide gate, because there is no form yet to hold a role
	// on. mid.RequireSiteAdmin says more about why that has to be a different
	// gate rather than RequireFormRole with an empty slug.
	mux.Handle("GET /build", guard(site(http.HandlerFunc(a.index))))
	mux.Handle("GET /build/new", guard(site(http.HandlerFunc(a.newForm))))
	mux.Handle("POST /build/new", guard(site(http.HandlerFunc(a.create))))

	behind := func(h http.HandlerFunc) http.Handler {
		return guard(admins(h))
	}

	slug := "/forms/{" + mid.FormSlugParam + "}/edit"

	mux.Handle("GET "+slug, behind(a.build))
	mux.Handle("POST "+slug+"/state", behind(a.state))
	mux.Handle("POST "+slug+"/delete", behind(a.remove))

	mux.Handle("GET "+slug+"/settings", behind(a.settings))
	mux.Handle("POST "+slug+"/settings", behind(a.saveSettings))

	mux.Handle("POST "+slug+"/fields", behind(a.addField))
	mux.Handle("GET "+slug+"/fields/{name}", behind(a.field))
	mux.Handle("POST "+slug+"/fields/{name}", behind(a.saveField))
	mux.Handle("POST "+slug+"/fields/{name}/move", behind(a.moveField))
	mux.Handle("POST "+slug+"/fields/{name}/remove", behind(a.removeField))

	mux.Handle("POST "+slug+"/items", behind(a.addItem))
	mux.Handle("GET "+slug+"/items/{id}", behind(a.item))
	mux.Handle("POST "+slug+"/items/{id}", behind(a.saveItem))
	mux.Handle("POST "+slug+"/items/{id}/move", behind(a.moveItem))
	mux.Handle("POST "+slug+"/items/{id}/remove", behind(a.removeItem))
}

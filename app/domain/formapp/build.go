package formapp

import (
	"errors"
	"net/http"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// indexView is /build: every form this service could serve, and which of them
// can be changed from here.
type indexView struct {
	Built   []indexRow
	Release []indexRow

	Problem string
}

type indexRow struct {
	ID      string
	Title   string
	Live    bool
	Fields  int
	Updated string

	// Ready marks a draft that would survive being published, so the list can
	// distinguish "not finished" from "finished and not switched on".
	Ready bool
}

func (v indexView) Any() bool { return len(v.Built) > 0 }

// index lists what there is.
//
// Two lists, and keeping them apart is the point of the page. The built ones
// have an Edit link; the ones in the release have a sentence saying they are
// changed by a deploy. Running them together under one heading would make the
// difference something somebody discovers by clicking.
func (a app) index(w http.ResponseWriter, r *http.Request) {
	view := indexView{}

	drafts, err := a.cfg.Catalog.Drafts(r.Context())
	if err != nil {
		a.oops(w, r, "the forms being built could not be listed", err)

		return
	}

	for _, s := range drafts {
		row := indexRow{
			ID:      s.Form.ID.String(),
			Title:   s.Form.Title,
			Live:    s.Live,
			Fields:  len(s.Form.Fields),
			Updated: s.UpdatedAt.Local().Format("2 Jan 2006"),
		}

		// On a copy, because Check compiles each pattern into the field it is
		// handed and this one is about to go out of scope.
		check := s.Form
		row.Ready = check.Check() == nil

		view.Built = append(view.Built, row)
	}

	// The built-in definitions, listed because leaving them out would make
	// this page look like the whole catalogue when it is not -- and somebody
	// wondering why the feast form is missing would go looking for a bug.
	for _, f := range a.cfg.Catalog.All() {
		if a.cfg.Catalog.Editable(f.ID) {
			continue
		}

		view.Release = append(view.Release, indexRow{
			ID:     f.ID.String(),
			Title:  f.Title,
			Live:   true,
			Fields: len(f.Fields),
		})
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "build-index", view)
}

// newView is the one question a form is created by answering.
type newView struct {
	Slug  string
	Title string

	Problem string
}

// newForm asks for a name and a title, and nothing else.
//
// Nothing else on purpose. A form is built by adding things to it, and a
// creation page that asked for a currency, a closing date and an embedding
// origin would be a definition typed into one request -- which is the thing
// this package exists not to be. The only value here that cannot be changed
// afterwards is the name, and that is what the page says.
func (a app) newForm(w http.ResponseWriter, r *http.Request) {
	a.cfg.Render.Render(w, r, http.StatusOK, "build-new", newView{})
}

// create records it, unpublished, and goes straight to the builder.
func (a app) create(w http.ResponseWriter, r *http.Request) {
	me, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "the builder was reached with no account", nil)

		return
	}

	if err := r.ParseForm(); err != nil {
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "build-new", newView{
			Problem: "We could not read that. Please try again.",
		})

		return
	}

	said := newView{Slug: value(r, "slug"), Title: value(r, "title")}

	slug, err := types.ParseSlug(said.Slug)
	if err != nil {
		said.Problem = "A form's name is the word that goes in its web address: lowercase letters, digits and hyphens, like feast-lunch-2026."
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "build-new", said)

		return
	}

	if said.Title == "" {
		said.Problem = "Give the form a title. It is the heading people see above it, and it can be changed later."
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "build-new", said)

		return
	}

	// Currency is set here rather than left to the settings page, because a
	// form with none fails Check with a message about ISO 4217 and this
	// service handles exactly one. A form that never sells anything carries it
	// harmlessly; a form that does would otherwise be created broken and stay
	// that way until somebody read the error.
	f := formbus.Form{ID: slug, Title: said.Title, Currency: defaultCurrency}

	if _, err := a.cfg.Catalog.Create(r.Context(), time.Now(), me.ID, f); err != nil {
		if errors.Is(err, formbus.ErrTaken) {
			said.Problem = "There is already a form called " + slug.String() + ". Pick another name."
			a.cfg.Render.Render(w, r, http.StatusConflict, "build-new", said)

			return
		}

		a.oops(w, r, "a form could not be created", err)

		return
	}

	http.Redirect(w, r, "/forms/"+slug.String()+"/edit", http.StatusSeeOther)
}

// defaultCurrency is what a new form is created with. One, because
// formbus.currencies holds one; the list there is the authority and this is
// the starting value, not a second opinion about what is allowed.
const defaultCurrency = "usd"

// build is the builder's home page for one form.
func (a app) build(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}

	a.show(w, r, http.StatusOK, s, buildView{})
}

// state publishes a form or takes it down.
//
// Publishing is the one place [formbus.Form.Check] is run against a definition
// that has never had to pass it, so this is where the to-do list becomes a
// refusal. Taking one down is never refused: a form that should not be on the
// web is a thing somebody needs to be able to do immediately, and it cannot
// leave the definition in a state anybody is served.
func (a app) state(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}

	if err := r.ParseForm(); err != nil {
		a.show(w, r, http.StatusBadRequest, s, buildView{
			Problem: "We could not read that. Please try again.",
		})

		return
	}

	live := r.PostFormValue("state") == "live"

	problems, ok := a.save(w, r, s.Form, live)
	if !ok {
		if problems == nil {
			return
		}

		a.show(w, r, http.StatusConflict, s, buildView{
			Problem:  "This form is not ready to go on the web yet.",
			Problems: problems,
		})

		return
	}

	s.Live = live

	if live {
		a.show(w, r, http.StatusOK, s, buildView{
			Done: "This form is live. Paste the two lines below into the page it belongs on.",
		})

		return
	}

	a.show(w, r, http.StatusOK, s, buildView{
		Done: "This form is no longer on the web. The page it was embedded on now shows nothing, and the submissions it already took are untouched.",
	})
}

// remove deletes a form that has never been published.
//
// The rule is formbus.Business.Delete's and the reasoning is there: a form
// that has been live may have submissions pointing at it, and those rows are
// unreadable without the definition that names their columns. What this
// covers is the form created with the wrong name ten seconds ago.
func (a app) remove(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}

	err := a.cfg.Catalog.Delete(r.Context(), s.Form.ID)

	switch {
	case errors.Is(err, formbus.ErrPublished):
		a.show(w, r, http.StatusConflict, s, buildView{
			Problem: "This form has been on the web, so it cannot be deleted: whatever was submitted to it can only be read through this definition. Take it down instead, which stops it appearing on any page.",
		})

		return
	case err != nil:
		a.oops(w, r, "a form could not be deleted", err)

		return
	}

	a.cfg.Log.Info("a form was deleted from the builder",
		"request_id", web.RequestIDFrom(r.Context()), "form", s.Form.ID.String())

	http.Redirect(w, r, "/build", http.StatusSeeOther)
}

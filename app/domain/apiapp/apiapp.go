// Package apiapp is the JSON API: building forms and reading what was
// submitted to them, from a program rather than a browser.
//
// The reason it exists is that somebody who builds a lot of forms wants to
// describe one and have it made -- most often by asking Claude, which can
// write a whole definition in one go far more easily than it can click
// through the builder. docs/api.md is the reference, written to be handed to
// whoever, or whatever, is going to call it.
//
// # Who is asking, and what they may do
//
// A request carries a personal key, made on the account page; apikeybus says
// why that is a key and what keeps it from being more. A key is its account:
// every route here sits behind the same accessbus gate the matching page sits
// behind, so a key can do exactly what its owner can do in the builder and
// nothing more. The gates are handed in by the muxer, which is where a route's
// position in a chain is decided.
//
//	GET    /api/v1/me                             a key
//	GET    /api/v1/forms                          a key; lists what it reaches
//	POST   /api/v1/forms                          RequireFormCreator
//	GET    /api/v1/forms/{slug}                   RequireFormRole(admin)
//	PUT    /api/v1/forms/{slug}                   the same
//	DELETE /api/v1/forms/{slug}                   the same
//	POST   /api/v1/forms/{slug}/publish           the same
//	POST   /api/v1/forms/{slug}/unpublish         the same
//	GET    /api/v1/forms/{slug}/submissions       RequireFormRole(results)
//	GET    /api/v1/forms/{slug}/submissions/{id}  the same
//
// Reading a definition is admin, as the builder is, rather than results: the
// definition carries the notification addresses and the payment settings,
// which are the administrator's business, and whoever counts lunches reads the
// submissions with their questions already attached.
//
// # A whole definition at a time
//
// The builder edits a form one field at a time because a person does. A
// program does not, and an API that made it add fields one request at a time
// would be the builder's page structure with the HTML taken off. So a form is
// made from a whole definition and replaced by a whole definition, and
// formbus.Revise keeps the builder's per-edit rules across the difference:
// new fields are named, dropped ones are retired, and a field cannot change
// its kind.
//
// The draft/live bargain is the builder's, unchanged. A draft may be
// half-finished and is stored as sent, with what is wrong with it listed in
// the answer's problems; publishing runs Check and refuses with all of them;
// and a live form cannot be replaced by a definition that would not pass.
//
// # What is deliberately not here
//
// Managing who may see a form, hiding a submission, the will-call table and
// the spreadsheet feed's keys. None was asked for, and each is a decision about
// other people's access or about the record of what was paid that is better
// made by a person on the page built for it. Nor can a key make or revoke a
// key: that is the account page, behind a session, so a key that leaks cannot
// leave another behind.
package apiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// Catalog is the form domain, both halves, as formapp has it.
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

// Grants is what this app asks of the access domain: what an account holds,
// for the list of forms, and the grant that makes a form's creator its
// administrator, which formapp makes for the same reason.
type Grants interface {
	Allowed(ctx context.Context, userID types.ID, form types.Slug, want accessbus.Role) (bool, error)
	CanCreateForms(ctx context.Context, userID types.ID) (bool, error)
	Grant(ctx context.Context, now time.Time, granter, userID types.ID, form types.Slug, role accessbus.Role) (accessbus.Grant, error)
}

// Submissions is the read half of the submission domain. Nothing here writes
// one.
type Submissions interface {
	ByForm(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error)
	ByID(ctx context.Context, id types.ID) (submissionbus.Submission, error)
}

// Config is what this app needs.
type Config struct {
	Log         *slog.Logger
	Catalog     Catalog
	Grants      Grants
	Submissions Submissions

	// EmbedBaseURL is the public surface's origin, for the address and the
	// paste snippet of a live form. Optional, as it is for the builder.
	EmbedBaseURL string

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// maxBody is the most a definition may weigh. The largest real form here is a
// few kilobytes; this is two orders of magnitude above it and still a bound on
// what one key can make the service hold in memory.
const maxBody = 1 << 20

type app struct {
	cfg Config
}

func (a app) now() time.Time {
	if a.cfg.Now != nil {
		return a.cfg.Now()
	}

	return time.Now()
}

// Prefix is where this app's routes live, which the muxer needs in order to
// send them to the API's chain rather than the browser's.
const Prefix = "/api/"

// Routes mounts this app. admins is RequireFormRole(admin), results is
// RequireFormRole(results), and creator is RequireFormCreator, all handed in
// by the muxer; who is asking was settled before any of them, by mid.Bearer.
func Routes(mux *http.ServeMux, cfg Config, admins, results, creator func(http.Handler) http.Handler) {
	a := app{cfg: cfg}

	const v1 = "/api/v1"

	form := v1 + "/forms/{" + mid.FormSlugParam + "}"

	mux.HandleFunc("GET "+v1+"/me", a.me)
	mux.HandleFunc("GET "+v1+"/forms", a.list)
	mux.Handle("POST "+v1+"/forms", creator(http.HandlerFunc(a.create)))

	mux.Handle("GET "+form, admins(http.HandlerFunc(a.read)))
	mux.Handle("PUT "+form, admins(http.HandlerFunc(a.replace)))
	mux.Handle("DELETE "+form, admins(http.HandlerFunc(a.remove)))
	mux.Handle("POST "+form+"/publish", admins(http.HandlerFunc(a.publish)))
	mux.Handle("POST "+form+"/unpublish", admins(http.HandlerFunc(a.unpublish)))

	mux.Handle("GET "+form+"/submissions", results(http.HandlerFunc(a.submissions)))
	mux.Handle("GET "+form+"/submissions/{id}", results(http.HandlerFunc(a.submission)))

	// Anything else under the prefix is a JSON 404 rather than the plain
	// text net/http would write, for the reason mid.refuse gives.
	mux.HandleFunc(Prefix, func(w http.ResponseWriter, _ *http.Request) {
		mid.WriteJSONError(w, http.StatusNotFound, "There is nothing at that address. docs/api.md lists what there is.")
	})
}

// --- who is asking -----------------------------------------------------------

func (a app) me(w http.ResponseWriter, r *http.Request) {
	u, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "the api was reached with no account", nil)

		return
	}

	can, err := a.cfg.Grants.CanCreateForms(r.Context(), u.ID)
	if err != nil {
		a.oops(w, r, "the site-wide grant could not be checked", err)

		return
	}

	a.reply(w, http.StatusOK, map[string]any{
		"email":            u.Email.String(),
		"name":             u.Name,
		"can_create_forms": can,
		"field_kinds":      formbus.Kinds(),
	})
}

// --- the list ------------------------------------------------------------------

// list is every form this account holds a role on, drafts included where it
// administers them.
//
// Asked of Allowed form by form rather than worked out from the grant rows,
// so that there is one authority on who may reach what. It is a handful of
// indexed reads per form, and a parish has tens of forms.
func (a app) list(w http.ResponseWriter, r *http.Request) {
	u, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "the api was reached with no account", nil)

		return
	}

	drafts, err := a.cfg.Catalog.Drafts(r.Context())
	if err != nil {
		a.oops(w, r, "the authored forms could not be listed", err)

		return
	}

	type candidate struct {
		form     formbus.Form
		live     bool
		updated  time.Time
		authored bool
	}

	seen := map[types.Slug]bool{}

	var all []candidate

	for _, s := range drafts {
		seen[s.Form.ID] = true
		all = append(all, candidate{form: s.Form, live: s.Live, updated: s.UpdatedAt, authored: true})
	}

	for _, f := range a.cfg.Catalog.All() {
		if !seen[f.ID] {
			all = append(all, candidate{form: f, live: true})
		}
	}

	out := []summaryDoc{}

	for _, c := range all {
		role, err := a.strongest(r.Context(), u.ID, c.form.ID)
		if err != nil {
			a.oops(w, r, "a grant could not be checked", err)

			return
		}

		// A draft only to its administrators, as on the landing page: a form
		// nobody can fill in yet has no submissions for a reader to read.
		if role.Zero() || (!c.live && !role.Includes(accessbus.RoleAdmin)) {
			continue
		}

		out = append(out, summaryDoc{
			Slug:      c.form.ID.String(),
			Title:     c.form.Title,
			Live:      c.live,
			Editable:  c.authored,
			Role:      role.String(),
			UpdatedAt: c.updated.UTC(),
		})
	}

	slices.SortFunc(out, func(x, y summaryDoc) int { return strings.Compare(x.Slug, y.Slug) })

	a.reply(w, http.StatusOK, map[string]any{"forms": out})
}

// strongest is the widest of the per-form roles this account holds on a form,
// or the zero role for none.
func (a app) strongest(ctx context.Context, userID types.ID, form types.Slug) (accessbus.Role, error) {
	for _, role := range []accessbus.Role{accessbus.RoleAdmin, accessbus.RoleDoor, accessbus.RoleResults} {
		ok, err := a.cfg.Grants.Allowed(ctx, userID, form, role)
		if err != nil {
			return accessbus.Role(""), err
		}

		if ok {
			return role, nil
		}
	}

	return accessbus.Role(""), nil
}

// --- one form ------------------------------------------------------------------

// create makes a form from a whole definition, unpublished.
func (a app) create(w http.ResponseWriter, r *http.Request) {
	me, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "the api was reached with no account", nil)

		return
	}

	d, ok := a.decode(w, r)
	if !ok {
		return
	}

	slug, err := types.ParseSlug(d.Slug)
	if err != nil {
		a.fail(w, http.StatusUnprocessableEntity,
			"A new form needs a slug: lowercase letters, digits and hyphens, like feast-lunch-2026. It is the word in the form's address and cannot be changed later.")

		return
	}

	if strings.TrimSpace(d.Title) == "" {
		a.fail(w, http.StatusUnprocessableEntity, "Give the form a title. It is the heading people see above it.")

		return
	}

	f, problems := formOf(d)
	if len(problems) > 0 {
		a.problems(w, problems)

		return
	}

	f, err = formbus.Revise(formbus.Form{ID: slug}, f)
	if bad, is := errors.AsType[formbus.DefinitionError](err); is {
		a.problems(w, bad.Problems)

		return
	}

	now := a.now()

	switch _, err := a.cfg.Catalog.Create(r.Context(), now, me.ID, f); {
	case errors.Is(err, formbus.ErrTaken):
		a.fail(w, http.StatusConflict, "There is already a form called "+slug.String()+". Pick another slug.")

		return
	case err != nil:
		a.oops(w, r, "a form could not be created", err)

		return
	}

	// Its creator administers it, for the reason formapp.create gives: an
	// account holding only the creator role could not otherwise reach the
	// form it has just made. Logged rather than refused if it fails, because
	// the form exists either way.
	if _, err := a.cfg.Grants.Grant(r.Context(), now, me.ID, me.ID, slug, accessbus.RoleAdmin); err != nil {
		a.cfg.Log.Error("the form was created but its creator could not be made its administrator",
			"request_id", web.RequestIDFrom(r.Context()), "form", slug.String(), "user_id", me.ID.String(), "error", err)
	}

	s, err := a.cfg.Catalog.Draft(r.Context(), slug)
	if err != nil {
		a.oops(w, r, "a form just created could not be read back", err)

		return
	}

	w.Header().Set("Location", "/api/v1/forms/"+slug.String())
	a.reply(w, http.StatusCreated, a.docOf(s, true))
}

func (a app) read(w http.ResponseWriter, r *http.Request) {
	slug, ok := a.slug(w, r)
	if !ok {
		return
	}

	// A form that ships with the service is readable and not editable, and
	// says so rather than answering 404 about a form the caller can see.
	if !a.cfg.Catalog.Editable(slug) {
		f, err := a.cfg.Catalog.ByID(slug)
		if err != nil {
			a.notFound(w)

			return
		}

		a.reply(w, http.StatusOK, a.docOf(formbus.Stored{Form: f, Live: true}, false))

		return
	}

	s, ok := a.load(w, r, slug)
	if !ok {
		return
	}

	a.reply(w, http.StatusOK, a.docOf(s, true))
}

// replace writes a whole new definition over a form, keeping whether it is
// live.
func (a app) replace(w http.ResponseWriter, r *http.Request) {
	s, ok := a.editable(w, r)
	if !ok {
		return
	}

	d, ok := a.decode(w, r)
	if !ok {
		return
	}

	if d.Slug != "" && d.Slug != s.Form.ID.String() {
		a.fail(w, http.StatusUnprocessableEntity,
			"A form's slug cannot be changed. Leave it out, or send "+s.Form.ID.String()+"; to use another, make a new form.")

		return
	}

	f, problems := formOf(d)
	if len(problems) > 0 {
		a.problems(w, problems)

		return
	}

	f, err := formbus.Revise(s.Form, f)
	if bad, is := errors.AsType[formbus.DefinitionError](err); is {
		a.problems(w, bad.Problems)

		return
	}

	a.save(w, r, f, s.Live)
}

func (a app) publish(w http.ResponseWriter, r *http.Request) {
	s, ok := a.editable(w, r)
	if !ok {
		return
	}

	a.save(w, r, s.Form, true)
}

// unpublish is never refused, for the reason the builder's is not: a form
// that should not be on the web is a thing somebody needs to be able to do
// immediately.
func (a app) unpublish(w http.ResponseWriter, r *http.Request) {
	s, ok := a.editable(w, r)
	if !ok {
		return
	}

	a.save(w, r, s.Form, false)
}

// remove deletes a form that has never been live. formbus.Business.Delete
// says why one that has been cannot be.
func (a app) remove(w http.ResponseWriter, r *http.Request) {
	s, ok := a.editable(w, r)
	if !ok {
		return
	}

	err := a.cfg.Catalog.Delete(r.Context(), s.Form.ID)

	switch {
	case errors.Is(err, formbus.ErrPublished):
		a.fail(w, http.StatusConflict,
			"This form has been on the web, so it cannot be deleted: whatever was submitted to it can only be read through this definition. Unpublish it instead.")

		return
	case err != nil:
		a.oops(w, r, "a form could not be deleted", err)

		return
	}

	a.cfg.Log.Info("a form was deleted through the api",
		"request_id", web.RequestIDFrom(r.Context()), "form", s.Form.ID.String())

	w.WriteHeader(http.StatusNoContent)
}

// save writes a definition back and answers with the form as it now stands,
// or with what stopped it.
func (a app) save(w http.ResponseWriter, r *http.Request, f formbus.Form, live bool) {
	me, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "the api was reached with no account", nil)

		return
	}

	err := a.cfg.Catalog.Save(r.Context(), a.now(), me.ID, f, live)

	if bad, is := errors.AsType[formbus.DefinitionError](err); is {
		a.problems(w, bad.Problems)

		return
	}

	if err != nil {
		a.oops(w, r, "a form could not be saved", err)

		return
	}

	s, err := a.cfg.Catalog.Draft(r.Context(), f.ID)
	if err != nil {
		a.oops(w, r, "a form just saved could not be read back", err)

		return
	}

	a.reply(w, http.StatusOK, a.docOf(s, true))
}

// docOf is the answer about one form.
func (a app) docOf(s formbus.Stored, editable bool) formDoc {
	doc := formDoc{
		Slug:        s.Form.ID.String(),
		Live:        s.Live,
		Editable:    editable,
		Problems:    []string{},
		Retired:     slices.Clone(s.Form.Retired),
		CreatedAt:   s.CreatedAt.UTC(),
		UpdatedAt:   s.UpdatedAt.UTC(),
		PublishedAt: s.PublishedAt.UTC(),
		Definition:  definitionOf(s.Form),
	}

	if doc.Retired == nil {
		doc.Retired = []string{}
	}

	// On a copy, because Check compiles each pattern into the form it is
	// handed, and stamps nothing -- the version is the stored one, which
	// Draft has already recomputed.
	check := s.Form
	if bad, is := errors.AsType[formbus.DefinitionError](check.Check()); is {
		doc.Problems = bad.Problems
	}

	doc.Version = s.Form.Version

	if a.cfg.EmbedBaseURL != "" && s.Live {
		doc.PublicURL = a.cfg.EmbedBaseURL + "/f/" + doc.Slug
		doc.EmbedHTML = fmt.Sprintf("<div data-dropin-form=%q></div>\n<script src=%q async></script>",
			doc.Slug, a.cfg.EmbedBaseURL+"/embed.js")
	}

	return doc
}

// --- submissions -----------------------------------------------------------------

func (a app) submissions(w http.ResponseWriter, r *http.Request) {
	f, ok := a.definition(w, r)
	if !ok {
		return
	}

	subs, err := a.cfg.Submissions.ByForm(r.Context(), f.ID)
	if err != nil {
		a.oops(w, r, "the submissions could not be read", err)

		return
	}

	// Oldest first, as the feed has them, so that a program writing them out
	// in order keeps each person where they were last time. The store's list
	// turned over rather than sorted again by time: it is newest first in the
	// exact order of arrival, ties included, and a sort by time alone would
	// put two submissions from the same millisecond back the wrong way round.
	subs = slices.Clone(subs)
	slices.Reverse(subs)

	doc := submissionsDoc{
		Form:        f.ID.String(),
		Title:       f.Title,
		GeneratedAt: a.now().UTC(),
		Fields:      []questionDoc{},
		Submissions: []submissionDoc{},
	}

	for _, fld := range f.Questions() {
		doc.Fields = append(doc.Fields, questionDoc{
			Name:     fld.Name,
			Label:    fld.Label,
			Kind:     string(fld.Kind),
			Multiple: fld.Kind.MultiValue(),
		})
	}

	for _, it := range f.Items {
		doc.Items = append(doc.Items, itemSummaryDoc{ID: it.ID, Label: it.Label, Price: int64(it.Price)})
	}

	for _, s := range subs {
		doc.Submissions = append(doc.Submissions, submissionOf(f, s))
	}

	a.reply(w, http.StatusOK, doc)
}

func (a app) submission(w http.ResponseWriter, r *http.Request) {
	f, ok := a.definition(w, r)
	if !ok {
		return
	}

	id, err := types.ParseID(r.PathValue("id"))
	if err != nil {
		a.fail(w, http.StatusNotFound, "There is no such submission on this form.")

		return
	}

	s, err := a.cfg.Submissions.ByID(r.Context(), id)

	switch {
	case errors.Is(err, submissionbus.ErrNotFound):
		a.fail(w, http.StatusNotFound, "There is no such submission on this form.")

		return
	case err != nil:
		a.oops(w, r, "a submission could not be read", err)

		return
	}

	// The same answer as for one that does not exist. The gate let this
	// account through for the form in the path, and a submission on another
	// form is not something that grant reaches -- nor is whether it exists.
	if s.Form != f.ID {
		a.fail(w, http.StatusNotFound, "There is no such submission on this form.")

		return
	}

	a.reply(w, http.StatusOK, submissionOf(f, s))
}

// definition is the form a submission is read against: the one being served,
// or, for a form that has been taken down since, its stored definition --
// whose answers are still somebody's to read.
func (a app) definition(w http.ResponseWriter, r *http.Request) (formbus.Form, bool) {
	slug, ok := a.slug(w, r)
	if !ok {
		return formbus.Form{}, false
	}

	f, err := a.cfg.Catalog.ByID(slug)
	if err == nil {
		return f, true
	}

	if !errors.Is(err, formbus.ErrNotFound) {
		a.oops(w, r, "a form could not be read", err)

		return formbus.Form{}, false
	}

	if !a.cfg.Catalog.Editable(slug) {
		a.notFound(w)

		return formbus.Form{}, false
	}

	s, ok := a.load(w, r, slug)

	return s.Form, ok
}

// --- the plumbing ----------------------------------------------------------------

func (a app) slug(w http.ResponseWriter, r *http.Request) (types.Slug, bool) {
	slug, err := types.ParseSlug(r.PathValue(mid.FormSlugParam))
	if err != nil {
		a.notFound(w)

		return types.Slug{}, false
	}

	return slug, true
}

// load reads an authored definition, live or not.
func (a app) load(w http.ResponseWriter, r *http.Request, slug types.Slug) (formbus.Stored, bool) {
	s, err := a.cfg.Catalog.Draft(r.Context(), slug)

	switch {
	case errors.Is(err, formbus.ErrNotFound):
		a.notFound(w)

		return formbus.Stored{}, false
	case err != nil:
		a.oops(w, r, "a form could not be read", err)

		return formbus.Stored{}, false
	}

	return s, true
}

// editable is load for a write: a form that ships with the service is
// refused with a sentence about why, rather than a 404 about a form the
// caller can read.
func (a app) editable(w http.ResponseWriter, r *http.Request) (formbus.Stored, bool) {
	slug, ok := a.slug(w, r)
	if !ok {
		return formbus.Stored{}, false
	}

	if !a.cfg.Catalog.Editable(slug) {
		a.fail(w, http.StatusConflict,
			"That form is part of this release of the service and is changed by a new release, not here.")

		return formbus.Stored{}, false
	}

	return a.load(w, r, slug)
}

// decode reads a definition out of the body.
//
// Unknown keys are refused rather than ignored. A program that misspells
// "required" as "requried" would otherwise be told its form was saved, and
// find out from the first person who skipped the question.
func (a app) decode(w http.ResponseWriter, r *http.Request) (definitionDoc, bool) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		a.fail(w, http.StatusUnsupportedMediaType, "Send the definition as JSON, with Content-Type: application/json.")

		return definitionDoc{}, false
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()

	var d definitionDoc

	if err := dec.Decode(&d); err != nil {
		if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
			a.fail(w, http.StatusRequestEntityTooLarge, "That definition is larger than a form can be.")

			return definitionDoc{}, false
		}

		a.fail(w, http.StatusBadRequest, "That is not a form definition this service can read: "+err.Error())

		return definitionDoc{}, false
	}

	// One document, not a stream of them.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		a.fail(w, http.StatusBadRequest, "Send one definition, with nothing after it.")

		return definitionDoc{}, false
	}

	return d, true
}

func (a app) reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(body); err != nil {
		a.cfg.Log.Error("an api answer could not be written", "error", err)
	}
}

func (a app) fail(w http.ResponseWriter, status int, msg string) {
	mid.WriteJSONError(w, status, msg)
}

// problems is a definition that is not usable as sent: every reason, so that
// a program can fix them all in one more request.
func (a app) problems(w http.ResponseWriter, problems []string) {
	a.reply(w, http.StatusUnprocessableEntity, map[string]any{
		"error":    "The form was not saved, because of the problems listed.",
		"problems": problems,
	})
}

func (a app) notFound(w http.ResponseWriter) {
	a.fail(w, http.StatusNotFound, "There is no form by that name.")
}

func (a app) oops(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.cfg.Log.Error(what, "request_id", web.RequestIDFrom(r.Context()), "error", err)
	a.fail(w, http.StatusInternalServerError, "Something went wrong at our end. Try again shortly.")
}

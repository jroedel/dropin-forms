package formbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jroedel/dropin-forms/business/types"
)

// ErrNotFound is returned for a slug no definition claims.
//
// It lives here rather than in a store because there are two stores, and a
// caller asking "is this form missing" should not have to know which of them
// was consulted. formtoml.ErrNotFound is this same value.
var ErrNotFound = errors.New("no such form")

// ErrBuiltIn is returned by any write naming a definition that ships in the
// binary. Those are files in the repository and a pull request is how they
// change; a browser that could edit one would be writing a definition the next
// deploy silently reverts.
var ErrBuiltIn = errors.New("that form is part of this release and cannot be edited here")

// ErrTaken is returned when a new form asks for a slug something already
// answers to. The slug is in the URL a form is embedded by, so two definitions
// claiming one is not a conflict to resolve but a collision to refuse.
var ErrTaken = errors.New("there is already a form with that name")

// ErrPublished is returned by Delete for a form that has been live at some
// point. See Delete, which is emphatic about why that is not the same rule as
// "is live right now".
var ErrPublished = errors.New("a form that has been published cannot be deleted, only taken down")

// Stored is one authored definition and what is known about it beyond the
// definition itself.
//
// The bookkeeping is separate from [Form] on purpose. A Form is what a
// submission is judged against and what the fingerprint is taken over; who
// last touched it and when are facts about the editing of it, and mixing the
// two would put an author's account id inside the hash that decides whether
// somebody's half-filled form is still valid.
type Stored struct {
	Form Form

	// Live says whether this definition is served. An unpublished one is
	// visible only in the builder: it is not in the snapshot, the embed
	// surface answers 404 for it, and it is therefore allowed to be
	// half-finished. That is the whole reason the flag exists -- [Form.Check]
	// is an unforgiving thing to hold a form to while somebody is still adding
	// its second field.
	Live bool

	CreatedAt time.Time
	UpdatedAt time.Time
	UpdatedBy types.ID

	// PublishedAt is when this definition first went live, and stays set after
	// it is taken down again. Delete reads it; nothing else does.
	PublishedAt time.Time
}

// Storer is where definitions authored in a browser are kept.
//
// Upsert receives a Stored whose Form has already been stamped and, when it is
// going live, checked. A store may assume both and must not re-derive either:
// two places computing a version is two places for it to differ.
type Storer interface {
	All(ctx context.Context) ([]Stored, error)
	ByID(ctx context.Context, slug types.Slug) (Stored, error)
	Upsert(ctx context.Context, s Stored) error
	Delete(ctx context.Context, slug types.Slug) error
}

// BuiltIn is the read-only source: the definitions compiled into the binary,
// which formtoml.Store satisfies.
type BuiltIn interface {
	ByID(slug types.Slug) (Form, error)
	All() []Form
}

// Business is every definition this service will serve, from both sources, and
// the rules for authoring the ones that can be authored.
//
// # Why there is a snapshot
//
// Reads are answered from an in-memory map that is replaced wholesale on every
// write, and [Business.ByID] therefore takes no context and cannot reach the
// database. That is a deliberate shape and not an optimisation reached for
// late.
//
// The read path is not what it looks like. Besides every request for a form,
// the per-form Content-Security-Policy is built above the mux -- see
// app/sdk/page.FormFrameAncestors -- which means a definition is looked up
// before routing on every request that surface takes, including ones for the
// stylesheet. Putting a query there would make a database round trip a
// precondition of answering anything at all, and would make an unreachable
// database present as a page nobody is allowed to frame rather than as an
// error.
//
// The write path is a person in a browser editing a handful of documents, so
// rebuilding the whole map on each save costs nothing worth measuring. And the
// snapshot holds only definitions that have passed [Form.Check], which is what
// lets every reader keep assuming that a Form it is handed has had its
// patterns compiled and its rules verified.
type Business struct {
	log     *slog.Logger
	store   Storer
	builtIn BuiltIn

	// live is *map[string]Form. Replaced, never mutated: a reader holding the
	// old map keeps serving the old definitions for the rest of its request,
	// which is a better answer than a map being written under it.
	live atomic.Pointer[map[string]Form]
}

// NewBusiness builds the catalogue and loads it.
//
// It returns an error if the stored definitions cannot be read at all, because
// a service that starts with an empty catalogue looks exactly like a service
// whose forms were all deleted. An individual definition that no longer passes
// Check is a different case and does not stop the process -- see refresh.
//
// builtIn may be nil, which is a catalogue holding only what was authored in a
// browser. store may not: without one there is nothing this type adds over the
// file store it would be wrapping.
func NewBusiness(log *slog.Logger, store Storer, builtIn BuiltIn) (*Business, error) {
	if log == nil {
		return nil, errors.New("the form catalogue needs a logger; a definition that stops being served says so in the log and nowhere else")
	}
	if store == nil {
		return nil, errors.New("the form catalogue needs somewhere to keep the forms that are authored in a browser")
	}

	b := Business{log: log, store: store, builtIn: builtIn}

	if err := b.refresh(context.Background()); err != nil {
		return nil, err
	}

	return &b, nil
}

// ByID returns the definition for a slug, from whichever source has it.
//
// The signature is the one every reader in this service already had against
// the file store, which is why adding a second source changed no caller.
func (b *Business) ByID(slug types.Slug) (Form, error) {
	f, ok := (*b.live.Load())[slug.String()]
	if !ok {
		return Form{}, fmt.Errorf("%w: %s", ErrNotFound, slug)
	}

	return f, nil
}

// All returns every definition being served, ordered by slug so that a listing
// page and a startup log line are stable.
func (b *Business) All() []Form {
	live := *b.live.Load()

	out := make([]Form, 0, len(live))
	for _, f := range live {
		out = append(out, f)
	}

	slices.SortFunc(out, func(x, y Form) int {
		return strings.Compare(x.ID.String(), y.ID.String())
	})

	return out
}

// Editable reports whether this slug names a form a browser may change, which
// is to say one that is not compiled into the binary.
//
// A slug nothing claims is editable, because the thing a caller does next with
// that answer is offer to create it.
func (b *Business) Editable(slug types.Slug) bool { return !b.isBuiltIn(slug) }

// Draft reads one authored definition, live or not.
//
// Separate from ByID, and the difference is the whole of what this method is
// for: ByID answers what the public is being served and Draft answers what the
// builder is editing. A form still being written has no answer from the first
// and an answer from the second.
func (b *Business) Draft(ctx context.Context, slug types.Slug) (Stored, error) {
	s, err := b.store.ByID(ctx, slug)
	if err != nil {
		return Stored{}, err
	}

	// Stamped on the way out as well as on the way in. The version is derived
	// from the content, so recomputing it is what makes it true rather than
	// what makes it change -- and it is the value Check will compare against.
	s.Form.Stamp()

	return s, nil
}

// Drafts is every authored definition, for the builder's own listing. The
// built-in ones are not here: they are listed by what serves them.
func (b *Business) Drafts(ctx context.Context) ([]Stored, error) {
	all, err := b.store.All(ctx)
	if err != nil {
		return nil, err
	}

	for i := range all {
		all[i].Form.Stamp()
	}

	slices.SortFunc(all, func(x, y Stored) int {
		return strings.Compare(x.Form.ID.String(), y.Form.ID.String())
	})

	return all, nil
}

// Create records a new definition, unpublished.
//
// Unpublished always, whatever the caller wants, and that is not a
// convenience. A form is created with a name and nothing else; holding it to
// Check at that moment would mean the only way to make one is to type a whole
// definition into a single request, which is the thing a builder exists not to
// be.
func (b *Business) Create(ctx context.Context, now time.Time, who types.ID, f Form) (Stored, error) {
	if f.ID.Zero() {
		return Stored{}, errors.New("a form needs a name")
	}

	if b.isBuiltIn(f.ID) {
		return Stored{}, fmt.Errorf("%w: %s", ErrTaken, f.ID)
	}

	switch _, err := b.store.ByID(ctx, f.ID); {
	case err == nil:
		return Stored{}, fmt.Errorf("%w: %s", ErrTaken, f.ID)
	case !errors.Is(err, ErrNotFound):
		return Stored{}, fmt.Errorf("looking for an existing form called %s: %w", f.ID, err)
	}

	f.Stamp()

	s := Stored{
		Form:      f,
		Live:      false,
		CreatedAt: now.UTC(),
		UpdatedAt: now.UTC(),
		UpdatedBy: who,
	}

	if err := b.store.Upsert(ctx, s); err != nil {
		return Stored{}, fmt.Errorf("creating the form %s: %w", f.ID, err)
	}

	b.log.Info("a form was created", "form", f.ID.String(), "by", who.String())

	return s, nil
}

// Save writes a definition back, and is the one place the live/draft bargain
// is enforced.
//
// A definition going live is stamped and checked, and a failing Check is
// returned as the [DefinitionError] it is, with every problem rather than the
// first -- the builder puts that list on the page, and fixing a form one save
// at a time is the bad afternoon DefinitionError's own comment describes.
//
// A definition that is not going live is stamped and stored as it stands. It
// is not served, so there is nothing it can break; refusing to store it would
// mean losing the half of a form somebody has typed so far.
//
// Editing a form that is already live changes its fingerprint, which
// invalidates every submission grant in flight against it. That is the
// mechanism working rather than a cost of using it: a tab left open at the old
// price is re-rendered at the new one instead of being charged a number nobody
// agreed to. [Form.Version] says so at length.
func (b *Business) Save(ctx context.Context, now time.Time, who types.ID, f Form, live bool) error {
	if b.isBuiltIn(f.ID) {
		return fmt.Errorf("%w: %s", ErrBuiltIn, f.ID)
	}

	was, err := b.store.ByID(ctx, f.ID)
	if err != nil {
		return err
	}

	f.Stamp()

	if live {
		if err := f.Check(); err != nil {
			return err
		}
	}

	was.Form = f
	was.Live = live
	was.UpdatedAt = now.UTC()
	was.UpdatedBy = who

	if live && was.PublishedAt.IsZero() {
		was.PublishedAt = now.UTC()
	}

	if err := b.store.Upsert(ctx, was); err != nil {
		return fmt.Errorf("saving the form %s: %w", f.ID, err)
	}

	if err := b.refresh(ctx); err != nil {
		return err
	}

	b.log.Info("a form was saved",
		"form", f.ID.String(), "version", f.Version, "live", live, "by", who.String())

	return nil
}

// Delete removes a definition that has never been published.
//
// Never published, rather than not published right now, and the difference is
// the point. Submissions record the slug of the form they were made against
// and are read back through that definition -- it is where the columns and
// their meanings come from. Deleting a form that has taken even one submission
// leaves rows nobody can interpret, and no confirmation dialog makes that
// recoverable. A form that has been live is taken down instead, with Save.
//
// What this does cover is the common mistake: a form created with the wrong
// name, or started and abandoned, which by construction cannot have taken a
// submission because it was never served.
func (b *Business) Delete(ctx context.Context, slug types.Slug) error {
	if b.isBuiltIn(slug) {
		return fmt.Errorf("%w: %s", ErrBuiltIn, slug)
	}

	s, err := b.store.ByID(ctx, slug)
	if err != nil {
		return err
	}

	if !s.PublishedAt.IsZero() {
		return fmt.Errorf("%w: %s", ErrPublished, slug)
	}

	if err := b.store.Delete(ctx, slug); err != nil {
		return fmt.Errorf("deleting the form %s: %w", slug, err)
	}

	if err := b.refresh(ctx); err != nil {
		return err
	}

	b.log.Info("a form was deleted", "form", slug.String())

	return nil
}

// isBuiltIn reports whether a definition of this name ships in the binary.
func (b *Business) isBuiltIn(slug types.Slug) bool {
	if b.builtIn == nil {
		return false
	}

	_, err := b.builtIn.ByID(slug)

	return err == nil
}

// refresh rebuilds the snapshot from both sources.
//
// Three things it does that are worth saying out loud:
//
// It stamps and checks every stored definition rather than trusting the
// version it was saved with, exactly as formtoml does for a file. The version
// is derived, so recomputing it is what keeps it true -- and the compile of
// each field's pattern happens inside Check, which is why a form that has not
// been through it must never reach a handler.
//
// A stored definition that no longer passes Check is dropped from the snapshot
// and logged as an error, rather than stopping the process. The case this is
// written for is a release that tightens a rule: refusing to start would take
// down every other form on the service, and on a host where the way in is a
// browser, would take down the page somebody would fix it from.
//
// A stored definition whose slug a built-in also claims loses. That can only
// happen by adding a .toml file for a name somebody had already built, since
// Create refuses the collision in the other direction -- so the built-in is
// the newer decision and the one the deploy just made. It is logged loudly,
// because the visible symptom is otherwise a form that quietly stops being the
// one somebody edited.
func (b *Business) refresh(ctx context.Context) error {
	live := map[string]Form{}

	if b.builtIn != nil {
		for _, f := range b.builtIn.All() {
			live[f.ID.String()] = f
		}
	}

	stored, err := b.store.All(ctx)
	if err != nil {
		return fmt.Errorf("the forms that were authored in a browser could not be read: %w", err)
	}

	for _, s := range stored {
		if !s.Live {
			continue
		}

		name := s.Form.ID.String()

		if _, shadowed := live[name]; shadowed {
			b.log.Error("a form authored here has the same name as one in this release, and the one in this release is being served",
				"form", name,
				"remedy", "rename the built form, or take it down, in the builder")

			continue
		}

		f := s.Form
		f.Stamp()

		if err := f.Check(); err != nil {
			b.log.Error("a published form is no longer usable and is not being served",
				"form", name, "error", err)

			continue
		}

		live[name] = f
	}

	b.live.Store(&live)

	return nil
}

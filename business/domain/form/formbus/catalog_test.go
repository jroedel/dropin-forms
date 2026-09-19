package formbus_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// The catalogue's tests run against a map rather than SQLite. What is being
// asserted here is the rules -- what may be edited, what has to pass Check and
// when, what is served -- and none of them is a property of storage. The store
// has its own tests.

type memStore struct {
	rows map[string]formbus.Stored

	// fail, when set, is what every method returns. One field rather than a
	// method per failure, because every test that wants one wants the same
	// thing: the catalogue refusing to build rather than starting empty.
	fail error
}

func newMem(forms ...formbus.Stored) *memStore {
	m := memStore{rows: map[string]formbus.Stored{}}

	for _, s := range forms {
		m.rows[s.Form.ID.String()] = s
	}

	return &m
}

func (m *memStore) All(context.Context) ([]formbus.Stored, error) {
	if m.fail != nil {
		return nil, m.fail
	}

	out := make([]formbus.Stored, 0, len(m.rows))
	for _, name := range slices.Sorted(maps.Keys(m.rows)) {
		out = append(out, m.rows[name])
	}

	return out, nil
}

func (m *memStore) ByID(_ context.Context, slug types.Slug) (formbus.Stored, error) {
	if m.fail != nil {
		return formbus.Stored{}, m.fail
	}

	s, ok := m.rows[slug.String()]
	if !ok {
		return formbus.Stored{}, formbus.ErrNotFound
	}

	return s, nil
}

func (m *memStore) Upsert(_ context.Context, s formbus.Stored) error {
	if m.fail != nil {
		return m.fail
	}

	m.rows[s.Form.ID.String()] = s

	return nil
}

func (m *memStore) Delete(_ context.Context, slug types.Slug) error {
	if m.fail != nil {
		return m.fail
	}

	delete(m.rows, slug.String())

	return nil
}

// release stands in for the definitions compiled into the binary.
type release struct {
	forms []formbus.Form
}

func (r release) ByID(slug types.Slug) (formbus.Form, error) {
	for _, f := range r.forms {
		if f.ID == slug {
			return f, nil
		}
	}

	return formbus.Form{}, formbus.ErrNotFound
}

func (r release) All() []formbus.Form { return slices.Clone(r.forms) }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// usable is the smallest definition that passes Check.
func usable(t *testing.T, name string) formbus.Form {
	t.Helper()

	f := formbus.Form{
		ID:       mustSlug(t, name),
		Title:    "A form",
		Currency: "usd",
		Fields: []formbus.Field{
			{Name: "who", Label: "Your name", Kind: formbus.KindText, Required: true},
		},
	}
	f.Stamp()

	return f
}

var at = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

// A draft is not served. This is the whole of the live/draft bargain: it is
// what lets a half-finished definition be stored at all, because nobody can
// reach it.
func TestADraftIsNotServed(t *testing.T) {
	b, err := formbus.NewBusiness(quiet(), newMem(), nil)
	if err != nil {
		t.Fatalf("NewBusiness: %v", err)
	}

	half := formbus.Form{ID: mustSlug(t, "supper"), Title: "Half done"}

	if _, err := b.Create(t.Context(), at, types.ID{}, half); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := b.ByID(half.ID); !errors.Is(err, formbus.ErrNotFound) {
		t.Errorf("ByID on a draft = %v, want ErrNotFound", err)
	}

	if got := b.All(); len(got) != 0 {
		t.Errorf("All lists %d forms, want none", len(got))
	}

	// And it is there for the builder, which is the other half of the same
	// statement: not served is not the same as not stored.
	if _, err := b.Draft(t.Context(), half.ID); err != nil {
		t.Errorf("Draft on a draft = %v, want it", err)
	}
}

// Create does not run Check, and that is deliberate: a form is created with a
// name and nothing else, and holding it to Check there would mean the only way
// to make one is to type a whole definition into one request.
func TestCreateDoesNotDemandAUsableForm(t *testing.T) {
	b, err := formbus.NewBusiness(quiet(), newMem(), nil)
	if err != nil {
		t.Fatalf("NewBusiness: %v", err)
	}

	bare := formbus.Form{ID: mustSlug(t, "supper"), Title: "Nothing in it yet"}

	if _, err := b.Create(t.Context(), at, types.ID{}, bare); err != nil {
		t.Errorf("Create on a form with no fields = %v, want it to be stored", err)
	}
}

// Publishing does run Check, and reports every problem rather than the first.
func TestPublishingRefusesAnUnusableForm(t *testing.T) {
	b, err := formbus.NewBusiness(quiet(), newMem(), nil)
	if err != nil {
		t.Fatalf("NewBusiness: %v", err)
	}

	bare := formbus.Form{ID: mustSlug(t, "supper"), Title: "Nothing in it yet"}

	if _, err := b.Create(t.Context(), at, types.ID{}, bare); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = b.Save(t.Context(), at, types.ID{}, bare, true)

	bad, is := errors.AsType[formbus.DefinitionError](err)
	if !is {
		t.Fatalf("publishing an unusable form = %v, want a DefinitionError", err)
	}

	// Two at least: it has no fields and its currency is empty. One at a time
	// is the bad afternoon DefinitionError exists to avoid.
	if len(bad.Problems) < 2 {
		t.Errorf("reported %d problems, want every one of them: %v", len(bad.Problems), bad.Problems)
	}

	if _, err := b.ByID(bare.ID); !errors.Is(err, formbus.ErrNotFound) {
		t.Errorf("a refused publish put the form into the catalogue anyway")
	}
}

// A published form is served, and a change that would break it is refused with
// the form still serving what it was.
func TestALiveFormCannotBeEditedIntoUselessness(t *testing.T) {
	b, err := formbus.NewBusiness(quiet(), newMem(), nil)
	if err != nil {
		t.Fatalf("NewBusiness: %v", err)
	}

	f := usable(t, "supper")

	if _, err := b.Create(t.Context(), at, types.ID{}, f); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := b.Save(t.Context(), at, types.ID{}, f, true); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	served, err := b.ByID(f.ID)
	if err != nil {
		t.Fatalf("ByID after publishing: %v", err)
	}

	// Now take its only field away, which Check refuses.
	broken := f
	broken.Fields = nil

	if err := b.Save(t.Context(), at, types.ID{}, broken, true); err == nil {
		t.Fatal("a live form was edited into one with no fields")
	}

	after, err := b.ByID(f.ID)
	if err != nil {
		t.Fatalf("ByID after the refused edit: %v", err)
	}

	if after.Version != served.Version {
		t.Errorf("the refused edit changed what is being served: version %q, was %q", after.Version, served.Version)
	}
}

// Editing a live form changes its fingerprint, which is what invalidates every
// grant in flight. Asserted because it is the mechanism a price change rests
// on, not a side effect of it.
func TestEditingALiveFormChangesItsVersion(t *testing.T) {
	b, err := formbus.NewBusiness(quiet(), newMem(), nil)
	if err != nil {
		t.Fatalf("NewBusiness: %v", err)
	}

	f := usable(t, "supper")
	f.Items = []formbus.Item{{ID: "adult", Label: "Adult", Price: types.Money(1500)}}
	f.ReturnURL = "https://www.example.org/supper"
	f.Stamp()

	if _, err := b.Create(t.Context(), at, types.ID{}, f); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Save(t.Context(), at, types.ID{}, f, true); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	before, _ := b.ByID(f.ID)

	dearer := f
	dearer.Items = []formbus.Item{{ID: "adult", Label: "Adult", Price: types.Money(1800)}}

	if err := b.Save(t.Context(), at, types.ID{}, dearer, true); err != nil {
		t.Fatalf("raising the price: %v", err)
	}

	after, _ := b.ByID(f.ID)

	if after.Version == before.Version {
		t.Error("the price changed and the version did not, so every grant in flight is still valid")
	}

	if after.Items[0].Price != types.Money(1800) {
		t.Errorf("price = %v, want 1800", after.Items[0].Price)
	}
}

// A definition that ships in the binary is not editable here, in either
// direction: it cannot be written over and its name cannot be claimed.
func TestAReleaseFormCannotBeEditedOrShadowed(t *testing.T) {
	feast := usable(t, "feast-lunch-2026")

	b, err := formbus.NewBusiness(quiet(), newMem(), release{forms: []formbus.Form{feast}})
	if err != nil {
		t.Fatalf("NewBusiness: %v", err)
	}

	if b.Editable(feast.ID) {
		t.Error("a form from the release reports as editable")
	}

	if err := b.Save(t.Context(), at, types.ID{}, feast, true); !errors.Is(err, formbus.ErrBuiltIn) {
		t.Errorf("Save on a release form = %v, want ErrBuiltIn", err)
	}

	if _, err := b.Create(t.Context(), at, types.ID{}, feast); !errors.Is(err, formbus.ErrTaken) {
		t.Errorf("Create claiming a release form's name = %v, want ErrTaken", err)
	}

	if err := b.Delete(t.Context(), feast.ID); !errors.Is(err, formbus.ErrBuiltIn) {
		t.Errorf("Delete on a release form = %v, want ErrBuiltIn", err)
	}

	// And it is served, which is the point of it being in the catalogue at
	// all.
	if _, err := b.ByID(feast.ID); err != nil {
		t.Errorf("ByID on a release form = %v, want it", err)
	}
}

func TestTwoFormsCannotShareAName(t *testing.T) {
	b, err := formbus.NewBusiness(quiet(), newMem(), nil)
	if err != nil {
		t.Fatalf("NewBusiness: %v", err)
	}

	f := usable(t, "supper")

	if _, err := b.Create(t.Context(), at, types.ID{}, f); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := b.Create(t.Context(), at, types.ID{}, f); !errors.Is(err, formbus.ErrTaken) {
		t.Errorf("Create on a name already taken = %v, want ErrTaken", err)
	}
}

// Delete is the rule about submissions, expressed as a rule about publishing:
// a form that has been live may have rows pointing at it, and those rows are
// unreadable without the definition that names their columns.
func TestOnlyAFormThatWasNeverPublishedCanBeDeleted(t *testing.T) {
	b, err := formbus.NewBusiness(quiet(), newMem(), nil)
	if err != nil {
		t.Fatalf("NewBusiness: %v", err)
	}

	never := usable(t, "never")
	once := usable(t, "once")

	for _, f := range []formbus.Form{never, once} {
		if _, err := b.Create(t.Context(), at, types.ID{}, f); err != nil {
			t.Fatalf("Create %s: %v", f.ID, err)
		}
	}

	if err := b.Save(t.Context(), at, types.ID{}, once, true); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	// And taken down again, which must not make it deletable: it has been on
	// the web, so somebody may have submitted it.
	if err := b.Save(t.Context(), at, types.ID{}, once, false); err != nil {
		t.Fatalf("taking it down: %v", err)
	}

	if err := b.Delete(t.Context(), once.ID); !errors.Is(err, formbus.ErrPublished) {
		t.Errorf("Delete on a form that has been live = %v, want ErrPublished", err)
	}

	if err := b.Delete(t.Context(), never.ID); err != nil {
		t.Errorf("Delete on a form that never was = %v, want it gone", err)
	}

	if _, err := b.Draft(t.Context(), never.ID); !errors.Is(err, formbus.ErrNotFound) {
		t.Errorf("the deleted form is still there: %v", err)
	}
}

// A published definition that stops passing Check -- because a release
// tightened a rule -- is dropped from the snapshot rather than stopping the
// process. Refusing to start would take down every other form, and on a host
// where the way in is a browser, the page somebody would fix it from.
func TestAStoredFormThatNoLongerChecksIsDroppedNotFatal(t *testing.T) {
	// Live, and impossible: no fields, no currency, and a version that does
	// not match its content.
	rotten := formbus.Stored{
		Form: formbus.Form{ID: mustSlug(t, "rotten"), Title: "Rotten", Version: "deadbeef1234"},
		Live: true,
	}

	good := formbus.Stored{Form: usable(t, "good"), Live: true}

	b, err := formbus.NewBusiness(quiet(), newMem(rotten, good), nil)
	if err != nil {
		t.Fatalf("NewBusiness refused to start over one bad definition: %v", err)
	}

	if _, err := b.ByID(rotten.Form.ID); !errors.Is(err, formbus.ErrNotFound) {
		t.Errorf("the unusable form is being served: %v", err)
	}

	if _, err := b.ByID(good.Form.ID); err != nil {
		t.Errorf("the usable form beside it is not being served: %v", err)
	}
}

// The other direction: a store that cannot be read at all is fatal, because a
// service that starts with an empty catalogue looks exactly like one whose
// forms were all deleted.
func TestAnUnreadableStoreStopsTheService(t *testing.T) {
	store := newMem()
	store.fail = errors.New("the disk is gone")

	if _, err := formbus.NewBusiness(quiet(), store, nil); err == nil {
		t.Fatal("the catalogue started over a store it could not read")
	}
}

// A stored definition whose name a release form also claims loses, and says
// so. It can only happen by adding a .toml file for a name somebody had
// already built, since Create refuses the collision the other way.
func TestAReleaseFormWinsAShadowedName(t *testing.T) {
	var lines strings.Builder

	log := slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelError}))

	mine := usable(t, "supper")
	mine.Title = "The one I built"

	theirs := usable(t, "supper")
	theirs.Title = "The one in the release"
	theirs.Stamp()

	b, err := formbus.NewBusiness(log, newMem(formbus.Stored{Form: mine, Live: true}), release{forms: []formbus.Form{theirs}})
	if err != nil {
		t.Fatalf("NewBusiness: %v", err)
	}

	served, err := b.ByID(mine.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if served.Title != "The one in the release" {
		t.Errorf("served %q, want the release's copy", served.Title)
	}

	if !strings.Contains(lines.String(), "same name") {
		t.Errorf("nothing was logged about the shadowed form:\n%s", lines.String())
	}
}

// Drafts lists what the builder can edit and nothing from the release, because
// the release's forms are listed by whatever serves them.
func TestDraftsListsOnlyWhatWasAuthored(t *testing.T) {
	feast := usable(t, "feast-lunch-2026")
	mine := formbus.Stored{Form: usable(t, "supper")}

	b, err := formbus.NewBusiness(quiet(), newMem(mine), release{forms: []formbus.Form{feast}})
	if err != nil {
		t.Fatalf("NewBusiness: %v", err)
	}

	drafts, err := b.Drafts(t.Context())
	if err != nil {
		t.Fatalf("Drafts: %v", err)
	}

	if len(drafts) != 1 || drafts[0].Form.ID != mine.Form.ID {
		t.Errorf("Drafts = %v, want only the authored one", drafts)
	}
}

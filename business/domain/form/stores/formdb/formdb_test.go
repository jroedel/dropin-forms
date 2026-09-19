package formdb_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formdb"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

var now = time.Date(2026, 9, 19, 15, 0, 0, 0, time.UTC)

// open makes a real database in a temporary directory, on the same terms
// accessdb's tests do: a file rather than :memory:, so the pragmas sqldb.Open
// sets are the ones production runs with.
func open(t *testing.T) (*sql.DB, *formdb.Store) {
	t.Helper()

	db, err := sqldb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	if err := sqldb.Init(t.Context(), db); err != nil {
		t.Fatalf("sqldb.Init: %v", err)
	}
	if err := formdb.Init(t.Context(), db); err != nil {
		t.Fatalf("formdb.Init: %v", err)
	}

	return db, formdb.NewStore(db)
}

func slug(t *testing.T, s string) types.Slug {
	t.Helper()

	id, err := types.ParseSlug(s)
	if err != nil {
		t.Fatalf("ParseSlug(%q): %v", s, err)
	}

	return id
}

// full is a definition using every part of the wire shape, so that the
// round-trip test below is about all of it rather than about the easy half.
func full(t *testing.T) formbus.Form {
	t.Helper()

	origin, err := types.ParseOrigin("https://www.example.org")
	if err != nil {
		t.Fatalf("ParseOrigin: %v", err)
	}

	min := int64(500)
	max := int64(20000)

	f := formbus.Form{
		ID:         slug(t, "supper-2026"),
		Title:      "Parish supper",
		Intro:      "Everybody welcome.",
		OpensAt:    time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
		ClosesAt:   time.Date(2026, 10, 17, 23, 59, 59, 0, time.UTC),
		ClosedNote: "Write to the office.",
		Currency:   "usd",
		Origins:    []types.Origin{origin},
		ReturnURL:  "https://www.example.org/supper",

		MinPerOrder: 1,
		MaxPerOrder: 10,
		MinTotal:    types.Money(1000),
		MaxTotal:    types.Money(50000),

		PaymentRequired: true,
		PaymentNote:     "Choose a ticket, or enter a donation.",

		Confirmation: "Thank you.",
		Notify:       []string{"office@example.org"},
		DailyCap:     500,

		Fields: []formbus.Field{
			{
				Name:  "sitting",
				Label: "Which sitting",
				Kind:  formbus.KindSelect,
				Options: []formbus.Option{
					{Value: "early", Label: "Six o'clock"},
					{Value: "late", Label: "Eight o'clock"},
				},
				Required: true,
			},
			{
				Name:        "dietary",
				Label:       "Anything we should know",
				Kind:        formbus.KindParagraph,
				Help:        "Allergies, and so on.",
				Placeholder: "No nuts, please",
				MinLen:      0,
				MaxLen:      500,
				ShowIf:      &formbus.Condition{Field: "sitting", Is: []string{"early"}},
			},
			{
				Name:        "postcode",
				Label:       "ZIP",
				Kind:        formbus.KindText,
				Pattern:     "[0-9]{5}",
				PatternNote: "Use five digits, like 78745.",
			},
			{
				Name:         "donation",
				Label:        "A donation",
				Kind:         formbus.KindAmount,
				Min:          &min,
				Max:          &max,
				Autocomplete: "off",
			},
		},

		Items: []formbus.Item{
			{ID: "adult", Label: "Adult", Note: "Two courses", Price: types.Money(1500), Max: 8},
			{ID: "child", Label: "Child", Price: types.Money(0)},
		},
	}

	f.Stamp()

	return f
}

// The property the whole store rests on: what comes back is what went in. A
// definition that survives the round trip with one bound turned from nil into
// zero is a rule that quietly changed on its way through storage.
func TestADefinitionSurvivesTheRoundTrip(t *testing.T) {
	_, store := open(t)

	want := full(t)

	if err := store.Upsert(t.Context(), formbus.Stored{
		Form: want, Live: true, CreatedAt: now, UpdatedAt: now, PublishedAt: now,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := store.ByID(t.Context(), want.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	// Compared by fingerprint as well as field by field, because the
	// fingerprint is what a submission grant is signed over: a definition that
	// reads back with a different one has invalidated every grant in flight,
	// whatever else about it looks right.
	if got.Form.Fingerprint() != want.Fingerprint() {
		t.Errorf("fingerprint after a round trip = %q, want %q", got.Form.Fingerprint(), want.Fingerprint())
	}

	if got.Form.ID != want.ID {
		t.Errorf("id = %q, want %q", got.Form.ID, want.ID)
	}

	if !got.Form.OpensAt.Equal(want.OpensAt) || !got.Form.ClosesAt.Equal(want.ClosesAt) {
		t.Errorf("times = %v/%v, want %v/%v",
			got.Form.OpensAt, got.Form.ClosesAt, want.OpensAt, want.ClosesAt)
	}

	if len(got.Form.Fields) != len(want.Fields) {
		t.Fatalf("read back %d fields, want %d", len(got.Form.Fields), len(want.Fields))
	}

	for i, fld := range got.Form.Fields {
		w := want.Fields[i]

		switch {
		case fld.Name != w.Name, fld.Kind != w.Kind, fld.Label != w.Label:
			t.Errorf("field %d = %+v, want %+v", i, fld, w)
		case fld.Pattern != w.Pattern, fld.PatternNote != w.PatternNote:
			t.Errorf("field %d pattern = %q/%q, want %q/%q", i, fld.Pattern, fld.PatternNote, w.Pattern, w.PatternNote)
		}

		if (fld.Min == nil) != (w.Min == nil) || (fld.Min != nil && *fld.Min != *w.Min) {
			t.Errorf("field %d min = %v, want %v", i, fld.Min, w.Min)
		}
	}

	if len(got.Form.Items) != 2 || got.Form.Items[0].Price != types.Money(1500) {
		t.Errorf("items = %+v", got.Form.Items)
	}

	// The free item, which is the one an over-eager "omit the zero" would have
	// lost -- and losing it means selling something for nothing.
	if got.Form.Items[1].ID != "child" || got.Form.Items[1].Price != 0 {
		t.Errorf("the free item read back as %+v", got.Form.Items[1])
	}

	if !got.Live || !got.PublishedAt.Equal(now) {
		t.Errorf("live = %v, published = %v", got.Live, got.PublishedAt)
	}
}

// A bound of zero and no bound at all are different rules, which is the whole
// reason Min and Max are pointers. Storage has to keep them apart.
func TestAZeroBoundIsNotTheSameAsNoBound(t *testing.T) {
	_, store := open(t)

	zero := int64(0)

	f := formbus.Form{
		ID:       slug(t, "bounds"),
		Title:    "Bounds",
		Currency: "usd",
		Fields: []formbus.Field{
			{Name: "floor", Label: "With a floor of nothing", Kind: formbus.KindAmount, Min: &zero},
			{Name: "none", Label: "With no floor", Kind: formbus.KindAmount},
		},
	}
	f.Stamp()

	if err := store.Upsert(t.Context(), formbus.Stored{Form: f, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := store.ByID(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if got.Form.Fields[0].Min == nil || *got.Form.Fields[0].Min != 0 {
		t.Errorf("a minimum of zero read back as %v, want a pointer to 0", got.Form.Fields[0].Min)
	}

	if got.Form.Fields[1].Min != nil {
		t.Errorf("no minimum read back as %v, want nil", got.Form.Fields[1].Min)
	}
}

// Never published is NULL rather than the epoch, because the epoch is a real
// instant and would read as a form published in 1970 -- which is the answer
// Delete consults before deciding whether a form can be removed.
func TestNeverPublishedComesBackAsZero(t *testing.T) {
	_, store := open(t)

	f := formbus.Form{ID: slug(t, "draft"), Title: "A draft", Currency: "usd"}
	f.Stamp()

	if err := store.Upsert(t.Context(), formbus.Stored{Form: f, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := store.ByID(t.Context(), f.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	if !got.PublishedAt.IsZero() {
		t.Errorf("published at %v, want the zero time", got.PublishedAt)
	}
}

// Upsert replaces, and keeps the creation time it is handed rather than
// inventing one.
func TestUpsertReplaces(t *testing.T) {
	_, store := open(t)

	f := formbus.Form{ID: slug(t, "supper-2026"), Title: "First", Currency: "usd"}
	f.Stamp()

	created := now.Add(-48 * time.Hour)

	if err := store.Upsert(t.Context(), formbus.Stored{Form: f, CreatedAt: created, UpdatedAt: created}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	f.Title = "Second"
	f.Stamp()

	if err := store.Upsert(t.Context(), formbus.Stored{Form: f, CreatedAt: created, UpdatedAt: now, Live: true}); err != nil {
		t.Fatalf("Upsert again: %v", err)
	}

	all, err := store.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	if len(all) != 1 {
		t.Fatalf("read back %d rows, want 1", len(all))
	}

	if all[0].Form.Title != "Second" {
		t.Errorf("title = %q, want Second", all[0].Form.Title)
	}

	if !all[0].CreatedAt.Equal(created) {
		t.Errorf("created at %v, want %v", all[0].CreatedAt, created)
	}
}

func TestByIDSaysNotFound(t *testing.T) {
	_, store := open(t)

	_, err := store.ByID(t.Context(), slug(t, "nothing"))
	if !errors.Is(err, formbus.ErrNotFound) {
		t.Errorf("ByID on an empty store = %v, want formbus.ErrNotFound", err)
	}
}

// Deleting nothing is not an error, for the reason the method's comment gives:
// the caller wanted the row gone and it is gone.
func TestDeletingNothingIsFine(t *testing.T) {
	_, store := open(t)

	if err := store.Delete(t.Context(), slug(t, "nothing")); err != nil {
		t.Errorf("Delete on an empty store = %v, want nil", err)
	}
}

// An attribute this binary has never heard of is refused rather than dropped.
// The case is a rollback reading a newer document, and the decision is in
// decode's comment: a rule that quietly does not apply is worse than a form
// that visibly stops being served.
func TestADocumentWithAnUnknownSettingIsRefused(t *testing.T) {
	db, store := open(t)

	const q = `
INSERT INTO form_definitions (slug, definition, live, created_at, updated_at, updated_by, published_at)
VALUES ('odd', '{"title":"Odd","currency":"usd","rounding_mode":"banker"}', 1, 0, 0, '', NULL)`

	if _, err := db.ExecContext(t.Context(), q); err != nil {
		t.Fatalf("seeding the row: %v", err)
	}

	_, err := store.ByID(t.Context(), slug(t, "odd"))
	if err == nil {
		t.Fatal("a definition with an unknown setting was read without complaint")
	}

	if !strings.Contains(err.Error(), "rounding_mode") {
		t.Errorf("the error does not name the setting it did not understand: %v", err)
	}
}

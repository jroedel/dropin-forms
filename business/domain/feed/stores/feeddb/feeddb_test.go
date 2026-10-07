package feeddb_test

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/feed/feedbus"
	"github.com/jroedel/dropin-forms/business/domain/feed/stores/feeddb"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

var now = time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

// keys is the bus over the real store, which is what is worth testing: the
// hash lookup, the form check and revocation all depend on both halves.
func keys(t *testing.T) (*feedbus.Business, *feeddb.Store) {
	t.Helper()

	db, err := sqldb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := feeddb.Init(t.Context(), db); err != nil {
		t.Fatalf("Init: %v", err)
	}

	store := feeddb.NewStore(db)

	return feedbus.NewBusiness(slog.New(slog.NewTextHandler(io.Discard, nil)), store), store
}

func slug(t *testing.T, s string) types.Slug {
	t.Helper()

	v, err := types.ParseSlug(s)
	if err != nil {
		t.Fatalf("ParseSlug: %v", err)
	}

	return v
}

func TestAKeyReadsItsOwnFormUntilItIsRevoked(t *testing.T) {
	b, _ := keys(t)
	form := slug(t, "ordination-2027")
	by := types.NewID()

	k, secret, err := b.Create(t.Context(), now, form, "Office sheet", by)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(secret, "dfk_") || len(secret) < 40 {
		t.Errorf("the key is %q", secret)
	}

	got, err := b.Check(t.Context(), now.Add(time.Hour), form, secret)
	if err != nil || got.ID != k.ID {
		t.Fatalf("Check: %v, %v", got.ID, err)
	}

	listed, _ := b.ForForm(t.Context(), form)
	if len(listed) != 1 || !listed[0].LastUsedAt.Equal(now.Add(time.Hour)) || listed[0].Label != "Office sheet" {
		t.Errorf("ForForm = %+v; want one key, used an hour after it was made", listed)
	}

	if _, err := b.Check(t.Context(), now, slug(t, "another-form"), secret); !errors.Is(err, feedbus.ErrRefused) {
		t.Errorf("Check on another form: %v, want ErrRefused", err)
	}

	// Revoking from another form's page does nothing.
	if err := b.Revoke(t.Context(), now, slug(t, "another-form"), k.ID, by); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := b.Check(t.Context(), now, form, secret); err != nil {
		t.Errorf("a key revoked under another form's name stopped working: %v", err)
	}

	if err := b.Revoke(t.Context(), now, form, k.ID, by); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := b.Check(t.Context(), now, form, secret); !errors.Is(err, feedbus.ErrRefused) {
		t.Errorf("Check after revoking: %v, want ErrRefused", err)
	}
}

func TestOnlyTheKeyItselfIsAccepted(t *testing.T) {
	b, _ := keys(t)
	form := slug(t, "ordination-2027")

	_, secret, err := b.Create(t.Context(), now, form, "", types.NewID())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for name, presented := range map[string]string{
		"empty":            "",
		"no prefix":        strings.TrimPrefix(secret, "dfk_"),
		"one more letter":  secret + "A",
		"one letter fewer": secret[:len(secret)-1],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := b.Check(t.Context(), now, form, presented); !errors.Is(err, feedbus.ErrRefused) {
				t.Errorf("Check: %v, want ErrRefused", err)
			}
		})
	}
}

func TestTheKeyIsNotStored(t *testing.T) {
	b, store := keys(t)
	form := slug(t, "ordination-2027")

	_, secret, err := b.Create(t.Context(), now, form, "", types.NewID())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := store.ByHash(t.Context(), secret); !errors.Is(err, feedbus.ErrNotFound) {
		t.Errorf("the key itself found a row: %v", err)
	}
}

package accessbus_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/types"
)

var now = time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

// key is what a grant is keyed by, which is the pair and not the role.
type key struct {
	user types.ID
	form types.Slug
}

// memStore is an accessbus.Storer in a map, so the rules can be tested without
// a database.
type memStore struct {
	grants map[key]accessbus.Grant

	// fail is returned by every method when set, which is how the tests check
	// that an unreadable store is an error and never a refusal.
	fail error

	lookups []types.Slug
}

func newMemStore() *memStore {
	return &memStore{grants: map[key]accessbus.Grant{}}
}

func (m *memStore) Upsert(_ context.Context, g accessbus.Grant) error {
	if m.fail != nil {
		return m.fail
	}

	m.grants[key{g.UserID, g.Form}] = g

	return nil
}

func (m *memStore) Delete(_ context.Context, userID types.ID, form types.Slug) error {
	if m.fail != nil {
		return m.fail
	}

	delete(m.grants, key{userID, form})

	return nil
}

func (m *memStore) ByUserAndForm(_ context.Context, userID types.ID, form types.Slug) (accessbus.Grant, error) {
	if m.fail != nil {
		return accessbus.Grant{}, m.fail
	}

	m.lookups = append(m.lookups, form)

	g, ok := m.grants[key{userID, form}]
	if !ok {
		return accessbus.Grant{}, accessbus.ErrNotFound
	}

	return g, nil
}

func (m *memStore) ByUser(_ context.Context, userID types.ID) ([]accessbus.Grant, error) {
	if m.fail != nil {
		return nil, m.fail
	}

	var out []accessbus.Grant
	for _, g := range m.grants {
		if g.UserID == userID {
			out = append(out, g)
		}
	}

	return out, nil
}

func (m *memStore) ByForm(_ context.Context, form types.Slug) ([]accessbus.Grant, error) {
	if m.fail != nil {
		return nil, m.fail
	}

	var out []accessbus.Grant
	for _, g := range m.grants {
		if g.Form == form {
			out = append(out, g)
		}
	}

	return out, nil
}

func (m *memStore) All(_ context.Context) ([]accessbus.Grant, error) {
	if m.fail != nil {
		return nil, m.fail
	}

	return slices.Collect(maps.Values(m.grants)), nil
}

func newBusiness() (*accessbus.Business, *memStore) {
	store := newMemStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	return accessbus.NewBusiness(log, store), store
}

func mustSlug(t *testing.T, s string) types.Slug {
	t.Helper()

	slug, err := types.ParseSlug(s)
	if err != nil {
		t.Fatalf("ParseSlug(%q): %v", s, err)
	}

	return slug
}

func TestAdminIncludesResultsAndResultsDoesNot(t *testing.T) {
	cases := []struct {
		held accessbus.Role
		want accessbus.Role
		ok   bool
	}{
		{accessbus.RoleAdmin, accessbus.RoleAdmin, true},
		{accessbus.RoleAdmin, accessbus.RoleResults, true},
		{accessbus.RoleResults, accessbus.RoleResults, true},

		// The one that matters: reading the numbers is not editing the price.
		{accessbus.RoleResults, accessbus.RoleAdmin, false},

		// A role from a newer binary includes nothing, in either position.
		// The safe reading of a value you do not recognise is none.
		{accessbus.Role("owner"), accessbus.RoleResults, false},
		{accessbus.RoleAdmin, accessbus.Role("owner"), false},
		{accessbus.Role(""), accessbus.RoleResults, false},
		{accessbus.RoleAdmin, accessbus.Role(""), false},
	}

	for _, c := range cases {
		if got := c.held.Includes(c.want); got != c.ok {
			t.Errorf("Role(%q).Includes(%q) = %v, want %v", c.held, c.want, got, c.ok)
		}
	}
}

func TestParseRoleRefusesAnythingElse(t *testing.T) {
	for _, r := range accessbus.Roles() {
		got, err := accessbus.ParseRole(r.String())
		if err != nil {
			t.Errorf("ParseRole(%q): %v", r, err)
		}
		if got != r {
			t.Errorf("ParseRole(%q) = %q", r, got)
		}
	}

	for _, s := range []string{"", "Admin", "ADMIN", "admin ", "results,admin", "owner", "*"} {
		if _, err := accessbus.ParseRole(s); !errors.Is(err, accessbus.ErrNotARole) {
			t.Errorf("ParseRole(%q) = %v, want ErrNotARole", s, err)
		}
	}
}

func TestAGrantOnOneFormIsNotAGrantOnAnother(t *testing.T) {
	b, _ := newBusiness()
	user := types.NewID()
	lunch := mustSlug(t, "feast-lunch-2026")
	retreat := mustSlug(t, "fall-retreat")

	if _, err := b.Grant(t.Context(), now, types.NewID(), user, lunch, accessbus.RoleResults); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	allowed, err := b.Allowed(t.Context(), user, lunch, accessbus.RoleResults)
	if err != nil || !allowed {
		t.Errorf("on the granted form: allowed = %v, err = %v", allowed, err)
	}

	allowed, err = b.Allowed(t.Context(), user, retreat, accessbus.RoleResults)
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if allowed {
		t.Error("a grant on one form authorised another")
	}

	// And the role is not promoted by asking for more.
	allowed, err = b.Allowed(t.Context(), user, lunch, accessbus.RoleAdmin)
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if allowed {
		t.Error("a results grant answered a request for admin")
	}
}

func TestASiteWideGrantCoversEveryForm(t *testing.T) {
	b, _ := newBusiness()
	user := types.NewID()

	if _, err := b.Grant(t.Context(), now, types.ID{}, user, types.Slug{}, accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	for _, name := range []string{"feast-lunch-2026", "fall-retreat", "something-nobody-has-made-yet"} {
		for _, want := range accessbus.Roles() {
			allowed, err := b.Allowed(t.Context(), user, mustSlug(t, name), want)
			if err != nil {
				t.Fatalf("Allowed: %v", err)
			}
			if !allowed {
				t.Errorf("site-wide admin was refused %s on %q", want, name)
			}
		}
	}
}

// The case the loop in Allowed is written for: a weak grant on this form must
// not stop the strong site-wide one being consulted.
func TestAFormGrantDoesNotShadowASiteWideOne(t *testing.T) {
	b, store := newBusiness()
	user := types.NewID()
	lunch := mustSlug(t, "feast-lunch-2026")

	if _, err := b.Grant(t.Context(), now, types.ID{}, user, types.Slug{}, accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := b.Grant(t.Context(), now, types.ID{}, user, lunch, accessbus.RoleResults); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	allowed, err := b.Allowed(t.Context(), user, lunch, accessbus.RoleAdmin)
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if !allowed {
		t.Error("a results grant on the form shadowed a site-wide admin grant")
	}

	// Both were consulted: the specific grant answered no and the loop
	// carried on rather than stopping at the first row it found.
	if got := len(store.lookups); got != 2 {
		t.Errorf("%d lookups, want 2 -- the form then the site-wide grant", got)
	}
}

func TestTheSpecificFormIsReadFirst(t *testing.T) {
	b, store := newBusiness()
	user := types.NewID()
	lunch := mustSlug(t, "feast-lunch-2026")

	if _, err := b.Grant(t.Context(), now, types.ID{}, user, lunch, accessbus.RoleResults); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	if _, err := b.Allowed(t.Context(), user, lunch, accessbus.RoleResults); err != nil {
		t.Fatalf("Allowed: %v", err)
	}

	// One read, on the form itself. The site-wide lookup is only reached when
	// the specific one does not answer, which keeps the ordinary request to a
	// single primary-key hit.
	if got := len(store.lookups); got != 1 {
		t.Fatalf("%d lookups, want 1", got)
	}
	if store.lookups[0] != lunch {
		t.Errorf("looked up %q, want %q", store.lookups[0], lunch)
	}
}

func TestNobodyIsAllowedAnythingByDefault(t *testing.T) {
	b, _ := newBusiness()

	allowed, err := b.Allowed(t.Context(), types.NewID(), mustSlug(t, "feast-lunch-2026"), accessbus.RoleResults)
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if allowed {
		t.Error("an account with no grants was allowed something")
	}
}

func TestAnUnreadableStoreIsAnErrorAndNotARefusal(t *testing.T) {
	b, store := newBusiness()
	store.fail = errors.New("the disk is on fire")

	allowed, err := b.Allowed(t.Context(), types.NewID(), mustSlug(t, "feast-lunch-2026"), accessbus.RoleResults)
	if err == nil {
		t.Fatal("Allowed returned no error when the store failed")
	}
	if allowed {
		t.Error("Allowed returned true alongside an error")
	}

	// The distinction the gate depends on: a caller that treats every false as
	// "not allowed" would turn this into a permission denial, and whoever hit
	// it would spend the afternoon wondering what they had done wrong.
	if !errors.Is(err, store.fail) {
		t.Errorf("error does not wrap the store's: %v", err)
	}
}

func TestGrantingAgainChangesTheRole(t *testing.T) {
	b, _ := newBusiness()
	user := types.NewID()
	lunch := mustSlug(t, "feast-lunch-2026")

	if _, err := b.Grant(t.Context(), now, types.ID{}, user, lunch, accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := b.Grant(t.Context(), now, types.ID{}, user, lunch, accessbus.RoleResults); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	allowed, err := b.Allowed(t.Context(), user, lunch, accessbus.RoleAdmin)
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if allowed {
		t.Error("demoting an account left it holding admin")
	}

	grants, err := b.ForUser(t.Context(), user)
	if err != nil {
		t.Fatalf("ForUser: %v", err)
	}
	if len(grants) != 1 {
		t.Errorf("%d grants after re-granting, want 1", len(grants))
	}
}

func TestGrantRefusesWhatCannotBeAGrant(t *testing.T) {
	b, _ := newBusiness()
	lunch := mustSlug(t, "feast-lunch-2026")

	if _, err := b.Grant(t.Context(), now, types.ID{}, types.ID{}, lunch, accessbus.RoleAdmin); err == nil {
		t.Error("granted a role to no account")
	}

	if _, err := b.Grant(t.Context(), now, types.ID{}, types.NewID(), lunch, accessbus.Role("owner")); !errors.Is(err, accessbus.ErrNotARole) {
		t.Errorf("Grant with an unknown role = %v, want ErrNotARole", err)
	}

	if _, err := b.Grant(t.Context(), now, types.ID{}, types.NewID(), lunch, accessbus.Role("")); !errors.Is(err, accessbus.ErrNotARole) {
		t.Errorf("Grant with no role = %v, want ErrNotARole", err)
	}
}

func TestRevokeRemovesAccess(t *testing.T) {
	b, _ := newBusiness()
	user := types.NewID()
	lunch := mustSlug(t, "feast-lunch-2026")

	if _, err := b.Grant(t.Context(), now, types.ID{}, user, lunch, accessbus.RoleResults); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := b.Revoke(t.Context(), user, lunch); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	allowed, err := b.Allowed(t.Context(), user, lunch, accessbus.RoleResults)
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if allowed {
		t.Error("a revoked grant still authorised something")
	}

	// Revoking again is not an error. The caller wanted it gone and it is
	// gone, and a revoke button that fails when somebody else got there first
	// needs a story nobody wants to write.
	if err := b.Revoke(t.Context(), user, lunch); err != nil {
		t.Errorf("revoking twice: %v", err)
	}
}

func TestTheLastSiteAdministratorCannotBeRevoked(t *testing.T) {
	b, _ := newBusiness()
	first := types.NewID()

	if _, err := b.Grant(t.Context(), now, types.ID{}, first, types.Slug{}, accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	if err := b.Revoke(t.Context(), first, types.Slug{}); !errors.Is(err, accessbus.ErrLastAdmin) {
		t.Fatalf("Revoke of the only administrator = %v, want ErrLastAdmin", err)
	}

	// With a second one, the first may go.
	second := types.NewID()
	if _, err := b.Grant(t.Context(), now, first, second, types.Slug{}, accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := b.Revoke(t.Context(), first, types.Slug{}); err != nil {
		t.Errorf("Revoke with two administrators: %v", err)
	}

	// And now the second is the last one.
	if err := b.Revoke(t.Context(), second, types.Slug{}); !errors.Is(err, accessbus.ErrLastAdmin) {
		t.Errorf("Revoke of the remaining administrator = %v, want ErrLastAdmin", err)
	}
}

// The check is about administrators, not about site-wide grants generally, and
// not about whoever happens to be passed in.
func TestRevokingSomebodyElsesSiteWideGrantIsNotRefused(t *testing.T) {
	b, _ := newBusiness()
	admin := types.NewID()
	reader := types.NewID()

	if _, err := b.Grant(t.Context(), now, types.ID{}, admin, types.Slug{}, accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := b.Grant(t.Context(), now, admin, reader, types.Slug{}, accessbus.RoleResults); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	// The reader is not an administrator, so the fact that there is exactly
	// one must not refuse this.
	if err := b.Revoke(t.Context(), reader, types.Slug{}); err != nil {
		t.Errorf("revoking a site-wide results grant: %v", err)
	}

	// A form-specific grant is never refused either, however lonely.
	lunch := mustSlug(t, "feast-lunch-2026")
	if _, err := b.Grant(t.Context(), now, admin, admin, lunch, accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := b.Revoke(t.Context(), admin, lunch); err != nil {
		t.Errorf("revoking a form-specific grant: %v", err)
	}
}

func TestEnsureSiteAdminStartsAServiceAndThenDeclines(t *testing.T) {
	b, _ := newBusiness()
	first := types.NewID()

	granted, err := b.EnsureSiteAdmin(t.Context(), now, first)
	if err != nil {
		t.Fatalf("EnsureSiteAdmin: %v", err)
	}
	if !granted {
		t.Fatal("the first account was not made an administrator")
	}

	allowed, err := b.Allowed(t.Context(), first, mustSlug(t, "feast-lunch-2026"), accessbus.RoleAdmin)
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if !allowed {
		t.Error("the bootstrapped administrator cannot reach a form")
	}

	// A second redemption -- which means a bootstrap secret that was reissued
	// by hand -- must not award authority over a service somebody is already
	// running.
	second := types.NewID()

	granted, err = b.EnsureSiteAdmin(t.Context(), now, second)
	if err != nil {
		t.Fatalf("EnsureSiteAdmin: %v", err)
	}
	if granted {
		t.Error("a second bootstrap awarded site-wide admin")
	}

	allowed, err = b.Allowed(t.Context(), second, mustSlug(t, "feast-lunch-2026"), accessbus.RoleResults)
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if allowed {
		t.Error("the second bootstrap account can see a form")
	}
}

// A site-wide results grant is not an administrator, so it must not make
// EnsureSiteAdmin decline -- otherwise granting somebody read access to
// everything would, on a service bootstrapped later, lock out the bootstrap.
func TestEnsureSiteAdminIgnoresASiteWideReader(t *testing.T) {
	b, _ := newBusiness()

	if _, err := b.Grant(t.Context(), now, types.ID{}, types.NewID(), types.Slug{}, accessbus.RoleResults); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	granted, err := b.EnsureSiteAdmin(t.Context(), now, types.NewID())
	if err != nil {
		t.Fatalf("EnsureSiteAdmin: %v", err)
	}
	if !granted {
		t.Error("a site-wide reader was mistaken for an administrator")
	}
}

func TestForFormIncludesTheSiteWideGrants(t *testing.T) {
	b, _ := newBusiness()
	lunch := mustSlug(t, "feast-lunch-2026")

	admin := types.NewID()
	reader := types.NewID()

	if _, err := b.Grant(t.Context(), now, types.ID{}, admin, types.Slug{}, accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := b.Grant(t.Context(), now, admin, reader, lunch, accessbus.RoleResults); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	grants, err := b.ForForm(t.Context(), lunch)
	if err != nil {
		t.Fatalf("ForForm: %v", err)
	}
	if len(grants) != 2 {
		t.Fatalf("%d grants on the form, want 2 (one specific, one site-wide)", len(grants))
	}

	var site int
	for _, g := range grants {
		if g.SiteWide() {
			site++
		}
	}
	if site != 1 {
		t.Errorf("%d site-wide grants in the listing, want 1", site)
	}

	// Asking about the site-wide "form" does not double-count.
	grants, err = b.ForForm(t.Context(), types.Slug{})
	if err != nil {
		t.Fatalf("ForForm: %v", err)
	}
	if len(grants) != 1 {
		t.Errorf("%d site-wide grants, want 1", len(grants))
	}
}

func TestGrantRecordsWhoDidIt(t *testing.T) {
	b, _ := newBusiness()
	granter := types.NewID()
	user := types.NewID()
	lunch := mustSlug(t, "feast-lunch-2026")

	g, err := b.Grant(t.Context(), now, granter, user, lunch, accessbus.RoleResults)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}

	if g.GrantedBy != granter {
		t.Errorf("GrantedBy = %v, want %v", g.GrantedBy, granter)
	}
	if !g.GrantedAt.Equal(now) {
		t.Errorf("GrantedAt = %v, want %v", g.GrantedAt, now)
	}
	if g.SiteWide() {
		t.Error("a grant on a named form reports itself as site-wide")
	}
}

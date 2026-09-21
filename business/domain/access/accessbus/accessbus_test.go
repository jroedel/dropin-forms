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

// RoleCreator is not a rung on the results/door/admin ladder, so holding it
// must not look like holding any of them -- an account with nothing but this
// grant should not be able to read a form's submissions by accident.
func TestCreatorIncludesNothing(t *testing.T) {
	for _, want := range accessbus.Roles() {
		if accessbus.RoleCreator.Includes(want) {
			t.Errorf("RoleCreator.Includes(%q) = true, want false", want)
		}
	}
}

// RoleCreator may only ever be granted site-wide: the form a form-specific
// grant would name already exists, so "may create it" is nothing that grant
// could mean.
func TestCreatorCannotBeGrantedOnAForm(t *testing.T) {
	b, _ := newBusiness()
	lunch := mustSlug(t, "feast-lunch-2026")

	if _, err := b.Grant(t.Context(), now, types.ID{}, types.NewID(), lunch, accessbus.RoleCreator); !errors.Is(err, accessbus.ErrNotARole) {
		t.Errorf("Grant(creator, on a form) = %v, want ErrNotARole", err)
	}

	if _, err := b.Grant(t.Context(), now, types.ID{}, types.NewID(), types.Slug{}, accessbus.RoleCreator); err != nil {
		t.Errorf("Grant(creator, site-wide): %v", err)
	}
}

// Every per-form role a site-wide grant could already hold still works after
// RoleCreator was added -- adding a role site-wide grants may hold must not
// narrow what one already could.
func TestEveryPerFormRoleIsStillValidSiteWide(t *testing.T) {
	b, _ := newBusiness()

	for _, r := range accessbus.Roles() {
		if _, err := b.Grant(t.Context(), now, types.ID{}, types.NewID(), types.Slug{}, r); err != nil {
			t.Errorf("Grant(%q, site-wide): %v", r, err)
		}
	}
}

func TestParseSiteRoleOffersCreatorAndAdminOnly(t *testing.T) {
	for _, r := range accessbus.SiteRoles() {
		got, err := accessbus.ParseSiteRole(r.String())
		if err != nil {
			t.Errorf("ParseSiteRole(%q): %v", r, err)
		}
		if got != r {
			t.Errorf("ParseSiteRole(%q) = %q", r, got)
		}
	}

	for _, s := range []string{"", "results", "door", "owner"} {
		if _, err := accessbus.ParseSiteRole(s); !errors.Is(err, accessbus.ErrNotARole) {
			t.Errorf("ParseSiteRole(%q) = %v, want ErrNotARole", s, err)
		}
	}
}

// CanCreateForms is the narrower question RoleCreator exists to answer, and it
// is deliberately not the same lookup Allowed makes -- RoleCreator includes
// nothing, so Allowed would say no even for the account the grant names.
func TestCanCreateForms(t *testing.T) {
	b, _ := newBusiness()

	nobody := types.NewID()
	if can, err := b.CanCreateForms(t.Context(), nobody); err != nil || can {
		t.Errorf("CanCreateForms(nobody) = %v, %v, want false, nil", can, err)
	}

	creator := types.NewID()
	if _, err := b.Grant(t.Context(), now, types.ID{}, creator, types.Slug{}, accessbus.RoleCreator); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if can, err := b.CanCreateForms(t.Context(), creator); err != nil || !can {
		t.Errorf("CanCreateForms(creator) = %v, %v, want true, nil", can, err)
	}

	admin := types.NewID()
	if _, err := b.Grant(t.Context(), now, types.ID{}, admin, types.Slug{}, accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if can, err := b.CanCreateForms(t.Context(), admin); err != nil || !can {
		t.Errorf("CanCreateForms(admin) = %v, %v, want true, nil", can, err)
	}

	// A results holder, even site-wide, may not create a form.
	reader := types.NewID()
	if _, err := b.Grant(t.Context(), now, types.ID{}, reader, types.Slug{}, accessbus.RoleResults); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if can, err := b.CanCreateForms(t.Context(), reader); err != nil || can {
		t.Errorf("CanCreateForms(site-wide reader) = %v, %v, want false, nil", can, err)
	}

	// Admin on a specific form is not the site-wide grant this asks about.
	onOneForm := types.NewID()
	if _, err := b.Grant(t.Context(), now, types.ID{}, onOneForm, mustSlug(t, "feast-lunch-2026"), accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if can, err := b.CanCreateForms(t.Context(), onOneForm); err != nil || can {
		t.Errorf("CanCreateForms(form admin) = %v, %v, want false, nil", can, err)
	}
}

// The role a row holds is a wider set than the role somebody may be typed
// into the per-form people page, and storage parses against the wider one.
// Without this a RoleCreator grant is writable and then unreadable: the write
// succeeds, and every read of that row afterwards is an error, which a gate
// correctly turns into a 500 rather than a refusal.
func TestParseStoredRoleAcceptsEveryRoleARowMayHold(t *testing.T) {
	for _, r := range append(accessbus.Roles(), accessbus.SiteRoles()...) {
		got, err := accessbus.ParseStoredRole(r.String())
		if err != nil {
			t.Errorf("ParseStoredRole(%q): %v", r, err)
		}
		if got != r {
			t.Errorf("ParseStoredRole(%q) = %q", r, got)
		}
	}

	for _, s := range []string{"", "owner", "Creator", "admin "} {
		if _, err := accessbus.ParseStoredRole(s); !errors.Is(err, accessbus.ErrNotARole) {
			t.Errorf("ParseStoredRole(%q) = %v, want ErrNotARole", s, err)
		}
	}
}

// ParseRole stays narrow, which is the other half of the same bargain: the
// per-form people page must not be able to hand out a role that no per-form
// gate ever asks for.
func TestParseRoleStillRefusesCreator(t *testing.T) {
	if _, err := accessbus.ParseRole(accessbus.RoleCreator.String()); !errors.Is(err, accessbus.ErrNotARole) {
		t.Errorf("ParseRole(creator) = %v, want ErrNotARole", err)
	}

	if slices.Contains(accessbus.Roles(), accessbus.RoleCreator) {
		t.Error("Roles() offers creator, which the per-form people page would then put in its dropdown")
	}
}

// CreatesForms is the rule in one place, and the gate in front of /build/new
// and the landing page that links to it both read it. Spelled out here so
// that moving a role between the two answers is a test failure rather than a
// link that refuses whoever follows it.
func TestWhichRolesCreateForms(t *testing.T) {
	cases := map[accessbus.Role]bool{
		accessbus.RoleCreator:   true,
		accessbus.RoleAdmin:     true,
		accessbus.RoleResults:   false,
		accessbus.RoleDoor:      false,
		accessbus.Role(""):      false,
		accessbus.Role("owner"): false,
	}

	for r, want := range cases {
		if got := r.CreatesForms(); got != want {
			t.Errorf("Role(%q).CreatesForms() = %v, want %v", r, got, want)
		}
	}
}

// Demoting the last site-wide administrator leaves precisely the service
// revoking them would: one nobody can grant anything on. Revoke has always
// refused it, and now that there is a second site-wide role to demote
// somebody to, Grant refuses it too.
func TestTheLastSiteAdminCannotBeDemoted(t *testing.T) {
	b, _ := newBusiness()

	boss := types.NewID()
	if _, err := b.Grant(t.Context(), now, types.ID{}, boss, types.Slug{}, accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	if _, err := b.Grant(t.Context(), now, types.ID{}, boss, types.Slug{}, accessbus.RoleCreator); !errors.Is(err, accessbus.ErrLastAdmin) {
		t.Errorf("demoting the only site administrator = %v, want ErrLastAdmin", err)
	}

	// And they are still what they were: a refused demotion must not be a
	// half-done one.
	if can, err := b.CanCreateForms(t.Context(), boss); err != nil || !can {
		t.Errorf("after the refusal CanCreateForms = %v, %v, want true, nil", can, err)
	}

	// Re-granting the role they already hold is not a demotion and still
	// works, which is how the audit line on the row gets refreshed.
	if _, err := b.Grant(t.Context(), now, types.ID{}, boss, types.Slug{}, accessbus.RoleAdmin); err != nil {
		t.Errorf("re-granting admin to the only administrator: %v", err)
	}

	// With a second administrator there is no last one, and the demotion goes
	// through.
	second := types.NewID()
	if _, err := b.Grant(t.Context(), now, types.ID{}, second, types.Slug{}, accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	if _, err := b.Grant(t.Context(), now, types.ID{}, boss, types.Slug{}, accessbus.RoleCreator); err != nil {
		t.Errorf("demoting one of two administrators: %v", err)
	}
}

// A creator is not an administrator for the purpose of the last-administrator
// count. Somebody holding it can grant nothing, so counting them would be
// counting a way back that does not exist.
func TestACreatorDoesNotCountAsTheAdministratorLeftBehind(t *testing.T) {
	b, _ := newBusiness()

	boss := types.NewID()
	if _, err := b.Grant(t.Context(), now, types.ID{}, boss, types.Slug{}, accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	volunteer := types.NewID()
	if _, err := b.Grant(t.Context(), now, types.ID{}, volunteer, types.Slug{}, accessbus.RoleCreator); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	if err := b.Revoke(t.Context(), boss, types.Slug{}); !errors.Is(err, accessbus.ErrLastAdmin) {
		t.Errorf("revoking the only administrator while a creator exists = %v, want ErrLastAdmin", err)
	}

	// Revoking the creator is never refused: nothing depends on them.
	if err := b.Revoke(t.Context(), volunteer, types.Slug{}); err != nil {
		t.Errorf("revoking a creator: %v", err)
	}
}

// Taking away the site-wide grant takes away only that row. The forms a
// creator has already made are theirs by a per-form grant, and those are a
// separate decision somebody has to make separately.
func TestRevokingSiteWideAccessLeavesTheFormsTheyAlreadyRun(t *testing.T) {
	b, _ := newBusiness()

	boss := types.NewID()
	if _, err := b.Grant(t.Context(), now, types.ID{}, boss, types.Slug{}, accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	volunteer := types.NewID()
	if _, err := b.Grant(t.Context(), now, types.ID{}, volunteer, types.Slug{}, accessbus.RoleCreator); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	theirs := mustSlug(t, "bake-sale-2026")
	if _, err := b.Grant(t.Context(), now, volunteer, volunteer, theirs, accessbus.RoleAdmin); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	if err := b.Revoke(t.Context(), volunteer, types.Slug{}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if can, err := b.CanCreateForms(t.Context(), volunteer); err != nil || can {
		t.Errorf("after revocation CanCreateForms = %v, %v, want false, nil", can, err)
	}

	if ok, err := b.Allowed(t.Context(), volunteer, theirs, accessbus.RoleAdmin); err != nil || !ok {
		t.Errorf("after revocation they lost the form they made: Allowed = %v, %v, want true, nil", ok, err)
	}
}

// An unreadable store is an error and never a refusal, the same rule Allowed
// is held to -- a gate that reads a failure as "no" turns a database outage
// into everybody being told they may not create a form.
func TestCanCreateFormsReportsAFailingStore(t *testing.T) {
	b, store := newBusiness()

	store.fail = errors.New("the database is on fire")

	can, err := b.CanCreateForms(t.Context(), types.NewID())
	if err == nil {
		t.Error("a failing store was not reported as an error")
	}
	if can {
		t.Error("a failing store answered true")
	}
}

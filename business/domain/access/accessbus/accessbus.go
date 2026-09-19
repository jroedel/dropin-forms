// Package accessbus holds who may read and change which form.
//
// A grant is one row -- (account, form, role) -- and there are two roles:
// results reads submissions and exports them, admin does that and edits the
// form itself. Nothing is granted by default, so a new account sees nothing
// until somebody says otherwise.
//
// # Why this is a domain of its own rather than a field on a user
//
// The alternative is an is_admin column on users, and it stops working the
// first time one person should read the lunch numbers without also being able
// to change the ticket price. Beyond that, keeping it separate is what lets
// formbus stay ignorant of what an account is and userbus stay ignorant of
// what a form is: neither imports the other, and this package imports neither.
// It deals in identifiers and knows nothing about either thing it connects.
//
// # The site-wide grant, and why it has to exist
//
// A grant whose form is the zero [types.Slug] applies to every form, present
// and future. Without one this package would be unusable on its first day:
// the bootstrap secret produces an account, that account holds no grants, and
// the only way to grant anything is to already hold a grant. Somebody has to
// be able to start, and a site-wide admin is that somebody.
//
// It is deliberately not a separate concept with its own table and its own
// check. One lookup shape, one revoke, one listing, and the only difference is
// which slug is in the row -- because a second code path for "and also the
// superuser" is how a permission check ends up with a branch nobody tested.
//
// # What this package does not check
//
// Whether the account is enabled: [userbus.Business.Authenticate] refuses a
// disabled account before a request reaches any gate, and repeating the check
// here would mean this package needed to read accounts.
//
// Whether the form exists: forms are defined in TOML and loaded at startup,
// not stored in SQL, so there is no row to point a foreign key at. A grant may
// name a form that has since been renamed away, which costs nothing -- it
// authorises access to something no route can reach. The app layer resolves
// the slug against the form store anyway, and does it before this gate is
// consulted.
package accessbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jroedel/dropin-forms/business/types"
)

// Role is what somebody may do with a form.
//
// The set is closed, and small on purpose. Every role here is enforced at a
// route, and a role that no route checks is a promise in a database.
type Role string

const (
	// RoleResults reads submissions and exports them, and changes nothing.
	// This is the role for whoever is counting lunches.
	RoleResults Role = "results"

	// RoleDoor marks an order collected at the will-call table, and includes
	// everything RoleResults can do.
	//
	// It exists because the two authorities either side of it are both wrong
	// for somebody working a table on their own phone. RoleResults changes
	// nothing, which is the boundary the roles were drawn on and is worth
	// keeping -- "the reading role can now also write one field" is a change
	// nobody can see six months later. RoleAdmin is too much: since the
	// builder shipped it means editing the form and setting the ticket price,
	// and the morning the tickets are sold is the worst moment for two
	// volunteers to hold that.
	//
	// So it is the narrowest thing that does the job: read this form's
	// submissions, and record that somebody was handed their tokens.
	RoleDoor Role = "door"

	// RoleAdmin edits the form and manages who else may see it, and includes
	// everything RoleDoor and RoleResults can do.
	RoleAdmin Role = "admin"
)

// roles is every Role, weakest first. The order is the implication order, and
// [Role.Includes] reads it -- so adding a role means putting it in the right
// place here rather than editing a comparison.
//
// RoleDoor went in the middle rather than on the end, which is the whole of
// what adding it took: whoever works the table can read the list they are
// checking off, and an administrator can work the table.
var roles = []Role{RoleResults, RoleDoor, RoleAdmin}

// The errors this package returns.
var (
	// ErrNotARole is an unrecognised role name, which reaching this package
	// means either a hand-typed request or a row written by a newer binary.
	ErrNotARole = errors.New("not a role")

	// ErrNotFound is no grant for that account on that form.
	ErrNotFound = errors.New("no such grant")

	// ErrLastAdmin refuses the revocation that would leave nobody able to
	// grant anything. See [Business.Revoke].
	ErrLastAdmin = errors.New("that is the last site-wide administrator")
)

// ParseRole reads a role name, and is the only way to get a Role from
// anything a person or a database supplied.
func ParseRole(s string) (Role, error) {
	if r := Role(s); slices.Contains(roles, r) {
		return r, nil
	}

	return "", fmt.Errorf("%w: %q is not one of %v", ErrNotARole, s, roles)
}

// Roles returns every role, weakest first, for a page offering a choice.
func Roles() []Role { return slices.Clone(roles) }

// Includes reports whether holding r is enough to do what want requires.
//
// This is where "admin implies results" lives, and it lives in one method so
// that no gate has to spell the implication out. A gate asks for the role its
// route needs and this answers.
func (r Role) Includes(want Role) bool {
	held := slices.Index(roles, r)
	needed := slices.Index(roles, want)

	// An unrecognised role on either side includes nothing. A row written by
	// a newer binary must not be read as more authority than this one
	// understands -- the safe reading of a value you do not recognise is
	// none.
	if held < 0 || needed < 0 {
		return false
	}

	return held >= needed
}

func (r Role) String() string { return string(r) }

// Zero reports whether this is the unset role, which authorises nothing.
func (r Role) Zero() bool { return r == "" }

// Grant is one account's authority over one form.
type Grant struct {
	UserID types.ID

	// Form is the form this applies to. The zero Slug means every form; see
	// the package comment.
	Form types.Slug

	Role Role

	// GrantedBy is who did it, for the audit line on the page that lists
	// these. The zero ID means the configuration did: the bootstrap secret
	// grants the account it creates, and there is no person behind that.
	GrantedBy types.ID

	GrantedAt time.Time
}

// SiteWide reports whether this grant covers every form.
func (g Grant) SiteWide() bool { return g.Form.Zero() }

// Storer is what this package needs from storage.
//
// Upsert rather than a create and an update, because a grant is keyed by
// (account, form) and re-granting is the ordinary way to change somebody's
// role. Two methods would mean every caller first asking whether a row exists,
// which is a race and more code for the same result.
type Storer interface {
	Upsert(ctx context.Context, g Grant) error
	Delete(ctx context.Context, userID types.ID, form types.Slug) error
	ByUserAndForm(ctx context.Context, userID types.ID, form types.Slug) (Grant, error)
	ByUser(ctx context.Context, userID types.ID) ([]Grant, error)
	ByForm(ctx context.Context, form types.Slug) ([]Grant, error)
	All(ctx context.Context) ([]Grant, error)
}

// Business is the set of operations on grants.
type Business struct {
	log   *slog.Logger
	store Storer
}

// NewBusiness constructs one.
func NewBusiness(log *slog.Logger, store Storer) *Business {
	return &Business{log: log, store: store}
}

// Allowed reports whether this account may do what want requires on this form.
//
// Two lookups, in this order: the grant for this form, then the site-wide one.
// The specific grant is tried first so that the common case -- somebody who
// holds results on exactly one form -- is answered by one indexed read on a
// primary key.
//
// A false with a nil error is a refusal. An error is an error, and a caller
// must not treat it as a refusal: a gate that reads "if !ok { refuse }" turns
// an unreachable database into a permission denial, and whoever hits it spends
// the afternoon wondering what they did wrong.
func (b *Business) Allowed(ctx context.Context, userID types.ID, form types.Slug, want Role) (bool, error) {
	if userID.Zero() || want.Zero() {
		return false, nil
	}

	for _, on := range []types.Slug{form, {}} {
		g, err := b.store.ByUserAndForm(ctx, userID, on)

		switch {
		case errors.Is(err, ErrNotFound):
			continue
		case err != nil:
			return false, fmt.Errorf("looking up the grant on %q: %w", on.String(), err)
		}

		if g.Role.Includes(want) {
			return true, nil
		}

		// No break. A form-specific results grant does not shadow a site-wide
		// admin one; holding both is holding the stronger.
	}

	return false, nil
}

// Grant gives an account a role on a form, or changes the role it already has.
//
// granter is recorded rather than checked. Whether the person doing this is
// allowed to is a question about the request, and it is answered by the gate in
// front of the route -- asking it twice, in two places, with two chances to
// disagree, is how the two answers end up differing.
func (b *Business) Grant(ctx context.Context, now time.Time, granter, userID types.ID, form types.Slug, role Role) (Grant, error) {
	if userID.Zero() {
		return Grant{}, errors.New("a grant needs an account")
	}

	if _, err := ParseRole(role.String()); err != nil {
		return Grant{}, err
	}

	g := Grant{
		UserID:    userID,
		Form:      form,
		Role:      role,
		GrantedBy: granter,
		GrantedAt: now.UTC(),
	}

	if err := b.store.Upsert(ctx, g); err != nil {
		return Grant{}, fmt.Errorf("granting %s on %q: %w", role, form.String(), err)
	}

	b.log.Info("granted", "user_id", userID, "form", form.String(), "role", role, "by", granter)

	return g, nil
}

// Revoke removes whatever grant an account holds on a form.
//
// It refuses to remove the last site-wide admin, with [ErrLastAdmin]. That
// leaves nobody who can grant anything, and the way back is the one-time
// bootstrap secret -- which is one-time, so for most installations there is no
// way back at all short of editing the database by hand. There is no CLI here
// to do that with.
//
// Revoking a form-specific grant is never refused, however lonely it is: the
// person holding site-wide admin can always restore it.
func (b *Business) Revoke(ctx context.Context, userID types.ID, form types.Slug) error {
	if form.Zero() {
		last, err := b.lastSiteAdmin(ctx, userID)
		if err != nil {
			return err
		}

		if last {
			return fmt.Errorf("%w, so removing it would leave nobody able to grant anything", ErrLastAdmin)
		}
	}

	if err := b.store.Delete(ctx, userID, form); err != nil {
		return fmt.Errorf("revoking the grant on %q: %w", form.String(), err)
	}

	b.log.Info("revoked", "user_id", userID, "form", form.String())

	return nil
}

// lastSiteAdmin reports whether this account is the only site-wide admin.
func (b *Business) lastSiteAdmin(ctx context.Context, userID types.ID) (bool, error) {
	grants, err := b.store.ByForm(ctx, types.Slug{})
	if err != nil {
		return false, fmt.Errorf("counting the site-wide administrators: %w", err)
	}

	var admins, mine int
	for _, g := range grants {
		if g.Role != RoleAdmin {
			continue
		}

		admins++
		if g.UserID == userID {
			mine++
		}
	}

	// mine > 0 as well as admins == 1, so that revoking somebody who is not a
	// site admin at all is not refused on the grounds that somebody else is
	// the last one.
	return admins == 1 && mine == 1, nil
}

// ForUser lists everything an account may reach, for the page it lands on
// after signing in.
func (b *Business) ForUser(ctx context.Context, userID types.ID) ([]Grant, error) {
	grants, err := b.store.ByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("listing the grants for that account: %w", err)
	}

	return grants, nil
}

// ForForm lists who may reach one form, for the page that manages that -- and,
// later, for deciding who a submission notification goes to.
//
// Site-wide grants are included, because they are grants on this form. A page
// listing them should say which are which, and [Grant.SiteWide] answers that.
func (b *Business) ForForm(ctx context.Context, form types.Slug) ([]Grant, error) {
	specific, err := b.store.ByForm(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("listing the grants on that form: %w", err)
	}

	if form.Zero() {
		return specific, nil
	}

	site, err := b.store.ByForm(ctx, types.Slug{})
	if err != nil {
		return nil, fmt.Errorf("listing the site-wide grants: %w", err)
	}

	return append(specific, site...), nil
}

// All lists every grant, for the page that shows the whole picture.
func (b *Business) All(ctx context.Context) ([]Grant, error) {
	grants, err := b.store.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the grants: %w", err)
	}

	return grants, nil
}

// EnsureSiteAdmin gives an account site-wide admin if nobody holds it yet.
//
// This is what the bootstrap sign-in calls, and the condition is "nobody yet"
// rather than "this account has none": the bootstrap secret is redeemable once,
// but if it were ever reissued by hand, it must not be a way to award yourself
// authority over a service that already has administrators. Granted returns
// false in that case and the sign-in still succeeds -- the person gets a
// session, and sees nothing, which is the correct outcome for somebody who was
// given a secret they should not have.
func (b *Business) EnsureSiteAdmin(ctx context.Context, now time.Time, userID types.ID) (bool, error) {
	grants, err := b.store.ByForm(ctx, types.Slug{})
	if err != nil {
		return false, fmt.Errorf("checking for an existing administrator: %w", err)
	}

	for _, g := range grants {
		if g.Role == RoleAdmin {
			return false, nil
		}
	}

	if _, err := b.Grant(ctx, now, types.ID{}, userID, types.Slug{}, RoleAdmin); err != nil {
		return false, err
	}

	return true, nil
}

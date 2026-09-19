// Package peopleapp is how somebody is given access to a form.
//
// It is the other half of the notification work. Submissions are emailed to
// whoever holds results on a form, which was a promise this service could not
// keep: there was no way to give anybody results on anything. The business
// rules were all present -- accessbus.Grant, accessbus.Revoke, userbus.Create
// -- and nothing anywhere exposed them, so the only route to a second account
// was hand-written SQL on the server. This package is that route, in a browser.
//
// # What guards each route
//
//	GET  /forms/{slug}/people          Require, then RequireFormRole(admin)
//	POST /forms/{slug}/people          the same
//	POST /forms/{slug}/people/revoke   the same
//
// admin rather than results throughout, and that is the whole difference
// between this package and submissionapp: reading the numbers is not deciding
// who else may read them. Somebody counting lunches holds results and never
// sees this page.
//
// # There is no self-registration and this is not it
//
// An account here can read other people's names, addresses and what they paid.
// So an account comes into existence exactly one way -- somebody who already
// administers a form types an address on this page -- and userbus.RequestSignIn
// deliberately refuses to create one, so that a stranger cannot make an account
// by trying to sign in.
//
// # Why the invitation is not a sign-in link
//
// The obvious invitation mail carries a link that signs the person in. The
// sign-in tokens this service mints last fifteen minutes, which is right for a
// link somebody asked for thirty seconds ago and wrong for one sent to a
// volunteer who reads their email in the evening: most such invitations would
// be dead on arrival, and a dead link looks like a broken service rather than
// an expired credential. A longer-lived token for this one case would be a
// second credential lifetime to reason about, sitting in a mailbox forever.
//
// So the invitation names the sign-in page instead and the person asks for
// their own link, which is fifteen minutes old when they use it. One credential
// shape, no expiry to explain.
package peopleapp

import (
	"cmp"
	"context"
	"embed"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formtoml"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/mail"
	"github.com/jroedel/dropin-forms/foundation/web"
)

//go:embed templates
var templates embed.FS

// Templates is this app's own template directory, handed to page.NewRenderer
// by whatever builds the renderer.
var Templates = templates

// Forms is where definitions come from. Read-only, and by id alone: this page
// is always about one named form.
type Forms interface {
	ByID(slug types.Slug) (formbus.Form, error)
}

// Accounts is the slice of the account domain this app needs.
//
// No sign-in and no session: making an account and finding one are the whole
// of it. That this interface cannot mint a credential is the point -- the
// invitation mail below names the sign-in page rather than carrying a token,
// and an interface that could not produce a token either way is the clearest
// statement of that.
type Accounts interface {
	ByID(ctx context.Context, id types.ID) (userbus.User, error)
	ByEmail(ctx context.Context, email types.Email) (userbus.User, error)
	Create(ctx context.Context, now time.Time, nu userbus.NewUser) (userbus.User, error)
}

// Grants is what this app changes.
type Grants interface {
	ForForm(ctx context.Context, form types.Slug) ([]accessbus.Grant, error)
	Grant(ctx context.Context, now time.Time, granter, userID types.ID, form types.Slug, role accessbus.Role) (accessbus.Grant, error)
	Revoke(ctx context.Context, userID types.ID, form types.Slug) error
}

// Notifications answers who has turned email about this form off, so the page
// can say who is actually hearing about submissions rather than who is
// theoretically entitled to.
type Notifications interface {
	MutedUsers(ctx context.Context, form types.Slug) ([]types.ID, error)
}

// Config is what this app needs.
type Config struct {
	Log      *slog.Logger
	Forms    Forms
	Accounts Accounts
	Grants   Grants
	Mail     mail.Sender
	Render   *page.Renderer

	// BaseURL is this service's own admin origin, which goes into the
	// invitation. Configured rather than taken from the request, for the same
	// reason the sign-in link is: a link built from a Host header is one a
	// stranger can aim at their own host, and whoever receives it cannot tell.
	BaseURL string

	// Notifications is optional, and without it the page simply does not
	// mention email -- which is right for an installation that sends none. A
	// column saying somebody is being emailed by a service with no relay is
	// worse than no column.
	Notifications Notifications
}

type app struct {
	cfg Config
}

// Routes mounts this app.
//
// guard is Require and admins is RequireFormRole(admin), both handed in by the
// muxer rather than built here: the chain is written down in one place and a
// route's position in it is not a decision an app package gets to make.
func Routes(mux *http.ServeMux, cfg Config, guard, admins func(http.Handler) http.Handler) {
	a := app{cfg: cfg}

	behind := func(h http.HandlerFunc) http.Handler {
		return guard(admins(h))
	}

	mux.Handle("GET /forms/{"+mid.FormSlugParam+"}/people", behind(a.list))
	mux.Handle("POST /forms/{"+mid.FormSlugParam+"}/people", behind(a.add))
	mux.Handle("POST /forms/{"+mid.FormSlugParam+"}/people/revoke", behind(a.revoke))
}

// listView is the page: who can see this form, and the form for adding
// somebody.
type listView struct {
	Form   formbus.Form
	FormID string
	People []personView
	Roles  []roleView

	// Email says whether this installation can tell anybody about a
	// submission, and therefore whether the email column means anything.
	Email bool

	// Done and Problem are what just happened. One of them at most, and both
	// are sentences rather than statuses, because this page is the only place
	// the outcome is reported.
	Done    string
	Problem string

	// What was typed, so that a refused entry comes back filled in rather than
	// blank. Nobody should have to retype an address because they picked the
	// wrong role.
	Name    string
	Address string
	Role    string
}

type personView struct {
	UserID string
	Name   string
	Email  string
	Role   string
	Since  string

	// SiteWide marks somebody who holds every form. They are listed because
	// they can read this one and are emailed about it, and leaving them out
	// would make the page a lie -- but this page cannot remove them; see
	// revoke.
	SiteWide bool

	// Emailed says whether this person currently hears about submissions.
	// False means they turned it off themselves, which is theirs to decide and
	// is shown here only so that "why am I the only one getting these" has an
	// answer somebody can look up.
	Emailed bool

	// You marks the reader's own row, which is also the row with no remove
	// button on it.
	You bool
}

// roleView is one choice in the role select. The label is here rather than on
// accessbus.Role because it is a sentence for a person, and the business
// package should not be the place a wording change is made.
type roleView struct {
	Value string
	Label string
}

func (v listView) Any() bool { return len(v.People) > 0 }

func (a app) list(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	a.show(w, r, http.StatusOK, f, listView{})
}

// add gives somebody a role on this form, creating the account if the address
// is new.
//
// Re-rendered rather than redirected, so the outcome can be a sentence naming
// the person. Submitting it twice grants the same role twice, which accessbus
// stores as one row -- so a reload is not a mistake worth guarding against.
func (a app) add(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	me, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "a people page was reached with no account", nil)

		return
	}

	if err := r.ParseForm(); err != nil {
		a.show(w, r, http.StatusBadRequest, f, listView{
			Problem: "We could not read that. Please try again.",
		})

		return
	}

	typed := strings.TrimSpace(r.PostFormValue("email"))
	name := strings.TrimSpace(r.PostFormValue("name"))

	// What was typed goes back onto the page with every refusal below.
	said := listView{Name: name, Address: typed, Role: r.PostFormValue("role")}

	role, err := accessbus.ParseRole(r.PostFormValue("role"))
	if err != nil {
		said.Problem = "Choose what they may do with this form."
		a.show(w, r, http.StatusBadRequest, f, said)

		return
	}

	email, err := types.ParseEmail(typed)
	if err != nil {
		said.Problem = "That does not look like an email address. Check for a typo."
		a.show(w, r, http.StatusBadRequest, f, said)

		return
	}

	now := time.Now()

	u, err := a.cfg.Accounts.ByEmail(r.Context(), email)
	created := false

	switch {
	case errors.Is(err, userbus.ErrNotFound):
		// A name is not required, because the address is what identifies
		// somebody and an invitation should not be blocked on knowing how they
		// spell their surname.
		u, err = a.cfg.Accounts.Create(r.Context(), now, userbus.NewUser{Email: email, Name: name})
		if err != nil {
			a.oops(w, r, "the account could not be created", err)

			return
		}

		created = true

	case err != nil:
		a.oops(w, r, "the accounts could not be read", err)

		return
	}

	// An existing account is not renamed from here, even when a name was
	// typed. This page is about access, and quietly changing what somebody is
	// called because a colleague guessed at their name while granting them a
	// role is a surprise nobody asked for.

	if _, err := a.cfg.Grants.Grant(r.Context(), now, me.ID, u.ID, f.ID, role); err != nil {
		a.oops(w, r, "the grant could not be saved", err)

		return
	}

	a.cfg.Log.Info("access granted",
		"request_id", web.RequestIDFrom(r.Context()),
		"form", f.ID.String(), "user_id", u.ID.String(), "role", role.String(),
		"by", me.ID.String(), "new_account", created)

	// The invitation, and its failure is reported rather than swallowed. On
	// the sign-in page a send failure has to be silent, because saying so
	// would say which addresses have accounts; here the reader is an
	// administrator who has just typed the address themselves, and the useful
	// answer is "tell them yourself, because we could not".
	invitation := a.invite(u, f, role, created)

	if err := a.cfg.Mail.Send(r.Context(), invitation); err != nil {
		a.cfg.Log.Error("an invitation could not be sent",
			"request_id", web.RequestIDFrom(r.Context()),
			"user_id", u.ID.String(), "error", err)

		a.show(w, r, http.StatusOK, f, listView{
			Problem: u.Email.String() + " now has access, but we could not send them an email about it. Please tell them yourself: they sign in at " + a.cfg.BaseURL + "/signin with that address.",
		})

		return
	}

	a.show(w, r, http.StatusOK, f, listView{
		Done: u.Email.String() + " can now " + verb(role) + " this form, and we have emailed them about it.",
	})
}

// revoke takes somebody's access to this form away.
//
// It can only ever remove a grant on the form in the path, because that is the
// slug it passes -- so a site-wide grant cannot be removed here however the
// request is made, which is why those rows have no button. Site-wide access is
// the thing that would leave a service with nobody able to grant anything, and
// accessbus refuses to remove the last of it; this page simply never asks.
func (a app) revoke(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	me, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "a people page was reached with no account", nil)

		return
	}

	if err := r.ParseForm(); err != nil {
		a.show(w, r, http.StatusBadRequest, f, listView{
			Problem: "We could not read that. Please try again.",
		})

		return
	}

	id, err := types.ParseID(r.PostFormValue("user"))
	if err != nil {
		a.show(w, r, http.StatusBadRequest, f, listView{
			Problem: "We could not tell who that was. Please try again.",
		})

		return
	}

	// Removing your own access to the form you are administering locks you out
	// of the page you are standing on, and the way back is another
	// administrator. Refused here rather than in accessbus: "you" is a fact
	// about the request, and the business rule -- do not remove the last
	// site-wide admin -- is a different and larger one.
	if id == me.ID {
		a.show(w, r, http.StatusConflict, f, listView{
			Problem: "You cannot remove your own access to this form. Ask another administrator to do it.",
		})

		return
	}

	if err := a.cfg.Grants.Revoke(r.Context(), id, f.ID); err != nil {
		a.oops(w, r, "the grant could not be revoked", err)

		return
	}

	a.cfg.Log.Info("access revoked",
		"request_id", web.RequestIDFrom(r.Context()),
		"form", f.ID.String(), "user_id", id.String(), "by", me.ID.String())

	// The account is left alone, and so is any preference it has recorded
	// about this form. Both are deliberate: an account may hold other forms,
	// and somebody who switched these emails off and is later given the form
	// back had a reason the first time.
	who := id.String()
	if u, err := a.cfg.Accounts.ByID(r.Context(), id); err == nil {
		who = u.Email.String()
	}

	a.show(w, r, http.StatusOK, f, listView{
		Done: who + " no longer has access to this form.",
	})
}

// show builds the listing and renders it, carrying whatever the caller wants
// said at the top.
//
// One function for all four outcomes, because every one of them ends on this
// page with a fresh list: a page that reported a change without showing it is
// a page somebody reloads to find out whether it worked.
func (a app) show(w http.ResponseWriter, r *http.Request, status int, f formbus.Form, view listView) {
	view.Form = f
	view.FormID = f.ID.String()
	view.Roles = rolesFor()

	me, _ := mid.UserFrom(r.Context())

	grants, err := a.cfg.Grants.ForForm(r.Context(), f.ID)
	if err != nil {
		a.oops(w, r, "the grants on that form could not be listed", err)

		return
	}

	// Who has turned these emails off, in one call before the loop. A failure
	// costs the column rather than the page: this list's job is who can see
	// the form.
	quiet := map[types.ID]bool{}

	if a.cfg.Notifications != nil {
		view.Email = true

		muted, err := a.cfg.Notifications.MutedUsers(r.Context(), f.ID)
		if err != nil {
			a.cfg.Log.Error("the notification preferences could not be read",
				"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)

			view.Email = false
		}

		for _, id := range muted {
			quiet[id] = true
		}
	}

	// One account lookup per grant. A form's grant holders are a parish office
	// rather than a mailing list, so this is a handful of primary-key reads;
	// if a form ever has hundreds, this is the line to change.
	for _, g := range grants {
		u, err := a.cfg.Accounts.ByID(r.Context(), g.UserID)
		if err != nil {
			// A grant naming an account that is not there. Storage cascades
			// deletes, so this should not happen -- logged and skipped rather
			// than shown as a blank row, which would be a person nobody could
			// identify and nobody could remove.
			a.cfg.Log.Error("a grant names an account that could not be read",
				"request_id", web.RequestIDFrom(r.Context()),
				"form", f.ID.String(), "user_id", g.UserID.String(), "error", err)

			continue
		}

		view.People = append(view.People, personView{
			UserID:   u.ID.String(),
			Name:     u.Name,
			Email:    u.Email.String(),
			Role:     g.Role.String(),
			Since:    g.GrantedAt.Local().Format("2 Jan 2006"),
			SiteWide: g.SiteWide(),
			Emailed:  !quiet[u.ID],
			You:      u.ID == me.ID,
		})
	}

	// Form-specific first, then site-wide, and by name inside each group. The
	// grouping is the useful one: the top of the list is what this page can
	// change, and everybody below it holds the whole service.
	slices.SortFunc(view.People, func(x, y personView) int {
		return cmp.Or(
			boolCmp(x.SiteWide, y.SiteWide),
			cmp.Compare(strings.ToLower(cmp.Or(x.Name, x.Email)), strings.ToLower(cmp.Or(y.Name, y.Email))),
		)
	})

	a.cfg.Render.Render(w, r, status, "people", view)
}

// boolCmp orders false before true.
func boolCmp(x, y bool) int {
	switch {
	case x == y:
		return 0
	case y:
		return -1
	default:
		return 1
	}
}

// rolesFor is the choice offered when adding somebody, weakest first -- which
// is accessbus's own order, so a role added there appears here without this
// file being edited. A role with no sentence written for it shows its own name
// rather than nothing.
func rolesFor() []roleView {
	all := accessbus.Roles()
	out := make([]roleView, 0, len(all))

	for _, r := range all {
		out = append(out, roleView{Value: r.String(), Label: describe(r)})
	}

	return out
}

func describe(r accessbus.Role) string {
	switch r {
	case accessbus.RoleResults:
		return "Read the submissions and download them"
	case accessbus.RoleAdmin:
		return "Read the submissions, and manage who else can"
	default:
		return r.String()
	}
}

// verb is how a granted role reads in a sentence about a person.
func verb(r accessbus.Role) string {
	if r == accessbus.RoleAdmin {
		return "read and manage"
	}

	return "read"
}

// invite is the message somebody gets when they are given a form.
func (a app) invite(u userbus.User, f formbus.Form, role accessbus.Role, created bool) mail.Message {
	var b strings.Builder

	if created {
		b.WriteString("You have been given an account for the Schoenstatt Austin forms admin, and access to " + f.Title + ".\r\n\r\n")
	} else {
		b.WriteString("You have been given access to " + f.Title + " in the Schoenstatt Austin forms admin.\r\n\r\n")
	}

	b.WriteString("You can read its submissions")

	if role == accessbus.RoleAdmin {
		b.WriteString(", and decide who else can")
	}

	b.WriteString(".\r\n\r\n")

	// The sign-in page rather than a link that signs them in. See the package
	// comment: a fifteen-minute token in a message read that evening is a dead
	// link, and a dead link looks like a broken service.
	b.WriteString("Sign in here with this address and we will email you a link:\r\n\r\n" +
		a.cfg.BaseURL + "/signin\r\n\r\n")

	if a.cfg.Notifications != nil {
		b.WriteString("You will also get an email each time somebody submits this form. " +
			"Every one of those has a link at the bottom for turning them off, and doing " +
			"that does not affect your access.\r\n")
	}

	return mail.Message{
		To:      u.Email.String(),
		Subject: "You have access to " + f.Title,
		Text:    b.String(),
	}
}

// form resolves the slug in the path.
//
// The role gate in front of these routes has already checked an admin grant on
// that slug, so reaching here means the account may manage this form -- but the
// gate does not know whether the form exists, because grants are rows and
// definitions are files. A grant on a form that has since been renamed away
// lands here and gets a 404.
func (a app) form(w http.ResponseWriter, r *http.Request) (formbus.Form, bool) {
	slug, err := types.ParseSlug(r.PathValue(mid.FormSlugParam))
	if err != nil {
		a.notFound(w, r)

		return formbus.Form{}, false
	}

	f, err := a.cfg.Forms.ByID(slug)

	switch {
	case errors.Is(err, formtoml.ErrNotFound):
		a.notFound(w, r)

		return formbus.Form{}, false
	case err != nil:
		a.oops(w, r, "a form could not be read", err)

		return formbus.Form{}, false
	}

	return f, true
}

// notFound is plain text rather than a page, and deliberately the same
// sentence mid.RequireFormRole uses for the same case. Borrowing
// submissionapp's "no-form" template would work -- one renderer holds every
// page on the surface -- but it would make this app depend on that one being
// mounted, which is the coupling the no-App-imports-an-App rule exists to
// prevent, minus the import that would have made it visible.
func (a app) notFound(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "there is no form by that name.", http.StatusNotFound)
}

// oops logs the detail and shows a sentence. The error never reaches the page:
// this surface is behind a session, but a database error can still name a
// table, and that belongs in the log rather than in front of anybody.
func (a app) oops(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.cfg.Log.Error(what,
		"request_id", web.RequestIDFrom(r.Context()), "error", err)
	http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)
}

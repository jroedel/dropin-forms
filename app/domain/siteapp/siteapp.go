// Package siteapp is who runs the whole service.
//
// Every other people-facing page in this repository is about one form:
// peopleapp decides who may see it, formapp's builder decides what it asks.
// This one is about the service itself -- the site-wide grant accessbus has
// always had, which until now only the one-time bootstrap secret could hand
// out. That was enough while there was exactly one thing a site-wide grant
// could mean, RoleAdmin, and exactly one moment it was needed, standing the
// service up. accessbus.RoleCreator is a second thing a site-wide grant can
// mean -- may make a form, and nothing else -- and a role nobody can be given
// is a role that does not exist, so this page is what gives it.
//
// # What guards each route
//
//	GET  /site/people          Require, then RequireSiteAdmin(admin)
//	POST /site/people          the same
//	POST /site/people/role     the same
//	POST /site/people/revoke   the same
//
// Admin throughout, and site-wide admin specifically rather than the
// per-form gate peopleapp sits behind: deciding who else administers the
// whole service, or who else may start a form of their own, is not a
// question any one form's administrator gets to answer.
//
// # Why this is not a page inside peopleapp
//
// peopleapp's every view, route and template carries a formbus.Form -- the
// page is always about one, named in the path. A site-wide grant is about
// none, and forcing it through that shape would mean a form parameter that
// is always empty and a template that says "site-wide" everywhere it would
// otherwise say the form's title. Small and separate reads better than the
// alternative, and it is not a new idea: mid.RequireSiteAdmin already exists
// as a second gate for the same reason formapp's /build/new is not behind
// RequireFormRole with an empty slug.
package siteapp

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

// Accounts is the slice of the account domain this app needs -- the same
// shape peopleapp asks for, and for the same reason: making an account and
// finding one is the whole of it, and nothing here can mint a credential.
type Accounts interface {
	ByID(ctx context.Context, id types.ID) (userbus.User, error)
	ByEmail(ctx context.Context, email types.Email) (userbus.User, error)
	Create(ctx context.Context, now time.Time, nu userbus.NewUser) (userbus.User, error)
}

// Grants is what this app changes, always with the zero [types.Slug]: every
// grant this page lists, gives or takes away is site-wide by definition.
type Grants interface {
	ForForm(ctx context.Context, form types.Slug) ([]accessbus.Grant, error)
	Grant(ctx context.Context, now time.Time, granter, userID types.ID, form types.Slug, role accessbus.Role) (accessbus.Grant, error)
	Revoke(ctx context.Context, userID types.ID, form types.Slug) error
}

// Config is what this app needs.
type Config struct {
	Log      *slog.Logger
	Accounts Accounts
	Grants   Grants
	Mail     mail.Sender
	Render   *page.Renderer

	// BaseURL is this service's own admin origin, which goes into the
	// invitation, for the same reason peopleapp's is: a link built from a
	// Host header is one a stranger can aim at their own host.
	BaseURL string
}

type app struct {
	cfg Config
}

// Routes mounts this app.
//
// guard is Require and site is RequireSiteAdmin(admin), both handed in by the
// muxer rather than built here, for the reason every other app's Routes gives:
// a route's position in the chain is written down in one place.
func Routes(mux *http.ServeMux, cfg Config, guard, site func(http.Handler) http.Handler) {
	a := app{cfg: cfg}

	behind := func(h http.HandlerFunc) http.Handler {
		return guard(site(h))
	}

	mux.Handle("GET /site/people", behind(a.list))
	mux.Handle("POST /site/people", behind(a.add))
	mux.Handle("POST /site/people/role", behind(a.changeRole))
	mux.Handle("POST /site/people/revoke", behind(a.revoke))
}

// listView is the page: who runs the service, and the form for adding
// somebody.
type listView struct {
	People []personView
	Roles  []roleView

	Done    string
	Problem string

	Address string
	Name    string
	Role    string
}

func (v listView) Any() bool { return len(v.People) > 0 }

type personView struct {
	UserID string
	Name   string
	Email  string
	Role   string
	Since  string

	// You marks the reader's own row, which has no role control and no
	// remove button -- the same reason peopleapp's does not: changing or
	// removing your own site-wide grant from this page is how you lock
	// yourself out of it, and the way back is another administrator.
	You bool

	Choices []roleView
}

type roleView struct {
	Value    string
	Label    string
	Selected bool
}

func (a app) list(w http.ResponseWriter, r *http.Request) {
	a.show(w, r, http.StatusOK, listView{})
}

// add gives somebody a site-wide role, creating the account if the address is
// new. Mirrors peopleapp.add; see its comments for why an account only ever
// comes from typing an address here, why the invitation names the sign-in
// page rather than carrying a token, and why a re-submission is not a
// mistake worth guarding against.
func (a app) add(w http.ResponseWriter, r *http.Request) {
	me, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "the site people page was reached with no account", nil)

		return
	}

	if err := r.ParseForm(); err != nil {
		a.show(w, r, http.StatusBadRequest, listView{
			Problem: "We could not read that. Please try again.",
		})

		return
	}

	typed := strings.TrimSpace(r.PostFormValue("email"))
	name := strings.TrimSpace(r.PostFormValue("name"))

	said := listView{Name: name, Address: typed, Role: r.PostFormValue("role")}

	role, err := accessbus.ParseSiteRole(r.PostFormValue("role"))
	if err != nil {
		said.Problem = "Choose what they may do."
		a.show(w, r, http.StatusBadRequest, said)

		return
	}

	email, err := types.ParseEmail(typed)
	if err != nil {
		said.Problem = "That does not look like an email address. Check for a typo."
		a.show(w, r, http.StatusBadRequest, said)

		return
	}

	now := time.Now()

	u, err := a.cfg.Accounts.ByEmail(r.Context(), email)
	created := false

	switch {
	case errors.Is(err, userbus.ErrNotFound):
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

	if u.ID == me.ID {
		said.Problem = "That is your own address. Your own role is not changed from here."
		a.show(w, r, http.StatusConflict, said)

		return
	}

	if _, err := a.cfg.Grants.Grant(r.Context(), now, me.ID, u.ID, types.Slug{}, role); err != nil {
		a.oops(w, r, "the grant could not be saved", err)

		return
	}

	a.cfg.Log.Info("site-wide access granted",
		"request_id", web.RequestIDFrom(r.Context()),
		"user_id", u.ID.String(), "role", role.String(), "by", me.ID.String(), "new_account", created)

	invitation := a.invite(u, role, created)

	if err := a.cfg.Mail.Send(r.Context(), invitation); err != nil {
		a.cfg.Log.Error("an invitation could not be sent",
			"request_id", web.RequestIDFrom(r.Context()), "user_id", u.ID.String(), "error", err)

		a.show(w, r, http.StatusOK, listView{
			Problem: u.Email.String() + " now has access, but we could not send them an email about it. Please tell them yourself: they sign in at " + a.cfg.BaseURL + "/signin with that address.",
		})

		return
	}

	a.show(w, r, http.StatusOK, listView{
		Done: u.Email.String() + " can now " + verb(role) + ", and we have emailed them about it.",
	})
}

// changeRole changes what somebody already holds site-wide. Deliberately
// narrower than add, for the reason peopleapp.changeRole gives for its own
// narrowness: granting somebody new means typing their address, and a route
// that could do that from a hidden field would be a second way in.
func (a app) changeRole(w http.ResponseWriter, r *http.Request) {
	me, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "the site people page was reached with no account", nil)

		return
	}

	if err := r.ParseForm(); err != nil {
		a.show(w, r, http.StatusBadRequest, listView{
			Problem: "We could not read that. Please try again.",
		})

		return
	}

	id, err := types.ParseID(r.PostFormValue("user"))
	if err != nil {
		a.show(w, r, http.StatusBadRequest, listView{
			Problem: "We could not tell who that was. Please try again.",
		})

		return
	}

	role, err := accessbus.ParseSiteRole(r.PostFormValue("role"))
	if err != nil {
		a.show(w, r, http.StatusBadRequest, listView{
			Problem: "Choose what they may do.",
		})

		return
	}

	if id == me.ID {
		a.show(w, r, http.StatusConflict, listView{
			Problem: "You cannot change your own role here. Ask another administrator to do it.",
		})

		return
	}

	held, err := a.cfg.Grants.ForForm(r.Context(), types.Slug{})
	if err != nil {
		a.oops(w, r, "the site-wide grants could not be listed", err)

		return
	}

	at := slices.IndexFunc(held, func(g accessbus.Grant) bool { return g.UserID == id })
	if at < 0 {
		a.show(w, r, http.StatusConflict, listView{
			Problem: "That person does not hold a site-wide role to change. Add them by their email address below.",
		})

		return
	}

	was := held[at].Role

	if was == role {
		a.show(w, r, http.StatusOK, listView{
			Done: "No change: they already " + verb(role) + ".",
		})

		return
	}

	if _, err := a.cfg.Grants.Grant(r.Context(), time.Now(), me.ID, id, types.Slug{}, role); err != nil {
		if errors.Is(err, accessbus.ErrLastAdmin) {
			a.show(w, r, http.StatusConflict, listView{
				Problem: "They are the only site-wide administrator. Make somebody else one first, so there is still a way to grant anything.",
			})

			return
		}

		a.oops(w, r, "the role could not be changed", err)

		return
	}

	a.cfg.Log.Info("site-wide role changed",
		"request_id", web.RequestIDFrom(r.Context()),
		"user_id", id.String(), "from", was.String(), "to", role.String(), "by", me.ID.String())

	who := id.String()
	if u, err := a.cfg.Accounts.ByID(r.Context(), id); err == nil {
		who = cmp.Or(u.Name, u.Email.String())
	}

	a.show(w, r, http.StatusOK, listView{
		Done: who + " can now " + verb(role) + ". They have not been emailed about it.",
	})
}

// revoke takes somebody's site-wide role away. accessbus.Revoke refuses to
// remove the last RoleAdmin, with [accessbus.ErrLastAdmin]; a RoleCreator
// grant carries no such protection; either way, this page reports whatever
// accessbus says rather than deciding for itself.
func (a app) revoke(w http.ResponseWriter, r *http.Request) {
	me, ok := mid.UserFrom(r.Context())
	if !ok {
		a.oops(w, r, "the site people page was reached with no account", nil)

		return
	}

	if err := r.ParseForm(); err != nil {
		a.show(w, r, http.StatusBadRequest, listView{
			Problem: "We could not read that. Please try again.",
		})

		return
	}

	id, err := types.ParseID(r.PostFormValue("user"))
	if err != nil {
		a.show(w, r, http.StatusBadRequest, listView{
			Problem: "We could not tell who that was. Please try again.",
		})

		return
	}

	if id == me.ID {
		a.show(w, r, http.StatusConflict, listView{
			Problem: "You cannot remove your own site-wide access. Ask another administrator to do it.",
		})

		return
	}

	if err := a.cfg.Grants.Revoke(r.Context(), id, types.Slug{}); err != nil {
		if errors.Is(err, accessbus.ErrLastAdmin) {
			a.show(w, r, http.StatusConflict, listView{
				Problem: "They are the only site-wide administrator. Make somebody else one first, so there is still a way to grant anything.",
			})

			return
		}

		a.oops(w, r, "the grant could not be revoked", err)

		return
	}

	a.cfg.Log.Info("site-wide access revoked",
		"request_id", web.RequestIDFrom(r.Context()), "user_id", id.String(), "by", me.ID.String())

	a.show(w, r, http.StatusOK, listView{Done: "Their site-wide access is gone."})
}

// show fills in what every page load needs and renders it.
func (a app) show(w http.ResponseWriter, r *http.Request, status int, view listView) {
	view.Roles = rolesFor()

	me, _ := mid.UserFrom(r.Context())

	grants, err := a.cfg.Grants.ForForm(r.Context(), types.Slug{})
	if err != nil {
		a.oops(w, r, "the site-wide grants could not be listed", err)

		return
	}

	for _, g := range grants {
		u, err := a.cfg.Accounts.ByID(r.Context(), g.UserID)
		if err != nil {
			a.cfg.Log.Error("a site-wide grant names an account that could not be read",
				"request_id", web.RequestIDFrom(r.Context()), "user_id", g.UserID.String(), "error", err)

			continue
		}

		you := me.ID == g.UserID

		row := personView{
			UserID: g.UserID.String(),
			Name:   u.Name,
			Email:  u.Email.String(),
			Role:   g.Role.String(),
			Since:  g.GrantedAt.Local().Format("2 Jan 2006"),
			You:    you,
		}

		if !you {
			row.Choices = rolesWith(g.Role)
		}

		view.People = append(view.People, row)
	}

	slices.SortFunc(view.People, func(x, y personView) int {
		return cmp.Compare(x.Email, y.Email)
	})

	a.cfg.Render.Render(w, r, status, "site-people", view)
}

func (a app) oops(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.cfg.Log.Error(what,
		"request_id", web.RequestIDFrom(r.Context()), "error", err)
	http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)
}

// rolesFor is the choice offered when adding somebody, weakest first --
// accessbus's own order for a site-wide grant.
func rolesFor() []roleView {
	all := accessbus.SiteRoles()
	out := make([]roleView, 0, len(all))

	for _, r := range all {
		out = append(out, roleView{Value: r.String(), Label: describe(r)})
	}

	return out
}

// rolesWith is the choice on one person's row, with what they hold already
// selected.
func rolesWith(held accessbus.Role) []roleView {
	all := accessbus.SiteRoles()
	out := make([]roleView, 0, len(all))

	for _, r := range all {
		out = append(out, roleView{
			Value:    r.String(),
			Label:    describe(r),
			Selected: r == held,
		})
	}

	return out
}

func describe(r accessbus.Role) string {
	switch r {
	case accessbus.RoleCreator:
		return "Make new forms, and administer only the ones they make"
	case accessbus.RoleAdmin:
		return "Administer the whole service: every form, present and future"
	default:
		return r.String()
	}
}

func verb(r accessbus.Role) string {
	switch r {
	case accessbus.RoleAdmin:
		return "administer the whole service"
	case accessbus.RoleCreator:
		return "make new forms"
	default:
		return r.String()
	}
}

// invite is the message somebody gets when they are given a site-wide role.
// Names the sign-in page rather than carrying a token, for the reason
// peopleapp's own invite does.
func (a app) invite(u userbus.User, role accessbus.Role, created bool) mail.Message {
	var b strings.Builder

	if created {
		b.WriteString("An account has been made for you on the Schoenstatt Austin forms service, and you can now " + verb(role) + ".\r\n\r\n")
	} else {
		b.WriteString("Your account on the Schoenstatt Austin forms service can now " + verb(role) + ".\r\n\r\n")
	}

	b.WriteString("Sign in at " + a.cfg.BaseURL + "/signin with this address, " + u.Email.String() + ", and we will email you a link.\r\n")

	return mail.Message{
		To:      u.Email.String(),
		Subject: "Your access on Schoenstatt Austin forms",
		Text:    b.String(),
	}
}

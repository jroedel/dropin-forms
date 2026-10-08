// Package muxer assembles the request chains and mounts the routes, once.
//
// Adding a route should be one entry in a list, and the thing most easily got
// wrong about a new route is what it sits behind -- so the chains are written
// down here, in one place, rather than re-derived at each mount point.
//
// # Three arrival shapes, not one
//
// The parent project had a single chain. This service has three, because it
// has three kinds of caller and they have nothing in common:
//
//	embed    a stranger on somebody else's website. No cookie, ever.
//	admin    the shrine, signed in, holding a __Host- session cookie.
//	webhook  Stripe, authenticated by a signature over the raw body.
//
// They are separated by *listener* rather than by path prefix. Apache's
// ProxyPreserveHost cannot be set in .htaccess, which is all konsoleH offers,
// so a proxied request arrives with Host: 127.0.0.1:<port> whatever the visitor
// typed -- Host is unusable for this, and a header set by the front end would
// be forgeable if the proxy were ever bypassed. Which socket accepted the
// connection is not. See docs/design/drop-in-forms.md section 3.1.
//
// # The chain, and why in that order
//
//	RequestID              mints the id every line of this request will carry
//	Logging                writes the one "request" line, whatever answers
//	Panics                 recovers, logs it, answers 500
//	SecureHeaders          the per-surface header policy
//	  SameOriginOnly       writes only: refuses a cross-site write
//	  FormEncodedOnly      writes only: refuses a body that is not a form
//	    Authenticate       admin only: establishes who, and refuses nobody
//	      Require          admin only: refuses a request with no account
//	        RequireFormRole    or RequireSiteAdmin, per route
//	          the app
//
// And on the admin listener, under /api/ only, a second chain in place of
// everything from SameOriginOnly down -- see Admin for why it is a separate
// chain rather than exceptions in this one:
//
//	Throttle             per address
//	  Bearer             a personal key, or 401. Never a cookie.
//	    RequireFormRole  or RequireFormCreator, per route: the same gates
//	      the app
//
// The three above SecureHeaders are not gates and refuse nothing. They are
// there so that whatever the gates below decide is logged, and so that a panic
// beneath them is one Error line and a 500 rather than net/http's unstructured
// "panic serving" past a request line that was never written. Logging is
// outside Panics so the request line records the 500; both are inside
// RequestID so both lines carry the id.
//
// # Two things about the webhook, kept here because this is where a route's
// position in a chain is decided
//
// The grant check must not be the parent project's RequireFormToken. That
// middleware passes through when there is no principal, which is correct there
// and would admit every POST on a permanently cookie-free surface while this
// comment still listed a gate. What guards the embed POST is the submission
// grant, redeemed inside the handler.
//
// And the Stripe webhook must not sit behind anything that reads the body.
// RequireFormToken calls r.ParseForm, which would consume the bytes the
// signature is computed over -- so the webhook has a listener of its own, with
// no origin gate and nothing above it that touches a body. See
// app/domain/paymentapp.
//
// # Where the throttles are, and why they are not here
//
// The rate limits of docs/design/drop-in-forms.md section 7.6 are mounted by
// the apps, beside the routes they hold back: embedapp.Limits and
// authapp.DefaultSignInRate. That is not an exception to the rule above but
// the other half of it. What belongs here is which gate a route sits behind,
// because that is a decision about the chain and about what must be withheld
// from the webhook. A throttle's key is route knowledge -- which wildcard
// names the form, which of two POSTs stores something -- and a copy of it here
// would be a second place that has to be edited when a route is renamed.
package muxer

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jroedel/dropin-forms/app/domain/apiapp"
	"github.com/jroedel/dropin-forms/app/domain/authapp"
	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/app/domain/feedapp"
	"github.com/jroedel/dropin-forms/app/domain/formapp"
	"github.com/jroedel/dropin-forms/app/domain/hideapp"
	"github.com/jroedel/dropin-forms/app/domain/mcpapp"
	"github.com/jroedel/dropin-forms/app/domain/notifyapp"
	"github.com/jroedel/dropin-forms/app/domain/oauthapp"
	"github.com/jroedel/dropin-forms/app/domain/paymentapp"
	"github.com/jroedel/dropin-forms/app/domain/peopleapp"
	"github.com/jroedel/dropin-forms/app/domain/siteapp"
	"github.com/jroedel/dropin-forms/app/domain/submissionapp"
	"github.com/jroedel/dropin-forms/app/domain/willcallapp"
	"github.com/jroedel/dropin-forms/app/sdk/health"
	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/apikey/apikeybus"
	"github.com/jroedel/dropin-forms/business/domain/notify/notifybus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/foundation/mail"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// Config is everything the routes need, built once in main.
type Config struct {
	Log      *slog.Logger
	DB       *sql.DB
	Expected sqldb.Expected

	// FrameAncestors answers which origins may frame a given form's page. It
	// is a function because the answer is per form, and main builds one from
	// the form store with page.FormFrameAncestors.
	FrameAncestors page.FrameAncestorsFor

	// Embed is what the public form surface needs. Its own config struct
	// rather than more fields here, because the two surfaces share almost
	// nothing and a flat Config made it easy to hand the embed surface a
	// dependency only the admin surface should have.
	Embed embedapp.Config

	// Users, Access, Mail and Render are what the admin surface needs and the
	// embed surface must not have. Embed ignores them entirely, which is the
	// clearest statement available that the public surface has no session,
	// sends no mail and renders no admin chrome.
	Users  *userbus.Business
	Access *accessbus.Business
	Mail   mail.Sender
	Render *page.Renderer

	// Submissions is what the admin surface reads to show what was submitted.
	// Read-only from here: the app that lists them cannot change one.
	Submissions submissionapp.Submissions

	// Forms is the definition store, needed on both surfaces -- the embed
	// surface renders them and the admin surface names their fields as
	// columns.
	Forms submissionapp.Forms

	// Table is what the will-call page changes: the same submissions
	// submissionapp reads, plus the three operations of the door. A separate
	// field from Submissions, and deliberately the wider interface of the two,
	// so that the read-only surface cannot acquire a write by being handed the
	// wrong value.
	//
	// Optional: without it the will-call routes are not mounted, which is
	// right for an installation that has no table to work.
	Table willcallapp.Orders

	// Hiding is what takes a submission out of the lists and puts it back:
	// the same submissions again, with two writes. Its own field for the
	// reason Table is -- the read-only surface must not acquire a write by
	// being handed the wider value. Optional: without it the routes are not
	// mounted and the page for one submission draws no button to them.
	Hiding hideapp.Submissions

	// Feeds and FeedRows are the spreadsheet feed: the keys, and the
	// submissions it reads -- the latter its own field for the reason Hiding
	// is, a narrower value than the read surface's. Optional, both: without
	// them the routes are not mounted and no key can be made.
	Feeds    feedapp.Keys
	FeedRows feedapp.Submissions

	// Builder is the write half of the form domain, and the one dependency
	// here that only one app has. Optional: without it the service serves
	// every form it has and offers no way to author one, which is exactly
	// right for a test that is not about authoring and is a defensible way to
	// run an installation whose forms all ship in the binary.
	Builder formapp.Catalog

	// EmbedBaseURL is the public surface's own origin, and the builder is the
	// only thing that wants it -- it is what turns the builder's page into the
	// two lines somebody pastes into their website. Optional, for the reason
	// formapp.Config.EmbedBaseURL gives: an installation that predates the
	// setting should keep starting.
	EmbedBaseURL string

	// Payments is what the webhook route calls. Embed.Payments is a different
	// and much narrower slice of the same domain: one confirms a payment and
	// the other can only start one, and nothing should be able to do both by
	// accident. They share a listener and not an interface.
	Payments paymentapp.Payments

	// Notify tells people about a submission. One value reaching two surfaces,
	// and each sees only what it is allowed to say: the embed surface can
	// report a submission that needed no payment, and the webhook can report
	// one that Stripe has confirmed. Neither can do the other's.
	//
	// The concrete type rather than an interface, so that a missing
	// notifier is a nil pointer here and not an interface holding one --
	// which is the Go trap main already works around for the payment domain,
	// and which would turn "mail is not configured" into a panic on the first
	// submission.
	Notify *notifybus.Business

	// TrustProxy says whether X-Forwarded-For carries the visitor's address,
	// for both surfaces.
	//
	// One field rather than one per surface, because it is a fact about the
	// deployment -- Apache is in front of both listeners or it is in front of
	// neither -- and two copies of one fact is how the two come to disagree.
	// Embed builds it into the embed app's own config, which keeps that app
	// testable on its own.
	TrustProxy bool

	// AdminBaseURL is the admin surface's own origin, used to build the
	// addresses it hands out -- an invitation's link to the sign-in page, a
	// spreadsheet feed's URL.
	// Configured rather than taken from the request: a link built from a Host
	// header is one an attacker can aim at their own host by sending a single
	// request, and the person who receives the mail cannot tell.
	AdminBaseURL string

	// Bootstrap is the one-time sign-in secret. Empty leaves those routes
	// unmounted.
	Bootstrap string

	// APIKeys is the personal key domain: the account page that makes and
	// revokes keys, and the API that accepts them. Optional, and the API is
	// mounted only alongside the builder, since building forms is most of
	// what it does. A concrete pointer rather than an interface, for the
	// reason Notify gives.
	APIKeys *apikeybus.Business

	// OAuthClients reads the metadata document a program signing in through
	// OAuth names itself by: foundation/oauth.Fetcher in production, a
	// stand-in for the network in a test. Optional: without it there is no
	// OAuth, and the API and MCP endpoint take keys made on the key page
	// only -- which is everything but a claude.ai connector.
	OAuthClients oauthapp.Clients
}

// Embed builds the public, embeddable surface.
//
// Like Admin, it returns an error rather than a handler alone: this surface
// cannot be built without a form store, a place to put submissions, a renderer
// and a grant key, and a Config missing any of them used to be a nil
// dereference on the first request -- a segfault in place of a sentence. A
// surface that cannot be built should say so while the process is starting.
func Embed(cfg Config) (http.Handler, error) {
	switch {
	case cfg.Embed.Forms == nil:
		return nil, errors.New("the embed surface needs somewhere to read form definitions from")
	case cfg.Embed.Submissions == nil:
		return nil, errors.New("the embed surface needs somewhere to put submissions")
	case cfg.Embed.Render == nil:
		return nil, errors.New("the embed surface needs a renderer")
	case cfg.Embed.GrantKey.Zero():
		return nil, errors.New("the embed surface needs a grant signing key; every form page carries a grant")
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", health.Handler(cfg.Log, cfg.DB, cfg.Expected))

	// The form routes. The origin gate goes to embedapp as an argument rather
	// than onto the chain below, so that it covers the one route that accepts
	// a write and nothing else -- in particular not the webhook.
	//
	// No Authenticate and no Require, and that is the whole shape of this
	// surface rather than an omission. It is framed cross-site, so SameSite
	// withholds any cookie it might carry from every request the frame makes
	// -- there is no session to establish and nothing for a CSRF gate to
	// protect. What guards the POST is the submission grant, inside the
	// handler, and embedapp's package comment says so at length.
	//
	// In particular: do not mount the parent project's RequireFormToken here.
	// It passes through when there is no principal, which is correct there and
	// would admit every POST unconditionally here.
	// Writes only. The origin check refuses a cross-site POST from a browser
	// and by its own admission lets a header-less client through, which is why
	// it is one of several things in front of a submission rather than the
	// thing in front of it; the content-type check narrows what those bodies
	// may be to the one shape a form produces, so that nothing on this surface
	// is ever asked to parse a multipart upload.
	//
	// Both are handed to embedapp rather than put on the chain below, and the
	// reason is the same in both cases and is the whole of why this is written
	// here: the Stripe webhook is on this listener, it posts JSON, and it must
	// not be behind either of them.
	cfg.Embed.TrustProxy = cfg.TrustProxy

	if cfg.Notify != nil {
		cfg.Embed.Notify = cfg.Notify
	}

	embedapp.Routes(mux, cfg.Embed, func(next http.Handler) http.Handler {
		return web.Wrap(next, web.SameOriginOnly(), web.FormEncodedOnly())
	})

	// The webhook, on this listener and deliberately *outside* that gate.
	//
	// It shares the listener because it does not need one of its own: nothing
	// on this chain reads a request body, which is the only property the
	// webhook actually requires of what sits above it. An earlier version of
	// this comment claimed the origin gate would refuse Stripe's delivery and
	// that a separate hostname was therefore forced. That was wrong --
	// web.sameOrigin treats a request with neither Sec-Fetch-Site nor Origin
	// as a pass, which is the hole its own comment documents, so a
	// server-to-server POST would go straight through it.
	//
	// But passing a gate through a hole is not the same as not being behind
	// it. Somebody may one day decide that hole should be closed, which would
	// be a defensible change to make for the form POST -- and it would
	// silently stop every payment being confirmed. So the gate is handed to
	// embedapp for its own write route instead of being put on the chain, and
	// this route is genuinely not behind it. That survives somebody tightening
	// the gate without knowing this route exists.
	//
	// Mounted only when there is a payment domain. Without one this would be
	// a public URL that verifies nothing, which is worse than no endpoint.
	if cfg.Payments != nil {
		pc := paymentapp.Config{
			Log:      cfg.Log,
			Payments: cfg.Payments,
		}

		if cfg.Notify != nil {
			pc.Notify = cfg.Notify
		}

		paymentapp.Routes(mux, pc)
	}

	return web.Wrap(mux,
		web.RequestID(),
		web.Logging(cfg.Log),
		web.Panics(cfg.Log),
		web.SecureHeaders(page.EmbedPolicy(cfg.FrameAncestors)),

		// And nothing else. The four above refuse nothing: an id, a log line,
		// a recovered panic and a header set. None of them reads a body, which
		// is what makes it safe for the webhook to sit under them -- the
		// signature covers the exact bytes, so anything calling ParseForm here
		// would destroy the only credential that surface has.
	), nil
}

// Admin builds the management surface.
//
// It answers /healthz too, and that is on purpose: the deploy checks each
// listener separately, because a release where one of the two came up is the
// failure this service is most likely to have.
// Admin returns an error rather than a handler alone, unlike Embed.
//
// The asymmetry is the honest signature: the embed surface can be built from
// nothing but a logger and a database, and this one cannot. It needs a
// renderer and an account domain, and a Config missing either produced a nil
// dereference on the first request -- a segfault in place of a sentence. A
// surface that cannot be built should say so while the process is still
// starting.
func Admin(cfg Config) (http.Handler, error) {
	switch {
	case cfg.Render == nil:
		return nil, errors.New("the admin surface needs a renderer")
	case cfg.Users == nil:
		return nil, errors.New("the admin surface needs the account domain")
	case cfg.Access == nil:
		return nil, errors.New("the admin surface needs the access domain, which is what decides who may read a form's submissions")
	case cfg.Submissions == nil:
		return nil, errors.New("the admin surface needs the submission domain; reading submissions is the whole reason it exists")
	case cfg.Forms == nil:
		return nil, errors.New("the admin surface needs the form definitions, which is where a submission's columns come from")
	case cfg.Mail == nil:
		return nil, errors.New("the admin surface needs somewhere to send mail, even if that is a recorder")
	case cfg.AdminBaseURL == "":
		return nil, errors.New("the admin surface needs its own base URL, which is what the addresses it hands out are built from")
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", health.Handler(cfg.Log, cfg.DB, cfg.Expected))

	// The stylesheet, the brand faces and the logo are outside every gate,
	// and deliberately so: they are files compiled into the binary rather
	// than anybody's data, and the sign-in page needs them. Put them behind
	// the session and the login page renders unstyled. Their paths carry a
	// hash of their content, so they are also the only responses on this
	// surface that may be cached.
	cfg.Render.Mount(mux)

	// Where a browser arriving at the bare hostname goes. Answered here
	// rather than left as a 404, because this hostname is what somebody types
	// from memory. {$} rather than / so this matches the root exactly and
	// does not become a catch-all that swallows every typo as a redirect.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		to := signInPath

		// The forms list rather than the account page. Somebody typing this
		// hostname from memory came to look at submissions; the account page
		// is for backup codes, which is a thing you do once.
		if _, ok := mid.UserFrom(r.Context()); ok {
			to = "/forms"
		}

		http.Redirect(w, r, to, http.StatusSeeOther)
	})

	guard := mid.Require(signInPath)

	authapp.Routes(mux, authapp.Config{
		Log:       cfg.Log,
		Users:     cfg.Users,
		Access:    cfg.Access,
		Mail:      cfg.Mail,
		Render:    cfg.Render,
		Bootstrap: cfg.Bootstrap,

		// Which is what the sign-in throttle counts. The default rate applies
		// unless a test says otherwise; authapp.DefaultSignInRate says what it
		// is and why.
		TrustProxy: cfg.TrustProxy,

		// Whether the account page links to the page for API keys, which is
		// mounted below when there is somewhere to keep them.
		KeysPage: cfg.APIKeys != nil,
	}, guard)

	// results is the second gate, and it is mounted here rather than inside
	// the app for the same reason guard is: a route's position in the chain is
	// written down in one place. "Who is this" is settled by guard before
	// "what may they do" is asked, which is the order the web skill is
	// explicit about.
	//
	// The role is results and not admin. Reading the numbers is not editing
	// the price, and whoever is counting lunches should not have to be able to
	// change what a ticket costs.
	results := mid.RequireFormRole(cfg.Log, cfg.Access, accessbus.RoleResults)

	sc := submissionapp.Config{
		Log:         cfg.Log,
		Forms:       cfg.Forms,
		Submissions: cfg.Submissions,
		Grants:      cfg.Access,
		Accounts:    cfg.Users,
		Render:      cfg.Render,
		CanHide:     cfg.Hiding != nil,
		CanFeed:     cfg.Feeds != nil && cfg.FeedRows != nil,
		CanEdit:     cfg.Builder != nil,
		CanWillCall: cfg.Table != nil,
	}

	if cfg.Notify != nil {
		sc.Notifications = cfg.Notify
	}

	// The drafts an account administers, listed on the page it lands on after
	// signing in. Only where the builder is mounted, because that is the only
	// thing that makes a draft: an installation serving forms from TOML alone
	// has none.
	//
	// It is the landing page and not the builder's own listing that grows
	// this, because the builder's listing is the whole picture and stays
	// behind the site-wide gate. An account holding nothing but
	// accessbus.RoleCreator has to be able to find the form it started
	// yesterday, and this is the page it already lands on.
	if cfg.Builder != nil {
		sc.Drafts = cfg.Builder
	}

	submissionapp.Routes(mux, sc, guard, results)

	// Managing who can see a form is behind admin on that form rather than
	// results, and that is the one difference between these two mounts. The
	// person counting lunches holds results and never sees the people page;
	// deciding who else may read the names and addresses on a form is the
	// other role.
	//
	// A site-wide admin passes this gate for every form, because Allowed
	// checks the form's own grant and then the site-wide one -- which is how
	// the founding account, whose only grant is site-wide, can give anybody
	// else their first form.
	admins := mid.RequireFormRole(cfg.Log, cfg.Access, accessbus.RoleAdmin)

	pc := peopleapp.Config{
		Log:      cfg.Log,
		Forms:    cfg.Forms,
		Accounts: cfg.Users,
		Grants:   cfg.Access,
		Mail:     cfg.Mail,
		Render:   cfg.Render,
		BaseURL:  cfg.AdminBaseURL,
	}

	// Only when there is somewhere to record a preference, which is the same
	// condition the unsubscribe page is mounted under. Without it the page
	// says nothing about email, which is right: an installation that sends no
	// notifications should not have a column claiming somebody is emailed.
	if cfg.Notify != nil && cfg.Notify.CanUnsubscribe() {
		pc.Notifications = cfg.Notify
	}

	peopleapp.Routes(mux, pc, guard, admins)

	// Hiding a submission, behind admin on that form: it changes the totals
	// somebody orders food against. The buttons are on submissionapp's page,
	// which is read-only and behind results; hideapp says why these are not
	// its routes.
	if cfg.Hiding != nil {
		hideapp.Routes(mux, hideapp.Config{
			Log:         cfg.Log,
			Forms:       cfg.Forms,
			Submissions: cfg.Hiding,
		}, guard, admins)
	}

	// The spreadsheet feed. Making and revoking a key is behind admin on the
	// form; the JSON is behind the key alone, and feedapp says why the chain
	// in front of it refuses nothing a script sends.
	if cfg.Feeds != nil && cfg.FeedRows != nil {
		feedapp.Routes(mux, feedapp.Config{
			Log:         cfg.Log,
			Forms:       cfg.Forms,
			Submissions: cfg.FeedRows,
			Keys:        cfg.Feeds,
			Render:      cfg.Render,
			BaseURL:     cfg.AdminBaseURL,
			TrustProxy:  cfg.TrustProxy,
		}, guard, admins)
	}

	// The will-call table, behind the door role -- the middle of the three,
	// which reads this form's submissions and marks one collected and can do
	// nothing else. accessbus.RoleDoor says why neither neighbour would do:
	// results cannot write, and admin has meant setting the ticket price ever
	// since the builder shipped.
	//
	// Mounted only when there is somewhere to record a collection.
	if cfg.Table != nil {
		door := mid.RequireFormRole(cfg.Log, cfg.Access, accessbus.RoleDoor)

		willcallapp.Routes(mux, willcallapp.Config{
			Log:      cfg.Log,
			Forms:    cfg.Forms,
			Orders:   cfg.Table,
			Accounts: cfg.Users,
			Render:   cfg.Render,
		}, guard, door)
	}

	// The builder, behind the same per-form admin gate as the people page for
	// everything that names a form, and behind a site-wide one for the routes
	// that make a form -- which cannot be a per-form question, because the
	// form does not exist yet and no grant on it can. mid.RequireSiteAdmin is
	// emphatic about why that has to be a second gate rather than
	// RequireFormRole with an empty slug.
	//
	// Mounted only when there is somewhere to write a definition. Without one
	// these would be pages whose every button fails.
	// The narrower gate for the routes that only need "may make a form",
	// which accessbus.RoleCreator answers without also answering "does this
	// account administer the whole service". See RequireFormCreator and
	// formapp's package comment for why that is not the same gate as site
	// below. Built here rather than inside the builder's block because the
	// API puts its own form-making route behind it too.
	creator := mid.RequireFormCreator(cfg.Log, cfg.Access)

	if cfg.Builder != nil {
		site := mid.RequireSiteAdmin(cfg.Log, cfg.Access, accessbus.RoleAdmin)

		formapp.Routes(mux, formapp.Config{
			Log:          cfg.Log,
			Catalog:      cfg.Builder,
			Grants:       cfg.Access,
			Render:       cfg.Render,
			EmbedBaseURL: cfg.EmbedBaseURL,
		}, guard, admins, site, creator)

		// Who holds a site-wide role at all -- full administration or just the
		// ability to start a form -- is managed here, behind the same
		// full-administrator gate as /build's whole-picture listing. Mounted
		// alongside the builder rather than unconditionally, because a
		// site-wide grant that can never create a form and holds no other
		// per-form grant reaches nothing, and a page for granting one would be
		// offering a role with nothing behind it.
		siteapp.Routes(mux, siteapp.Config{
			Log:      cfg.Log,
			Accounts: cfg.Users,
			Grants:   cfg.Access,
			Mail:     cfg.Mail,
			Render:   cfg.Render,
			BaseURL:  cfg.AdminBaseURL,
		}, guard, site)
	}

	// The page that turns email about a form off and on, and the one route on
	// this surface that is deliberately *outside* guard.
	//
	// It has to be, because the link that leads to it is at the bottom of a
	// notification and is opened from a mailbox: behind the session gate it
	// would be a sign-in round trip to stop an email. What stands in for the
	// session is a signed token naming the account and the form, checked in
	// the handler -- see app/domain/notifyapp, which is emphatic about why
	// that check cannot be a middleware and about why its GET changes nothing.
	//
	// Mounted only when there is somewhere to record the preference. Without
	// one this would be a page whose button does nothing.
	if cfg.Notify != nil && cfg.Notify.CanUnsubscribe() {
		notifyapp.Routes(mux, notifyapp.Config{
			Log:         cfg.Log,
			Forms:       cfg.Forms,
			Preferences: cfg.Notify,
			Render:      cfg.Render,
			SignInPath:  signInPath,
		})
	}

	browser := web.Wrap(mux,
		web.SameOriginOnly(),

		// Every write from a browser on this surface is a form this service
		// rendered, and there is no upload anywhere in the management app --
		// so the set of content types it will parse is exactly one. On the
		// browser's chain rather than per route, unlike the embed surface,
		// because there is no webhook on this listener; the API, which posts
		// JSON, has a chain of its own below rather than a hole in this one.
		web.FormEncodedOnly(),

		// Below the origin gates, above every handler. It refuses nobody,
		// which is what lets the sign-in pages live in the same chain as the
		// account page they lead to.
		mid.Authenticate(cfg.Log, cfg.Users),
	)

	root := browser

	if cfg.APIKeys != nil {
		// Making and revoking your own keys is a page, on the browser's
		// chain, behind a session and nothing more: anybody with an account
		// may make a key, and it reaches what they do.
		apiapp.KeyRoutes(mux, apiapp.KeysConfig{
			Log:     cfg.Log,
			Keys:    cfg.APIKeys,
			Render:  cfg.Render,
			BaseURL: cfg.AdminBaseURL,
		}, guard)
	}

	// The API, on a chain of its own, and the one place on this listener
	// that is not behind the browser's.
	//
	// It is not behind SameOriginOnly or FormEncodedOnly because it posts
	// JSON from programs, and it does not need them: both exist because a
	// browser attaches the session cookie to a request somebody else's page
	// started, and this chain never reads the cookie. mid.Bearer says why
	// that is the property everything rests on.
	//
	// The permission gates are the same values the pages sit behind. A key
	// is its account, and a route here is reachable by exactly the accounts
	// that reach the matching page.
	//
	// Mounted only alongside the builder, because without somewhere to write
	// a definition most of it would be routes whose every call fails.
	if cfg.APIKeys != nil && cfg.Builder != nil {
		api := http.NewServeMux()

		apiapp.Routes(api, apiapp.Config{
			Log:          cfg.Log,
			Catalog:      cfg.Builder,
			Grants:       cfg.Access,
			Submissions:  cfg.Submissions,
			EmbedBaseURL: cfg.EmbedBaseURL,
		}, admins, results, creator)

		// The same API as MCP tools, for a client like Claude that would
		// rather call a tool than write a request. On this mux, so it is
		// behind the same throttle and key; and handed this mux, so that
		// each tool is a request through the same gates as the route it
		// names. mcpapp says why it decides nothing of its own.
		mcpapp.Routes(api, mcpapp.Config{Log: cfg.Log, API: api})

		top := http.NewServeMux()

		// Signing in through OAuth, which is how a claude.ai connector gets a
		// key: it has nowhere to paste one. oauthapp says what each route sits
		// behind and why. The page is on the browser's chain, behind a
		// session; the token endpoint is called by Claude's servers, carries
		// no cookie, and is beside the API instead, with a throttle of its
		// own -- a handful of connections a day is the real rate.
		//
		// And the MCP endpoint's refusal says where to sign in, which is what
		// makes a connector start. The plain API's does not: a script holding
		// no key is not going to open a browser.
		var challenge func(*http.Request) string

		if cfg.OAuthClients != nil {
			oc := oauthapp.Config{
				Log:     cfg.Log,
				Render:  cfg.Render,
				Grants:  cfg.APIKeys,
				Clients: cfg.OAuthClients,
				BaseURL: cfg.AdminBaseURL,
			}

			oauthapp.Routes(mux, oc, guard)
			mcpapp.ResourceRoutes(mux, cfg.AdminBaseURL)

			top.Handle("POST "+oauthapp.TokenPath, web.Wrap(oauthapp.TokenHandler(oc),
				web.Throttle(web.Throttling{
					Rate: web.Rate{Burst: 10, Every: 6 * time.Second},
					Key: func(r *http.Request) string {
						return "oauth|" + web.IPBucket(web.ClientIP(r, cfg.TrustProxy))
					},
					Log: cfg.Log,
				}),
			))

			signIn := mcpapp.Challenge(cfg.AdminBaseURL)

			challenge = func(r *http.Request) string {
				if r.URL.Path == mcpapp.Path {
					return signIn
				}

				return `Bearer realm="api"`
			}
		}

		top.Handle(apiapp.Prefix, web.Wrap(api,
			// Ahead of the key, so that a flood is turned away before it
			// costs a database read each. Two a second, after a burst of
			// sixty, is more than a program building a form needs and far
			// less than would trouble a single-writer database.
			web.Throttle(web.Throttling{
				Rate: web.Rate{Burst: 60, Every: 500 * time.Millisecond},
				Key: func(r *http.Request) string {
					return "api|" + web.IPBucket(web.ClientIP(r, cfg.TrustProxy))
				},
				Log: cfg.Log,
				Refuse: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					mid.WriteJSONError(w, http.StatusTooManyRequests,
						"That is faster than this service takes requests. Wait for the number of seconds in Retry-After and try again.")
				}),
			}),
			mid.Bearer(cfg.Log, cfg.APIKeys, challenge),
		))
		top.Handle("/", browser)

		root = top
	}

	return web.Wrap(root,
		web.RequestID(),
		web.Logging(cfg.Log),
		web.Panics(cfg.Log),
		web.SecureHeaders(page.AdminPolicy()),
	), nil
}

// signInPath is where Require sends a signed-out reader, and it is the one
// route authapp and the muxer both have to agree on.
const signInPath = "/signin"

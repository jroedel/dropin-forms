// Package submissionapp is how somebody reads what was submitted.
//
// It exists because there is no CLI. Every operational thing about this
// service happens in a browser, so the list, the detail view and the CSV
// export are not a convenience on top of a command -- they are the only way to
// find out how many lunches were sold. That is why the design document puts
// this before the payment work rather than after it: money arriving with no
// way to read the orders is worse than orders with no money.
//
// # What guards each route
//
// Everything here is behind a session, and every per-form route is behind a
// grant on that form:
//
//	GET /forms                          Require. The list is filtered to the
//	                                    forms this account holds a grant on,
//	                                    so it leaks no form it may not read.
//	GET /forms/{slug}/submissions       Require, then RequireFormRole(results)
//	GET /forms/{slug}/submissions.csv   the same
//	GET /forms/{slug}/submissions/{id}  the same
//
// Nothing here changes a submission. The Hide and Unhide buttons on a
// submission's page post to hideapp, behind admin, and are shown only to an
// account that holds it -- a results reader sees that a row is hidden and
// cannot change it.
//
// The role is results rather than admin throughout. Reading the numbers is not
// editing the price, and the person counting lunches should not have to be
// able to change what a ticket costs.
//
// # Every page here is somebody else's personal data
//
// A submission holds a name, an email address, free text somebody typed, and
// what they paid. So the admin policy's Cache-Control: no-store matters here
// more than anywhere else in the service, and the CSV is served as an
// attachment with the same header rather than something a browser keeps in a
// temporary directory.
package submissionapp

import (
	"context"
	"embed"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formtoml"
	"github.com/jroedel/dropin-forms/business/domain/payment/paybus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

//go:embed templates
var templates embed.FS

// Templates is this app's own template directory, handed to page.NewRenderer
// by whatever builds the renderer.
var Templates = templates

// Forms is where definitions come from. An interface because this app reads
// them and never writes one.
type Forms interface {
	ByID(slug types.Slug) (formbus.Form, error)
	All() []formbus.Form
}

// Submissions is the slice of the submission domain this app needs. Read-only:
// nothing here changes a submission, which is why Settle is absent.
//
// ByForm leaves out hidden submissions and unpaid ones, so the list, its
// totals, the CSV and the index counts do too without a line here saying so.
// Hidden and HidingOf are the two ways back to the first; Unsettled is the way
// back to the second, and only an administrator's page asks it.
type Submissions interface {
	ByForm(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error)
	Unsettled(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error)
	ByID(ctx context.Context, id types.ID) (submissionbus.Submission, error)
	Hidden(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error)
	HidingOf(ctx context.Context, id types.ID) (submissionbus.Hiding, bool, error)
	Revisions(ctx context.Context, s submissionbus.Submission) ([]submissionbus.Revision, error)
	Taken(ctx context.Context, form types.Slug) (formbus.Taken, error)
	Declined(ctx context.Context, form types.Slug) ([]submissionbus.Submission, error)
}

// Grants answers which forms an account may reach, for the index page. The
// per-route check is the middleware's job; this is only for deciding what to
// list.
//
// Allowed is for one decision on one page: whether to show the Hide button.
// The route it posts to has its own gate, so a wrong answer here is a button
// that is refused, never a write that is allowed.
type Grants interface {
	ForUser(ctx context.Context, userID types.ID) ([]accessbus.Grant, error)
	Allowed(ctx context.Context, userID types.ID, form types.Slug, want accessbus.Role) (bool, error)
}

// Accounts names whoever hid a submission, so the page says a person rather
// than an identifier.
type Accounts interface {
	ByID(ctx context.Context, id types.ID) (userbus.User, error)
}

// Drafts is every authored definition, published or not, which is a different
// question from [Forms] and the reason both are here. Forms is the catalogue
// being served, and a draft is by definition not in it.
//
// This app has no business editing one and does not: it lists the drafts the
// reader administers so that somebody who started a form yesterday can find
// it again. Before this, the only page that listed a draft was the builder's
// own, behind the site-wide gate -- so an account holding nothing but
// accessbus.RoleCreator could make a form, leave the page, and have no link
// back to it anywhere on the service.
type Drafts interface {
	Drafts(ctx context.Context) ([]formbus.Stored, error)
}

// Notifications answers which forms an account has turned email off for, so
// that the list can say which is which.
//
// One call for the whole page rather than one per row: this is a listing, and
// a query per form is how a page that is fast with two forms is slow with
// forty.
type Notifications interface {
	MutedForms(ctx context.Context, userID types.ID) ([]types.Slug, error)
}

// Config is what this app needs.
type Config struct {
	Log         *slog.Logger
	Forms       Forms
	Submissions Submissions
	Grants      Grants
	Accounts    Accounts
	Render      *page.Renderer

	// CanHide says the routes that hide a submission are mounted. Without
	// them the page draws no button, rather than one that answers 404.
	CanHide bool

	// CanReview says the routes that approve and decline are mounted, for
	// the same reason. Anybody who can read this page may use them, so it
	// needs no grant of its own -- reviewapp says why.
	CanReview bool

	// CanFeed says the spreadsheet feed is mounted, for the same reason: the
	// list links to it only for a reader who may use it and only when it is
	// there.
	CanFeed bool

	// CanEdit and CanWillCall say the builder and the will-call table are
	// mounted, which decides whether a form's page links to them. The people
	// page is always mounted and needs no flag.
	CanEdit     bool
	CanWillCall bool

	// CanDeposits says the deposits page is mounted. It is behind results,
	// the same gate as this page, so the only questions are whether it is
	// there and whether the form sells anything.
	CanDeposits bool

	// Notifications is optional. Without it the list simply does not mention
	// email, which is right for an installation that cannot record the
	// preference: a column offering a choice that will not stick is worse than
	// no column.
	Notifications Notifications

	// Drafts is optional, and mounted by the muxer only where the builder is.
	// An installation serving forms from TOML alone has no drafts to list, and
	// asking a store that is not there for them would be a page that fails for
	// want of something nobody wanted.
	Drafts Drafts
}

type app struct {
	cfg Config
}

// Routes mounts this app.
//
// guard is Require and results is RequireFormRole(results), both handed in by
// the muxer rather than built here -- the chain is written down in one place
// and a route's position in it is not a decision an app package gets to make.
func Routes(mux *http.ServeMux, cfg Config, guard, results func(http.Handler) http.Handler) {
	a := app{cfg: cfg}

	behind := func(role func(http.Handler) http.Handler, h http.HandlerFunc) http.Handler {
		return guard(role(h))
	}

	mux.Handle("GET /forms", guard(http.HandlerFunc(a.index)))

	// The CSV route is registered before the list route only for readability;
	// net/http matches the most specific pattern regardless of order, and
	// "submissions.csv" is a literal segment rather than a wildcard, so the
	// two cannot shadow each other.
	mux.Handle("GET /forms/{"+mid.FormSlugParam+"}/submissions.csv", behind(results, a.export))
	mux.Handle("GET /forms/{"+mid.FormSlugParam+"}/submissions", behind(results, a.list))
	mux.Handle("GET /forms/{"+mid.FormSlugParam+"}/submissions/{id}", behind(results, a.one))
}

// indexView is the landing page: which forms this account may read.
type indexView struct {
	Forms []indexRow

	// Email says whether this installation can record a notification
	// preference at all, and therefore whether the column means anything.
	Email bool

	// SiteAdmin marks a reader who administers the whole service, and
	// therefore the builder's whole-picture listing at /build. The link to it
	// is shown to them alone -- offering it to everybody would be offering a
	// page that answers 403, and a link that refuses you is worse than no
	// link.
	//
	// It is not this app's business to know what the builder is, and it does
	// not: this is a fact about the reader's grants, which this handler
	// already has in its hand, and the template spends it on one anchor.
	SiteAdmin bool

	// CanCreate marks a reader who may start a form, which is a wider set than
	// SiteAdmin: an account holding nothing but accessbus.RoleCreator may do
	// that and nothing else.
	//
	// It is separate from SiteAdmin because the two lead to different pages --
	// /build is the whole picture and refuses a creator, /build/new is the one
	// thing a creator came for -- and because without it this landing page is
	// the entire reason a RoleCreator grant is unusable: the account is told it
	// has access to nothing at all, next to no way to reach the page its one
	// grant exists for.
	CanCreate bool
}

type indexRow struct {
	ID    string
	Title string
	Role  string

	// Count is how many submissions there are, and Open says whether the form
	// is still taking them. Both are what somebody wants at a glance before
	// clicking anything.
	Count int
	Open  bool

	// Door says whether this account may work this form's will-call table, so
	// that the row can offer it. Computed here rather than compared in the
	// template, because "door or anything above it" is accessbus.Includes's
	// question and a string comparison in markup is the second place it would
	// be answered.
	Door bool

	// Emailed says whether this account hears about submissions to this form.
	// Shown because "am I getting these" is a question somebody asks while
	// looking at the list, and because the answer is the one thing on the page
	// that is about them rather than about the form.
	Emailed bool

	// Draft marks a definition that is not being served, and it changes what
	// the row may offer rather than only what it says. Every other page this
	// row links to -- the submissions, the will-call table, the people --
	// resolves the form through the served catalogue and answers 404 for a
	// draft. The editor is the one link that works, so it is the only one a
	// draft row carries.
	Draft bool
}

func (v indexView) Any() bool { return len(v.Forms) > 0 }

// index lists the forms this account holds a grant on.
//
// Filtered by the grants rather than showing every form with some locked, so
// the page cannot tell somebody that a form exists which they may not read.
// A parish's list of what it is collecting money for is not sensitive in
// itself, but it is not this page's business to publish either.
func (a app) index(w http.ResponseWriter, r *http.Request) {
	u, ok := mid.UserFrom(r.Context())
	if !ok {
		// Unreachable behind Require, and handled rather than assumed so a
		// route mounted wrongly fails loudly instead of showing an empty list
		// that looks like "you have no access".
		a.oops(w, r, "a submission page was reached with no account", nil)

		return
	}

	grants, err := a.cfg.Grants.ForUser(r.Context(), u.ID)
	if err != nil {
		a.oops(w, r, "the grants could not be listed", err)

		return
	}

	// A site-wide grant means every form, so the listing is built from the
	// form store in that case rather than from the grant rows.
	//
	// Assigned rather than compared against what is already there. A grant is
	// keyed by (account, form) and site-wide is the zero form, so there is at
	// most one of these rows per account -- and comparing with Includes here
	// was a bug rather than caution: Includes refuses an unrecognised role on
	// either side, deliberately, so against the zero value it answered false
	// and a site-wide admin saw an empty list.
	var site accessbus.Role

	named := map[types.Slug]accessbus.Role{}

	for _, g := range grants {
		if g.SiteWide() {
			site = g.Role

			continue
		}

		if held, seen := named[g.Form]; !seen || g.Role.Includes(held) {
			named[g.Form] = g.Role
		}
	}

	now := time.Now()

	var view indexView

	view.SiteAdmin = site.Includes(accessbus.RoleAdmin)

	// The same rule the gate in front of /build/new applies, asked of the
	// site-wide grant this handler has already read rather than by a second
	// lookup. accessbus.Role.CreatesForms is where that rule lives.
	view.CanCreate = site.CreatesForms()

	// Which forms this account has turned email off for, in one call before
	// the loop. A failure is not fatal to the page: the list's job is the
	// submissions, and losing the email column is a worse page rather than a
	// broken one.
	quiet := map[types.Slug]bool{}

	if a.cfg.Notifications != nil {
		view.Email = true

		muted, err := a.cfg.Notifications.MutedForms(r.Context(), u.ID)
		if err != nil {
			a.cfg.Log.Error("the notification preferences could not be read",
				"request_id", web.RequestIDFrom(r.Context()), "user_id", u.ID.String(), "error", err)
		}

		for _, slug := range muted {
			quiet[slug] = true
		}
	}

	for _, f := range a.cfg.Forms.All() {
		role := named[f.ID]
		if site.Includes(accessbus.RoleResults) && !role.Includes(site) {
			role = site
		}

		if !role.Includes(accessbus.RoleResults) {
			continue
		}

		subs, err := a.cfg.Submissions.ByForm(r.Context(), f.ID)
		if err != nil {
			a.oops(w, r, "the submissions could not be counted", err)

			return
		}

		view.Forms = append(view.Forms, indexRow{
			ID:      f.ID.String(),
			Title:   f.Title,
			Role:    role.String(),
			Door:    role.Includes(accessbus.RoleDoor),
			Emailed: !quiet[f.ID],
			Count:   len(subs),
			Open:    f.Open(now),
		})
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "forms", a.withDrafts(r, view, named, site))
}

// withDrafts appends the unpublished definitions this account administers.
//
// Filtered by the grant and by admin specifically, which is narrower than the
// rest of this page: a draft is not being served, so the only thing anybody
// can usefully do with it is finish it, and that is the editor's gate. Listing
// one to somebody holding results would be offering a row whose every link
// refuses them.
//
// A failure is logged and the page is rendered without them, the same bargain
// the notification column makes. This listing's job is the forms that are
// live, and losing the drafts off the bottom of it is a worse page rather than
// a broken one.
func (a app) withDrafts(r *http.Request, view indexView, named map[types.Slug]accessbus.Role, site accessbus.Role) indexView {
	if a.cfg.Drafts == nil {
		return view
	}

	drafts, err := a.cfg.Drafts.Drafts(r.Context())
	if err != nil {
		a.cfg.Log.Error("the drafts could not be listed",
			"request_id", web.RequestIDFrom(r.Context()), "error", err)

		return view
	}

	for _, d := range drafts {
		if d.Live {
			// Already in the loop above, through the catalogue that serves it.
			continue
		}

		role := named[d.Form.ID]
		if site.Includes(accessbus.RoleAdmin) {
			role = accessbus.RoleAdmin
		}

		if !role.Includes(accessbus.RoleAdmin) {
			continue
		}

		view.Forms = append(view.Forms, indexRow{
			ID:    d.Form.ID.String(),
			Title: d.Form.Title,
			Role:  role.String(),
			Draft: true,
		})
	}

	return view
}

// listView is one form's submissions.
type listView struct {
	Form    formbus.Form
	FormID  string
	Columns []string
	Rows    []rowView

	// Summary is the answer to the question somebody actually came with: how
	// many, how much, and how much of it is still owed.
	Summary summaryView

	// Places is how full each time slot, or other limited option, is.
	Places []placeTally

	// Awaiting is how many of the rows are waiting for somebody to approve
	// them, on a form that asks. Declined is how many were turned down, which
	// leave the list the way hidden ones do, and ShowingDeclined says this
	// page is the list of them instead.
	Awaiting        int
	Declined        int
	ShowingDeclined bool

	// Hidden is how many submissions are out of the list, and ShowingHidden
	// says this page is the list of them instead. A count rather than nothing,
	// so somebody who cannot find an order is told where it might be.
	Hidden        int
	ShowingHidden bool

	// Unpaid is how many of the rows are not paid -- still in checkout, or
	// abandoned -- which the list leaves out until somebody ticks the box that
	// says this number. Zero draws no box, and it is zero for everybody but an
	// administrator of the form, because nobody else is given these rows.
	//
	// Only an administrator, because for anybody else they were worse than
	// nothing. The list is read for who is coming, and on a form that sells, a
	// third of the rows can be somebody who reached Stripe and stopped: the
	// same name twice, once unpaid and once paid, reads as two people, and a
	// row marked pending read as something still in progress. For an
	// administrator they are a question about the form -- how many reach the
	// payment page and stop -- and a tick is closer than a link. Never in the
	// counts, the totals or the CSV, which are about what was sold.
	Unpaid int

	// Window is how many hours a payment page stays open, for the sentence
	// beside that box which says what "not paid" means. From
	// paybus.CheckoutWindow, so the page cannot say two hours while the
	// deadline says something else.
	Window int

	// Feed is whether to link to the spreadsheet page: mounted, and this
	// reader administers the form.
	Feed bool

	// The other pages of this form this reader may open, linked under its
	// title: the same three the forms list offers beneath it, by the same
	// rules -- will-call from door upwards, the builder and the people page
	// for an administrator. Without them, somebody who opened a form to read
	// it had to go back to the list to do anything else with it.
	Edit     bool
	People   bool
	WillCall bool

	// Deposits is whether to link to the deposits page, which is beside the
	// collected total rather than in the line above: it is the next question
	// about that number -- how much of it has reached the bank.
	Deposits bool
}

type rowView struct {
	ID     string
	When   string
	Status string
	Total  string

	// Cells line up with Columns, one per field of the definition, so the
	// table has the same shape as the form somebody filled in.
	Cells []string

	// Unpaid marks a row the list hides until asked; see listView.Unpaid.
	Unpaid bool
}

// summaryView is the headline: how many, and how much was collected. Only
// what was paid for or received -- an order nobody paid for is not in any of
// these numbers, because they are what somebody caters from, and a figure for
// what was started and never paid was read as food to order.
type summaryView struct {
	Count    int
	Paid     int
	Received int

	Items     []itemTally
	PaidTotal string
}

type itemTally struct {
	Label     string
	Confirmed int
}

// placeTally is one option that only so many may take, and how many hold it.
//
// Held, not confirmed: an order waiting for its payment holds its place, so
// this can be more than the rows listed beneath it. Over is a slot holding more
// than its limit, which a payment arriving after its checkout was abandoned,
// or a submission put back from hidden, can cause -- shown rather than
// clipped, because it is a person somebody has to ring.
type placeTally struct {
	Question string
	Label    string
	Taken    int
	Limit    int
	Over     bool
}

// places is the tally of a form's limited options, in the definition's order.
// A count that fails costs the table and not the page, for the reason the
// grant check above costs a link.
func (a app) places(r *http.Request, f formbus.Form) []placeTally {
	if !f.Limited() {
		return nil
	}

	taken, err := a.cfg.Submissions.Taken(r.Context(), f.ID)
	if err != nil {
		a.cfg.Log.Error("the places taken could not be counted",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)

		return nil
	}

	var out []placeTally

	for _, fld := range f.Questions() {
		for _, o := range fld.Options {
			if o.Limit == 0 {
				continue
			}

			n := taken.Count(fld.Name, o.Value)
			out = append(out, placeTally{
				Question: fld.Label, Label: o.Label, Taken: n, Limit: o.Limit, Over: n > o.Limit,
			})
		}
	}

	return out
}

func (a app) list(w http.ResponseWriter, r *http.Request) {
	f, subs, ok := a.formAndSubmissions(w, r)
	if !ok {
		return
	}

	// A failure to read a grant costs a link and not the page: the
	// submissions are what somebody came for, and the gate in front of every
	// linked page asks the same question again.
	may := func(accessbus.Role) bool { return false }

	if me, ok := mid.UserFrom(r.Context()); ok {
		may = func(role accessbus.Role) bool {
			allowed, err := a.cfg.Grants.Allowed(r.Context(), me.ID, f.ID, role)
			if err != nil {
				a.cfg.Log.Error("whether the reader may open this form's other pages could not be read",
					"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "role", role.String(), "error", err)
			}

			return allowed
		}
	}

	admin := may(accessbus.RoleAdmin)

	hidden, err := a.cfg.Submissions.Hidden(r.Context(), f.ID)
	if err != nil {
		a.oops(w, r, "the hidden submissions could not be listed", err)

		return
	}

	// Hidden is every status, because hiding an order says nothing about
	// whether it was paid. What an unpaid one is to anybody but an
	// administrator is the same whether it is hidden or not.
	if !admin {
		hidden = slices.DeleteFunc(hidden, func(s submissionbus.Submission) bool { return !s.Status.Settled() })
	}

	view := listView{
		Form:    f,
		FormID:  f.ID.String(),
		Columns: columnsOf(f),
		Summary: summarise(f, subs),
		Places:  a.places(r, f),
		Hidden:  len(hidden),
		Window:  int(paybus.CheckoutWindow / time.Hour),
	}

	// Read on every form rather than only one that asks for approval now:
	// a form that stopped asking still has the ones it declined, and this is
	// the only way back to them.
	declined, err := a.cfg.Submissions.Declined(r.Context(), f.ID)
	if err != nil {
		a.oops(w, r, "the declined submissions could not be listed", err)

		return
	}

	view.Declined = len(declined)

	// The hidden ones, instead of the rest, with the totals still those of
	// the rest: the summary is what to cater for, and a hidden row is by
	// definition not in it. The declined ones the same way, for the same
	// reason.
	if r.URL.Query().Get("hidden") != "" {
		view.ShowingHidden = true
		subs = hidden
	} else if r.URL.Query().Get("declined") != "" {
		view.ShowingDeclined = true
		subs = declined
	} else if admin && f.Sells() {
		unpaid, err := a.cfg.Submissions.Unsettled(r.Context(), f.ID)
		if err != nil {
			a.oops(w, r, "the unpaid submissions could not be listed", err)

			return
		}

		// Into the one list, in time order, so that somebody who stopped at
		// the payment page and came back to pay reads as two rows next to
		// each other rather than as a row at the bottom.
		subs = append(subs, unpaid...)
		slices.SortStableFunc(subs, func(x, y submissionbus.Submission) int { return y.CreatedAt.Compare(x.CreatedAt) })
	}

	// Anybody who can read this page can read that one, so it needs no grant
	// of its own: only the form has to sell something.
	view.Deposits = a.cfg.CanDeposits && f.Sells()

	view.Feed = admin && a.cfg.CanFeed
	view.Edit = admin && a.cfg.CanEdit
	view.People = admin
	view.WillCall = a.cfg.CanWillCall && (admin || may(accessbus.RoleDoor))

	for _, s := range subs {
		unpaid := !s.Status.Settled()

		// The hidden list shows what it was asked for, all of it: a row put
		// out of the way once is not put out of the way again.
		if unpaid && !view.ShowingHidden {
			view.Unpaid++
		}

		if s.Review == submissionbus.ReviewAwaiting && !view.ShowingHidden {
			view.Awaiting++
		}

		view.Rows = append(view.Rows, rowView{
			Unpaid: unpaid && !view.ShowingHidden,
			ID:     s.ID.String(),
			When:   s.CreatedAt.Local().Format("2 Jan 2006, 15:04"),
			Status: wordsFor(s),
			Total:  formbus.Show(s.Answers.Total, f.Currency),
			Cells:  cellsOf(f, s),
		})
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "submissions", view)
}

// statusWords is how a status reads on these pages.
//
// Not the stored word, for two of the three that are not settled. "pending"
// is what the database calls an order whose payment page is open, and people
// read it as something under way long after it had stopped being anything;
// "failed" is a declined card, which is not a failure of anything here and is
// still payable with another card until the page closes. Only these pages ever
// show either: the CSV and the API carry settled submissions alone, in the
// stored words a formula or a program matches on.
func statusWords(s submissionbus.Status) string {
	switch s {
	case submissionbus.StatusPending:
		return "in checkout"
	case submissionbus.StatusFailed:
		return "card declined"
	}

	return s.String()
}

// wordsFor is how a submission's standing reads on these pages: where it is
// with the office, on a form that asks for approval, and otherwise where it is
// with its payment. The two never both have something to say, because a form
// that asks for approval sells nothing.
func wordsFor(s submissionbus.Submission) string {
	switch s.Review {
	case submissionbus.ReviewAwaiting:
		return "awaiting approval"
	case submissionbus.ReviewApproved:
		return "approved"
	case submissionbus.ReviewDeclined:
		return "declined"
	}

	return statusWords(s.Status)
}

// oneView is a single submission in full.
type oneView struct {
	Form   formbus.Form
	FormID string

	ID     string
	When   string
	Status string
	Email  string
	Total  string
	Ref    string

	Answers []answerView
	Lines   []lineView

	// Hidden says the submission is out of every list, and who took it out
	// and when. CanHide is whether this reader may change that, which decides
	// only whether the button is drawn.
	Hidden   bool
	HiddenBy string
	HiddenAt string
	CanHide  bool

	// Review is where it stands with the office, on a form that asks for
	// approval, and empty on any other. ReviewedBy and ReviewedAt say who last
	// decided it and when, ReviewNote what a decline told the person.
	// CanReview is whether the buttons are drawn, and Taken says a re-approval
	// was just refused because its place has gone.
	Review     string
	ReviewedBy string
	ReviewedAt string
	ReviewNote string
	CanReview  bool
	Taken      bool

	// Changed is when the person last changed these answers through the link
	// we sent them, or empty if they never have. Earlier is what the answers
	// said before each change, newest first: what the office reads when it
	// asks "what did he tell us in October".
	Changed string
	Earlier []revisionView
}

// revisionView is one set of answers a submission held and no longer does.
type revisionView struct {
	Number  int
	From    string
	Until   string
	Answers []answerView
}

type answerView struct {
	Label string
	Value string

	// Answered is false for a field the person was not asked -- one hidden by
	// its condition -- which reads differently from a field they left blank
	// and is worth showing as such rather than as an empty cell.
	Answered bool
}

type lineView struct {
	Label  string
	Qty    int
	Price  string
	Amount string
}

func (a app) one(w http.ResponseWriter, r *http.Request) {
	f, ok := a.form(w, r)
	if !ok {
		return
	}

	id, err := types.ParseID(r.PathValue("id"))
	if err != nil {
		a.notFound(w, r)

		return
	}

	s, err := a.cfg.Submissions.ByID(r.Context(), id)

	switch {
	case errors.Is(err, submissionbus.ErrNotFound):
		a.notFound(w, r)

		return
	case err != nil:
		a.oops(w, r, "the submission could not be read", err)

		return
	}

	// The submission has to belong to the form in the path. Without this, a
	// results grant on any one form would read every submission in the
	// service by guessing identifiers -- the role gate checked the form in the
	// URL, and this is what ties the record to it.
	if s.Form != f.ID {
		a.notFound(w, r)

		return
	}

	// An order nobody paid for is an administrator's alone, here as in the
	// list -- and this is reachable without the list, from a link in an
	// email sent before these were taken out of everybody else's view.
	if !s.Status.Settled() && !a.administers(r, f.ID) {
		a.notFound(w, r)

		return
	}

	view := oneView{
		Form:   f,
		FormID: f.ID.String(),
		ID:     s.ID.String(),
		When:   s.CreatedAt.Local().Format(longWhen),
		Status: wordsFor(s),
		Email:  s.Email.String(),
		Total:  formbus.Show(s.Answers.Total, f.Currency),
		Ref:    s.PaymentRef,
	}

	view.Answers = answersOf(f, s.Answers)

	for _, l := range s.Answers.Lines {
		view.Lines = append(view.Lines, lineView{
			Label:  l.Label,
			Qty:    l.Qty,
			Price:  formbus.Show(l.Price, f.Currency),
			Amount: formbus.Show(l.Amount, f.Currency),
		})
	}

	a.hiding(r, s, &view)
	a.review(r, s, &view)
	a.history(r, f, s, &view)

	a.cfg.Render.Render(w, r, http.StatusOK, "submission", view)
}

// longWhen is how this page writes an instant.
const longWhen = "Monday 2 January 2006, 15:04"

// answersOf lays answers out against a definition.
//
// Walked in the definition's order rather than the record's, so two
// submissions to the same form read the same way down the page even when one
// of them skipped a conditional field -- and so an earlier version of one
// submission's answers reads row for row against its current one, with a
// question added since showing as not asked.
func answersOf(f formbus.Form, a formbus.Answers) []answerView {
	out := make([]answerView, 0, len(f.Fields))

	for _, fld := range f.Questions() {
		ans, answered := a.Field(fld.Name)

		out = append(out, answerView{
			Label:    fld.Label,
			Value:    ans.Readable(),
			Answered: answered,
		})
	}

	return out
}

// history fills in what the answers said before each change.
//
// A failure is logged and the page is rendered without it, as hiding's is:
// somebody who came to read the current answers should get them.
func (a app) history(r *http.Request, f formbus.Form, s submissionbus.Submission, view *oneView) {
	revs, err := a.cfg.Submissions.Revisions(r.Context(), s)
	if err != nil {
		a.cfg.Log.Error("the earlier answers could not be read",
			"request_id", web.RequestIDFrom(r.Context()), "submission_id", s.ID.String(), "error", err)

		return
	}

	if len(revs) == 0 {
		return
	}

	view.Changed = s.UpdatedAt.Local().Format(longWhen)

	for _, rev := range slices.Backward(revs) {
		view.Earlier = append(view.Earlier, revisionView{
			Number:  rev.Number,
			From:    rev.From.Local().Format(longWhen),
			Until:   rev.Until.Local().Format(longWhen),
			Answers: answersOf(f, rev.Answers),
		})
	}
}

// hiding fills in whether a submission is hidden and whether this reader may
// change it.
//
// Failures are logged and the page is rendered without the answer rather than
// refused: a reader who came to look up an order should see it, and not being
// told it is hidden costs them a sentence, not the order.
func (a app) hiding(r *http.Request, s submissionbus.Submission, view *oneView) {
	log := func(what string, err error) {
		a.cfg.Log.Error(what,
			"request_id", web.RequestIDFrom(r.Context()), "submission_id", s.ID.String(), "error", err)
	}

	h, hidden, err := a.cfg.Submissions.HidingOf(r.Context(), s.ID)
	if err != nil {
		log("whether the submission is hidden could not be read", err)
	}

	if hidden {
		view.Hidden = true
		view.HiddenAt = h.HiddenAt.Local().Format("Monday 2 January 2006, 15:04")
		view.HiddenBy = "somebody"

		if a.cfg.Accounts != nil && !h.HiddenBy.Zero() {
			if u, err := a.cfg.Accounts.ByID(r.Context(), h.HiddenBy); err == nil {
				view.HiddenBy = u.Email.String()
			}
		}
	}

	if me, ok := mid.UserFrom(r.Context()); ok && a.cfg.CanHide {
		allowed, err := a.cfg.Grants.Allowed(r.Context(), me.ID, s.Form, accessbus.RoleAdmin)
		if err != nil {
			log("whether the reader may hide the submission could not be read", err)
		}

		view.CanHide = allowed
	}
}

// review fills in where the submission stands with the office.
func (a app) review(r *http.Request, s submissionbus.Submission, view *oneView) {
	if s.Review == submissionbus.ReviewNone {
		return
	}

	view.Review = s.Review.String()
	view.ReviewNote = s.ReviewNote
	view.CanReview = a.cfg.CanReview && !view.Hidden
	view.Taken = r.URL.Query().Get("taken") != ""

	if s.ReviewedAt.IsZero() {
		return
	}

	view.ReviewedAt = s.ReviewedAt.Local().Format(longWhen)
	view.ReviewedBy = "somebody"

	if a.cfg.Accounts != nil && !s.ReviewedBy.Zero() {
		if u, err := a.cfg.Accounts.ByID(r.Context(), s.ReviewedBy); err == nil {
			view.ReviewedBy = u.Email.String()
		}
	}
}

// export writes the CSV.
//
// encoding/csv, so quoting and embedded commas, quotes and newlines are the
// standard library's problem rather than a string join that works until
// somebody types a comma in the notes field.
func (a app) export(w http.ResponseWriter, r *http.Request) {
	f, subs, ok := a.formAndSubmissions(w, r)
	if !ok {
		return
	}

	// Built in memory first. A CSV of a few hundred rows is small, and writing
	// straight to the ResponseWriter would mean a failure halfway through
	// arriving as a truncated file under a 200 -- a spreadsheet that opens and
	// is quietly missing its last rows, which is worse than an error.
	var body strings.Builder

	out := csv.NewWriter(&body)

	header := []string{"submitted_at", "submission_id", "status", "total", "currency"}
	header = append(header, columnsOf(f)...)
	header = append(header, "payment_reference")

	// A column only on a form that asks for approval, so every other form's
	// download keeps the columns a spreadsheet was already built on. A
	// declined submission is not in the download at all; "awaiting" and
	// "approved" are what this column can say.
	if f.NeedsApproval {
		header = append(header, "review")
	}

	if err := out.Write(header); err != nil {
		a.oops(w, r, "the export header could not be written", err)

		return
	}

	for _, s := range subs {
		// RFC 3339 in UTC, because a spreadsheet full of local times with no
		// offset is a spreadsheet nobody can compare against anything.
		row := []string{
			s.CreatedAt.UTC().Format(time.RFC3339),
			s.ID.String(),
			s.Status.String(),
			s.Answers.Total.String(),
			s.Answers.Currency,
		}

		row = append(row, cellsOf(f, s)...)
		row = append(row, s.PaymentRef)

		if f.NeedsApproval {
			row = append(row, s.Review.String())
		}

		if err := out.Write(row); err != nil {
			a.oops(w, r, "an export row could not be written", err)

			return
		}
	}

	out.Flush()

	if err := out.Error(); err != nil {
		a.oops(w, r, "the export could not be written", err)

		return
	}

	// The visitor's IP address is deliberately not a column. It is kept on the
	// submission for Stripe's fraud checks, and a spreadsheet that gets
	// emailed around a parish office is the wrong place for it.

	name := fmt.Sprintf("%s-submissions-%s.csv", f.ID, time.Now().Format("2006-01-02"))

	h := w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Content-Length", strconv.Itoa(body.Len()))

	// no-store is already set by the surface's policy and is left alone here
	// on purpose: this file is a list of names, addresses and amounts.

	if _, err := io.WriteString(w, body.String()); err != nil {
		a.cfg.Log.Warn("an export was cut off while being sent",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
	}
}

// columnsOf is the column heading per field, in the definition's order,
// followed by one per item.
//
// From the definition rather than from the submissions, which is the whole
// reason formbus.Answers keeps its fields as an ordered slice. Derived from
// the rows instead, a form whose first submission skipped a conditional field
// would export a spreadsheet missing that column entirely, and two exports of
// the same form could have different columns.
func columnsOf(f formbus.Form) []string {
	out := make([]string, 0, len(f.Fields)+len(f.Items))

	for _, fld := range f.Questions() {
		out = append(out, fld.Label)
	}

	for _, it := range f.Items {
		out = append(out, it.Label)
	}

	return out
}

// cellsOf is one submission's values, lined up with columnsOf.
func cellsOf(f formbus.Form, s submissionbus.Submission) []string {
	out := make([]string, 0, len(f.Fields)+len(f.Items))

	for _, fld := range f.Questions() {
		ans, ok := s.Answers.Field(fld.Name)
		if !ok {
			// Not asked, or left blank. Empty either way in a spreadsheet;
			// the detail page is where the difference is visible.
			out = append(out, "")

			continue
		}

		out = append(out, strings.Join(ans.Values, ", "))
	}

	for _, it := range f.Items {
		qty := ""

		for _, l := range s.Answers.Lines {
			if l.ItemID == it.ID {
				qty = strconv.Itoa(l.Qty)

				break
			}
		}

		out = append(out, qty)
	}

	return out
}

// summarise counts what somebody came to the page to find out.
func summarise(f formbus.Form, subs []submissionbus.Submission) summaryView {
	var s summaryView

	tally := map[string]*itemTally{}
	order := make([]string, 0, len(f.Items))

	for _, it := range f.Items {
		tally[it.ID] = &itemTally{Label: it.Label}
		order = append(order, it.ID)
	}

	var paid types.Money

	for _, sub := range subs {
		// ByForm has already left these out. Asked again here because this is
		// where an unpaid order counted by mistake would become food ordered
		// for somebody who is not coming.
		if !sub.Status.Settled() {
			continue
		}

		s.Count++

		switch sub.Status {
		case submissionbus.StatusPaid:
			s.Paid++
		case submissionbus.StatusReceived:
			s.Received++
		}

		paid += sub.Answers.Total

		for _, l := range sub.Answers.Lines {
			t, known := tally[l.ItemID]
			if !known {
				// An item the definition no longer has, which a submission
				// from before an edit can legitimately hold. Not counted
				// against a column that does not exist; the row still shows
				// it on the detail page.
				continue
			}

			t.Confirmed += l.Qty
		}
	}

	for _, id := range order {
		s.Items = append(s.Items, *tally[id])
	}

	s.PaidTotal = formbus.Show(paid, f.Currency)

	return s
}

// formAndSubmissions resolves the form in the path and reads its submissions.
func (a app) formAndSubmissions(w http.ResponseWriter, r *http.Request) (formbus.Form, []submissionbus.Submission, bool) {
	f, ok := a.form(w, r)
	if !ok {
		return formbus.Form{}, nil, false
	}

	subs, err := a.cfg.Submissions.ByForm(r.Context(), f.ID)
	if err != nil {
		a.oops(w, r, "the submissions could not be listed", err)

		return formbus.Form{}, nil, false
	}

	return f, subs, true
}

// administers reports whether the reader holds admin on a form. A failure to
// read the grant is a no, logged: what it decides is whether an unpaid order is
// shown, and the safe answer is not to.
func (a app) administers(r *http.Request, form types.Slug) bool {
	me, ok := mid.UserFrom(r.Context())
	if !ok {
		return false
	}

	allowed, err := a.cfg.Grants.Allowed(r.Context(), me.ID, form, accessbus.RoleAdmin)
	if err != nil {
		a.cfg.Log.Error("whether the reader administers this form could not be read",
			"request_id", web.RequestIDFrom(r.Context()), "form", form.String(), "error", err)
	}

	return allowed
}

// form resolves the slug in the path.
//
// The role gate in front of this route has already checked a grant on that
// slug, so reaching here means the account may read this form -- but the gate
// does not know whether the form exists, because grants are rows and
// definitions are files. So a grant on a form that has since been renamed
// away lands here and gets a 404.
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

func (a app) notFound(w http.ResponseWriter, r *http.Request) {
	a.cfg.Render.Render(w, r, http.StatusNotFound, "no-form", struct{}{})
}

// oops logs the detail and shows a sentence. The error never reaches the page:
// this surface is behind a session, but a database error message can still
// name a table or a path, and neither belongs in front of anybody.
func (a app) oops(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.cfg.Log.Error(what,
		"request_id", web.RequestIDFrom(r.Context()), "error", err)
	http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)
}

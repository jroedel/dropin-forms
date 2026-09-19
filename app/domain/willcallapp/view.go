package willcallapp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// show reads the form's orders and renders the queue.
func (a app) show(w http.ResponseWriter, r *http.Request, status int, f formbus.Form, view listView) {
	view.FormID = f.ID.String()
	view.Title = f.Title
	view.Refresh = a.refresh

	// Paused is opt-in and off by default, because the reason this page exists
	// is that a list which stopped updating is wrong within a minute.
	view.Live = r.URL.Query().Get("live") != "off"
	view.Query = strings.TrimSpace(r.URL.Query().Get("q"))

	// The sentence from the write that redirected here. It is in the query
	// because every write on this page redirects -- see the package comment on
	// the meta refresh -- and a query is the only place left to put it.
	//
	// Whatever is in these is rendered as text by html/template, which escapes
	// it; nothing here is trusted because it arrived in a URL somebody could
	// have typed.
	if view.Done == "" {
		view.Done = r.URL.Query().Get("done")
	}
	if view.Problem == "" {
		view.Problem = r.URL.Query().Get("problem")
	}

	view.RefreshURL = a.pageURL(f, view.Query, !view.Live)
	view.ToggleURL = a.pageURL(f, view.Query, view.Live)

	orders, err := a.cfg.Orders.ByForm(r.Context(), f.ID)
	if err != nil {
		a.oops(w, r, "the orders on that form could not be listed", err)

		return
	}

	handed, err := a.cfg.Orders.Collected(r.Context(), f.ID)
	if err != nil {
		a.oops(w, r, "the collections on that form could not be read", err)

		return
	}

	// One account lookup per distinct volunteer rather than per row. There are
	// two of them at the table, and a query per order is how a page that is
	// fast in rehearsal is slow with a queue in front of it.
	names := a.namesOf(r.Context(), handed)

	for _, sub := range orders {
		// Only what has been paid for. An order that owes money is not a
		// person waiting for tokens, it is somebody who closed the checkout
		// page -- and the will-call list is not where that is chased up.
		if !sub.Status.Settled() {
			continue
		}

		tokens := tokensOf(sub)

		row := orderView{
			ID:     sub.ID.String(),
			Who:    who(sub),
			Email:  sub.Email.String(),
			Bought: bought(sub),
			Tokens: tokens,
			Total:  formbus.Show(sub.Answers.Total, sub.Answers.Currency),
			When:   sub.CreatedAt.Local().Format("15:04"),
		}

		if c, done := handed[sub.ID]; done {
			row.Collected = true
			row.By = names[c.CollectedBy]
			row.At = c.CollectedAt.Local().Format("15:04")
		}

		// Counted before the filter, so the three numbers at the top describe
		// the morning rather than describing the search box.
		if row.Collected {
			view.Collected++
		} else {
			view.Waiting++
			view.Tokens += tokens
		}

		if !matches(row, view.Query) {
			continue
		}

		view.Orders = append(view.Orders, row)
	}

	// Waiting first, and oldest first inside each group. Somebody who has
	// already been seen is not who the person at the table is looking for, and
	// the queue is roughly in the order people bought.
	slices.SortStableFunc(view.Orders, func(x, y orderView) int {
		if x.Collected != y.Collected {
			if x.Collected {
				return 1
			}

			return -1
		}

		return strings.Compare(x.When, y.When)
	})

	view.Showing = len(view.Orders)
	view.Filtered = view.Query != "" && view.Showing < view.Waiting+view.Collected

	a.cfg.Render.Render(w, r, status, "will-call", view)
}

// pageURL is this page with a search kept and the live flag set, which both
// the meta refresh and the pause link need.
func (a app) pageURL(f formbus.Form, query string, paused bool) string {
	q := url.Values{}

	if query != "" {
		q.Set("q", query)
	}

	if paused {
		q.Set("live", "off")
	}

	to := "/forms/" + f.ID.String() + "/will-call"
	if len(q) > 0 {
		to += "?" + q.Encode()
	}

	return to
}

// namesOf turns the account ids on the collections into names, once per
// distinct account rather than once per row.
//
// A lookup that fails costs the name and not the row: the page's job is the
// queue, and an order nobody could label with a volunteer's name is still an
// order that has been collected.
func (a app) namesOf(ctx context.Context, handed map[types.ID]submissionbus.Collection) map[types.ID]string {
	names := map[types.ID]string{}

	for _, c := range handed {
		if _, known := names[c.CollectedBy]; known {
			continue
		}

		u, err := a.cfg.Accounts.ByID(ctx, c.CollectedBy)
		if err != nil {
			names[c.CollectedBy] = "somebody"

			continue
		}

		if u.Name != "" {
			names[c.CollectedBy] = u.Name

			continue
		}

		names[c.CollectedBy] = u.Email.String()
	}

	return names
}

// byWhom is the same answer for one collection, as a phrase for a sentence.
func (a app) byWhom(ctx context.Context, c submissionbus.Collection) string {
	if c.CollectedBy.Zero() {
		return ""
	}

	u, err := a.cfg.Accounts.ByID(ctx, c.CollectedBy)
	if err != nil {
		return ""
	}

	name := u.Name
	if name == "" {
		name = u.Email.String()
	}

	return " by " + name + " at " + c.CollectedAt.Local().Format("15:04")
}

// who is the name to look for in a queue.
//
// The first answer on the form that looks like somebody's name, falling back
// to the address. A convention rather than a declared role, the same shape
// formbus.Answers.SubmitterEmail settles on for the same reason: a definition
// does not mark which field is the person, and demanding that it did would
// mean every existing form needed editing before this page worked.
func who(sub submissionbus.Submission) string {
	for _, ans := range sub.Answers.Fields {
		if !ans.Kind.Textual() || ans.Kind == formbus.KindEmail || ans.Kind == formbus.KindTel {
			continue
		}

		if v := strings.TrimSpace(strings.Join(ans.Values, " ")); v != "" {
			return v
		}
	}

	if sub.Email.String() != "" {
		return sub.Email.String()
	}

	return "somebody"
}

// bought is what the order is owed, in the definition's own words.
func bought(sub submissionbus.Submission) string {
	parts := make([]string, 0, len(sub.Answers.Lines))

	for _, l := range sub.Answers.Lines {
		if l.Qty <= 0 {
			continue
		}

		parts = append(parts, strconv.Itoa(l.Qty)+" × "+l.Label)
	}

	if len(parts) == 0 {
		// A donation and no tickets. Worth saying rather than leaving blank,
		// because an empty cell at a table reads as a page that failed to load.
		return "nothing to collect"
	}

	return strings.Join(parts, ", ")
}

// tokensOf is how many physical tokens this order is owed.
func tokensOf(sub submissionbus.Submission) int {
	var n int

	for _, l := range sub.Answers.Lines {
		if l.Qty > 0 {
			n += l.Qty
		}
	}

	return n
}

// tokenPhrase writes that count the way it is said out loud.
func tokenPhrase(n int) string {
	if n == 1 {
		return "1 token"
	}

	return strconv.Itoa(n) + " tokens"
}

// matches is the search, over the things somebody at a table would read off a
// phone screen held up to them: a name, an address, and what they bought.
//
// Case-folded and substring rather than anything cleverer. The query is two or
// three letters of a surname typed with one thumb.
func matches(row orderView, query string) bool {
	if query == "" {
		return true
	}

	q := strings.ToLower(query)

	for _, field := range []string{row.Who, row.Email, row.Bought} {
		if strings.Contains(strings.ToLower(field), q) {
			return true
		}
	}

	return false
}

// form resolves the slug in the path.
//
// The role gate in front of these routes has already checked a door grant on
// that slug, so reaching here means the account may work this form's table --
// but the gate does not know whether the form exists.
func (a app) form(w http.ResponseWriter, r *http.Request) (formbus.Form, bool) {
	slug, err := types.ParseSlug(r.PathValue(mid.FormSlugParam))
	if err != nil {
		a.notFound(w)

		return formbus.Form{}, false
	}

	f, err := a.cfg.Forms.ByID(slug)

	switch {
	case errors.Is(err, formbus.ErrNotFound):
		a.notFound(w)

		return formbus.Form{}, false
	case err != nil:
		a.oops(w, r, "a form could not be read", err)

		return formbus.Form{}, false
	}

	return f, true
}

// order resolves the submission in the path, and refuses one that belongs to
// another form.
//
// That check is the point of this function and is not incidental. The gate in
// front of the route authorises the account against the slug in the path; the
// id is a separate wildcard and nothing has checked it. Without this, somebody
// who works one form's table could collect an order on a form they have no
// grant on at all, by editing the id in the URL.
func (a app) order(w http.ResponseWriter, r *http.Request, f formbus.Form) (submissionbus.Submission, bool) {
	id, err := types.ParseID(r.PathValue("id"))
	if err != nil {
		a.notFound(w)

		return submissionbus.Submission{}, false
	}

	orders, err := a.cfg.Orders.ByForm(r.Context(), f.ID)
	if err != nil {
		a.oops(w, r, "the orders on that form could not be listed", err)

		return submissionbus.Submission{}, false
	}

	at := slices.IndexFunc(orders, func(c submissionbus.Submission) bool { return c.ID == id })
	if at < 0 {
		// A 404 and not a 403, deliberately. Whether a submission exists on
		// some other form is not this reader's business, and answering
		// differently for "no such order" and "not your order" would say so.
		a.notFound(w)

		return submissionbus.Submission{}, false
	}

	return orders[at], true
}

// notFound is plain text and the same sentence mid.RequireFormRole uses, for
// the reason peopleapp.notFound sets out.
func (a app) notFound(w http.ResponseWriter) {
	http.Error(w, "there is no form by that name.", http.StatusNotFound)
}

// oops logs the detail and shows a sentence.
func (a app) oops(w http.ResponseWriter, r *http.Request, what string, err error) {
	a.cfg.Log.Error(what,
		"request_id", web.RequestIDFrom(r.Context()), "error", err)
	http.Error(w, "something went wrong at our end. Please try again shortly.", http.StatusInternalServerError)
}

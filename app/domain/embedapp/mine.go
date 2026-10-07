package embedapp

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/web"
)

// The way back to answers already given, for a form that allows it.
//
// The link comes from us -- in every message to the person who answered, and
// on the page that thanked them -- and holding it is the whole of the
// credential. submissionbus/answerlink.go says why that is enough and why the
// link itself never expires; the form's ChangeableUntil is the clock.
//
// # Where the token travels
//
// In the query string of the GET, because that is what a link in an email is.
// Our own request log records the path and never the query, so the token does
// not land in it; Apache's access log on the same host does, and that is the
// same trust boundary as the database the token unlocks a row of.
//
// In a hidden field of the POST, not in its action. The page is a response to
// a GET that already carried it, so nothing new is exposed by putting it in
// the body, and a body is in nobody's access log.
//
// # Why these pages are not framed
//
// They can be -- the CSP is the form's own -- but nobody arrives that way.
// The link opens a tab of its own, which is why every link to it is
// target="_top" when it appears inside a frame, and why nothing here reads a
// parent origin from anything but the parameter the rest of this app uses.

// answersField is the hidden input that carries the link's token on the POST.
// Underscored, like the grant's, so that no field a form defines can collide
// with it -- formbus refuses a field name starting with one.
const answersField = "_answers"

// seenField carries the moment the answers on the page were drawn from, in
// milliseconds, so a save from a tab opened before somebody else's is
// refused rather than allowed to undo it. See submissionbus.Edit.Seen.
const seenField = "_seen"

// mineView is what the form page says when it is showing somebody their own
// answers rather than a blank form.
type mineView struct {
	// Token is the link's token, back into the hidden field.
	Token string

	// Seen is the UpdatedAt of the answers this page was drawn from, in
	// milliseconds, for seenField.
	Seen int64

	// Since is when these answers were given, or last changed, and Until is
	// the form's ChangeableUntil, both already words.
	Since string
	Until string
}

// unreachableView is the page for a link that does not lead anywhere now.
type unreachableView struct {
	Form formbus.Form

	// TooLate distinguishes a form that has stopped taking changes from a
	// link that does not work, which want different sentences: one of them
	// is worth replying to the email about, and the other may be worth
	// checking the address in.
	TooLate bool
	Until   string

	// LinkAgain is where to ask for a fresh link, when the form still takes
	// changes.
	LinkAgain string
}

// mine shows somebody their own answers, ready to change.
func (a app) mine(w http.ResponseWriter, r *http.Request) {
	f, ok := a.lookup(w, r)
	if !ok {
		return
	}

	token := r.URL.Query().Get("t")

	id, ok := a.answerLink(w, r, f, token)
	if !ok {
		return
	}

	sub, ok := a.own(w, r, f, id)
	if !ok {
		return
	}

	a.renderMine(w, r, http.StatusOK, f, token, sub, sub.UpdatedAt, sub.Answers.Values(), formbus.Invalid{}, "")
}

// change stores changed answers and says so.
//
// The order of checks is submit's, for submit's reasons: the link, then the
// grant that says which definition the page was drawn from, then the rules,
// then the store. Each refusal re-renders what was typed, because the person
// who typed it was told the form would keep it.
func (a app) change(w http.ResponseWriter, r *http.Request) {
	f, ok := a.lookup(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	if err := r.ParseForm(); err != nil {
		// Without a body there is no token either, and no answers to put
		// back. The link they followed still works; say so.
		a.unreachable(w, r, http.StatusBadRequest, f, false)

		return
	}

	token := r.PostFormValue(answersField)

	id, ok := a.answerLink(w, r, f, token)
	if !ok {
		return
	}

	// Unreadable is the same as stale: the answers as they stand are shown,
	// and the person decides again. Nothing they could put here reaches
	// anybody else's row.
	var seen time.Time
	if ms, err := strconv.ParseInt(r.PostFormValue(seenField), 10, 64); err == nil {
		seen = time.UnixMilli(ms)
	}

	// What the person posted, less the fields this app put there. Kept apart
	// from PostForm so that none appears to formbus as an answer to a
	// question, and so a re-render does not echo them twice.
	values := formbus.Values(r.PostForm)
	delete(values, answersField)
	delete(values, seenField)

	now := a.now()

	g, err := formbus.Redeem(a.cfg.GrantKey, r.PostFormValue(grantField), f, now)
	if err != nil {
		if !errors.Is(err, formbus.ErrGrantRefused) {
			a.cfg.Log.Error("a grant could not be checked",
				"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
			a.oops(w, r)

			return
		}

		sub, ok := a.own(w, r, f, id)
		if !ok {
			return
		}

		a.renderMine(w, r, http.StatusOK, f, token, sub, seen, values, formbus.Invalid{},
			"This page had been open a while, so we have refreshed it. Please check your answers and save them again.")

		return
	}

	answers, err := f.ValidateChange(now, values)

	switch {
	case err == nil:

	case isUnchangeable(err):
		a.unreachable(w, r, http.StatusOK, f, true)

		return

	default:
		invalid, ok := errors.AsType[formbus.Invalid](err)
		if !ok {
			a.cfg.Log.Error("a change could not be validated",
				"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
			a.oops(w, r)

			return
		}

		sub, ok := a.own(w, r, f, id)
		if !ok {
			return
		}

		a.renderMine(w, r, http.StatusUnprocessableEntity, f, token, sub, seen, values, invalid, "")

		return
	}

	before, after, err := a.cfg.Submissions.Change(r.Context(), now, g, submissionbus.Edit{
		ID:       id,
		Seen:     seen,
		Answers:  answers,
		RemoteIP: a.remoteIP(r),
	})

	switch {
	case err == nil:

	case errors.Is(err, submissionbus.ErrUnchanged):
		// Somebody pressed Save to be sure. Nothing was written, nobody is
		// told, and the page says that much rather than "thank you" -- which
		// would read as though something had been sent.
		a.cfg.Render.Render(w, r, http.StatusOK, "done", doneView{
			Form:         f,
			Submission:   before,
			ParentOrigin: parentOrigin(f, r),
			Unchanged:    true,
			ChangeURL:    a.changeURL(f, before),
			ChangeUntil:  untilWords(f.ChangeableUntil),
		})

		return

	case errors.Is(err, submissionbus.ErrReplayed):
		a.cfg.Log.Info("a replayed change was refused",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String())

		a.cfg.Render.Render(w, r, http.StatusOK, "done", doneView{
			Form:         f,
			ParentOrigin: parentOrigin(f, r),
			Duplicate:    true,
		})

		return

	case errors.Is(err, submissionbus.ErrChangedMeanwhile):
		// Another tab, or another device, saved first. What they now say is
		// shown, with this attempt discarded rather than merged: a merge
		// would be this code deciding which of two things a person said was
		// the one they meant.
		sub, ok := a.own(w, r, f, id)
		if !ok {
			return
		}

		a.renderMine(w, r, http.StatusConflict, f, token, sub, sub.UpdatedAt, sub.Answers.Values(), formbus.Invalid{},
			"These answers were changed somewhere else while this page was open. Here they are as they now stand; make your change again if it is still needed.")

		return

	case errors.Is(err, submissionbus.ErrNotFound):
		// Hidden, most likely, since the link was read above. The same
		// sentence as any other link that does not work.
		a.unreachable(w, r, http.StatusNotFound, f, false)

		return

	default:
		a.cfg.Log.Error("a change could not be stored",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "submission_id", id.String(), "error", err)
		a.oops(w, r)

		return
	}

	a.cfg.Log.Info("submission changed",
		"request_id", web.RequestIDFrom(r.Context()),
		"form", f.ID.String(), "submission_id", after.ID.String())

	// Before the render, for the reason submit tells before it renders.
	if a.cfg.Notify != nil {
		a.cfg.Notify.Changed(r.Context(), before, after)
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "done", doneView{
		Form:         f,
		Submission:   after,
		ParentOrigin: parentOrigin(f, r),
		Changed:      true,
		ChangeURL:    a.changeURL(f, after),
		ChangeUntil:  untilWords(f.ChangeableUntil),
	})
}

// linkView is the page that sends somebody their link again.
type linkView struct {
	Form  formbus.Form
	Grant string

	// Email is what was typed, back in the box; Problem is what was wrong
	// with it, or with the page.
	Email   string
	Problem string

	// Sent is the outcome, which reads the same whether anything was sent.
	Sent bool
}

// lostLink asks for an address to send the link to.
func (a app) lostLink(w http.ResponseWriter, r *http.Request) {
	f, ok := a.changeable(w, r)
	if !ok {
		return
	}

	a.renderLink(w, r, http.StatusOK, f, linkView{})
}

// sendLink mails the links for an address, if it has any, and says the same
// thing either way.
//
// The same thing, because the page takes any address anybody types: "we have
// no answers from that address" would tell a stranger who has and has not
// answered, which for this form is who is coming to an ordination. The mail
// goes only to the address typed, so asking for somebody else's link sends it
// to them and not to whoever asked.
//
// The grant is checked and not spent. Spending it would need a write per
// request, and what it would prevent -- the same address asked for twice --
// costs one more message to somebody who has answered, which the submit
// allowance already bounds.
func (a app) sendLink(w http.ResponseWriter, r *http.Request) {
	f, ok := a.changeable(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	if err := r.ParseForm(); err != nil {
		a.renderLink(w, r, http.StatusBadRequest, f, linkView{Problem: "We could not read that. Please try again."})

		return
	}

	typed := r.PostFormValue("email")

	if _, err := formbus.Redeem(a.cfg.GrantKey, r.PostFormValue(grantField), f, a.now()); err != nil {
		a.renderLink(w, r, http.StatusOK, f, linkView{
			Email:   typed,
			Problem: "This page had been open a while, so we have refreshed it. Please send it again.",
		})

		return
	}

	email, err := types.ParseEmail(typed)
	if err != nil {
		a.renderLink(w, r, http.StatusUnprocessableEntity, f, linkView{
			Email:   typed,
			Problem: "That does not look like an email address. Please check it and try again.",
		})

		return
	}

	if a.cfg.Notify != nil {
		a.cfg.Notify.SendLinks(r.Context(), f.ID, email)
	}

	a.renderLink(w, r, http.StatusOK, f, linkView{Email: email.String(), Sent: true})
}

// changeable resolves the form and answers the page itself when it does not
// take changes now.
func (a app) changeable(w http.ResponseWriter, r *http.Request) (formbus.Form, bool) {
	f, ok := a.lookup(w, r)
	if !ok {
		return formbus.Form{}, false
	}

	if !f.Changeable(a.now()) || a.cfg.AnswerKey.Zero() {
		a.unreachable(w, r, http.StatusOK, f, true)

		return formbus.Form{}, false
	}

	return f, true
}

func (a app) renderLink(w http.ResponseWriter, r *http.Request, status int, f formbus.Form, v linkView) {
	grant, err := formbus.Mint(a.cfg.GrantKey, f, a.now())
	if err != nil {
		a.cfg.Log.Error("a grant could not be minted",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
		a.oops(w, r)

		return
	}

	v.Form = f
	v.Grant = grant

	a.cfg.Render.Render(w, r, status, "link", v)
}

// answerLink reads a presented token and checks it names this form, answering
// the page itself when it does not.
//
// The form is checked here as well as by submissionbus.Mine, because the
// sentence differs: a link for one form pasted under another's address is a
// link that does not work, and saying so needs no database.
func (a app) answerLink(w http.ResponseWriter, r *http.Request, f formbus.Form, token string) (types.ID, bool) {
	form, id, err := submissionbus.ReadAnswerLink(a.cfg.AnswerKey, token)

	switch {
	case errors.Is(err, submissionbus.ErrBadAnswerLink), err == nil && form != f.ID:
		a.cfg.Log.Info("an answer link was refused",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "reason", err)
		a.unreachable(w, r, http.StatusNotFound, f, false)

		return types.ID{}, false

	case err != nil:
		// No key configured, which is a deployment without the feature
		// rather than a stranger's mistake.
		a.cfg.Log.Error("an answer link could not be checked",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "error", err)
		a.unreachable(w, r, http.StatusNotFound, f, false)

		return types.ID{}, false
	}

	if !f.Changeable(a.now()) {
		a.unreachable(w, r, http.StatusOK, f, true)

		return types.ID{}, false
	}

	return id, true
}

// own reads the submission a link names, answering the page itself when there
// is none to show.
func (a app) own(w http.ResponseWriter, r *http.Request, f formbus.Form, id types.ID) (submissionbus.Submission, bool) {
	sub, err := a.cfg.Submissions.Mine(r.Context(), f.ID, id)

	switch {
	case errors.Is(err, submissionbus.ErrNotFound):
		a.unreachable(w, r, http.StatusNotFound, f, false)

		return submissionbus.Submission{}, false

	case err != nil:
		a.cfg.Log.Error("a submission could not be read for changing",
			"request_id", web.RequestIDFrom(r.Context()), "form", f.ID.String(), "submission_id", id.String(), "error", err)
		a.oops(w, r)

		return submissionbus.Submission{}, false
	}

	return sub, true
}

// renderMine is render, for somebody's own answers.
func (a app) renderMine(
	w http.ResponseWriter, r *http.Request, status int,
	f formbus.Form, token string, sub submissionbus.Submission, seen time.Time,
	values formbus.Values, problems formbus.Invalid, note string,
) {
	view, ok := a.formView(w, r, f, values, problems, note)
	if !ok {
		return
	}

	view.Action = withParent("/f/"+f.ID.String()+"/mine", view.ParentOrigin)
	view.Mine = &mineView{
		Token: token,
		Seen:  seen.UnixMilli(),
		Since: sub.UpdatedAt.Local().Format("Monday 2 January 2006"),
		Until: untilWords(f.ChangeableUntil),
	}

	a.cfg.Render.Render(w, r, status, "form", view)
}

func (a app) unreachable(w http.ResponseWriter, r *http.Request, status int, f formbus.Form, tooLate bool) {
	a.cfg.Render.Render(w, r, status, "unreachable", unreachableView{
		Form:      f,
		TooLate:   tooLate,
		Until:     untilWords(f.ChangeableUntil),
		LinkAgain: a.linkAgain(f),
	})
}

// linkAgain is the page that sends somebody their link again, or empty on a
// form where there is no link to send.
func (a app) linkAgain(f formbus.Form) string {
	if !f.Changeable(a.now()) || a.cfg.AnswerKey.Zero() {
		return ""
	}

	return "/f/" + f.ID.String() + "/link"
}

// changeURL is the link back to one submission's answers, relative to this
// surface, or empty when there is none to offer: a form that does not take
// changes, or a deployment with no key to sign the link with.
//
// Relative on purpose. It is rendered inside the frame on somebody else's
// site, and a relative link there resolves against the frame's own address,
// which is ours.
func (a app) changeURL(f formbus.Form, sub submissionbus.Submission) string {
	if !f.Changeable(a.now()) || a.cfg.AnswerKey.Zero() || sub.ID.Zero() {
		return ""
	}

	token, err := submissionbus.MintAnswerLink(a.cfg.AnswerKey, f.ID, sub.ID)
	if err != nil {
		a.cfg.Log.Error("an answer link could not be minted",
			"form", f.ID.String(), "submission_id", sub.ID.String(), "error", err)

		return ""
	}

	return "/f/" + f.ID.String() + "/mine?t=" + token
}

// untilWords is a ChangeableUntil as a person reads it, in the zone this
// service runs in, which is the zone the builder took it in.
func untilWords(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return t.Local().Format("Monday 2 January 2006, 3:04pm MST")
}

// isUnchangeable reports whether validation refused a change because the form
// no longer takes them.
func isUnchangeable(err error) bool {
	_, ok := errors.AsType[formbus.Unchangeable](err)

	return ok
}

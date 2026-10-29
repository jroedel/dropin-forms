package muxer_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/notify/notifybus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/foundation/mail"
)

// Approving and declining submissions, on both surfaces as they are mounted:
// the visit sign-up of #73 with somebody in the office deciding each one.

const requestsPath = "/f/visit-requests"

func visitRequests(t *testing.T) formbus.Form {
	t.Helper()

	f := homeVisits(t)
	f.ID = mustSlug(t, "visit-requests")
	f.NeedsApproval = true
	f.Confirmation = "Father will see you then."
	f.Fields = append(f.Fields[:1:1],
		formbus.Field{Name: "email", Label: "Email", Kind: formbus.KindEmail, Required: true},
		f.Fields[1])
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("the visit requests form does not pass Check: %v", err)
	}

	return f
}

type requestsHarness struct {
	office
	sent   *mail.Recorder
	cookie string
}

func requestsSurface(t *testing.T) requestsHarness {
	t.Helper()

	f := visitRequests(t)
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

	var ticks time.Duration

	cfg := newConfig(t, nil, nil)
	cfg.Embed.Now = func() time.Time {
		ticks += time.Second

		return now.Add(ticks)
	}
	cfg.Embed.Forms = formAt{&f}
	cfg.Embed.AnswerKey = answerKey
	cfg.Forms = formAt{&f}

	subs, ok := cfg.Embed.Submissions.(*submissionbus.Business)
	if !ok {
		t.Fatalf("the test config holds a %T rather than the submission domain", cfg.Embed.Submissions)
	}

	cfg.Reviewing = subs

	sent := &mail.Recorder{}

	notifier, err := notifybus.NewBusiness(notifybus.Config{
		Log:          cfg.Log,
		Mail:         sent,
		Forms:        cfg.Embed.Forms,
		Submissions:  subs,
		Grants:       cfg.Access,
		Accounts:     cfg.Users,
		Office:       "office@schoenstatt.test",
		AdminBaseURL: "https://forms.test",
		AnswerKey:    answerKey,
		EmbedBaseURL: "https://f.forms.test",
		Now:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("notifybus.NewBusiness: %v", err)
	}

	cfg.Notify = notifier

	o := office{admin: adminOf(t, cfg), embed: embedOf(t, cfg), cfg: cfg}

	// Somebody who reads the sign-ups and nothing more.
	who, cookie := o.reader(t, "secretary@schoenstatt.test")
	o.grant(t, who, "visit-requests", accessbus.RoleResults)

	return requestsHarness{office: o, sent: sent, cookie: cookie}
}

// ask submits a request and returns the thank-you page.
func (rh requestsHarness) ask(t *testing.T, name, email, slot string) string {
	t.Helper()

	page := getPage(t, rh.embed, requestsPath).Body.String()

	w := postTo(t, rh.embed, requestsPath, url.Values{
		embedapp.GrantField: {grantIn(t, page)},
		"name":              {name},
		"email":             {email},
		"slot":              {slot},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("asking for %s = %d:\n%s", slot, w.Code, short(w.Body.String()))
	}

	return w.Body.String()
}

// last is the newest message to an address.
func (rh requestsHarness) last(t *testing.T, address string) mail.Message {
	t.Helper()

	for i := len(rh.sent.Sent) - 1; i >= 0; i-- {
		if rh.sent.Sent[i].To == address {
			return rh.sent.Sent[i]
		}
	}

	t.Fatalf("nothing was sent to %s", address)

	return mail.Message{}
}

// pageOf is the results page of the one submission from an address.
func (rh requestsHarness) pageOf(t *testing.T, email string) string {
	t.Helper()

	for _, id := range rh.idsOf(t, "visit-requests", rh.cookie) {
		p := "/forms/visit-requests/submissions/" + id
		if strings.Contains(rh.read(t, p, rh.cookie).Body.String(), email) {
			return p
		}
	}

	t.Fatalf("no submission from %s in the list", email)

	return ""
}

func TestARequestWaitsForTheOfficeAndIsTold(t *testing.T) {
	rh := requestsSurface(t)

	// Ana asks for B. The page and her email say it is not confirmed yet,
	// and hold back the form's "see you then".
	done := rh.ask(t, "Ana", "ana@example.org", "b")
	if !strings.Contains(done, "It is not confirmed yet") || strings.Contains(done, "Father will see you then.") {
		t.Errorf("the thank-you page does not say it is waiting:\n%s", short(done))
	}

	if m := rh.last(t, "ana@example.org"); !strings.HasSuffix(m.Subject, ": we have your request") || strings.Contains(m.Text, "Father will see you then.") {
		t.Errorf("Ana was sent %q:\n%s", m.Subject, m.Text)
	}

	if m := rh.last(t, "office@schoenstatt.test"); m.Subject != "Awaiting your approval: Home visits" {
		t.Errorf("the office was sent %q", m.Subject)
	}

	// Her slot is held, and she is not on the public list yet.
	blank := getPage(t, rh.embed, requestsPath).Body.String()
	if !strings.Contains(blank, `value="b" disabled>`) || strings.Contains(blank, "<li>Ana") {
		t.Errorf("a waiting request does not hold its slot, or is listed:\n%s", short(blank))
	}

	page := rh.pageOf(t, "ana@example.org")
	body := rh.read(t, page, rh.cookie).Body.String()

	for _, want := range []string{"awaiting approval", `action="` + page + `/approve"`, `action="` + page + `/decline"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the results reader's page does not contain %q:\n%s", want, short(body))
		}
	}

	// Another site cannot approve it on the reader's behalf.
	r := httptest.NewRequest(http.MethodPost, page+"/approve", strings.NewReader(""))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.AddCookie(&http.Cookie{Name: mid.CookieName, Value: rh.cookie})

	w := httptest.NewRecorder()
	rh.admin.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("a cross-site approval = %d, want 403", w.Code)
	}

	// The reader approves it.
	if w := rh.post(t, page+"/approve", rh.cookie); w.Code != http.StatusSeeOther || w.Header().Get("Location") != page {
		t.Fatalf("approving = %d to %q, want 303 back to the page", w.Code, w.Header().Get("Location"))
	}

	if body := rh.read(t, page, rh.cookie).Body.String(); !strings.Contains(body, "Approved by secretary@schoenstatt.test") {
		t.Errorf("the page does not say who approved it:\n%s", short(body))
	}

	if m := rh.last(t, "ana@example.org"); !strings.HasSuffix(m.Subject, ": confirmed") || !strings.Contains(m.Text, "Father will see you then.") {
		t.Errorf("Ana was sent %q:\n%s", m.Subject, m.Text)
	}

	if blank := getPage(t, rh.embed, requestsPath).Body.String(); !strings.Contains(blank, "<li>Ana -- Monday 10 am</li>") {
		t.Errorf("an approved request is not on the public list:\n%s", short(blank))
	}

	// She moves to another slot through her link: back to waiting, and the
	// office is told so.
	mine := getPage(t, rh.embed, changeLinkIn(t, done)).Body.String()

	values := url.Values{
		embedapp.GrantField: {grantIn(t, mine)},
		"_answers":          {answersTokenPattern.FindStringSubmatch(mine)[1]},
		"_seen":             {seenIn(t, mine)},
		"name":              {"Ana"},
		"email":             {"ana@example.org"},
		"slot":              {"c"},
	}

	if w := postTo(t, rh.embed, requestsPath+"/mine", values); w.Code != http.StatusOK {
		t.Fatalf("changing the slot = %d:\n%s", w.Code, short(w.Body.String()))
	}

	if body := rh.read(t, page, rh.cookie).Body.String(); !strings.Contains(body, "awaiting approval") {
		t.Errorf("a changed approval is not waiting again:\n%s", short(body))
	}

	if m := rh.last(t, "office@schoenstatt.test"); !strings.Contains(m.Text, "They are waiting for approval.") {
		t.Errorf("the office was not told it waits again:\n%s", m.Text)
	}
}

func TestADeclineFreesTheSlotAndCannotBeUndoneOnceItIsTaken(t *testing.T) {
	rh := requestsSurface(t)

	rh.ask(t, "Ben", "ben@example.org", "a")
	page := rh.pageOf(t, "ben@example.org")

	w := rh.postValues(t, page+"/decline", rh.cookie, url.Values{"note": {"Father is away that Monday."}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("declining = %d", w.Code)
	}

	m := rh.last(t, "ben@example.org")
	if !strings.HasSuffix(m.Subject, ": your request was not accepted") || !strings.Contains(m.Text, "Father is away that Monday.") {
		t.Errorf("Ben was sent %q:\n%s", m.Subject, m.Text)
	}

	if ids := rh.idsOf(t, "visit-requests", rh.cookie); len(ids) != 0 {
		t.Errorf("the list has %d submissions; a declined one should leave it", len(ids))
	}

	// A is free again, and Cara takes it.
	if blank := getPage(t, rh.embed, requestsPath).Body.String(); strings.Contains(blank, `value="a" disabled`) {
		t.Errorf("declining did not free A:\n%s", short(blank))
	}

	rh.ask(t, "Cara", "cara@example.org", "a")

	// Approving Ben after all is refused: his slot has gone.
	w = rh.post(t, page+"/approve", rh.cookie)
	if w.Code != http.StatusSeeOther || !strings.HasSuffix(w.Header().Get("Location"), "?taken=1") {
		t.Fatalf("re-approving into a taken slot = %d to %q", w.Code, w.Header().Get("Location"))
	}

	body := rh.read(t, page+"?taken=1", rh.cookie).Body.String()
	if !strings.Contains(body, "It was not approved") || !strings.Contains(body, "Declined by") {
		t.Errorf("the page does not say the re-approval was refused:\n%s", short(body))
	}

	list := rh.read(t, "/forms/visit-requests/submissions?declined=1", rh.cookie).Body.String()
	if !strings.Contains(list, page+`"`) {
		t.Errorf("Ben is not under show declined:\n%s", short(list))
	}
}

func TestSomebodyWhoCannotReadTheFormCannotDecide(t *testing.T) {
	rh := requestsSurface(t)

	rh.ask(t, "Ana", "ana@example.org", "a")
	page := rh.pageOf(t, "ana@example.org")

	_, stranger := rh.reader(t, "stranger@example.org")

	if w := rh.post(t, page+"/approve", stranger); w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Errorf("an account with no grant approving = %d, want 403 or 404", w.Code)
	}

	if body := rh.read(t, page, rh.cookie).Body.String(); !strings.Contains(body, "awaiting approval") {
		t.Error("the refused approval happened anyway")
	}
}

func seenIn(t *testing.T, page string) string {
	t.Helper()

	_, after, ok := strings.Cut(page, `name="_seen" value="`)
	if !ok {
		t.Fatalf("no _seen in the page:\n%s", short(page))
	}

	v, _, _ := strings.Cut(after, `"`)

	return v
}

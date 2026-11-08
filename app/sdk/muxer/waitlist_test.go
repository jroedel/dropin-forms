package muxer_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/notify/notifybus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/mail"
)

// A form with a number of places, and a waiting list once they are held, on
// both surfaces as they are mounted: two beds, four people, and the office
// choosing who gets the bed that comes free.

const quietDayPath = "/f/quiet-day"

func quietDay(t *testing.T, waitlist bool) formbus.Form {
	t.Helper()

	f := formbus.Form{
		ID:           mustSlug(t, "quiet-day"),
		Title:        "Advent quiet day",
		Currency:     "usd",
		Confirmation: "See you on the 6th.",
		Fields: []formbus.Field{
			{Name: "name", Label: "Your name", Kind: formbus.KindText, Required: true, MaxLen: 100},
			{Name: "email", Label: "Email", Kind: formbus.KindEmail, Required: true},
		},
		Capacity: 2,
		Waitlist: waitlist,
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("the quiet day form does not pass Check: %v", err)
	}

	return f
}

type quietHarness struct {
	office
	subs   *submissionbus.Business
	sent   *mail.Recorder
	pay    *counter
	refund *refunder
	cookie string
}

func quietSurface(t *testing.T, waitlist bool) quietHarness {
	t.Helper()

	return quietSurfaceFor(t, quietDay(t, waitlist))
}

func quietSurfaceFor(t *testing.T, f formbus.Form) quietHarness {
	t.Helper()

	// From the real clock, because the office's side -- offering a place,
	// and the hold that starts then -- reads it, and the public side has to
	// agree with it about whether that hold has ended.
	start := time.Now().UTC()

	// A minute between requests, so the order people joined the list in is
	// the order they asked rather than a tie.
	var ticks time.Duration

	cfg := newConfig(t, nil, nil)
	cfg.Embed.Now = func() time.Time {
		ticks += time.Minute

		return start.Add(ticks)
	}
	cfg.Embed.Forms = formAt{&f}
	cfg.Embed.AnswerKey = answerKey
	cfg.Forms = formAt{&f}

	pay := &counter{}
	cfg.Embed.Payments = pay

	subs, ok := cfg.Embed.Submissions.(*submissionbus.Business)
	if !ok {
		t.Fatalf("the test config holds a %T rather than the submission domain", cfg.Embed.Submissions)
	}

	cfg.Reviewing = subs

	refund := &refunder{}
	cfg.Refunding = subs
	cfg.Refunds = refund

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
	})
	if err != nil {
		t.Fatalf("notifybus.NewBusiness: %v", err)
	}

	cfg.Notify = notifier

	o := office{admin: adminOf(t, cfg), embed: embedOf(t, cfg), cfg: cfg}

	who, cookie := o.reader(t, "secretary@schoenstatt.test")
	o.grant(t, who, "quiet-day", accessbus.RoleResults)

	return quietHarness{office: o, subs: subs, sent: sent, pay: pay, refund: refund, cookie: cookie}
}

// sign fills the form in from a freshly drawn page.
func (qh quietHarness) sign(t *testing.T, name string) *httptest.ResponseRecorder {
	t.Helper()

	page := getPage(t, qh.embed, quietDayPath).Body.String()

	return postTo(t, qh.embed, quietDayPath, url.Values{
		embedapp.GrantField: {grantIn(t, page)},
		"name":              {name},
		"email":             {strings.ToLower(name) + "@example.org"},
	})
}

func (qh quietHarness) last(t *testing.T, address string) mail.Message {
	t.Helper()

	for i := len(qh.sent.Sent) - 1; i >= 0; i-- {
		if qh.sent.Sent[i].To == address {
			return qh.sent.Sent[i]
		}
	}

	t.Fatalf("nothing was sent to %s", address)

	return mail.Message{}
}

func (qh quietHarness) waiting(t *testing.T) []submissionbus.Submission {
	t.Helper()

	list, err := qh.subs.Waiting(t.Context(), mustSlug(t, "quiet-day"))
	if err != nil {
		t.Fatalf("Waiting: %v", err)
	}

	return list
}

func TestAFullFormTakesAWaitingListAndTheOfficeOffersTheNextPlace(t *testing.T) {
	qh := quietSurface(t, true)

	for _, name := range []string{"Ana", "Ben"} {
		if w := qh.sign(t, name); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "See you on the 6th.") {
			t.Fatalf("%s signing up = %d:\n%s", name, w.Code, short(w.Body.String()))
		}
	}

	// Both places are held. The form says so, and what sending it now means.
	page := getPage(t, qh.embed, quietDayPath).Body.String()
	if !strings.Contains(page, "Every place is taken just now") || !strings.Contains(page, "Join the waiting list") {
		t.Errorf("a full form with a waiting list does not say so:\n%s", short(page))
	}

	// Cara and Dan join the waiting list, in that order.
	for i, name := range []string{"Cara", "Dan"} {
		w := qh.sign(t, name)
		body := w.Body.String()

		if w.Code != http.StatusOK || !strings.Contains(body, "You are on the waiting list") {
			t.Fatalf("%s joining = %d:\n%s", name, w.Code, short(body))
		}

		if want := []string{"number 1 on", "number 2 on"}[i]; !strings.Contains(body, want) {
			t.Errorf("%s is not told %q:\n%s", name, want, short(body))
		}

		if strings.Contains(body, "See you on the 6th.") {
			t.Errorf("%s is told the form's confirmation without a place:\n%s", name, short(body))
		}
	}

	if m := qh.last(t, "cara@example.org"); !strings.HasSuffix(m.Subject, ": you are on the waiting list") || strings.Contains(m.Text, "See you on the 6th.") {
		t.Errorf("Cara was sent %q:\n%s", m.Subject, m.Text)
	}

	if m := qh.last(t, "office@schoenstatt.test"); m.Subject != "Joined the waiting list: Advent quiet day" {
		t.Errorf("the office was last sent %q", m.Subject)
	}

	list := qh.waiting(t)
	if len(list) != 2 {
		t.Fatalf("%d on the waiting list, want Cara and Dan", len(list))
	}

	cara, dan := list[0], list[1]

	// The results page: how full it is, and who is waiting -- with no offer
	// to make while every place is held.
	results := qh.read(t, "/forms/quiet-day/submissions", qh.cookie).Body.String()

	for _, want := range []string{"<strong>2</strong> of 2 places held", "<strong>2</strong> on the waiting list", "Cara", "Dan"} {
		if !strings.Contains(results, want) {
			t.Errorf("the results page does not show %q:\n%s", want, short(results))
		}
	}

	if strings.Contains(results, "/offer") {
		t.Errorf("the results page offers a place on a full form:\n%s", short(results))
	}

	// The waiting are not in the download: nobody is caterering for them.
	if csv := qh.read(t, "/forms/quiet-day/submissions.csv", qh.cookie).Body.String(); strings.Contains(csv, "Cara") {
		t.Errorf("the CSV has somebody on the waiting list in it:\n%s", csv)
	}

	// Somebody who reads the sign-ups opens a waiting row, though it is not
	// settled, and an offer while full is refused with a sentence.
	daysPage := "/forms/quiet-day/submissions/" + dan.ID.String()

	body := qh.read(t, daysPage, qh.cookie).Body.String()
	if !strings.Contains(body, "on the waiting list, number 2") || !strings.Contains(body, `action="`+daysPage+`/offer"`) {
		t.Errorf("the results reader's page of a waiting row:\n%s", short(body))
	}

	if w := qh.post(t, daysPage+"/offer", qh.cookie); w.Code != http.StatusSeeOther || w.Header().Get("Location") != daysPage+"?full=1" {
		t.Fatalf("offering a place on a full form = %d to %q", w.Code, w.Header().Get("Location"))
	}

	if body := qh.read(t, daysPage+"?full=1", qh.cookie).Body.String(); !strings.Contains(body, "No place was offered") {
		t.Errorf("a refused offer is not explained:\n%s", short(body))
	}

	// Ben drops out.
	ben := findByName(t, qh, "Ben")
	if _, _, err := qh.subs.Hide(t.Context(), time.Now(), ben.ID, types.NewID()); err != nil {
		t.Fatalf("Hide: %v", err)
	}

	if results := qh.read(t, "/forms/quiet-day/submissions", qh.cookie).Body.String(); !strings.Contains(results, "/offer") {
		t.Errorf("a place is free and nothing offers it:\n%s", short(results))
	}

	// The office gives it to Dan rather than Cara, which is theirs to choose.
	if w := qh.post(t, daysPage+"/offer", qh.cookie); w.Code != http.StatusSeeOther || w.Header().Get("Location") != daysPage {
		t.Fatalf("offering Dan the place = %d to %q", w.Code, w.Header().Get("Location"))
	}

	if got, err := qh.subs.ByID(t.Context(), dan.ID); err != nil || got.Status != submissionbus.StatusReceived {
		t.Errorf("Dan is %s, %v; want received", got.Status, err)
	}

	if m := qh.last(t, "dan@example.org"); !strings.HasSuffix(m.Subject, ": a place for you") || !strings.Contains(m.Text, "See you on the 6th.") {
		t.Errorf("Dan was sent %q:\n%s", m.Subject, m.Text)
	}

	// Full again, Cara still waiting -- now number 1 -- and Dan in the list.
	if list := qh.waiting(t); len(list) != 1 || list[0].ID != cara.ID {
		t.Errorf("the waiting list is %v, want only Cara", ids(list))
	}

	if csv := qh.read(t, "/forms/quiet-day/submissions.csv", qh.cookie).Body.String(); !strings.Contains(csv, "Dan") {
		t.Errorf("Dan, given a place, is not in the CSV:\n%s", csv)
	}

	carasPage := "/forms/quiet-day/submissions/" + cara.ID.String()
	if w := qh.post(t, carasPage+"/offer", qh.cookie); w.Header().Get("Location") != carasPage+"?full=1" {
		t.Errorf("a second offer for the one place went to %q", w.Header().Get("Location"))
	}
}

// Without a waiting list a full form closes, and somebody who was filling it
// in as the last place went is told so rather than handed their answers back.
func TestAFullFormWithNoWaitingListCloses(t *testing.T) {
	qh := quietSurface(t, false)

	stale := getPage(t, qh.embed, quietDayPath).Body.String()

	for _, name := range []string{"Ana", "Ben"} {
		if w := qh.sign(t, name); w.Code != http.StatusOK {
			t.Fatalf("%s signing up = %d", name, w.Code)
		}
	}

	if page := getPage(t, qh.embed, quietDayPath).Body.String(); !strings.Contains(page, "Every place has been taken") || strings.Contains(page, "<form") {
		t.Errorf("a full form with no waiting list is still drawn:\n%s", short(page))
	}

	w := postTo(t, qh.embed, quietDayPath, url.Values{
		embedapp.GrantField: {grantIn(t, stale)},
		"name":              {"Cara"},
		"email":             {"cara@example.org"},
	})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Every place has been taken") {
		t.Errorf("a submission that lost the last place = %d:\n%s", w.Code, short(w.Body.String()))
	}

	if list := qh.waiting(t); len(list) != 0 {
		t.Errorf("a form with no waiting list put %d on one", len(list))
	}
}

func findByName(t *testing.T, qh quietHarness, name string) submissionbus.Submission {
	t.Helper()

	all, err := qh.subs.ByForm(t.Context(), mustSlug(t, "quiet-day"))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}

	for _, s := range all {
		if a, ok := s.Answers.Field("name"); ok && a.Value() == name {
			return s
		}
	}

	t.Fatalf("no submission from %s", name)

	return submissionbus.Submission{}
}

func ids(list []submissionbus.Submission) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.ID.String())
	}

	return out
}

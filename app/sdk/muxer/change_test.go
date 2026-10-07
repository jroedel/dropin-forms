package muxer_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/app/sdk/muxer"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formtoml"
	"github.com/jroedel/dropin-forms/business/domain/notify/notifybus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/mail"
)

// Changing answers already given, through the link we hand out, on the
// surface as it is mounted in production.

const retreatPath = "/f/retreat"

var changeLinkPattern = regexp.MustCompile(`href="(/f/[a-z0-9-]+/mine\?t=[^"]+)"`)

// changeLinkIn finds the link back to one's answers on a rendered page.
func changeLinkIn(t *testing.T, body string) string {
	t.Helper()

	m := changeLinkPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no change link in the page:\n%s", short(body))
	}

	return m[1]
}

var answersTokenPattern = regexp.MustCompile(`name="_answers" value="([^"]+)"`)

// retreatForm is a form that takes changes until the first of December and
// stops taking new answers on the fifteenth of November.
func retreatForm(t *testing.T) formbus.Form {
	t.Helper()

	f := formbus.Form{
		ID:              mustSlug(t, "retreat"),
		Title:           "Fathers' retreat",
		Currency:        "usd",
		ClosesAt:        time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC),
		ChangeableUntil: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
		Fields: []formbus.Field{
			{Name: "name", Label: "Your name", Kind: formbus.KindText, Required: true, MaxLen: 100},
			{Name: "email", Label: "Email", Kind: formbus.KindEmail, Required: true},
			{Name: "plans", Label: "Plans", Kind: formbus.KindRadio, Required: true, Options: []formbus.Option{
				{Value: "considering", Label: "Seriously considering"},
				{Value: "booked", Label: "Travel booked"},
			}},
		},
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("the retreat form does not pass Check: %v", err)
	}

	return f
}

// retreatHarness is the surface, the form it serves, and the clock it reads,
// each changeable by a test between requests.
type retreatHarness struct {
	h    http.Handler
	form *formbus.Form
	now  *time.Time
	subs *submissionbus.Business
	cfg  muxer.Config
	sent *mail.Recorder
}

// mailTo is everything sent to one address, in order.
func (rh retreatHarness) mailTo(address string) []mail.Message {
	var out []mail.Message

	for _, m := range rh.sent.Sent {
		if m.To == address {
			out = append(out, m)
		}
	}

	return out
}

type formAt struct{ f *formbus.Form }

func (o formAt) ByID(slug types.Slug) (formbus.Form, error) {
	if slug != o.f.ID {
		return formbus.Form{}, formtoml.ErrNotFound
	}

	return *o.f, nil
}

func (o formAt) All() []formbus.Form { return []formbus.Form{*o.f} }

func retreatSurface(t *testing.T) retreatHarness {
	t.Helper()

	f := retreatForm(t)
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

	cfg := newConfig(t, nil, nil)
	cfg.Embed.Now = func() time.Time { return now }
	cfg.Embed.Forms = formAt{&f}
	cfg.Embed.AnswerKey = answerKey

	subs, ok := cfg.Embed.Submissions.(*submissionbus.Business)
	if !ok {
		t.Fatalf("the test config holds a %T rather than the submission domain", cfg.Embed.Submissions)
	}

	cfg.Forms = formAt{&f}

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

	return retreatHarness{h: embedOf(t, cfg), form: &f, now: &now, subs: subs, cfg: cfg, sent: sent}
}

// answerKey signs the test's answer links. Not a secret, as grantKey is not.
var answerKey = func() submissionbus.AnswerKey {
	k, err := submissionbus.ParseAnswerKey("a-test-answer-signing-key-long-enough")
	if err != nil {
		panic(err)
	}

	return k
}()

// answer submits the retreat form and returns the link from the thank-you
// page.
func (rh retreatHarness) answer(t *testing.T, plans string) string {
	t.Helper()

	grant := grantIn(t, getPage(t, rh.h, retreatPath).Body.String())

	w := postTo(t, rh.h, retreatPath, url.Values{
		embedapp.GrantField: {grant},
		"name":              {"Fr. Hector"},
		"email":             {"hector@example.org"},
		"plans":             {plans},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d:\n%s", w.Code, short(w.Body.String()))
	}

	return changeLinkIn(t, w.Body.String())
}

// save posts a page's own grant and token back with the given answers.
func (rh retreatHarness) save(t *testing.T, page string, answers url.Values) (int, string) {
	t.Helper()

	m := answersTokenPattern.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no answer token in the page:\n%s", short(page))
	}

	seen := regexp.MustCompile(`name="_seen" value="([0-9]+)"`).FindStringSubmatch(page)
	if seen == nil {
		t.Fatalf("no _seen in the page:\n%s", short(page))
	}

	values := url.Values{
		embedapp.GrantField: {grantIn(t, page)},
		"_answers":          {m[1]},
		"_seen":             {seen[1]},
	}
	for k, v := range answers {
		values[k] = v
	}

	w := postTo(t, rh.h, retreatPath+"/mine", values)

	return w.Code, w.Body.String()
}

func (rh retreatHarness) only(t *testing.T) submissionbus.Submission {
	t.Helper()

	subs, err := rh.subs.ByForm(t.Context(), rh.form.ID)
	if err != nil || len(subs) != 1 {
		t.Fatalf("ByForm: %d submissions, %v; want exactly one", len(subs), err)
	}

	return subs[0]
}

func TestAnswersAreChangedThroughTheLinkOnTheThankYouPage(t *testing.T) {
	rh := retreatSurface(t)

	link := rh.answer(t, "considering")

	*rh.now = rh.now.Add(30 * 24 * time.Hour)

	w := getPage(t, rh.h, link)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d:\n%s", link, w.Code, short(w.Body.String()))
	}

	page := w.Body.String()
	for _, want := range []string{
		`value="Fr. Hector"`,
		`value="considering" checked`,
		`These are the answers you sent`,
		`Save changes`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not contain %q:\n%s", want, short(page))
		}
	}

	code, body := rh.save(t, page, url.Values{
		"name":  {"Fr. Hector"},
		"email": {"hector@example.org"},
		"plans": {"booked"},
	})
	if code != http.StatusOK || !strings.Contains(body, "Your changes are saved") {
		t.Fatalf("save = %d:\n%s", code, short(body))
	}

	sub := rh.only(t)
	if a, _ := sub.Answers.Field("plans"); a.Value() != "booked" {
		t.Errorf("plans is %q, want the changed answer", a.Value())
	}

	revs, err := rh.subs.Revisions(t.Context(), sub)
	if err != nil || len(revs) != 1 {
		t.Fatalf("Revisions: %d, %v; want the first answers kept", len(revs), err)
	}
	if a, _ := revs[0].Answers.Field("plans"); a.Value() != "considering" {
		t.Errorf("revision 1 says %q", a.Value())
	}

	// And the same link still works afterwards: it names the submission,
	// not a version of it.
	if w := getPage(t, rh.h, link); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `value="booked" checked`) {
		t.Errorf("following the link again = %d", w.Code)
	}
}

func TestChangesAreTakenAfterTheFormClosesToNewAnswers(t *testing.T) {
	rh := retreatSurface(t)

	link := rh.answer(t, "considering")

	// After ClosesAt, before ChangeableUntil.
	*rh.now = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

	if w := getPage(t, rh.h, retreatPath); !strings.Contains(w.Body.String(), "no longer accepting") {
		t.Fatalf("the blank form is still open:\n%s", short(w.Body.String()))
	}

	w := getPage(t, rh.h, link)
	if w.Code != http.StatusOK {
		t.Fatalf("GET link = %d:\n%s", w.Code, short(w.Body.String()))
	}

	code, body := rh.save(t, w.Body.String(), url.Values{
		"name": {"Fr. Hector"}, "email": {"hector@example.org"}, "plans": {"booked"},
	})
	if code != http.StatusOK || !strings.Contains(body, "Your changes are saved") {
		t.Fatalf("save = %d:\n%s", code, short(body))
	}
}

func TestNoChangesAreTakenAfterTheFormsDate(t *testing.T) {
	rh := retreatSurface(t)

	link := rh.answer(t, "considering")
	page := getPage(t, rh.h, link).Body.String()

	*rh.now = rh.form.ChangeableUntil

	w := getPage(t, rh.h, link)
	if !strings.Contains(w.Body.String(), "can no longer be changed here") {
		t.Errorf("GET after the date = %d:\n%s", w.Code, short(w.Body.String()))
	}

	// And a page opened before the date and saved after it.
	code, body := rh.save(t, page, url.Values{
		"name": {"Fr. Hector"}, "email": {"hector@example.org"}, "plans": {"booked"},
	})
	if !strings.Contains(body, "can no longer be changed here") {
		t.Errorf("save after the date = %d:\n%s", code, short(body))
	}

	if a, _ := rh.only(t).Answers.Field("plans"); a.Value() != "considering" {
		t.Errorf("plans is %q; nothing should have been saved", a.Value())
	}
}

func TestSavingWithoutChangingAnythingSaysSo(t *testing.T) {
	rh := retreatSurface(t)

	link := rh.answer(t, "considering")

	code, body := rh.save(t, getPage(t, rh.h, link).Body.String(), url.Values{
		"name": {"Fr. Hector"}, "email": {"hector@example.org"}, "plans": {"considering"},
	})
	if code != http.StatusOK || !strings.Contains(body, "Nothing has changed") {
		t.Fatalf("save = %d:\n%s", code, short(body))
	}

	revs, _ := rh.subs.Revisions(t.Context(), rh.only(t))
	if len(revs) != 0 {
		t.Errorf("%d revisions written for no change", len(revs))
	}
}

func TestALinkThatIsNotOursLeadsNowhere(t *testing.T) {
	rh := retreatSurface(t)

	link := rh.answer(t, "considering")
	token := strings.TrimPrefix(link, retreatPath+"/mine?t=")

	other, err := submissionbus.MintAnswerLink(answerKey, mustSlug(t, "feast-lunch-2026"), rh.only(t).ID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	nobody, err := submissionbus.MintAnswerLink(answerKey, rh.form.ID, types.NewID())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	for name, target := range map[string]string{
		"no token":                retreatPath + "/mine",
		"cut short by a mail app": retreatPath + "/mine?t=" + token[:len(token)/2],
		"for another form":        retreatPath + "/mine?t=" + other,
		"for no submission":       retreatPath + "/mine?t=" + nobody,
	} {
		t.Run(name, func(t *testing.T) {
			w := getPage(t, rh.h, target)
			if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "does not lead to any answers") {
				t.Errorf("GET = %d:\n%s", w.Code, short(w.Body.String()))
			}
		})
	}
}

func TestHidingASubmissionWithdrawsItsLink(t *testing.T) {
	rh := retreatSurface(t)

	link := rh.answer(t, "considering")
	page := getPage(t, rh.h, link).Body.String()

	if _, _, err := rh.subs.Hide(t.Context(), *rh.now, rh.only(t).ID, types.NewID()); err != nil {
		t.Fatalf("Hide: %v", err)
	}

	if w := getPage(t, rh.h, link); w.Code != http.StatusNotFound {
		t.Errorf("GET a hidden submission's link = %d", w.Code)
	}

	if code, _ := rh.save(t, page, url.Values{
		"name": {"x"}, "email": {"hector@example.org"}, "plans": {"booked"},
	}); code != http.StatusNotFound {
		t.Errorf("save to a hidden submission = %d", code)
	}
}

func TestTheSecondOfTwoTabsIsShownTheFirstOnesAnswers(t *testing.T) {
	rh := retreatSurface(t)

	link := rh.answer(t, "considering")
	first := getPage(t, rh.h, link).Body.String()
	second := getPage(t, rh.h, link).Body.String()

	*rh.now = rh.now.Add(time.Minute)

	if code, _ := rh.save(t, first, url.Values{
		"name": {"Fr. Hector"}, "email": {"hector@example.org"}, "plans": {"booked"},
	}); code != http.StatusOK {
		t.Fatalf("first save = %d", code)
	}

	code, body := rh.save(t, second, url.Values{
		"name": {"Fr. Hector Islas"}, "email": {"hector@example.org"}, "plans": {"considering"},
	})
	if code != http.StatusConflict || !strings.Contains(body, `value="booked" checked`) {
		t.Fatalf("second save = %d, want 409 with the first tab's answers:\n%s", code, short(body))
	}

	if a, _ := rh.only(t).Answers.Field("name"); a.Value() != "Fr. Hector" {
		t.Errorf("name is %q; the second tab must not have overwritten the first", a.Value())
	}
}

func TestAQuestionAddedLaterIsAskedThroughTheLink(t *testing.T) {
	rh := retreatSurface(t)

	link := rh.answer(t, "considering")

	// December's question, added after October's answer.
	rh.form.Fields = append(rh.form.Fields, formbus.Field{
		Name: "flight", Label: "Arrival flight", Kind: formbus.KindText, MaxLen: 50,
	})
	rh.form.Stamp()
	if err := rh.form.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	w := getPage(t, rh.h, link)
	if !strings.Contains(w.Body.String(), `name="flight"`) {
		t.Fatalf("the new question is not on the page:\n%s", short(w.Body.String()))
	}

	code, body := rh.save(t, w.Body.String(), url.Values{
		"name": {"Fr. Hector"}, "email": {"hector@example.org"}, "plans": {"booked"}, "flight": {"UA 1234"},
	})
	if code != http.StatusOK {
		t.Fatalf("save = %d:\n%s", code, short(body))
	}

	sub := rh.only(t)
	if a, _ := sub.Answers.Field("flight"); a.Value() != "UA 1234" {
		t.Errorf("flight is %q", a.Value())
	}
	if sub.Version != rh.form.Version {
		t.Errorf("the row is at version %s, want the definition it was last checked against, %s", sub.Version, rh.form.Version)
	}
}

func TestACrossSiteChangeIsRefused(t *testing.T) {
	rh := retreatSurface(t)

	link := rh.answer(t, "considering")
	page := getPage(t, rh.h, link).Body.String()
	token := answersTokenPattern.FindStringSubmatch(page)[1]

	body := url.Values{
		embedapp.GrantField: {grantIn(t, page)},
		"_answers":          {token},
		"name":              {"Someone else"},
		"email":             {"hector@example.org"},
		"plans":             {"booked"},
	}

	r := httptest.NewRequest(http.MethodPost, retreatPath+"/mine", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "cross-site")

	w := httptest.NewRecorder()
	rh.h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST = %d", w.Code)
	}

	if a, _ := rh.only(t).Answers.Field("name"); a.Value() != "Fr. Hector" {
		t.Errorf("name is %q", a.Value())
	}
}

func TestAFormThatDoesNotTakeChangesOffersNoLink(t *testing.T) {
	rh := retreatSurface(t)

	rh.form.ChangeableUntil = time.Time{}
	rh.form.Stamp()

	grant := grantIn(t, getPage(t, rh.h, retreatPath).Body.String())

	w := postTo(t, rh.h, retreatPath, url.Values{
		embedapp.GrantField: {grant},
		"name":              {"Fr. Hector"},
		"email":             {"hector@example.org"},
		"plans":             {"considering"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d", w.Code)
	}

	if changeLinkPattern.MatchString(w.Body.String()) {
		t.Errorf("a form without a change date offered a link:\n%s", short(w.Body.String()))
	}

	token, _ := submissionbus.MintAnswerLink(answerKey, rh.form.ID, rh.only(t).ID)
	if w := getPage(t, rh.h, retreatPath+"/mine?t="+token); !strings.Contains(w.Body.String(), "can no longer be changed here") {
		t.Errorf("a valid link on such a form = %d:\n%s", w.Code, short(w.Body.String()))
	}
}

func TestTheOfficeSeesWhatTheAnswersSaidBeforeTheyChanged(t *testing.T) {
	rh := retreatSurface(t)

	link := rh.answer(t, "considering")

	*rh.now = rh.now.Add(24 * time.Hour)

	if code, body := rh.save(t, getPage(t, rh.h, link).Body.String(), url.Values{
		"name": {"Fr. Hector"}, "email": {"hector@example.org"}, "plans": {"booked"},
	}); code != http.StatusOK {
		t.Fatalf("save = %d:\n%s", code, short(body))
	}

	o := office{admin: adminOf(t, rh.cfg), embed: rh.h, cfg: rh.cfg}
	reader, cookie := o.reader(t, "office@schoenstatt.test")
	o.grant(t, reader, "retreat", accessbus.RoleResults)

	ids := o.idsOf(t, "retreat", cookie)
	if len(ids) != 1 {
		t.Fatalf("%d submissions listed, want 1", len(ids))
	}

	body := o.read(t, "/forms/retreat/submissions/"+ids[0], cookie).Body.String()
	for _, want := range []string{"Last changed", "Earlier answers", "<td>considering</td>", "<td>booked</td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the submission page does not say %q:\n%s", want, short(body))
		}
	}
}

// The email is the way back: the link in the first message opens the
// person's own answers, and a change sends the office what changed.
func TestTheLinkInTheEmailIsTheWayBack(t *testing.T) {
	rh := retreatSurface(t)

	rh.answer(t, "considering")

	first := rh.mailTo("hector@example.org")
	if len(first) != 1 {
		t.Fatalf("%d messages to the person, want 1", len(first))
	}

	m := regexp.MustCompile(`https://f\.forms\.test(/f/retreat/mine\?t=\S+)`).FindStringSubmatch(first[0].Text)
	if m == nil {
		t.Fatalf("no link in the first message:\n%s", first[0].Text)
	}

	// Weeks later, and before the form's date.
	*rh.now = rh.now.Add(30 * 24 * time.Hour)

	page := getPage(t, rh.h, m[1])
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `value="considering" checked`) {
		t.Fatalf("following the emailed link = %d:\n%s", page.Code, short(page.Body.String()))
	}

	rh.sent.Sent = nil

	code, body := rh.save(t, page.Body.String(), url.Values{
		"name": {"Fr. Hector"}, "email": {"hector@example.org"}, "plans": {"booked"},
	})
	if code != http.StatusOK || !strings.Contains(body, "A copy is on its way") {
		t.Fatalf("save = %d:\n%s", code, short(body))
	}

	office := rh.mailTo("office@schoenstatt.test")
	if len(office) != 1 || !strings.Contains(office[0].Text, "Plans: booked (was: considering)") {
		t.Errorf("the office was not told what changed: %v", subjects(rh.sent.Sent))
	}

	if mine := rh.mailTo("hector@example.org"); len(mine) != 1 || !strings.Contains(mine[0].Text, "/f/retreat/mine?t=") {
		t.Errorf("the person's copy of the change does not carry the link again")
	}
}

func TestALostLinkIsSentOnlyToTheAddressThatAnswered(t *testing.T) {
	rh := retreatSurface(t)

	rh.answer(t, "considering")
	rh.sent.Sent = nil

	ask := func(email string) string {
		t.Helper()

		page := getPage(t, rh.h, retreatPath+"/link")
		if page.Code != http.StatusOK {
			t.Fatalf("the lost-link page = %d:\n%s", page.Code, short(page.Body.String()))
		}

		w := postTo(t, rh.h, retreatPath+"/link", url.Values{
			embedapp.GrantField: {grantIn(t, page.Body.String())},
			"email":             {email},
		})
		if w.Code != http.StatusOK {
			t.Fatalf("asking = %d:\n%s", w.Code, short(w.Body.String()))
		}

		return w.Body.String()
	}

	known := ask("Hector@Example.org")
	unknown := ask("stranger@example.org")

	// The same page either way, apart from the address echoed back.
	if strings.ReplaceAll(known, "hector@example.org", "X") != strings.ReplaceAll(unknown, "stranger@example.org", "X") {
		t.Errorf("the page differs for an address that answered and one that did not")
	}

	if got := rh.mailTo("hector@example.org"); len(got) != 1 || !strings.Contains(got[0].Text, "/f/retreat/mine?t=") {
		t.Errorf("the link was not sent to the address that answered")
	}
	if got := rh.mailTo("stranger@example.org"); len(got) != 0 {
		t.Errorf("mail was sent to an address that never answered")
	}
}

func TestTheFormOffersTheLinkAgainOnlyWhenItTakesChanges(t *testing.T) {
	rh := retreatSurface(t)

	if !strings.Contains(getPage(t, rh.h, retreatPath).Body.String(), `href="/f/retreat/link"`) {
		t.Error("a form that takes changes does not offer the link again")
	}

	*rh.now = rh.form.ChangeableUntil

	if w := getPage(t, rh.h, retreatPath+"/link"); !strings.Contains(w.Body.String(), "can no longer be changed here") {
		t.Errorf("the lost-link page after the date = %d:\n%s", w.Code, short(w.Body.String()))
	}
}

package muxer_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// Time slots that only so many may take (#73), on the public surface as it is
// mounted: the walk-through the issue was written as.

const slotsPath = "/f/home-visits"

// homeVisits is a priest's home visits in one-hour blocks, one household
// each, with answers that can be changed through the emailed link.
func homeVisits(t *testing.T) formbus.Form {
	t.Helper()

	f := formbus.Form{
		ID:              mustSlug(t, "home-visits"),
		Title:           "Home visits",
		Currency:        "usd",
		ClosedNote:      "Every visit is taken. Thank you!",
		ChangeableUntil: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
		Fields: []formbus.Field{
			{Name: "name", Label: "Your name", Kind: formbus.KindText, Required: true, MaxLen: 100},
			{Name: "slot", Label: "Time slot", Kind: formbus.KindRadio, Required: true, Options: []formbus.Option{
				{Value: "a", Label: "Monday 9 am", Limit: 1},
				{Value: "b", Label: "Monday 10 am", Limit: 1},
				{Value: "c", Label: "Monday 11 am", Limit: 1},
			}},
		},
		Listing: formbus.Listing{Heading: "Visits", Line: "{name} -- {slot}"},
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("the home visits form does not pass Check: %v", err)
	}

	return f
}

type slotsHarness struct {
	h    http.Handler
	form formbus.Form
	subs *submissionbus.Business
}

func slotsSurface(t *testing.T) slotsHarness {
	t.Helper()

	f := homeVisits(t)

	var ticks time.Duration

	cfg := newConfig(t, nil, nil)
	cfg.Embed.Now = func() time.Time {
		ticks += time.Second

		return time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC).Add(ticks)
	}
	cfg.Embed.Forms = oneForm{f}
	cfg.Embed.AnswerKey = answerKey

	subs, ok := cfg.Embed.Submissions.(*submissionbus.Business)
	if !ok {
		t.Fatalf("the test config holds a %T rather than the submission domain", cfg.Embed.Submissions)
	}

	return slotsHarness{h: embedOf(t, cfg), form: f, subs: subs}
}

// take submits the form from a page drawn beforehand.
func (sh slotsHarness) take(t *testing.T, page, name, slot string) (int, string) {
	t.Helper()

	w := postTo(t, sh.h, slotsPath, url.Values{
		embedapp.GrantField: {grantIn(t, page)},
		"name":              {name},
		"slot":              {slot},
	})

	return w.Code, w.Body.String()
}

func (sh slotsHarness) blank(t *testing.T) string {
	t.Helper()

	return getPage(t, sh.h, slotsPath).Body.String()
}

func TestATimeSlotGoesToOneHousehold(t *testing.T) {
	sh := slotsSurface(t)

	// 1. Person 1 takes B, and the list beneath the form says so.
	if code, body := sh.take(t, sh.blank(t), "Ana", "b"); code != http.StatusOK {
		t.Fatalf("taking B = %d:\n%s", code, short(body))
	}

	page := sh.blank(t)

	if !strings.Contains(page, "<li>Ana -- Monday 10 am</li>") {
		t.Errorf("the list does not show Ana in B:\n%s", short(page))
	}

	// 2. The next visitor sees A and C open and B taken.
	if !strings.Contains(page, `value="b" disabled>`) || !strings.Contains(page, "Monday 10 am — taken") {
		t.Errorf("B is not drawn as taken:\n%s", short(page))
	}

	if strings.Contains(page, `value="a" disabled`) || strings.Contains(page, `value="c" disabled`) {
		t.Errorf("an open slot is drawn as taken:\n%s", short(page))
	}

	// 3. Two people loaded the form before either took A. The first is saved;
	// the second is told and keeps the name they typed.
	first, second := sh.blank(t), sh.blank(t)

	if code, body := sh.take(t, first, "Ben", "a"); code != http.StatusOK {
		t.Fatalf("Ben taking A = %d:\n%s", code, short(body))
	}

	code, body := sh.take(t, second, "Cara", "a")
	if code != http.StatusConflict {
		t.Fatalf("Cara taking A after Ben = %d, want 409:\n%s", code, short(body))
	}

	for _, want := range []string{"That one was just taken", `value="Cara"`, `value="a" disabled>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not contain %q:\n%s", want, short(body))
		}
	}

	// Cara picks C from the page she was given back.
	if code, body := sh.take(t, body, "Cara", "c"); code != http.StatusOK {
		t.Fatalf("Cara taking C = %d:\n%s", code, short(body))
	}

	// 4. Every slot is taken, so the form shows its closing note.
	page = sh.blank(t)
	if !strings.Contains(page, "Every visit is taken. Thank you!") || strings.Contains(page, embedapp.GrantField) {
		t.Errorf("a form with every slot taken is still offered:\n%s", short(page))
	}

	// Hiding one gives its place back.
	subs, err := sh.subs.ByForm(t.Context(), sh.form.ID)
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}

	ben := subs[indexOfName(subs, "Ben")]
	if _, _, err := sh.subs.Hide(t.Context(), time.Now(), ben.ID, types.NewID()); err != nil {
		t.Fatalf("Hide: %v", err)
	}

	page = sh.blank(t)
	if strings.Contains(page, `value="a" disabled`) || !strings.Contains(page, `value="a"`) {
		t.Errorf("hiding Ben's sign-up did not free A:\n%s", short(page))
	}
}

func TestYourOwnSlotIsStillYoursWhenYouComeBack(t *testing.T) {
	sh := slotsSurface(t)

	code, body := sh.take(t, sh.blank(t), "Ana", "b")
	if code != http.StatusOK {
		t.Fatalf("taking B = %d:\n%s", code, short(body))
	}

	page := getPage(t, sh.h, changeLinkIn(t, body)).Body.String()

	if strings.Contains(page, `value="b" disabled`) || !strings.Contains(page, `value="b" checked`) {
		t.Errorf("Ana's own slot is not hers to keep on her change page:\n%s", short(page))
	}
}

// indexOfName is the position of the submission whose name answer is name.
func indexOfName(subs []submissionbus.Submission, name string) int {
	for i, s := range subs {
		if a, ok := s.Answers.Field("name"); ok && a.Value() == name {
			return i
		}
	}

	return -1
}

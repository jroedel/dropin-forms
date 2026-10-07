package notifybus_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/notify/notifybus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// The link back to one's own answers, in the messages that carry it.

// ordination is a free form that takes changes until a date after when.
func ordination(t *testing.T) formbus.Form {
	t.Helper()

	return formbus.Form{
		ID:              mustSlug(t, "ordination-2027"),
		Title:           "Ordination of Dcn. Hector Islas",
		Currency:        "usd",
		ChangeableUntil: when.Add(120 * 24 * time.Hour),
		Fields: []formbus.Field{
			{Name: "name", Label: "Your name", Kind: formbus.KindText},
			{Name: "email", Label: "Email", Kind: formbus.KindEmail},
			{Name: "plans", Label: "Plans", Kind: formbus.KindRadio},
			{Name: "flight", Label: "Arrival flight", Kind: formbus.KindText},
		},
	}
}

func answer(t *testing.T, f formbus.Form, email string, pairs ...string) submissionbus.Submission {
	t.Helper()

	sub := submissionbus.Submission{
		ID:        types.NewID(),
		Form:      f.ID,
		Status:    submissionbus.StatusReceived,
		Email:     mustEmail(t, email),
		CreatedAt: when,
		UpdatedAt: when,
		Answers:   formbus.Answers{FormID: f.ID, Currency: "usd"},
	}

	for i := 0; i+1 < len(pairs); i += 2 {
		fld, _ := f.Field(pairs[i])
		sub.Answers.Fields = append(sub.Answers.Fields, formbus.Answer{
			Name: fld.Name, Label: fld.Label, Kind: fld.Kind, Values: []string{pairs[i+1]},
		})
	}

	return sub
}

func linked(f formbus.Form, sub submissionbus.Submission) notifybus.Config {
	return notifybus.Config{
		Forms:        forms{form: f},
		Submissions:  submissions{sub: sub, yours: []submissionbus.Submission{sub}},
		Office:       "office@schoenstatt.test",
		AnswerKey:    testAnswerKey,
		EmbedBaseURL: "https://f.example.test",
		Now:          func() time.Time { return when },
	}
}

var testAnswerKey = func() submissionbus.AnswerKey {
	k, err := submissionbus.ParseAnswerKey("a-test-answer-signing-key-long-enough")
	if err != nil {
		panic(err)
	}

	return k
}()

func TestTheFirstMessageCarriesTheLinkOnAFormThatTakesChanges(t *testing.T) {
	f := ordination(t)
	sub := answer(t, f, "hector@example.org", "name", "Fr. Hector", "email", "hector@example.org", "plans", "considering")

	h := newHarness(t, linked(f, sub))
	h.b.Received(t.Context(), sub.ID)

	mine := h.messageTo(t, "hector@example.org").Text
	if !strings.Contains(mine, "https://f.example.test/f/ordination-2027/mine?t=") {
		t.Errorf("the submitter's message carries no link:\n%s", mine)
	}
	if !strings.Contains(mine, "Keep it to yourself") {
		t.Errorf("the message does not say the link is theirs:\n%s", mine)
	}

	if office := h.messageTo(t, "office@schoenstatt.test").Text; strings.Contains(office, "/mine?t=") {
		t.Errorf("the office was sent the submitter's link:\n%s", office)
	}
}

func TestNoLinkOnAFormThatDoesNotTakeChanges(t *testing.T) {
	cases := map[string]func(*notifybus.Config, *formbus.Form){
		"no date on the form": func(_ *notifybus.Config, f *formbus.Form) { f.ChangeableUntil = time.Time{} },
		"the date has passed": func(c *notifybus.Config, f *formbus.Form) {
			c.Now = func() time.Time { return f.ChangeableUntil }
		},
		"no key":            func(c *notifybus.Config, _ *formbus.Form) { c.AnswerKey = submissionbus.AnswerKey{} },
		"no public address": func(c *notifybus.Config, _ *formbus.Form) { c.EmbedBaseURL = "" },
	}

	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := ordination(t)
			sub := answer(t, f, "hector@example.org", "name", "Fr. Hector")

			cfg := linked(f, sub)
			change(&cfg, &f)
			cfg.Forms = forms{form: f}

			h := newHarness(t, cfg)
			h.b.Received(t.Context(), sub.ID)

			mine := h.messageTo(t, "hector@example.org").Text
			if strings.Contains(mine, "/mine?t=") {
				t.Errorf("a link was sent:\n%s", mine)
			}
			if !strings.Contains(mine, "reply to this message") {
				t.Errorf("the message does not say to reply instead:\n%s", mine)
			}
		})
	}
}

func TestTheOfficeIsToldWhatChanged(t *testing.T) {
	f := ordination(t)
	before := answer(t, f, "hector@example.org",
		"name", "Fr. Hector", "email", "hector@example.org", "plans", "considering")

	after := answer(t, f, "hector@example.org",
		"name", "Fr. Hector", "email", "hector@example.org", "plans", "booked", "flight", "UA 1234")
	after.ID = before.ID
	after.UpdatedAt = when.Add(48 * time.Hour)

	h := newHarness(t, linked(f, after))
	h.b.Changed(t.Context(), before, after)

	if got := h.to(); !slices.Equal(got, []string{"hector@example.org", "office@schoenstatt.test"}) {
		t.Fatalf("sent to %v, want the person and the office", got)
	}

	office := h.messageTo(t, "office@schoenstatt.test")
	if !strings.Contains(office.Subject, "Changed") || !strings.Contains(office.Subject, "Fr. Hector") {
		t.Errorf("office subject = %q", office.Subject)
	}

	for _, want := range []string{
		"  Plans: booked (was: considering)\n",
		"  Arrival flight: UA 1234 (was: not answered)\n",
	} {
		if !strings.Contains(office.Text, want) {
			t.Errorf("the office's message does not say %q:\n%s", want, office.Text)
		}
	}

	if strings.Contains(office.Text, "Your name: Fr. Hector (was") {
		t.Errorf("an unchanged answer is listed as changed:\n%s", office.Text)
	}

	mine := h.messageTo(t, "hector@example.org")
	if !strings.Contains(mine.Text, "UA 1234") || !strings.Contains(mine.Text, "/mine?t=") {
		t.Errorf("the person's copy lacks the new answers or the link:\n%s", mine.Text)
	}
}

func TestTheOldAddressIsToldWhenTheAddressChanges(t *testing.T) {
	f := ordination(t)
	before := answer(t, f, "hector@example.org", "email", "hector@example.org")
	after := answer(t, f, "secretary@example.org", "email", "secretary@example.org")
	after.ID = before.ID

	h := newHarness(t, linked(f, after))
	h.b.Changed(t.Context(), before, after)

	old := h.messageTo(t, "hector@example.org")
	if !strings.Contains(old.Text, "secretary@example.org") || !strings.Contains(old.Text, "/mine?t=") {
		t.Errorf("the old address was not told where mail now goes, with the way back:\n%s", old.Text)
	}

	h.messageTo(t, "secretary@example.org")
}

func TestLinksAreSentOnlyToAnAddressThatHasAnswered(t *testing.T) {
	f := ordination(t)
	sub := answer(t, f, "hector@example.org", "name", "Fr. Hector")

	h := newHarness(t, linked(f, sub))
	h.b.SendLinks(t.Context(), f.ID, sub.Email)

	m := h.messageTo(t, "hector@example.org")
	if !strings.Contains(m.Text, "/mine?t=") {
		t.Errorf("no link in the message:\n%s", m.Text)
	}

	cfg := linked(f, sub)
	cfg.Submissions = submissions{}

	nobody := newHarness(t, cfg)
	nobody.b.SendLinks(t.Context(), f.ID, mustEmail(t, "stranger@example.org"))

	if len(nobody.sent.Sent) != 0 {
		t.Errorf("mail was sent to an address that never answered: %v", nobody.to())
	}
}

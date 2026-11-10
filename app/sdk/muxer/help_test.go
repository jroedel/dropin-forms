package muxer_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The smaller text under a question: written when the question is added, and
// drawn on the public form with the line breaks it was written with.

func TestAddingAQuestionSaysTheRestIsOnItsOwnPage(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	if page := a.get(t, "/forms/supper-2026/edit", cookie).Body.String(); !strings.Contains(page, "its own page\n  opens with the rest: smaller text under it") {
		t.Fatalf("Add a question does not say where the rest is:\n%s", page)
	}

	w := a.post(t, "/forms/supper-2026/edit/fields", url.Values{
		"label": {"Your name"},
		"kind":  {"text"},
	}, cookie)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/forms/supper-2026/edit/fields/text_1" {
		t.Fatalf("adding a question = %d to %q, want its own page", w.Code, w.Header().Get("Location"))
	}

	if page := a.get(t, "/forms/supper-2026/edit/fields/text_1", cookie).Body.String(); !strings.Contains(page, "Smaller text under it") {
		t.Errorf("the question's own page has no box for the text under it:\n%s", page)
	}
}

func TestTextUnderAQuestionKeepsItsLineBreaksOnTheForm(t *testing.T) {
	f := quietDay(t, false)
	f.Intro = "A day of silence before Christmas.\n\nLunch is provided."
	f.Fields[0].Help = "Early registration: $35\nDay of the retreat: $40\n\n<b>Pay at the door</b> if you prefer."
	f.Stamp()

	qh := quietSurfaceFor(t, f)

	body := getPage(t, qh.embed, quietDayPath).Body.String()

	for _, want := range []string{
		`<p class="help">Early registration: $35<br>Day of the retreat: $40</p>`,
		`<p class="help">&lt;b&gt;Pay at the door&lt;/b&gt; if you prefer.</p>`,
		`<p class="intro">A day of silence before Christmas.</p><p class="intro">Lunch is provided.</p>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the form does not contain %s:\n%s", want, short(body))
		}
	}
}

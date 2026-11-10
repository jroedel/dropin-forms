package muxer_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The smaller text under a question: written when the question is added, and
// drawn on the public form with the line breaks it was written with.

func TestTextUnderAQuestionCanBeWrittenWhenItIsAdded(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "supper-2026", "Parish supper")

	if page := a.get(t, "/forms/supper-2026/edit", cookie).Body.String(); !strings.Contains(page, `name="help"`) {
		t.Fatalf("Add a question has no box for the text under it:\n%s", page)
	}

	w := a.post(t, "/forms/supper-2026/edit/fields", url.Values{
		"label": {"Your name"},
		"kind":  {"text"},
		"help":  {"As it should be on your name badge.\r\nFirst names are fine."},
	}, cookie)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("adding a question with text under it = %d:\n%s", w.Code, w.Body)
	}

	s, err := a.catalogue.Draft(t.Context(), mustSlug(t, "supper-2026"))
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}

	if len(s.Form.Fields) != 1 || s.Form.Fields[0].Help != "As it should be on your name badge.\r\nFirst names are fine." {
		t.Errorf("stored %+v, want the text under the question kept as written", s.Form.Fields)
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

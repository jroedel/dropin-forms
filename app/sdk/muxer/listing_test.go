package muxer_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formtoml"
	"github.com/jroedel/dropin-forms/business/types"
)

// The list of earlier answers beneath a form, through the mounted surface.
//
// Against a definition of its own rather than the feast form, because the
// feast form has no list and should not grow one to make a test possible.

// oneForm serves a single definition, which is the whole of what the embed
// surface asks of its catalogue.
type oneForm struct{ f formbus.Form }

func (o oneForm) ByID(slug types.Slug) (formbus.Form, error) {
	if slug != o.f.ID {
		return formbus.Form{}, formtoml.ErrNotFound
	}

	return o.f, nil
}

const potluckPath = "/f/potluck"

func potluck(t *testing.T) formbus.Form {
	t.Helper()

	f := formbus.Form{
		ID:       mustSlug(t, "potluck"),
		Title:    "Parish potluck",
		Currency: "usd",
		Fields: []formbus.Field{
			{Name: "name", Label: "Your name", Kind: formbus.KindText, Required: true, MaxLen: 100},
			{Name: "email", Label: "Email", Kind: formbus.KindEmail},
			{Name: "dish", Label: "What you are bringing", Kind: formbus.KindSelect, Options: []formbus.Option{
				{Value: "main", Label: "A main dish"},
				{Value: "dessert", Label: "Dessert"},
			}},
		},
		Listing: formbus.Listing{Heading: "Who is bringing what", Line: "{name} -- {dish}"},
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("the potluck form does not pass Check: %v", err)
	}

	return f
}

func potluckSurface(t *testing.T) http.Handler {
	t.Helper()

	return potluckSurfaceWith(t, func(*formbus.Listing) {})
}

// potluckSurfaceWith is the same surface with the list's settings changed
// first.
func potluckSurfaceWith(t *testing.T, change func(*formbus.Listing)) http.Handler {
	t.Helper()

	f := potluck(t)
	change(&f.Listing)
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("the changed potluck form does not pass Check: %v", err)
	}

	// A clock that moves, a second per reading, because the list is newest
	// first and two sign-ups stamped with the same instant have no order.
	var ticks time.Duration

	cfg := newConfig(t, nil, nil)
	cfg.Embed.Now = func() time.Time {
		ticks += time.Second

		return beforeTheFeast.Add(ticks)
	}
	cfg.Embed.Forms = oneForm{f}

	return embedOf(t, cfg)
}

func signUp(t *testing.T, h http.Handler, name, email, dish string) {
	t.Helper()

	grant := grantIn(t, getPage(t, h, potluckPath).Body.String())

	w := postTo(t, h, potluckPath, url.Values{
		embedapp.GrantField: {grant},
		"name":              {name},
		"email":             {email},
		"dish":              {dish},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d:\n%s", w.Code, short(w.Body.String()))
	}
}

func TestAnEmptyListSaysSoAndEveryShownFieldIsMarked(t *testing.T) {
	h := potluckSurface(t)

	body := getPage(t, h, potluckPath).Body.String()

	for _, want := range []string{"Who is bringing what", "Nobody yet."} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not contain %q:\n%s", want, short(body))
		}
	}

	// Name and dish are shown; the email is not, and must not be marked as
	// though it were.
	if n := strings.Count(body, `class="help public-note"`); n != 2 {
		t.Errorf("%d fields are marked as shown publicly, want 2 (name and dish)", n)
	}
}

func TestTheListShowsEarlierAnswersNewestFirst(t *testing.T) {
	h := potluckSurface(t)

	signUp(t, h, "Maria G.", "maria@example.org", "dessert")
	signUp(t, h, "Tom R.", "tom@example.org", "main")

	body := getPage(t, h, potluckPath).Body.String()

	tom := strings.Index(body, "<li>Tom R. -- A main dish</li>")
	maria := strings.Index(body, "<li>Maria G. -- Dessert</li>")

	switch {
	case tom < 0 || maria < 0:
		t.Fatalf("the list does not show both sign-ups by their option labels:\n%s", body)
	case tom > maria:
		t.Error("the older sign-up is listed first; want newest first")
	}

	if strings.Contains(body, "Nobody yet.") {
		t.Error("the list says nobody yet, with two sign-ups in it")
	}

	// The line does not name the email, so it is nowhere on a page anybody can
	// open.
	if strings.Contains(body, "maria@example.org") {
		t.Error("an address the list does not name is on the public page")
	}
}

// What somebody typed is published to strangers, so it is exactly the input
// that must reach the page as text and never as markup.
func TestTheListEscapesWhatWasTyped(t *testing.T) {
	h := potluckSurface(t)

	signUp(t, h, `<script>alert(1)</script>`, "", "main")

	body := getPage(t, h, potluckPath).Body.String()

	if strings.Contains(body, "<script>alert(1)") {
		t.Fatalf("a typed tag reached the page as markup:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt; -- A main dish") {
		t.Errorf("the typed name is not in the list as text:\n%s", body)
	}
}

// Oldest at the top, and past the limit the newest are still the ones kept:
// the person who has just signed up is at the bottom of the list rather than
// missing from it.
func TestAnOldestFirstListKeepsTheNewestAndTurnsThemOver(t *testing.T) {
	h := potluckSurfaceWith(t, func(l *formbus.Listing) {
		l.OldestFirst = true
		l.Limit = 2
	})

	signUp(t, h, "Ann K.", "", "main")
	signUp(t, h, "Maria G.", "", "dessert")
	signUp(t, h, "Tom R.", "", "main")

	body := getPage(t, h, potluckPath).Body.String()

	maria := strings.Index(body, "<li>Maria G. -- Dessert</li>")
	tom := strings.Index(body, "<li>Tom R. -- A main dish</li>")

	switch {
	case maria < 0 || tom < 0:
		t.Fatalf("the list does not show the two newest sign-ups:\n%s", body)
	case maria > tom:
		t.Error("the newer sign-up is listed first; want oldest at the top")
	}

	if strings.Contains(body, "Ann K.") {
		t.Error("the oldest sign-up is shown past a limit of two; want the newest two kept")
	}
}

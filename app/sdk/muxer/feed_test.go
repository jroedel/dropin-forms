package muxer_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
)

// The spreadsheet feed, through the admin surface as it is mounted.

var feedKeyPattern = regexp.MustCompile(`dfk_[A-Za-z0-9_-]+`)

// feedOffice is the retreat surface's admin half, with an administrator of the
// form signed in.
func feedOffice(t *testing.T, rh retreatHarness, role accessbus.Role) (office, string) {
	t.Helper()

	o := office{admin: adminOf(t, rh.cfg), embed: rh.h, cfg: rh.cfg}
	who, cookie := o.reader(t, "office@schoenstatt.test")
	o.grant(t, who, "retreat", role)

	return o, cookie
}

func (o office) postValues(t *testing.T, target, cookie string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: mid.CookieName, Value: cookie})

	w := httptest.NewRecorder()
	o.admin.ServeHTTP(w, r)

	return w
}

// pullFeed is what a sheet's script does: a GET with a bearer key and no
// cookie, no Origin and no Sec-Fetch-Site.
func pullFeed(t *testing.T, h http.Handler, form, key string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, "/forms/"+form+"/feed.json", nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	return w
}

type feedReply struct {
	Form   string `json:"form"`
	Fields []struct {
		Name     string `json:"name"`
		Label    string `json:"label"`
		Multiple bool   `json:"multiple"`
	} `json:"fields"`
	Responses []struct {
		ID      string         `json:"id"`
		Changes int            `json:"changes"`
		Email   string         `json:"email"`
		Answers map[string]any `json:"answers"`
	} `json:"responses"`
}

func TestASheetReadsTheFormWithAKeyAndStopsWhenItIsRevoked(t *testing.T) {
	rh := retreatSurface(t)

	link := rh.answer(t, "considering")
	*rh.now = rh.now.Add(time.Hour)
	if code, body := rh.save(t, getPage(t, rh.h, link).Body.String(), url.Values{
		"name": {"Fr. Hector"}, "email": {"hector@example.org"}, "plans": {"booked"},
	}); code != http.StatusOK {
		t.Fatalf("save = %d:\n%s", code, short(body))
	}

	o, cookie := feedOffice(t, rh, accessbus.RoleAdmin)

	if list := o.read(t, "/forms/retreat/submissions", cookie).Body.String(); !strings.Contains(list, `href="/forms/retreat/feed"`) {
		t.Errorf("the list does not link an administrator to the feed:\n%s", short(list))
	}

	w := o.postValues(t, "/forms/retreat/feed", cookie, url.Values{"label": {"Office sheet"}})
	if w.Code != http.StatusOK {
		t.Fatalf("making a key = %d:\n%s", w.Code, short(w.Body.String()))
	}

	page := w.Body.String()
	key := feedKeyPattern.FindString(page)
	if key == "" {
		t.Fatalf("no key on the page:\n%s", short(page))
	}
	if !strings.Contains(page, "https://forms.test/forms/retreat/feed.json") {
		t.Errorf("the script on the page does not carry the feed's address")
	}

	// And it is shown once: the page read again does not have it.
	if again := o.read(t, "/forms/retreat/feed", cookie).Body.String(); strings.Contains(again, key) {
		t.Error("the key is shown again on a later visit")
	}

	got := pullFeed(t, o.admin, "retreat", key)
	if got.Code != http.StatusOK || !strings.HasPrefix(got.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("feed = %d %s:\n%s", got.Code, got.Header().Get("Content-Type"), short(got.Body.String()))
	}

	var feed feedReply
	if err := json.Unmarshal(got.Body.Bytes(), &feed); err != nil {
		t.Fatalf("the feed is not JSON: %v", err)
	}

	if feed.Form != "retreat" || len(feed.Fields) != 3 || len(feed.Responses) != 1 {
		t.Fatalf("feed = %+v", feed)
	}

	r := feed.Responses[0]
	if r.Answers["plans"] != "booked" || r.Changes != 1 || r.Email != "hector@example.org" {
		t.Errorf("response = %+v; want the current answers, changed once", r)
	}

	// Revoked from the same page.
	keys := regexp.MustCompile(`action="(/forms/retreat/feed/[^/]+/revoke)"`).FindStringSubmatch(o.read(t, "/forms/retreat/feed", cookie).Body.String())
	if keys == nil {
		t.Fatal("no revoke button")
	}

	if w := o.postValues(t, keys[1], cookie, nil); w.Code != http.StatusSeeOther {
		t.Fatalf("revoke = %d", w.Code)
	}

	if w := pullFeed(t, o.admin, "retreat", key); w.Code != http.StatusUnauthorized {
		t.Errorf("feed after revoking = %d, want 401", w.Code)
	}
}

func TestTheFeedRefusesWithoutTheRightKey(t *testing.T) {
	rh := retreatSurface(t)
	rh.answer(t, "considering")

	o, cookie := feedOffice(t, rh, accessbus.RoleAdmin)
	key := feedKeyPattern.FindString(o.postValues(t, "/forms/retreat/feed", cookie, url.Values{}).Body.String())

	cases := map[string]struct {
		form, key string
	}{
		"no key":                 {"retreat", ""},
		"a key that is not ours": {"retreat", "dfk_" + strings.Repeat("A", 43)},
		"another form":           {theForm, key},
		"no such form":           {"no-such-form", key},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := pullFeed(t, o.admin, tc.form, tc.key)
			if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
				t.Errorf("feed = %d, want 401 with a challenge:\n%s", w.Code, short(w.Body.String()))
			}
			if strings.Contains(w.Body.String(), "hector") {
				t.Error("a refusal carried an answer")
			}
		})
	}

	// A session is not a key: the cookie of an administrator of the form
	// reads nothing here.
	r := httptest.NewRequest(http.MethodGet, "/forms/retreat/feed.json", nil)
	r.AddCookie(&http.Cookie{Name: mid.CookieName, Value: cookie})

	w := httptest.NewRecorder()
	o.admin.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("feed with a session and no key = %d, want 401", w.Code)
	}
}

func TestOnlyAnAdministratorMakesAKey(t *testing.T) {
	rh := retreatSurface(t)

	o, cookie := feedOffice(t, rh, accessbus.RoleResults)

	if w := o.read(t, "/forms/retreat/feed", cookie); w.Code != http.StatusForbidden {
		t.Errorf("the feed page for a results reader = %d, want 403", w.Code)
	}

	if w := o.postValues(t, "/forms/retreat/feed", cookie, url.Values{}); w.Code != http.StatusForbidden {
		t.Errorf("making a key as a results reader = %d, want 403", w.Code)
	}

	if list := o.read(t, "/forms/retreat/submissions", cookie).Body.String(); strings.Contains(list, `/feed"`) {
		t.Error("a results reader is offered the feed")
	}
}

func TestAHiddenSubmissionIsNotInTheFeed(t *testing.T) {
	rh := retreatSurface(t)
	rh.answer(t, "considering")

	if _, _, err := rh.subs.Hide(t.Context(), *rh.now, rh.only(t).ID, rh.only(t).ID); err != nil {
		t.Fatalf("Hide: %v", err)
	}

	o, cookie := feedOffice(t, rh, accessbus.RoleAdmin)
	key := feedKeyPattern.FindString(o.postValues(t, "/forms/retreat/feed", cookie, url.Values{}).Body.String())

	var feed feedReply
	if err := json.Unmarshal(pullFeed(t, o.admin, "retreat", key).Body.Bytes(), &feed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(feed.Responses) != 0 {
		t.Errorf("%d responses in the feed, want the hidden one left out", len(feed.Responses))
	}
}

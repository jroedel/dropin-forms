package muxer_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// The JSON API, through the admin surface as it is mounted: a key is its
// account, reaches exactly what that account reaches, and is never a cookie.

var apiKeyPattern = regexp.MustCompile(`dfa_[A-Za-z0-9_-]+`)

// keyFor makes a key for an account straight through the domain, because
// signing in for one is throttled and most of these tests are about what a
// key reaches rather than how it was made. The page that makes one has a test
// of its own below.
func keyFor(t *testing.T, a harness, u userbus.User) string {
	t.Helper()

	_, secret, err := a.keys.Create(t.Context(), time.Now(), u.ID, "test")
	if err != nil {
		t.Fatalf("keys.Create: %v", err)
	}

	return secret
}

// call is what a program does: JSON in, a bearer key, and no cookie, no
// Origin and no Sec-Fetch-Site.
func call(t *testing.T, a harness, method, target, key string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var in io.Reader

	if body != nil {
		raw, ok := body.(string)
		if !ok {
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			raw = string(b)
		}

		in = strings.NewReader(raw)
	}

	r := httptest.NewRequest(method, target, in)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}

	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)

	return w
}

// answer decodes a JSON reply, failing the test if it is not JSON at all --
// which is itself the assertion that every answer under /api/ is.
func answer(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("answered %d with %q, not JSON:\n%s", w.Code, ct, short(w.Body.String()))
	}

	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("answered %d with something that is not a JSON object: %v\n%s", w.Code, err, short(w.Body.String()))
	}

	return out
}

// formReply is the parts of one form's answer these tests read.
type formReply struct {
	Slug      string   `json:"slug"`
	Live      bool     `json:"live"`
	Editable  bool     `json:"editable"`
	Problems  []string `json:"problems"`
	Retired   []string `json:"retired"`
	PublicURL string   `json:"public_url"`
	EmbedHTML string   `json:"embed_html"`

	Definition struct {
		Title  string `json:"title"`
		Fields []struct {
			Name  string `json:"name"`
			Label string `json:"label"`
			Kind  string `json:"kind"`
		} `json:"fields"`
		Items []struct {
			ID    string `json:"id"`
			Price int64  `json:"price"`
		} `json:"items"`
	} `json:"definition"`
}

func formOf(t *testing.T, w *httptest.ResponseRecorder) formReply {
	t.Helper()

	answer(t, w)

	var f formReply
	if err := json.Unmarshal(w.Body.Bytes(), &f); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	return f
}

// retreat is a definition a program might send: nothing named, one thing for
// sale, and enough to publish.
func retreat() map[string]any {
	return map[string]any{
		"slug":         "advent-retreat-2026",
		"title":        "Advent retreat",
		"intro":        "A morning of recollection.",
		"return_url":   "https://www.example.org/retreat",
		"origins":      []string{"https://www.example.org"},
		"confirmation": "Thank you. We will see you there.",
		"fields": []map[string]any{
			{"label": "Your name", "kind": "text", "required": true, "max_length": 100},
			{"label": "Email", "kind": "email", "required": true},
			{"label": "Lunch", "kind": "radio", "required": true, "options": []map[string]any{
				{"value": "yes", "label": "Yes, please"}, {"value": "no"},
			}},
		},
		"items": []map[string]any{
			{"label": "Retreat fee", "price": 2500, "max": 4},
		},
		"min_per_order": 1,
	}
}

func TestAKeyIsMadeOnTheAccountPageShownOnceAndRevokedThere(t *testing.T) {
	a := newAdmin(t, "")
	me := formAdmin(t, a, "office@schoenstatt.test")
	cookie := sessionFor(t, a, me)

	if page := a.get(t, "/account", cookie).Body.String(); !strings.Contains(page, `href="/account/keys"`) {
		t.Errorf("the account page does not link to the key page:\n%s", short(page))
	}

	w := a.post(t, "/account/keys", url.Values{"label": {"Claude"}}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("making a key = %d:\n%s", w.Code, short(w.Body.String()))
	}

	key := apiKeyPattern.FindString(w.Body.String())
	if key == "" {
		t.Fatalf("no key on the page:\n%s", short(w.Body.String()))
	}

	if again := a.get(t, "/account/keys", cookie).Body.String(); strings.Contains(again, key) {
		t.Error("the key is shown again on a later visit")
	}

	got := answer(t, call(t, a, http.MethodGet, "/api/v1/me", key, nil))
	if got["email"] != "office@schoenstatt.test" {
		t.Errorf("/me = %v, want the account that made the key", got)
	}

	revoke := regexp.MustCompile(`action="(/account/keys/[^/]+/revoke)"`).FindStringSubmatch(a.get(t, "/account/keys", cookie).Body.String())
	if revoke == nil {
		t.Fatal("no revoke button")
	}

	if w := a.post(t, revoke[1], nil, cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("revoke = %d", w.Code)
	}

	if w := call(t, a, http.MethodGet, "/api/v1/me", key, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("a revoked key = %d, want 401", w.Code)
	}
}

// Making a key needs nothing but an account. The request was that any user
// may make their own, and a key with no grants behind it reaches nothing.
func TestAnyAccountMayMakeAKeyAndItReachesOnlyWhatTheAccountDoes(t *testing.T) {
	a := newAdmin(t, "")
	siteBoss(t, a, "site@schoenstatt.test")

	nobody, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{Email: mustEmail(t, "nobody@schoenstatt.test"), Name: "Nobody"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cookie := sessionFor(t, a, nobody)

	key := apiKeyPattern.FindString(a.post(t, "/account/keys", url.Values{}, cookie).Body.String())
	if key == "" {
		t.Fatal("an account with no grants could not make a key")
	}

	list := answer(t, call(t, a, http.MethodGet, "/api/v1/forms", key, nil))
	if forms, _ := list["forms"].([]any); len(forms) != 0 {
		t.Errorf("forms = %v, want none", forms)
	}

	for _, tc := range []struct{ method, target string }{
		{http.MethodPost, "/api/v1/forms"},
		{http.MethodGet, "/api/v1/forms/" + theForm},
		{http.MethodGet, "/api/v1/forms/" + theForm + "/submissions"},
	} {
		var body any
		if tc.method == http.MethodPost {
			body = retreat()
		}

		w := call(t, a, tc.method, tc.target, key, body)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", tc.method, tc.target, w.Code)
		}

		if msg, _ := answer(t, w)["error"].(string); !strings.Contains(msg, "nobody@schoenstatt.test") {
			t.Errorf("%s %s refused with %q, want it to name whose key it is", tc.method, tc.target, msg)
		}
	}
}

func TestTheAPIRefusesWithoutAWorkingKey(t *testing.T) {
	a := newAdmin(t, "")
	boss := siteBoss(t, a, "site@schoenstatt.test")
	key := keyFor(t, a, boss)

	cases := map[string]string{
		"no key":                    "",
		"a key that is not ours":    "dfa_" + strings.Repeat("A", 43),
		"a spreadsheet's feed key":  "dfk_" + strings.Repeat("A", 43),
		"the key with a typo in it": key[:len(key)-1] + "x",
	}

	for name, presented := range cases {
		t.Run(name, func(t *testing.T) {
			w := call(t, a, http.MethodGet, "/api/v1/forms", presented, nil)
			if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
				t.Errorf("= %d, want 401 with a challenge", w.Code)
			}
			answer(t, w)
		})
	}
}

// The property the API's whole CSRF story rests on: a session is not a key.
// A page on another site can make a signed-in person's browser send a POST
// with their cookie on it, and that must not make a form.
func TestASessionCookieIsNotAKey(t *testing.T) {
	a := newAdmin(t, "")
	boss := siteBoss(t, a, "site@schoenstatt.test")
	cookie := sessionFor(t, a, boss)

	body, _ := json.Marshal(retreat())

	for _, ct := range []string{"application/json", "text/plain"} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/forms", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", ct)
		r.Header.Set("Origin", "https://evil.example")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.AddCookie(&http.Cookie{Name: mid.CookieName, Value: cookie})

		w := httptest.NewRecorder()
		a.h.ServeHTTP(w, r)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("a cross-site %s POST with a session = %d, want 401", ct, w.Code)
		}
	}

	if _, err := a.catalogue.Draft(t.Context(), mustSlug(t, "advent-retreat-2026")); err == nil {
		t.Error("a form was made by a request that carried only a cookie")
	}
}

// The whole life of a form, the way Claude would drive it.
func TestACreatorBuildsPublishesAndRevisesAFormThroughTheAPI(t *testing.T) {
	a := newAdmin(t, "")
	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	key := keyFor(t, a, volunteer)

	w := call(t, a, http.MethodPost, "/api/v1/forms", key, retreat())
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d:\n%s", w.Code, short(w.Body.String()))
	}
	if loc := w.Header().Get("Location"); loc != "/api/v1/forms/advent-retreat-2026" {
		t.Errorf("Location = %q", loc)
	}

	f := formOf(t, w)
	if f.Live || !f.Editable || len(f.Problems) != 0 {
		t.Errorf("made = live %v, editable %v, problems %v; want an unpublished draft that could go live", f.Live, f.Editable, f.Problems)
	}

	var names []string
	for _, fld := range f.Definition.Fields {
		names = append(names, fld.Name)
	}
	if want := "text_1 email_1 radio_1"; strings.Join(names, " ") != want {
		t.Errorf("names = %v, want the builder's: %s", names, want)
	}
	if len(f.Definition.Items) != 1 || f.Definition.Items[0].ID != "item_1" || f.Definition.Items[0].Price != 2500 {
		t.Errorf("items = %+v", f.Definition.Items)
	}

	// Its creator administers it, which is what lets the key reach it next.
	if role := roleOnForm(t, a, volunteer, "advent-retreat-2026"); role != accessbus.RoleAdmin {
		t.Errorf("creator's role = %q, want admin", role)
	}

	// Not served until it is published.
	if _, err := a.catalogue.ByID(mustSlug(t, "advent-retreat-2026")); err == nil {
		t.Error("a draft made through the API is being served")
	}

	w = call(t, a, http.MethodPost, "/api/v1/forms/advent-retreat-2026/publish", key, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("publish = %d:\n%s", w.Code, short(w.Body.String()))
	}

	f = formOf(t, w)
	if !f.Live || f.PublicURL != "https://f.forms.test/f/advent-retreat-2026" || !strings.Contains(f.EmbedHTML, `data-dropin-form="advent-retreat-2026"`) {
		t.Errorf("published = live %v, url %q, embed %q", f.Live, f.PublicURL, f.EmbedHTML)
	}

	if _, err := a.catalogue.ByID(mustSlug(t, "advent-retreat-2026")); err != nil {
		t.Errorf("published and not served: %v", err)
	}

	// Read, change, send back: the definition in the answer is a body PUT
	// takes as it stands. Drop the email question and add a new one.
	read := answer(t, call(t, a, http.MethodGet, "/api/v1/forms/advent-retreat-2026", key, nil))
	def := read["definition"].(map[string]any)

	fields := def["fields"].([]any)
	def["fields"] = []any{
		fields[0],
		fields[2],
		map[string]any{"label": "Anything we should know?", "kind": "paragraph"},
	}

	w = call(t, a, http.MethodPut, "/api/v1/forms/advent-retreat-2026", key, def)
	if w.Code != http.StatusOK {
		t.Fatalf("replace = %d:\n%s", w.Code, short(w.Body.String()))
	}

	f = formOf(t, w)
	if !f.Live {
		t.Error("replacing the definition took the form down")
	}
	if !slices.Contains(f.Retired, "email_1") {
		t.Errorf("retired = %v, want the dropped question's name in it", f.Retired)
	}
	if last := f.Definition.Fields[len(f.Definition.Fields)-1]; last.Name != "paragraph_1" {
		t.Errorf("the new question is called %q, want paragraph_1", last.Name)
	}

	// A change that would break a live form is refused with every reason, and
	// the form goes on serving what it served before.
	def["items"] = []any{}

	w = call(t, a, http.MethodPut, "/api/v1/forms/advent-retreat-2026", key, def)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("breaking replace = %d, want 422:\n%s", w.Code, short(w.Body.String()))
	}
	if problems, _ := answer(t, w)["problems"].([]any); len(problems) == 0 {
		t.Error("refused without saying why")
	}

	served, err := a.catalogue.ByID(mustSlug(t, "advent-retreat-2026"))
	if err != nil || len(served.Items) != 1 {
		t.Errorf("after a refused change the served form has %d items, err %v; want it untouched", len(served.Items), err)
	}

	// Taken down, never refused; and once it has been live, not deleted.
	if w := call(t, a, http.MethodPost, "/api/v1/forms/advent-retreat-2026/unpublish", key, nil); w.Code != http.StatusOK || formOf(t, w).Live {
		t.Errorf("unpublish = %d", w.Code)
	}

	if w := call(t, a, http.MethodDelete, "/api/v1/forms/advent-retreat-2026", key, nil); w.Code != http.StatusConflict {
		t.Errorf("deleting a form that has been live = %d, want 409", w.Code)
	}
}

func TestADraftMayBeHalfFinishedAndSaysWhatIsMissing(t *testing.T) {
	a := newAdmin(t, "")
	boss := siteBoss(t, a, "site@schoenstatt.test")
	key := keyFor(t, a, boss)

	w := call(t, a, http.MethodPost, "/api/v1/forms", key, map[string]any{"slug": "bake-sale-2026", "title": "Bake sale"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d:\n%s", w.Code, short(w.Body.String()))
	}

	if f := formOf(t, w); len(f.Problems) == 0 {
		t.Error("a form with no questions lists no problems")
	}

	w = call(t, a, http.MethodPost, "/api/v1/forms/bake-sale-2026/publish", key, nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("publishing an empty form = %d, want 422", w.Code)
	}

	// And a draft that was never live can be deleted.
	if w := call(t, a, http.MethodDelete, "/api/v1/forms/bake-sale-2026", key, nil); w.Code != http.StatusNoContent {
		t.Errorf("delete = %d, want 204:\n%s", w.Code, short(w.Body.String()))
	}
}

func TestADefinitionIsReadStrictly(t *testing.T) {
	a := newAdmin(t, "")
	boss := siteBoss(t, a, "site@schoenstatt.test")
	key := keyFor(t, a, boss)

	typo := retreat()
	typo["fields"] = []map[string]any{{"label": "Name", "kind": "text", "requried": true}}

	cases := map[string]struct {
		body any
		want int
	}{
		"a misspelt key":        {typo, http.StatusBadRequest},
		"not JSON":              {"slug=x&title=y", http.StatusBadRequest},
		"two documents":         {`{"slug":"a-b","title":"x"} {}`, http.StatusBadRequest},
		"no slug":               {map[string]any{"title": "x"}, http.StatusUnprocessableEntity},
		"a kind there is none":  {map[string]any{"slug": "x-y", "title": "x", "fields": []map[string]any{{"label": "Diet", "kind": "banana"}}}, http.StatusUnprocessableEntity},
		"an origin with a path": {map[string]any{"slug": "x-z", "title": "x", "origins": []string{"https://example.org/page"}}, http.StatusUnprocessableEntity},
		"a form that exists":    {map[string]any{"slug": theForm, "title": "x"}, http.StatusConflict},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := call(t, a, http.MethodPost, "/api/v1/forms", key, tc.body)
			if w.Code != tc.want {
				t.Errorf("= %d, want %d:\n%s", w.Code, tc.want, short(w.Body.String()))
			}
			answer(t, w)
		})
	}

	// A form's body that is not JSON at all, by its content type.
	r := httptest.NewRequest(http.MethodPost, "/api/v1/forms", strings.NewReader(`{"slug":"x-q","title":"x"}`))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Authorization", "Bearer "+key)

	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("a form-encoded body = %d, want 415", w.Code)
	}
}

func TestAQuestionKeepsItsKind(t *testing.T) {
	a := newAdmin(t, "")
	boss := siteBoss(t, a, "site@schoenstatt.test")
	key := keyFor(t, a, boss)

	if w := call(t, a, http.MethodPost, "/api/v1/forms", key, retreat()); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}

	def := answer(t, call(t, a, http.MethodGet, "/api/v1/forms/advent-retreat-2026", key, nil))["definition"].(map[string]any)
	def["fields"].([]any)[0].(map[string]any)["kind"] = "date"

	w := call(t, a, http.MethodPut, "/api/v1/forms/advent-retreat-2026", key, def)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("changing a question's kind = %d, want 422", w.Code)
	}

	problems, _ := answer(t, w)["problems"].([]any)
	if len(problems) != 1 || !strings.Contains(problems[0].(string), "text_1") {
		t.Errorf("problems = %v, want one naming the question", problems)
	}
}

func TestAReleaseFormIsReadButNotWrittenThroughTheAPI(t *testing.T) {
	a := newAdmin(t, "")
	boss := siteBoss(t, a, "site@schoenstatt.test")
	key := keyFor(t, a, boss)

	w := call(t, a, http.MethodGet, "/api/v1/forms/"+theForm, key, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("read = %d:\n%s", w.Code, short(w.Body.String()))
	}

	if f := formOf(t, w); f.Editable || !f.Live || len(f.Definition.Fields) == 0 {
		t.Errorf("release form = editable %v, live %v, %d fields", f.Editable, f.Live, len(f.Definition.Fields))
	}

	for _, tc := range []struct{ method, target string }{
		{http.MethodPut, "/api/v1/forms/" + theForm},
		{http.MethodPost, "/api/v1/forms/" + theForm + "/unpublish"},
		{http.MethodDelete, "/api/v1/forms/" + theForm},
	} {
		var body any
		if tc.method == http.MethodPut {
			body = map[string]any{"title": "x"}
		}

		if w := call(t, a, tc.method, tc.target, key, body); w.Code != http.StatusConflict {
			t.Errorf("%s %s = %d, want 409", tc.method, tc.target, w.Code)
		}
	}
}

// Results is enough to read the answers, and not enough to read or change the
// definition -- the same line the pages draw.
func TestAResultsReaderReadsSubmissionsAndNotTheDefinition(t *testing.T) {
	a := newAdmin(t, "")
	boss := siteBoss(t, a, "site@schoenstatt.test")

	reader, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{Email: mustEmail(t, "count@schoenstatt.test"), Name: "Counter"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := a.access.Grant(t.Context(), time.Now(), boss.ID, reader.ID, mustSlug(t, theForm), accessbus.RoleResults); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	key := keyFor(t, a, reader)

	first := order(t, a, "Maria", 2, submissionbus.StatusPaid)
	order(t, a, "Hector", 1, submissionbus.StatusPaid)

	list := answer(t, call(t, a, http.MethodGet, "/api/v1/forms", key, nil))["forms"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["role"] != "results" {
		t.Errorf("forms = %v, want the one form, as results", list)
	}

	w := call(t, a, http.MethodGet, "/api/v1/forms/"+theForm+"/submissions", key, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("submissions = %d:\n%s", w.Code, short(w.Body.String()))
	}

	var got struct {
		Form        string `json:"form"`
		Submissions []struct {
			ID      string         `json:"id"`
			Status  string         `json:"status"`
			Answers map[string]any `json:"answers"`
			Total   int64          `json:"total"`
			Lines   []struct {
				Item     string `json:"item"`
				Quantity int    `json:"quantity"`
			} `json:"lines"`
		} `json:"submissions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(got.Submissions) != 2 {
		t.Fatalf("submissions = %+v, want two", got.Submissions)
	}

	s := got.Submissions[0]
	if s.ID != first.ID.String() || s.Answers["name"] != "Maria" || s.Total != 2400 || s.Status != "paid" {
		t.Errorf("first = %+v, want Maria's paid order, oldest first", s)
	}
	if len(s.Lines) != 1 || s.Lines[0].Item != "ticket" || s.Lines[0].Quantity != 2 {
		t.Errorf("lines = %+v", s.Lines)
	}

	one := answer(t, call(t, a, http.MethodGet, "/api/v1/forms/"+theForm+"/submissions/"+first.ID.String(), key, nil))
	if one["id"] != first.ID.String() {
		t.Errorf("one = %v", one)
	}

	if w := call(t, a, http.MethodGet, "/api/v1/forms/"+theForm+"/submissions/"+types.NewID().String(), key, nil); w.Code != http.StatusNotFound {
		t.Errorf("a submission that does not exist = %d, want 404", w.Code)
	}

	if w := call(t, a, http.MethodGet, "/api/v1/forms/"+theForm, key, nil); w.Code != http.StatusForbidden {
		t.Errorf("the definition, to a results reader = %d, want 403", w.Code)
	}
}

// A submission is reached through the form it belongs to, and a grant on one
// form does not reach another form's submissions by naming them under it.
func TestASubmissionIsOnlyReadThroughItsOwnForm(t *testing.T) {
	a := newAdmin(t, "")
	volunteer := formCreator(t, a, "volunteer@schoenstatt.test")
	key := keyFor(t, a, volunteer)

	theirs := order(t, a, "Maria", 1, submissionbus.StatusPaid)

	if w := call(t, a, http.MethodPost, "/api/v1/forms", key, retreat()); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	if w := call(t, a, http.MethodPost, "/api/v1/forms/advent-retreat-2026/publish", key, nil); w.Code != http.StatusOK {
		t.Fatalf("publish = %d", w.Code)
	}

	w := call(t, a, http.MethodGet, "/api/v1/forms/advent-retreat-2026/submissions/"+theirs.ID.String(), key, nil)
	if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "Maria") {
		t.Errorf("another form's submission through mine = %d:\n%s", w.Code, short(w.Body.String()))
	}
}

func TestAnUnknownAPIAddressIsAJSON404(t *testing.T) {
	a := newAdmin(t, "")
	key := keyFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	w := call(t, a, http.MethodGet, "/api/v1/nothing-here", key, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("= %d, want 404", w.Code)
	}
	answer(t, w)
}

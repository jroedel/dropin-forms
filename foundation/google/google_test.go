package google_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/foundation/google"
)

// fakeGoogle answers each path with a handler and remembers what it was sent.
type fakeGoogle struct {
	t      *testing.T
	routes map[string]http.HandlerFunc
	bodies map[string]string
}

func newClient(t *testing.T, routes map[string]http.HandlerFunc) (*google.Client, *fakeGoogle) {
	t.Helper()

	f := &fakeGoogle{t: t, routes: routes, bodies: map[string]string{}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.bodies[r.URL.Path] = string(b)

		h, ok := f.routes[r.Method+" "+r.URL.Path]
		if !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)

			return
		}

		r.Body = io.NopCloser(strings.NewReader(string(b)))
		h(w, r)
	}))
	t.Cleanup(srv.Close)

	c, err := google.New(google.Config{
		ClientID: "cid", ClientSecret: "secret", RedirectURL: "https://forms.test/google/callback",
		HTTP: srv.Client(), AuthURL: srv.URL + "/auth", TokenURL: srv.URL + "/token", RevokeURL: srv.URL + "/revoke",
		SheetsURL: srv.URL + "/sheets", DriveURL: srv.URL + "/drive",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return c, f
}

func reply(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func idToken(email string) string {
	enc := base64.RawURLEncoding

	return enc.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." +
		enc.EncodeToString([]byte(`{"email":"`+email+`","email_verified":true}`)) + ".sig"
}

func TestTheConsentAddressAsksForAnOfflineGrantWithPKCE(t *testing.T) {
	c, _ := newClient(t, nil)

	u, err := url.Parse(c.AuthURL("st", "verifier-verifier-verifier-verifier-verif", "me@gmail.test"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	q := u.Query()

	for k, want := range map[string]string{
		"client_id": "cid", "redirect_uri": "https://forms.test/google/callback", "response_type": "code",
		"state": "st", "access_type": "offline", "prompt": "consent", "code_challenge_method": "S256",
		"login_hint": "me@gmail.test",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}

	if !strings.Contains(q.Get("scope"), google.ScopeDriveFile) || strings.Contains(q.Get("scope"), "/auth/drive ") || strings.Contains(q.Get("scope"), "spreadsheets") {
		t.Errorf("scope = %q: drive.file and nothing wider", q.Get("scope"))
	}

	if q.Get("code_challenge") == "" || q.Get("code_challenge") == "verifier-verifier-verifier-verifier-verif" {
		t.Errorf("code_challenge = %q, want the S256 of the verifier", q.Get("code_challenge"))
	}
}

func TestACodeIsExchangedForTokensAndTheAccount(t *testing.T) {
	c, f := newClient(t, map[string]http.HandlerFunc{
		"POST /token": reply(200, `{"access_token":"ya29","expires_in":3599,"refresh_token":"1//r","scope":"openid https://www.googleapis.com/auth/userinfo.email `+google.ScopeDriveFile+`","id_token":"`+idToken("me@gmail.test")+`"}`),
	})

	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

	g, err := c.Exchange(t.Context(), now, "the-code", "the-verifier")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	if g.Refresh != "1//r" || g.Access != "ya29" || g.Email != "me@gmail.test" || !g.Files || !g.Expires.Equal(now.Add(3599*time.Second)) {
		t.Errorf("grant = %+v", g)
	}

	sent, _ := url.ParseQuery(f.bodies["/token"])
	if sent.Get("code_verifier") != "the-verifier" || sent.Get("grant_type") != "authorization_code" || sent.Get("client_secret") != "secret" {
		t.Errorf("sent %v", sent)
	}
}

func TestADriveFilePermissionUntickedIsSaid(t *testing.T) {
	c, _ := newClient(t, map[string]http.HandlerFunc{
		"POST /token": reply(200, `{"access_token":"ya29","expires_in":3599,"refresh_token":"1//r","scope":"openid email","id_token":"`+idToken("me@gmail.test")+`"}`),
	})

	g, err := c.Exchange(t.Context(), time.Now(), "c", "v")
	if err != nil || g.Files {
		t.Errorf("Exchange = %+v, %v; want Files false", g, err)
	}
}

func TestRefusalsAreSortedIntoWhatToDo(t *testing.T) {
	c, _ := newClient(t, map[string]http.HandlerFunc{
		"POST /token":       reply(400, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`),
		"GET /drive/gone":   reply(404, `{"error":{"code":404,"message":"File not found","status":"NOT_FOUND"}}`),
		"GET /drive/denied": reply(403, `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`),
		"GET /drive/busy": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "30")
			reply(429, `{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED"}}`)(w, r)
		},
	})

	if _, err := c.Refresh(t.Context(), time.Now(), "1//r"); !errors.Is(err, google.ErrRevoked) {
		t.Errorf("invalid_grant = %v, want ErrRevoked", err)
	}

	if _, err := c.FileOf(t.Context(), "tok", "gone"); !errors.Is(err, google.ErrGone) {
		t.Errorf("404 = %v, want ErrGone", err)
	}

	if _, err := c.FileOf(t.Context(), "tok", "denied"); !errors.Is(err, google.ErrDenied) {
		t.Errorf("403 = %v, want ErrDenied", err)
	}

	_, err := c.FileOf(t.Context(), "tok", "busy")

	busy, ok := errors.AsType[google.Busy](err)
	if !ok || !errors.Is(err, google.ErrBusy) || busy.RetryAfter != 30*time.Second {
		t.Errorf("429 = %v (%+v), want Busy for 30s", err, busy)
	}
}

func TestASpreadsheetIsMadeReadAndWrittenInTheShapesGoogleWants(t *testing.T) {
	c, f := newClient(t, map[string]http.HandlerFunc{
		"POST /sheets": reply(200, `{"spreadsheetId":"ss1","spreadsheetUrl":"https://docs.google.com/x","sheets":[{"properties":{"sheetId":0,"title":"Responses"}}]}`),
		"POST /sheets/ss1/developerMetadata:search": reply(200, `{"matchedDeveloperMetadata":[
			{"developerMetadata":{"metadataKey":"k","metadataValue":"ref","location":{"locationType":"COLUMN","dimensionRange":{"sheetId":0,"dimension":"COLUMNS","startIndex":3,"endIndex":4}}}},
			{"developerMetadata":{"metadataKey":"k","metadataValue":"form","location":{"locationType":"SHEET","sheetId":0}}}]}`),
		"POST /sheets/ss1/values:batchGetByDataFilter": reply(200, `{"valueRanges":[
			{"valueRange":{"values":[["Reference","abc",""]]}},
			{"valueRange":{}}]}`),
		"POST /sheets/ss1:batchUpdate": reply(200, `{"replies":[{},{"addSheet":{"properties":{"sheetId":7}}}]}`),
	})

	made, err := c.Create(t.Context(), "tok", "Advent Retreat", "Responses", "en_US", "America/Chicago")
	if err != nil || made.SpreadsheetID != "ss1" || made.SheetID != 0 {
		t.Fatalf("Create = %+v, %v", made, err)
	}

	if !strings.Contains(f.bodies["/sheets"], `"timeZone":"America/Chicago"`) {
		t.Errorf("create sent %s", f.bodies["/sheets"])
	}

	tags, err := c.Tagged(t.Context(), "tok", "ss1", "k")
	if err != nil || len(tags) != 2 || tags[0] != (google.Tag{Value: "ref", SheetID: 0, Column: 3}) || tags[1].Column != -1 {
		t.Errorf("Tagged = %+v, %v", tags, err)
	}

	cols, err := c.ReadColumns(t.Context(), "tok", "ss1", 0, []int{3, 5})
	if err != nil || len(cols) != 2 || len(cols[0]) != 3 || cols[0][1] != "abc" || cols[1] != nil {
		t.Errorf("ReadColumns = %v, %v", cols, err)
	}

	if !strings.Contains(f.bodies["/sheets/ss1/values:batchGetByDataFilter"], `"majorDimension":"COLUMNS"`) {
		t.Errorf("read sent %s", f.bodies["/sheets/ss1/values:batchGetByDataFilter"])
	}

	when := time.Date(2026, 11, 12, 18, 0, 0, 0, time.FixedZone("CST", -6*3600))

	tabs, err := c.Apply(t.Context(), "tok", "ss1", []google.Edit{
		google.SetCells{Sheet: 0, Row: 1, Column: 2, Rows: [][]google.Value{{
			{Kind: google.Text, Text: "=IMPORTXML(1)"},
			{Kind: google.Money, Number: 35},
			{Kind: google.Date, Time: when},
			{},
			{Kind: google.Text, Text: "abc", Link: "https://forms.test/forms/x/submissions/abc"},
		}}},
		google.AddTab{Title: "Responses (rebuilt 12 Nov)"},
	})
	if err != nil || len(tabs) != 1 || tabs[0] != 7 {
		t.Fatalf("Apply = %v, %v", tabs, err)
	}

	var sent struct {
		Requests []map[string]json.RawMessage `json:"requests"`
	}

	if err := json.Unmarshal([]byte(f.bodies["/sheets/ss1:batchUpdate"]), &sent); err != nil {
		t.Fatalf("the batch is not JSON: %v", err)
	}

	update := string(sent.Requests[0]["updateCells"])

	// A formula typed into a form is a string here, never a formula.
	for _, want := range []string{`"stringValue":"=IMPORTXML(1)"`, `"numberValue":35`, `"type":"DATE_TIME"`, `"fields":"userEnteredValue,`,
		`"link":{"uri":"https://forms.test/forms/x/submissions/abc"}`, `userEnteredFormat.textFormat.link"`} {
		if !strings.Contains(update, want) {
			t.Errorf("updateCells does not contain %s:\n%s", want, update)
		}
	}

	if strings.Contains(update, "formulaValue") {
		t.Error("a cell was sent as a formula")
	}
}

func TestADateIsTheSpreadsheetSerialOfItsWallClock(t *testing.T) {
	if got := google.Serial(time.Date(1899, 12, 31, 12, 0, 0, 0, time.UTC)); got != 1.5 {
		t.Errorf("Serial(31 Dec 1899 noon) = %v, want 1.5", got)
	}

	// The wall clock, not UTC: 6 pm in Chicago is 6 pm in a Chicago sheet.
	a := google.Serial(time.Date(2026, 11, 12, 18, 0, 0, 0, time.FixedZone("CST", -6*3600)))
	b := google.Serial(time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC))

	if a != b {
		t.Errorf("serials differ by zone: %v and %v", a, b)
	}
}

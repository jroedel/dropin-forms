package muxer_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/sheet/sheetbus"
	"github.com/jroedel/dropin-forms/business/domain/sheet/stores/sheetdb"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/foundation/google"
	"github.com/jroedel/dropin-forms/foundation/mail"
)

// Connecting a Google Sheet, through the admin surface as it is mounted.
// sheetbus's own tests are where the sheet itself is kept and damaged; these
// are about who may reach the page, what the page shows whom, and the trip to
// Google and back.

// googleDouble is Google answering yes to everything, and remembering what it
// was asked to make and withdraw.
type googleDouble struct {
	made    int
	revoked []string
	noFiles bool
}

func (g *googleDouble) AuthURL(state, verifier, hint string) string {
	return "https://accounts.google.test/auth?state=" + url.QueryEscape(state)
}

func (g *googleDouble) Exchange(_ context.Context, now time.Time, code, verifier string) (google.Grant, error) {
	return google.Grant{Refresh: "1//r", Access: "a", Expires: now.Add(time.Hour), Email: "secretary@gmail.test", Files: !g.noFiles}, nil
}

func (g *googleDouble) Refresh(_ context.Context, now time.Time, _ string) (google.Access, error) {
	return google.Access{Token: "a", Expires: now.Add(time.Hour)}, nil
}

func (g *googleDouble) Revoke(_ context.Context, token string) error {
	g.revoked = append(g.revoked, token)

	return nil
}

func (g *googleDouble) Create(context.Context, string, string, string, string, string) (google.Created, error) {
	g.made++

	return google.Created{SpreadsheetID: "ss1", SheetID: 0, URL: "https://docs.google.test/ss1"}, nil
}

func (g *googleDouble) Tabs(context.Context, string, string) (map[int64]string, error) {
	return map[int64]string{0: "Responses"}, nil
}

func (g *googleDouble) Tagged(context.Context, string, string, string) ([]google.Tag, error) {
	return []google.Tag{{Value: "ref", Column: 1}}, nil
}

func (g *googleDouble) ReadColumns(_ context.Context, _, _ string, _ int64, cols []int) ([][]any, error) {
	return make([][]any, len(cols)), nil
}

func (g *googleDouble) FileOf(context.Context, string, string) (google.File, error) {
	return google.File{}, nil
}

func (g *googleDouble) Apply(context.Context, string, string, []google.Edit) ([]int64, error) {
	return nil, nil
}

type sheetHarness struct {
	office
	google *googleDouble
	reader string // results
	other  string // results too, somebody else
	none   string // an account with no access to this form
}

func sheetSurface(t *testing.T) sheetHarness {
	t.Helper()

	f := quietDay(t, false)

	cfg := newConfig(t, nil, nil)
	cfg.Forms = formAt{&f}
	cfg.Embed.Forms = formAt{&f}

	subs, ok := cfg.Embed.Submissions.(*submissionbus.Business)
	if !ok {
		t.Fatalf("the test config holds a %T", cfg.Embed.Submissions)
	}

	g := &googleDouble{}

	sheets, err := sheetbus.NewBusiness(sheetbus.Config{
		Log:          cfg.Log,
		Store:        sheetdb.NewStore(cfg.DB),
		Google:       g,
		Forms:        formAt{&f},
		Subs:         subs,
		Access:       cfg.Access,
		Accounts:     cfg.Users,
		Mail:         &mail.Recorder{},
		SecretBytes:  []byte("a test secret long enough to derive from"),
		Zone:         time.UTC,
		ZoneName:     "UTC",
		AdminBaseURL: "https://forms.test",
	})
	if err != nil {
		t.Fatalf("sheetbus.NewBusiness: %v", err)
	}

	cfg.Sheets = sheets

	o := office{admin: adminOf(t, cfg), embed: embedOf(t, cfg), cfg: cfg}

	h := sheetHarness{office: o, google: g}

	who, cookie := o.reader(t, "secretary@schoenstatt.test")
	o.grant(t, who, "quiet-day", accessbus.RoleResults)
	h.reader = cookie

	who, cookie = o.reader(t, "treasurer@schoenstatt.test")
	o.grant(t, who, "quiet-day", accessbus.RoleResults)
	h.other = cookie

	_, cookie = o.reader(t, "stranger@schoenstatt.test")
	h.none = cookie

	return h
}

// get is a GET with the session and any other cookies.
func (h sheetHarness) get(t *testing.T, target, session string, more ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.AddCookie(&http.Cookie{Name: mid.CookieName, Value: session})

	for _, c := range more {
		r.AddCookie(c)
	}

	w := httptest.NewRecorder()
	h.admin.ServeHTTP(w, r)

	return w
}

// connect goes to Google and back for whoever holds session.
func (h sheetHarness) connect(t *testing.T, session string) *httptest.ResponseRecorder {
	t.Helper()

	w := h.get(t, "/forms/quiet-day/sheet/connect", session)
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "https://accounts.google.test/auth?") {
		t.Fatalf("Connect = %d to %q", w.Code, w.Header().Get("Location"))
	}

	var verifier *http.Cookie

	for _, c := range w.Result().Cookies() {
		if c.Name == "__Host-sheet-verifier" {
			verifier = c
		}
	}

	if verifier == nil || !verifier.Secure || !verifier.HttpOnly {
		t.Fatalf("the verifier cookie is %+v", verifier)
	}

	to, _ := url.Parse(w.Header().Get("Location"))

	return h.get(t, "/google/callback?code=c&state="+url.QueryEscape(to.Query().Get("state")), session, verifier)
}

func TestAReaderConnectsTheirOwnGoogleSheet(t *testing.T) {
	h := sheetSurface(t)

	if list := h.read(t, "/forms/quiet-day/submissions", h.reader).Body.String(); !strings.Contains(list, `href="/forms/quiet-day/sheet"`) {
		t.Errorf("the list does not link to the sheet page:\n%s", short(list))
	}

	if page := h.read(t, "/forms/quiet-day/sheet", h.reader).Body.String(); !strings.Contains(page, `href="/forms/quiet-day/sheet/connect"`) {
		t.Fatalf("the page offers no Connect:\n%s", short(page))
	}

	w := h.connect(t, h.reader)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/forms/quiet-day/sheet?sheet=connected" {
		t.Fatalf("coming back from Google = %d to %q:\n%s", w.Code, w.Header().Get("Location"), w.Body)
	}

	if h.google.made != 1 {
		t.Errorf("%d spreadsheets made, want 1", h.google.made)
	}

	page := h.read(t, "/forms/quiet-day/sheet?sheet=connected", h.reader).Body.String()
	for _, want := range []string{"Connected.", "secretary@gmail.test", "https://docs.google.test/ss1", "Kept up to date"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not say %q:\n%s", want, short(page))
		}
	}

	// Somebody else who reads the same form sees nothing of it.
	other := h.read(t, "/forms/quiet-day/sheet", h.other).Body.String()
	if strings.Contains(other, "secretary@gmail.test") || strings.Contains(other, "docs.google.test") || !strings.Contains(other, "Connect a Google Sheet") {
		t.Errorf("another reader's page shows the first one's sheet:\n%s", short(other))
	}

	// Disconnecting withdraws the permission at Google.
	if w := h.postValues(t, "/forms/quiet-day/sheet/disconnect", h.reader, nil); w.Header().Get("Location") != "/forms/quiet-day/sheet?sheet=disconnected" {
		t.Fatalf("Disconnect went to %q", w.Header().Get("Location"))
	}

	if len(h.google.revoked) != 1 {
		t.Errorf("revoked %v at Google", h.google.revoked)
	}
}

func TestTheSheetPageIsForReadersOfTheAnswersOnly(t *testing.T) {
	h := sheetSurface(t)

	if w := h.read(t, "/forms/quiet-day/sheet", h.none); w.Code != http.StatusForbidden {
		t.Errorf("the sheet page for an account with no access = %d, want 403", w.Code)
	}

	if w := h.get(t, "/forms/quiet-day/sheet/connect", h.none); w.Code != http.StatusForbidden {
		t.Errorf("Connect for an account with no access = %d, want 403", w.Code)
	}

	// Another site cannot disconnect it on the reader's behalf.
	h.connect(t, h.reader)

	r := httptest.NewRequest(http.MethodPost, "/forms/quiet-day/sheet/disconnect", strings.NewReader(""))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.AddCookie(&http.Cookie{Name: mid.CookieName, Value: h.reader})

	w := httptest.NewRecorder()
	h.admin.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden || len(h.google.revoked) != 0 {
		t.Errorf("a cross-site disconnect = %d, revoked %v", w.Code, h.google.revoked)
	}
}

func TestGoogleSendingSomebodyElseBackIsRefused(t *testing.T) {
	h := sheetSurface(t)

	w := h.get(t, "/forms/quiet-day/sheet/connect", h.reader)
	to, _ := url.Parse(w.Header().Get("Location"))
	state := to.Query().Get("state")

	// The other reader arriving with the first one's state.
	w = h.get(t, "/google/callback?code=c&state="+url.QueryEscape(state), h.other, &http.Cookie{Name: "__Host-sheet-verifier", Value: "v"})
	if w.Code != http.StatusBadRequest || h.google.made != 0 {
		t.Errorf("another person's state = %d, %d spreadsheets made", w.Code, h.google.made)
	}

	// The person said no on Google's page.
	w = h.get(t, "/google/callback?error=access_denied&state="+url.QueryEscape(state), h.reader)
	if w.Header().Get("Location") != "/forms/quiet-day/sheet?sheet=declined" {
		t.Errorf("a refusal at Google went to %q", w.Header().Get("Location"))
	}

	// They unticked the box that lets the spreadsheet be made.
	h.google.noFiles = true

	if w := h.connect(t, h.reader); w.Header().Get("Location") != "/forms/quiet-day/sheet?sheet=no-files" || h.google.made != 0 {
		t.Errorf("without the files permission: to %q, %d made", w.Header().Get("Location"), h.google.made)
	}
}

// Google's consent screen links to the privacy page, and Google checks that
// it can be read by somebody who is not signed in.
func TestThePrivacyPageNeedsNoSignIn(t *testing.T) {
	h := sheetSurface(t)

	w := httptest.NewRecorder()
	h.admin.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/privacy", nil))

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Limited Use") {
		t.Errorf("/privacy without a session = %d:\n%s", w.Code, short(w.Body.String()))
	}
}

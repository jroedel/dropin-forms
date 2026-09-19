package muxer_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/domain/authapp"
	"github.com/jroedel/dropin-forms/app/domain/formapp"
	"github.com/jroedel/dropin-forms/app/domain/notifyapp"
	"github.com/jroedel/dropin-forms/app/domain/peopleapp"
	"github.com/jroedel/dropin-forms/app/domain/submissionapp"
	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/app/sdk/muxer"
	"github.com/jroedel/dropin-forms/app/sdk/page"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/access/stores/accessdb"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/form/stores/formdb"
	"github.com/jroedel/dropin-forms/business/domain/notify/notifybus"
	"github.com/jroedel/dropin-forms/business/domain/notify/stores/notifydb"
	"github.com/jroedel/dropin-forms/business/domain/submission/stores/submissiondb"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/stores/userdb"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/mail"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

const bootstrapSecret = "a-long-enough-one-time-bootstrap-secret"

// admin builds the real admin surface over a real database, so that every
// assertion below is about the chain as it is actually mounted rather than
// about a handler called directly. The thing most worth testing about a route
// is what sits in front of it.
type harness struct {
	h      http.Handler
	users  *userbus.Business
	access *accessbus.Business
	sent   *mail.Recorder

	// notify is the real notification domain over the same database, so that a
	// test can read back what the unsubscribe page wrote. muteKey is what
	// signs the links that page accepts.
	notify  *notifybus.Business
	muteKey notifybus.MuteKey

	// catalogue is the form domain over both stores, so that a test can make
	// a draft without going through the builder's own pages -- and so that
	// every other test on this harness reads its forms the way production
	// does, through the catalogue rather than straight out of the file store.
	catalogue *formbus.Business
}

func newAdmin(t *testing.T, bootstrap string) harness {
	t.Helper()

	db, err := sqldb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	if err := sqldb.Init(t.Context(), db); err != nil {
		t.Fatalf("sqldb.Init: %v", err)
	}
	if err := userdb.Init(t.Context(), db); err != nil {
		t.Fatalf("userdb.Init: %v", err)
	}
	if err := accessdb.Init(t.Context(), db); err != nil {
		t.Fatalf("accessdb.Init: %v", err)
	}
	if err := submissiondb.Init(t.Context(), db); err != nil {
		t.Fatalf("submissiondb.Init: %v", err)
	}
	if err := notifydb.Init(t.Context(), db); err != nil {
		t.Fatalf("notifydb.Init: %v", err)
	}
	if err := formdb.Init(t.Context(), db); err != nil {
		t.Fatalf("formdb.Init: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	users := userbus.NewBusiness(log, userdb.NewStore(db))
	access := accessbus.NewBusiness(log, accessdb.NewStore(db))
	submissions := submissionbus.NewBusiness(log, submissiondb.NewStore(db))
	sent := &mail.Recorder{}

	renderer, err := page.NewRenderer(log, page.AdminChrome(),
		authapp.Templates, submissionapp.Templates, notifyapp.Templates, peopleapp.Templates,
		formapp.Templates)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	catalogue, err := formbus.NewBusiness(log, formdb.NewStore(db), definitions)
	if err != nil {
		t.Fatalf("formbus.NewBusiness: %v", err)
	}

	muteKey, err := notifybus.ParseMuteKey("a-test-secret-long-enough-to-be-a-key")
	if err != nil {
		t.Fatalf("ParseMuteKey: %v", err)
	}

	notifier, err := notifybus.NewBusiness(notifybus.Config{
		Log:          log,
		Mail:         brokenMail{},
		Forms:        definitions,
		Submissions:  submissions,
		Grants:       access,
		Accounts:     users,
		Mutes:        notifydb.NewStore(db),
		MuteKey:      muteKey,
		AdminBaseURL: "https://forms.test",
	})
	if err != nil {
		t.Fatalf("notifybus.NewBusiness: %v", err)
	}

	expected := sqldb.Expected{}
	for table, columns := range sqldb.Infrastructure {
		expected[table] = columns
	}
	for table, columns := range userdb.Expected {
		expected[table] = columns
	}
	for table, columns := range accessdb.Expected {
		expected[table] = columns
	}
	for table, columns := range submissiondb.Expected {
		expected[table] = columns
	}
	for table, columns := range notifydb.Expected {
		expected[table] = columns
	}
	for table, columns := range formdb.Expected {
		expected[table] = columns
	}

	h, err := muxer.Admin(muxer.Config{
		Log:          log,
		DB:           db,
		Expected:     expected,
		Users:        users,
		Access:       access,
		Submissions:  submissions,
		Forms:        catalogue,
		Builder:      catalogue,
		EmbedBaseURL: "https://f.forms.test",
		Mail:         sent,
		Render:       renderer,
		Notify:       notifier,
		AdminBaseURL: "https://forms.test",
		Bootstrap:    bootstrap,
	})
	if err != nil {
		t.Fatalf("muxer.Admin: %v", err)
	}

	return harness{
		h: h, users: users, access: access, sent: sent,
		notify: notifier, muteKey: muteKey, catalogue: catalogue,
	}
}

func (a harness) get(t *testing.T, target string, cookie string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, target, nil)
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: mid.CookieName, Value: cookie})
	}

	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)

	return w
}

func (a harness) post(t *testing.T, target string, form url.Values, cookie string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: mid.CookieName, Value: cookie})
	}

	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, r)

	return w
}

func sessionCookie(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()

	for _, c := range w.Result().Cookies() {
		if c.Name == mid.CookieName && c.Value != "" {
			return c.Value
		}
	}

	return ""
}

var linkPattern = regexp.MustCompile(`https://forms\.test/signin/link\?t=([^\s]+)`)

// signInLink pulls the link out of the message that was recorded, the way the
// person receiving it would read it out of their inbox.
func signInLink(t *testing.T, a harness) string {
	t.Helper()

	m, ok := a.sent.Last()
	if !ok {
		t.Fatal("no mail was sent")
	}

	found := linkPattern.FindStringSubmatch(m.Text)
	if found == nil {
		t.Fatalf("no sign-in link in the message:\n%s", m.Text)
	}

	return found[1]
}

func mustEmail(t *testing.T, s string) types.Email {
	t.Helper()

	e, err := types.ParseEmail(s)
	if err != nil {
		t.Fatalf("ParseEmail(%q): %v", s, err)
	}

	return e
}

// The whole journey, through the mounted chain, with the assertion that
// matters most in the middle of it.
func TestSignInFromLinkToAccount(t *testing.T) {
	a := newAdmin(t, "")

	u, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, "frjeff@schoenstatt.us"),
		Name:  "Fr Jeff",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The sign-in page is reachable with no session, which is the reason
	// Authenticate refuses nobody.
	if w := a.get(t, "/signin", ""); w.Code != http.StatusOK {
		t.Fatalf("GET /signin = %d, want 200", w.Code)
	}

	w := a.post(t, "/signin", url.Values{"email": {"frjeff@schoenstatt.us"}}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("POST /signin = %d, want 200:\n%s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "Check your email") {
		t.Errorf("the page does not say to check your email:\n%s", w.Body)
	}

	token := signInLink(t, a)

	// Opening the emailed link must not sign anybody in. Mail scanners and
	// link previewers fetch URLs found in messages without anybody clicking,
	// so a token redeemed on GET is a token spent by software before the
	// person ever sees it.
	w = a.get(t, "/signin/link?t="+token, "")
	switch {
	case w.Code != http.StatusOK:
		t.Fatalf("GET the link = %d, want 200:\n%s", w.Code, w.Body)
	case sessionCookie(t, w) != "":
		t.Fatal("opening the emailed link set a session cookie; it must only render a button")
	case !strings.Contains(w.Body.String(), `action="/signin/link"`):
		t.Errorf("the page has no form to submit:\n%s", w.Body)
	}

	// Opening it again, as a previewer and then the person would: still not
	// spent.
	if w := a.get(t, "/signin/link?t="+token, ""); w.Code != http.StatusOK {
		t.Fatalf("opening the link twice = %d; opening it must never spend it", w.Code)
	}

	// Now the button.
	w = a.post(t, "/signin/link", url.Values{"token": {token}}, "")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST the token = %d, want 303:\n%s", w.Code, w.Body)
	}

	cookie := sessionCookie(t, w)
	if cookie == "" {
		t.Fatal("signing in set no session cookie")
	}
	if got := w.Header().Get("Location"); got != "/account" {
		t.Errorf("Location = %q, want /account", got)
	}

	// And the session works on a guarded page.
	w = a.get(t, "/account", cookie)
	switch {
	case w.Code != http.StatusOK:
		t.Fatalf("GET /account = %d, want 200:\n%s", w.Code, w.Body)
	case !strings.Contains(w.Body.String(), u.Email.String()):
		t.Errorf("the account page does not name the account:\n%s", w.Body)
	}

	// Single use: the same token again is refused.
	if w := a.post(t, "/signin/link", url.Values{"token": {token}}, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("reusing the token = %d, want 401", w.Code)
	}
}

// Asking for a link must look the same whether or not an account exists. This
// compares the two responses directly, because "looks the same" is easy to
// believe and easy to get wrong.
func TestRequestingALinkRevealsNothingAboutWhoExists(t *testing.T) {
	a := newAdmin(t, "")

	if _, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, "known@schoenstatt.us"),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	known := a.post(t, "/signin", url.Values{"email": {"known@schoenstatt.us"}}, "")
	sentToKnown := len(a.sent.Sent)

	unknown := a.post(t, "/signin", url.Values{"email": {"unknown@schoenstatt.us"}}, "")
	sentToUnknown := len(a.sent.Sent) - sentToKnown

	if known.Code != unknown.Code {
		t.Errorf("status differs: known %d, unknown %d", known.Code, unknown.Code)
	}

	// The address is echoed back, so the bodies differ by exactly that and
	// nothing else. Each response has its own address replaced -- and the
	// longer one first, because "known@schoenstatt.us" is a substring of
	// "unknown@schoenstatt.us" and replacing it first leaves "unADDRESS".
	normalise := func(body, addr string) string {
		return strings.ReplaceAll(body, addr, "ADDRESS")
	}

	if normalise(known.Body.String(), "known@schoenstatt.us") !=
		normalise(unknown.Body.String(), "unknown@schoenstatt.us") {
		t.Error("the two pages differ by more than the address, which tells an attacker which addresses have accounts")
	}

	// And exactly one message was sent, for the account that exists.
	if sentToKnown != 1 {
		t.Errorf("%d messages sent for the known address, want 1", sentToKnown)
	}
	if sentToUnknown != 0 {
		t.Errorf("%d messages sent for the unknown address, want 0", sentToUnknown)
	}
}

// A relay failure must not become a different answer, because the difference
// would only ever appear for addresses that do have an account.
func TestASendFailureLooksTheSameAsASuccess(t *testing.T) {
	a := newAdmin(t, "")

	if _, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, "known@schoenstatt.us"),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	working := a.post(t, "/signin", url.Values{"email": {"known@schoenstatt.us"}}, "")

	// Same surface, same account, but the mailer now fails.
	broken := newAdminWithBrokenMail(t)
	if _, err := broken.users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, "known@schoenstatt.us"),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	failed := broken.post(t, "/signin", url.Values{"email": {"known@schoenstatt.us"}}, "")

	if working.Code != failed.Code {
		t.Errorf("status differs: working relay %d, broken relay %d", working.Code, failed.Code)
	}
	if working.Body.String() != failed.Body.String() {
		t.Error("a broken relay produces a different page, which is only visible for addresses that have an account")
	}
}

// brokenMail fails every send, like a relay that is down.
type brokenMail struct{}

func (brokenMail) Send(_ context.Context, _ mail.Message) error {
	return errors.New("the relay is refusing connections")
}

func newAdminWithBrokenMail(t *testing.T) harness {
	t.Helper()

	a := newAdmin(t, "")

	// Rebuilt with the failing sender. Simpler than threading an option
	// through newAdmin for one test.
	db, err := sqldb.Open(filepath.Join(t.TempDir(), "broken.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	if err := sqldb.Init(t.Context(), db); err != nil {
		t.Fatalf("sqldb.Init: %v", err)
	}
	if err := userdb.Init(t.Context(), db); err != nil {
		t.Fatalf("userdb.Init: %v", err)
	}
	if err := accessdb.Init(t.Context(), db); err != nil {
		t.Fatalf("accessdb.Init: %v", err)
	}
	if err := submissiondb.Init(t.Context(), db); err != nil {
		t.Fatalf("submissiondb.Init: %v", err)
	}
	if err := notifydb.Init(t.Context(), db); err != nil {
		t.Fatalf("notifydb.Init: %v", err)
	}
	if err := formdb.Init(t.Context(), db); err != nil {
		t.Fatalf("formdb.Init: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	users := userbus.NewBusiness(log, userdb.NewStore(db))
	access := accessbus.NewBusiness(log, accessdb.NewStore(db))
	submissions := submissionbus.NewBusiness(log, submissiondb.NewStore(db))

	renderer, err := page.NewRenderer(log, page.AdminChrome(),
		authapp.Templates, submissionapp.Templates, notifyapp.Templates)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	muteKey, err := notifybus.ParseMuteKey("a-test-secret-long-enough-to-be-a-key")
	if err != nil {
		t.Fatalf("ParseMuteKey: %v", err)
	}

	notifier, err := notifybus.NewBusiness(notifybus.Config{
		Log:          log,
		Mail:         brokenMail{},
		Forms:        definitions,
		Submissions:  submissions,
		Grants:       access,
		Accounts:     users,
		Mutes:        notifydb.NewStore(db),
		MuteKey:      muteKey,
		AdminBaseURL: "https://forms.test",
	})
	if err != nil {
		t.Fatalf("notifybus.NewBusiness: %v", err)
	}

	expected := sqldb.Expected{}
	for table, columns := range sqldb.Infrastructure {
		expected[table] = columns
	}
	for table, columns := range userdb.Expected {
		expected[table] = columns
	}
	for table, columns := range accessdb.Expected {
		expected[table] = columns
	}
	for table, columns := range submissiondb.Expected {
		expected[table] = columns
	}
	for table, columns := range notifydb.Expected {
		expected[table] = columns
	}

	h, err := muxer.Admin(muxer.Config{
		Log:          log,
		DB:           db,
		Expected:     expected,
		Users:        users,
		Access:       access,
		Submissions:  submissions,
		Forms:        definitions,
		Mail:         brokenMail{},
		Render:       renderer,
		Notify:       notifier,
		AdminBaseURL: "https://forms.test",
	})
	if err != nil {
		t.Fatalf("muxer.Admin: %v", err)
	}

	a.h = h
	a.users = users
	a.access = access
	a.notify = notifier
	a.muteKey = muteKey

	return a
}

func TestGuardedRoutes(t *testing.T) {
	a := newAdmin(t, "")

	tests := []struct {
		name   string
		method string
		target string
		status int
		loc    string
	}{
		{
			name: "the account page, signed out", method: http.MethodGet, target: "/account",
			status: http.StatusSeeOther, loc: "/signin?next=%2Faccount",
		},
		{
			// A POST redirected to a login page loses its body.
			name: "issuing backup codes, signed out", method: http.MethodPost, target: "/account/backup-codes",
			status: http.StatusForbidden,
		},
		{
			name: "the root, signed out", method: http.MethodGet, target: "/",
			status: http.StatusSeeOther, loc: "/signin",
		},
		{name: "the sign-in page", method: http.MethodGet, target: "/signin", status: http.StatusOK},
		{name: "the backup code page", method: http.MethodGet, target: "/signin/code", status: http.StatusOK},
		{
			// Not configured in this harness, so the route does not exist at
			// all -- a better answer than a form that can never succeed.
			name: "the bootstrap page, unconfigured", method: http.MethodGet, target: "/signin/bootstrap",
			status: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var w *httptest.ResponseRecorder

			if tt.method == http.MethodGet {
				w = a.get(t, tt.target, "")
			} else {
				w = a.post(t, tt.target, nil, "")
			}

			if w.Code != tt.status {
				t.Errorf("%s %s = %d, want %d", tt.method, tt.target, w.Code, tt.status)
			}
			if tt.loc != "" && w.Header().Get("Location") != tt.loc {
				t.Errorf("Location = %q, want %q", w.Header().Get("Location"), tt.loc)
			}
		})
	}
}

func TestBootstrapWorksOnceAndCreatesTheFirstAccount(t *testing.T) {
	a := newAdmin(t, bootstrapSecret)

	if w := a.get(t, "/signin/bootstrap", ""); w.Code != http.StatusOK {
		t.Fatalf("GET /signin/bootstrap = %d, want 200", w.Code)
	}

	// There are no accounts at all. That is the situation this is for.
	w := a.post(t, "/signin/bootstrap", url.Values{
		"email":  {"frjeff@schoenstatt.us"},
		"secret": {bootstrapSecret},
	}, "")

	if w.Code != http.StatusSeeOther {
		t.Fatalf("bootstrap = %d, want 303:\n%s", w.Code, w.Body)
	}

	cookie := sessionCookie(t, w)
	if cookie == "" {
		t.Fatal("bootstrap issued no session")
	}

	if w := a.get(t, "/account", cookie); w.Code != http.StatusOK {
		t.Fatalf("GET /account with the bootstrap session = %d:\n%s", w.Code, w.Body)
	}

	// Exactly once, and a wrong secret does not spend it.
	w = a.post(t, "/signin/bootstrap", url.Values{
		"email":  {"frjeff@schoenstatt.us"},
		"secret": {bootstrapSecret},
	}, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("bootstrap a second time = %d, want 401", w.Code)
	}

	// No mail was involved at any point, which is the entire reason it exists.
	if len(a.sent.Sent) != 0 {
		t.Errorf("bootstrap sent %d messages, want none", len(a.sent.Sent))
	}
}

// The bootstrap secret has to produce somebody who can grant, or it produces a
// session that can see nothing and the service is unusable on its first day:
// the only way to grant anything would be to already hold a grant.
func TestBootstrapMakesTheFirstAccountASiteAdministrator(t *testing.T) {
	a := newAdmin(t, bootstrapSecret)

	w := a.post(t, "/signin/bootstrap", url.Values{
		"email":  {"frjeff@schoenstatt.us"},
		"secret": {bootstrapSecret},
	}, "")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("bootstrap = %d, want 303:\n%s", w.Code, w.Body)
	}

	grants, err := a.access.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("%d grants after bootstrapping, want 1", len(grants))
	}

	g := grants[0]

	if !g.SiteWide() {
		t.Errorf("the grant is on %q, want every form", g.Form)
	}
	if g.Role != accessbus.RoleAdmin {
		t.Errorf("Role = %q, want %q", g.Role, accessbus.RoleAdmin)
	}

	// Nobody granted it. The configuration did, and the audit field says so
	// rather than naming the account as its own granter.
	if !g.GrantedBy.Zero() {
		t.Errorf("GrantedBy = %q, want the zero id", g.GrantedBy)
	}

	// And it is enough to reach a form.
	allowed, err := a.access.Allowed(t.Context(), g.UserID, mustSlug(t, "feast-lunch-2026"), accessbus.RoleAdmin)
	if err != nil {
		t.Fatalf("Allowed: %v", err)
	}
	if !allowed {
		t.Error("the bootstrapped administrator cannot reach a form")
	}
}

func mustSlug(t *testing.T, s string) types.Slug {
	t.Helper()

	slug, err := types.ParseSlug(s)
	if err != nil {
		t.Fatalf("ParseSlug(%q): %v", s, err)
	}

	return slug
}

func TestBackupCodeJourney(t *testing.T) {
	a := newAdmin(t, bootstrapSecret)

	// In through the bootstrap, since that is the realistic order: mail is
	// broken, so you bootstrap and then take codes.
	w := a.post(t, "/signin/bootstrap", url.Values{
		"email":  {"frjeff@schoenstatt.us"},
		"secret": {bootstrapSecret},
	}, "")

	cookie := sessionCookie(t, w)
	if cookie == "" {
		t.Fatal("bootstrap issued no session")
	}

	w = a.post(t, "/account/backup-codes", nil, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("issuing codes = %d:\n%s", w.Code, w.Body)
	}

	codes := regexp.MustCompile(`[A-Z2-7]{4}-[A-Z2-7]{4}-[A-Z2-7]{4}-[A-Z2-7]{4}`).
		FindAllString(w.Body.String(), -1)

	if len(codes) != 10 {
		t.Fatalf("the page shows %d codes, want 10:\n%s", len(codes), w.Body)
	}

	// Signed out, and in again with a code.
	w = a.post(t, "/signin/code", url.Values{
		"email": {"frjeff@schoenstatt.us"},
		"code":  {codes[0]},
	}, "")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("signing in with a code = %d, want 303:\n%s", w.Code, w.Body)
	}
	if sessionCookie(t, w) == "" {
		t.Error("signing in with a code issued no session")
	}

	// Once.
	w = a.post(t, "/signin/code", url.Values{
		"email": {"frjeff@schoenstatt.us"},
		"code":  {codes[0]},
	}, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("reusing a code = %d, want 401", w.Code)
	}
}

func TestSignOutClearsTheSession(t *testing.T) {
	a := newAdmin(t, bootstrapSecret)

	w := a.post(t, "/signin/bootstrap", url.Values{
		"email":  {"frjeff@schoenstatt.us"},
		"secret": {bootstrapSecret},
	}, "")

	cookie := sessionCookie(t, w)

	w = a.post(t, "/signout", nil, cookie)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /signout = %d, want 303", w.Code)
	}
	if !strings.Contains(w.Header().Get("Set-Cookie"), mid.CookieName+"=;") {
		t.Errorf("sign out did not clear the cookie: %q", w.Header().Get("Set-Cookie"))
	}

	// The cookie the browser still holds no longer works, and the request is
	// sent to the sign-in page rather than answered with an error.
	w = a.get(t, "/account", cookie)
	if w.Code != http.StatusSeeOther {
		t.Errorf("GET /account after signing out = %d, want 303", w.Code)
	}
}

// The stylesheet is outside every gate, because the sign-in page needs it and
// a stylesheet behind a session renders the login page unstyled.
func TestStylesheetIsServedWithoutASession(t *testing.T) {
	a := newAdmin(t, "")

	// Found the way a browser finds it: from the markup.
	signin := a.get(t, "/signin", "")

	href := regexp.MustCompile(`href="(/static/app\.[0-9a-f]+\.css)"`).
		FindStringSubmatch(signin.Body.String())
	if href == nil {
		t.Fatalf("the sign-in page links to no stylesheet:\n%s", signin.Body)
	}

	w := a.get(t, href[1], "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", href[1], w.Code)
	}

	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/css") {
		t.Errorf("Content-Type = %q", got)
	}

	// The path carries a hash of the content, so a new stylesheet is a new
	// URL and this one can be kept forever. That means overriding the
	// surface's no-store, which is right for a page of somebody's answers and
	// wrong for a file compiled into the binary.
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q, want it cacheable", got)
	}

	// And a path that is not the current hash is not served, so a stale link
	// fails loudly rather than returning the wrong bytes.
	if w := a.get(t, "/static/app.deadbeefcafe.css", ""); w.Code == http.StatusOK {
		t.Error("a stylesheet path with the wrong hash was served")
	}
}

// No page on this surface may be framed, and none of them may load a script.
// Asserted here as well as in the policy test because these are the headers a
// sign-in page is worth being certain about.
func TestAuthPagesCarryTheAdminPolicy(t *testing.T) {
	a := newAdmin(t, bootstrapSecret)

	for _, target := range []string{"/signin", "/signin/code", "/signin/bootstrap", "/signin/link?t=x"} {
		t.Run(target, func(t *testing.T) {
			w := a.get(t, target, "")

			csp := w.Header().Get("Content-Security-Policy")

			switch {
			case !strings.Contains(csp, "frame-ancestors 'none'"):
				t.Errorf("a sign-in page may be framed: %q", csp)
			case strings.Contains(csp, "script-src"):
				t.Errorf("a sign-in page allows script: %q", csp)
			case !strings.Contains(csp, "form-action 'self'"):
				t.Errorf("a sign-in page may post elsewhere: %q", csp)
			}

			if w.Header().Get("X-Frame-Options") != "DENY" {
				t.Errorf("X-Frame-Options = %q", w.Header().Get("X-Frame-Options"))
			}
			if w.Header().Get("Strict-Transport-Security") == "" {
				t.Error("no Strict-Transport-Security")
			}
		})
	}
}

// A cross-site write is refused before it reaches a handler, so another site
// cannot walk somebody through signing in or out.
func TestCrossSiteWritesAreRefused(t *testing.T) {
	a := newAdmin(t, bootstrapSecret)

	for _, target := range []string{"/signin", "/signin/link", "/signin/code", "/signin/bootstrap", "/signout"} {
		t.Run(target, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, target, strings.NewReader("email=a%40b.co"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Sec-Fetch-Site", "cross-site")

			w := httptest.NewRecorder()
			a.h.ServeHTTP(w, r)

			if w.Code != http.StatusForbidden {
				t.Errorf("a cross-site POST to %s = %d, want 403", target, w.Code)
			}
		})
	}
}

// A malformed address is the one thing the sign-in page complains about, and
// the complaint is about the text rather than about whether anybody holds it.
func TestAMalformedAddressIsRefusedWithoutRevealingAnything(t *testing.T) {
	a := newAdmin(t, "")

	w := a.post(t, "/signin", url.Values{"email": {"not an address"}}, "")

	if w.Code != http.StatusBadRequest {
		t.Errorf("POST /signin with junk = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Check for a typo") {
		t.Errorf("the page does not say what is wrong:\n%s", w.Body)
	}
	if len(a.sent.Sent) != 0 {
		t.Error("a malformed address still sent mail")
	}
}

// An invitation puts the address in the query so the field arrives filled in.
// It grants nothing -- signing in still means receiving mail at the address --
// so the only question is what the page will display.
func TestTheSignInPageFillsInAnAddressFromTheLink(t *testing.T) {
	a := newAdmin(t, "")

	w := a.get(t, "/signin?email=kitchen%40schoenstatt.test", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /signin = %d, want 200", w.Code)
	}

	if body := w.Body.String(); !strings.Contains(body, `value="kitchen@schoenstatt.test"`) {
		t.Errorf("the address from the link is not in the field:\n%s", body)
	}

	// And anything that is not an address is dropped rather than reflected.
	// html/template would escape it safely; a sign-in field pre-filled with a
	// sentence somebody else wrote is still a sentence somebody else wrote on
	// our page.
	w = a.get(t, "/signin?email="+url.QueryEscape("Call 555-0199 to verify your account"), "")
	switch {
	case w.Code != http.StatusOK:
		t.Fatalf("GET /signin with rubbish = %d, want 200", w.Code)
	case strings.Contains(w.Body.String(), "555-0199"):
		t.Errorf("the page echoed something that is not an address:\n%s", w.Body)
	}
}

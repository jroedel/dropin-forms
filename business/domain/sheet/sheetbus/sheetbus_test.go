package sheetbus_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/sheet/sheetbus"
	"github.com/jroedel/dropin-forms/business/domain/sheet/stores/sheetdb"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/google"
	"github.com/jroedel/dropin-forms/foundation/mail"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// --- the world around a sheet -----------------------------------------------------

type forms map[types.Slug]formbus.Form

func (f forms) ByID(s types.Slug) (formbus.Form, error) {
	if out, ok := f[s]; ok {
		return out, nil
	}

	return formbus.Form{}, formbus.ErrNotFound
}

type subs struct {
	all    []submissionbus.Submission
	hidden map[types.ID]bool
}

func (s *subs) ForSheet(context.Context, types.Slug) ([]submissionbus.Submission, map[types.ID]bool, error) {
	return slices.Clone(s.all), s.hidden, nil
}

type access map[types.ID]bool

func (a access) Allowed(_ context.Context, u types.ID, _ types.Slug, _ accessbus.Role) (bool, error) {
	return a[u], nil
}

type accounts map[types.ID]userbus.User

func (a accounts) ByID(_ context.Context, id types.ID) (userbus.User, error) {
	return a[id], nil
}

type world struct {
	bus    *sheetbus.Business
	google *fakeGoogle
	forms  forms
	subs   *subs
	access access
	sent   *mail.Recorder
	user   types.ID
	form   types.Slug
	now    time.Time
	n      int
}

var zone = time.FixedZone("CST", -6*3600)

func newWorld(t *testing.T) *world {
	t.Helper()

	db, err := sqldb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	if err := sheetdb.Init(t.Context(), db); err != nil {
		t.Fatalf("sheetdb.Init: %v", err)
	}

	form, _ := types.ParseSlug("advent-retreat")
	user := types.NewID()
	email, _ := types.ParseEmail("secretary@schoenstatt.test")

	w := &world{
		google: newFakeGoogle("secretary@gmail.test"),
		forms: forms{form: {
			ID: form, Title: "Advent Retreat", Currency: "usd", Fee: 3500,
			Fields: []formbus.Field{
				{Name: "name", Label: "Your name", Kind: formbus.KindText},
				{Name: "diet", Label: "Diet", Kind: formbus.KindText},
			},
		}},
		subs:   &subs{hidden: map[types.ID]bool{}},
		access: access{user: true},
		sent:   &mail.Recorder{},
		user:   user,
		form:   form,
		now:    time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC),
	}

	bus, err := sheetbus.NewBusiness(sheetbus.Config{
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:        sheetdb.NewStore(db),
		Google:       w.google,
		Forms:        w.forms,
		Subs:         w.subs,
		Access:       w.access,
		Accounts:     accounts{user: {ID: user, Email: email, Enabled: true}},
		Mail:         w.sent,
		SecretBytes:  []byte("a test secret that is long enough"),
		Zone:         zone,
		ZoneName:     "America/Chicago",
		AdminBaseURL: "https://forms.test",
	})
	if err != nil {
		t.Fatalf("NewBusiness: %v", err)
	}

	w.bus = bus

	return w
}

// submit adds a paid registration.
func (w *world) submit(name, diet string) submissionbus.Submission {
	w.n++

	s := submissionbus.Submission{
		ID:        types.NewID(),
		Form:      w.form,
		CreatedAt: w.now.Add(time.Duration(w.n) * time.Minute),
		Status:    submissionbus.StatusPaid,
		Answers: formbus.Answers{
			Currency: "usd",
			Fields: []formbus.Answer{
				{Name: "name", Kind: formbus.KindText, Values: []string{name}},
				{Name: "diet", Kind: formbus.KindText, Values: []string{diet}},
			},
			Total: 3500,
		},
	}

	w.subs.all = append(w.subs.all, s)

	return s
}

func (w *world) change(id types.ID, fn func(*submissionbus.Submission)) {
	for i := range w.subs.all {
		if w.subs.all[i].ID == id {
			fn(&w.subs.all[i])
		}
	}
}

func (w *world) connect(t *testing.T) sheetbus.Connection {
	t.Helper()

	to, verifier, err := w.bus.Begin(t.Context(), w.now, w.user, w.form, "")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	state := strings.TrimPrefix(to, "https://google.test/auth?state=")

	c, err := w.bus.Finish(t.Context(), w.now, w.user, state, "code", verifier)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	return c
}

// pass is the worker's pass, later by d.
func (w *world) pass(t *testing.T, d time.Duration) sheetbus.Connection {
	t.Helper()

	w.now = w.now.Add(d)

	c, _, err := w.bus.Mine(t.Context(), w.user, w.form)
	if err != nil {
		t.Fatalf("Mine: %v", err)
	}

	_ = w.bus.Pass(t.Context(), w.now, c.ID)

	c, _, err = w.bus.Mine(t.Context(), w.user, w.form)
	if err != nil {
		t.Fatalf("Mine: %v", err)
	}

	return c
}

func (w *world) mailed(subject string) int {
	n := 0

	for _, m := range w.sent.Sent {
		if strings.Contains(m.Subject, subject) {
			n++
		}
	}

	return n
}

// --- connecting ---------------------------------------------------------------------

func TestConnectingMakesASpreadsheetAndFillsIt(t *testing.T) {
	w := newWorld(t)

	ana := w.submit("Ana", "=1+1")
	w.submit("Ben", "none")

	unpaid := w.submit("Cy", "")
	w.change(unpaid.ID, func(s *submissionbus.Submission) { s.Status = submissionbus.StatusPending })

	c := w.connect(t)

	if c.State != sheetbus.StateOK || c.Account != "secretary@gmail.test" || c.URL == "" {
		t.Fatalf("connected: %+v", c)
	}

	b, tab := w.google.only(t)
	if b.title != "Advent Retreat" || tab.title != "Responses" {
		t.Errorf("made %q / %q", b.title, tab.title)
	}

	// The heading row and two rows: Cy, still in checkout, is nobody yet.
	if len(tab.rows) != 3 {
		t.Fatalf("%d rows, want a heading and 2:\n%v", len(tab.rows), tab.rows)
	}

	if got := tab.cell(0, tab.column("q:name")); got != "Your name" {
		t.Errorf("the name column's heading is %v", got)
	}

	r := tab.rowOf(ana.ID.String())

	// Typed: a date and a number, and an answer that looks like a formula
	// kept as the text it is.
	if _, ok := tab.cell(r, tab.column("received")).(float64); !ok {
		t.Errorf("Received is %T, want a date", tab.cell(r, tab.column("received")))
	}

	if got := tab.cell(r, tab.column("total")); got != 35.0 {
		t.Errorf("Total is %v, want 35", got)
	}

	if got := tab.cell(r, tab.column("q:diet")); got != "=1+1" {
		t.Errorf("an answer beginning with = is %v", got)
	}

	if tab.protected == 0 {
		t.Error("the form's columns carry no warning against editing them")
	}
}

func TestAStateFromAnotherPersonOrPermissionWithheldIsRefused(t *testing.T) {
	w := newWorld(t)

	to, verifier, err := w.bus.Begin(t.Context(), w.now, w.user, w.form, "")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	state := strings.TrimPrefix(to, "https://google.test/auth?state=")

	if _, err := w.bus.Finish(t.Context(), w.now, types.NewID(), state, "code", verifier); !errors.Is(err, sheetbus.ErrBadState) {
		t.Errorf("another person finishing = %v, want ErrBadState", err)
	}

	if _, err := w.bus.Finish(t.Context(), w.now.Add(time.Hour), w.user, state, "code", verifier); !errors.Is(err, sheetbus.ErrBadState) {
		t.Errorf("finishing an hour later = %v, want ErrBadState", err)
	}

	// The Drive permission unticked on Google's consent screen.
	w.google.grant.Files = false

	if _, err := w.bus.Finish(t.Context(), w.now, w.user, state, "code", verifier); !errors.Is(err, sheetbus.ErrNoFiles) {
		t.Errorf("finishing without the files permission = %v, want ErrNoFiles", err)
	}

	if len(w.google.books) != 0 {
		t.Error("a spreadsheet was made without permission to make one")
	}

	// Somebody who cannot read the answers cannot start.
	delete(w.access, w.user)

	if _, _, err := w.bus.Begin(t.Context(), w.now, w.user, w.form, ""); !errors.Is(err, sheetbus.ErrNoAccess) {
		t.Errorf("Begin without access = %v", err)
	}
}

// --- keeping it -------------------------------------------------------------------------

func TestASheetSomebodyRearrangedIsStillKeptInPlace(t *testing.T) {
	w := newWorld(t)

	ana := w.submit("Ana", "")
	ben := w.submit("Ben", "")
	w.connect(t)

	_, tab := w.google.only(t)

	// Notes of their own beside the rows, the tab renamed, the rows sorted
	// Z to A by name, and the Diet column dragged to the front.
	notes := len(tab.colTags) + 1
	tab.set(0, notes, "Room")
	tab.set(tab.rowOf(ana.ID.String()), notes, "Room 4")
	tab.set(tab.rowOf(ben.ID.String()), notes, "Room 7")
	tab.title = "Who's coming"
	tab.sortBelowHeader(tab.column("q:name"))
	tab.moveColumn(tab.column("q:diet"), 0)

	w.change(ana.ID, func(s *submissionbus.Submission) { s.Answers.Fields[1].Values = []string{"vegan"} })
	cy := w.submit("Cy", "")

	c := w.pass(t, 3*time.Minute)
	if c.State != sheetbus.StateOK || c.Problem != "" {
		t.Fatalf("after the pass: %s %q", c.State, c.Problem)
	}

	_, tab = w.google.only(t)

	if r := tab.rowOf(ana.ID.String()); tab.cell(r, tab.column("q:diet")) != "vegan" || tab.cell(r, notes) != "Room 4" {
		t.Errorf("Ana's row: diet %v, note %v", tab.cell(r, tab.column("q:diet")), tab.cell(r, notes))
	}

	if r := tab.rowOf(ben.ID.String()); tab.cell(r, notes) != "Room 7" {
		t.Errorf("Ben's note moved: %v", tab.cell(r, notes))
	}

	if r := tab.rowOf(cy.ID.String()); r < 0 || tab.cell(r, tab.column("q:name")) != "Cy" {
		t.Errorf("Cy was not added: row %d", r)
	}
}

func TestNothingIsAskedOfGoogleWhenNothingHasChanged(t *testing.T) {
	w := newWorld(t)

	w.submit("Ana", "")
	w.connect(t)

	before := w.google.asked
	w.pass(t, 3*time.Minute)

	if w.google.asked != before {
		t.Errorf("a pass with nothing new made %d requests to Google", w.google.asked-before)
	}

	// An hour on it reads the sheet anyway, and puts back a cell somebody
	// typed over.
	_, tab := w.google.only(t)
	tab.set(1, tab.column("q:name"), "Typo")

	w.pass(t, time.Hour)

	if _, tab = w.google.only(t); tab.cell(1, tab.column("q:name")) != "Ana" {
		t.Errorf("an edited cell is %v after the hourly read", tab.cell(1, tab.column("q:name")))
	}
}

func TestWhatTheyDeleteStaysDeletedUntilTheyRefill(t *testing.T) {
	w := newWorld(t)

	ana := w.submit("Ana", "")
	ben := w.submit("Ben", "")
	w.connect(t)

	_, tab := w.google.only(t)
	tab.deleteRow(tab.rowOf(ana.ID.String()))
	tab.deleteColumn(tab.column("q:diet"))

	// Ana changes her answers; a question is added to the form.
	w.change(ana.ID, func(s *submissionbus.Submission) { s.Answers.Fields[0].Values = []string{"Ana María"} })
	f := w.forms[w.form]
	f.Fields = append(f.Fields, formbus.Field{Name: "room", Label: "Room wanted", Kind: formbus.KindText})
	w.forms[w.form] = f

	w.pass(t, 3*time.Minute)

	_, tab = w.google.only(t)

	if tab.rowOf(ana.ID.String()) >= 0 {
		t.Error("Ana's deleted row was put back")
	}

	if tab.column("q:diet") >= 0 {
		t.Error("the deleted Diet column was put back")
	}

	if tab.column("q:room") < 0 || tab.cell(0, tab.column("q:room")) != "Room wanted" {
		t.Error("the new question's column did not arrive")
	}

	if tab.rowOf(ben.ID.String()) < 0 {
		t.Error("Ben's row went missing")
	}

	if _, err := w.bus.Now(t.Context(), w.now, w.user, w.form, true); err != nil {
		t.Fatalf("Refill: %v", err)
	}

	_, tab = w.google.only(t)

	if r := tab.rowOf(ana.ID.String()); r < 0 || tab.cell(r, tab.column("q:name")) != "Ana María" {
		t.Errorf("Refill did not bring Ana back: row %d", r)
	}

	if tab.column("q:diet") < 0 {
		t.Error("Refill did not bring the Diet column back")
	}
}

func TestRowsThatStopCountingStayWithTheirStatus(t *testing.T) {
	w := newWorld(t)

	ana := w.submit("Ana", "")
	ben := w.submit("Ben", "")
	w.connect(t)

	w.subs.hidden[ana.ID] = true
	w.change(ben.ID, func(s *submissionbus.Submission) { s.Status = submissionbus.StatusCancelled; s.Refunded = 3500 })

	// Somebody hidden before the sheet ever had them is not added.
	cy := w.submit("Cy", "")
	w.subs.hidden[cy.ID] = true

	w.pass(t, 3*time.Minute)

	_, tab := w.google.only(t)
	status := tab.column("status")

	if got := tab.cell(tab.rowOf(ana.ID.String()), status); got != "hidden" {
		t.Errorf("Ana's status is %v, want hidden", got)
	}

	if got := tab.cell(tab.rowOf(ben.ID.String()), status); got != "cancelled" {
		t.Errorf("Ben's status is %v, want cancelled", got)
	}

	if tab.rowOf(cy.ID.String()) >= 0 {
		t.Error("Cy, hidden before the sheet saw them, was added")
	}
}

// --- when it is beyond use, or out of reach ---------------------------------------

func TestASheetBeyondUseGetsANewTabAndTheyAreTold(t *testing.T) {
	for _, damage := range []struct {
		name string
		do   func(b *book, t *tab)
	}{
		{"the tab deleted", func(b *book, t *tab) {
			for k := range b.tabs {
				delete(b.tabs, k)
			}
		}},
		{"the Reference column deleted", func(b *book, t *tab) { t.deleteColumn(t.column("ref")) }},
	} {
		t.Run(damage.name, func(t *testing.T) {
			w := newWorld(t)

			ana := w.submit("Ana", "")
			w.connect(t)

			b, old := w.google.only(t)
			damage.do(b, old)
			old.set(1, 20, "my note")

			c := w.pass(t, time.Hour+time.Minute)
			if c.State != sheetbus.StateOK {
				t.Fatalf("after the rebuild: %s %q", c.State, c.Problem)
			}

			_, tab := w.google.only(t)
			if !strings.HasPrefix(tab.title, "Responses (rebuilt ") || tab.rowOf(ana.ID.String()) < 0 {
				t.Errorf("the new tab is %q with Ana at row %d", tab.title, tab.rowOf(ana.ID.String()))
			}

			if w.mailed("We rebuilt your Google Sheet's tab") != 1 {
				t.Errorf("sent %v", w.sent.Sent)
			}

			if len(b.tabs) == 2 && old.cell(1, 20) != "my note" {
				t.Error("the old tab was changed")
			}
		})
	}
}

func TestATrashedSheetOrWithdrawnPermissionIsToldOnce(t *testing.T) {
	w := newWorld(t)
	w.submit("Ana", "")
	w.connect(t)

	b, _ := w.google.only(t)
	b.trashed = true

	if c := w.pass(t, time.Hour+time.Minute); c.State != sheetbus.StateGone {
		t.Fatalf("a trashed spreadsheet left the connection %s", c.State)
	}

	w.pass(t, time.Hour)

	if n := w.mailed("can't be reached"); n != 1 {
		t.Errorf("told %d times about the trashed spreadsheet", n)
	}

	// Made again from the page, and kept.
	b.trashed = false

	if c, err := w.bus.Remake(t.Context(), w.now, w.user, w.form); err != nil || c.State != sheetbus.StateOK || c.Spreadsheet != "ss2" {
		t.Fatalf("Remake = %+v, %v", c, err)
	}

	// Permission withdrawn in the Google account. Noticed once the access
	// token held from the last refresh runs out, within the hour.
	w.google.revoked["1//refresh"] = true

	if c := w.pass(t, time.Hour+time.Minute); c.State != sheetbus.StateReconnect {
		t.Fatalf("a withdrawn permission left the connection %s", c.State)
	}

	if n := w.mailed("Reconnect your Google Sheet"); n != 1 {
		t.Errorf("told %d times to reconnect", n)
	}
}

func TestLosingAccessStopsTheSheetAndRegainingItResumes(t *testing.T) {
	w := newWorld(t)
	w.submit("Ana", "")
	w.connect(t)

	delete(w.access, w.user)

	if c := w.pass(t, 3*time.Minute); c.State != sheetbus.StateStopped {
		t.Fatalf("losing access left the connection %s", c.State)
	}

	w.pass(t, 3*time.Minute)

	if n := w.mailed("stopped updating"); n != 1 {
		t.Errorf("told %d times that it stopped", n)
	}

	before := w.google.applied
	w.submit("Ben", "")
	w.pass(t, 3*time.Minute)

	if w.google.applied != before {
		t.Error("the sheet was written to while access was gone")
	}

	w.access[w.user] = true

	if c := w.pass(t, 3*time.Minute); c.State != sheetbus.StateOK {
		t.Fatalf("regaining access left the connection %s", c.State)
	}

	if _, tab := w.google.only(t); len(tab.rows) != 3 {
		t.Errorf("Ben did not arrive once access was back: %d rows", len(tab.rows))
	}
}

func TestAFormDeletedStopsTheSheet(t *testing.T) {
	w := newWorld(t)
	w.submit("Ana", "")
	w.connect(t)

	delete(w.forms, w.form)

	if c := w.pass(t, 3*time.Minute); c.State != sheetbus.StateStopped || !strings.Contains(c.Problem, "deleted") {
		t.Fatalf("a deleted form left the connection %s %q", c.State, c.Problem)
	}

	if w.mailed("stopped updating") != 1 {
		t.Errorf("sent %v", w.sent.Sent)
	}
}

func TestGoogleBusyIsRetriedLessOftenAndToldAfterADay(t *testing.T) {
	w := newWorld(t)
	w.submit("Ana", "")
	w.connect(t)

	w.google.busy = google.Busy{Message: "rate limited"}
	w.submit("Ben", "")

	c := w.pass(t, 3*time.Minute)
	if c.State != sheetbus.StateOK || c.Problem == "" || !c.NextAt.After(w.now) {
		t.Fatalf("a busy Google: %s %q next %v", c.State, c.Problem, c.NextAt)
	}

	first := c.NextAt.Sub(w.now)

	c = w.pass(t, first)
	if second := c.NextAt.Sub(w.now); second <= first {
		t.Errorf("the second wait %v is not longer than the first %v", second, first)
	}

	if w.mailed("has not updated") != 0 {
		t.Error("told about a failure within minutes")
	}

	c = w.pass(t, 25*time.Hour)
	w.pass(t, time.Hour)

	if n := w.mailed("has not updated for a day"); n != 1 {
		t.Errorf("told %d times after a day of failures", n)
	}

	w.google.busy = nil

	if c = w.pass(t, time.Hour); c.Problem != "" || c.State != sheetbus.StateOK {
		t.Errorf("once Google answers: %s %q", c.State, c.Problem)
	}
}

func TestDisconnectingWithdrawsThePermissionAndKeepsTheSpreadsheet(t *testing.T) {
	w := newWorld(t)
	w.submit("Ana", "")
	w.connect(t)

	if err := w.bus.Disconnect(t.Context(), w.user, w.form); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	if !w.google.revoked["1//refresh"] {
		t.Error("the permission was not withdrawn at Google")
	}

	if len(w.google.books) != 1 {
		t.Error("the spreadsheet was removed; it is theirs")
	}

	if _, _, err := w.bus.Mine(t.Context(), w.user, w.form); !errors.Is(err, sheetbus.ErrNotFound) {
		t.Errorf("after disconnecting, Mine = %v", err)
	}
}

func TestADeletedHeadingRowIsPutBackWithoutTouchingTheirRows(t *testing.T) {
	w := newWorld(t)

	ana := w.submit("Ana", "")
	w.connect(t)

	_, tab := w.google.only(t)
	tab.deleteRow(0)

	// A row of their own, with no reference, at the bottom.
	tab.set(5, tab.column("q:name"), "Fr. Visiting (not on the form)")

	w.pass(t, time.Hour+time.Minute)

	_, tab = w.google.only(t)

	if got := tab.cell(0, tab.column("ref")); got != "Reference" {
		t.Errorf("the top row's Reference cell is %v, want the heading back", got)
	}

	if r := tab.rowOf(ana.ID.String()); r != 1 || tab.cell(r, tab.column("q:name")) != "Ana" {
		t.Errorf("Ana is at row %d", r)
	}

	found := false
	for _, row := range tab.rows {
		if slices.Contains(row, any("Fr. Visiting (not on the form)")) {
			found = true
		}
	}

	if !found {
		t.Error("their own row was lost")
	}
}

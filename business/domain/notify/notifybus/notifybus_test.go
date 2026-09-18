package notifybus_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/notify/notifybus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/mail"
)

// Who is told, and what they are told, with no HTTP and no relay.
//
// The interesting half of this package is the first: a submission that reaches
// nobody is the failure the whole step exists to fix, and it fails quietly by
// nature -- an empty notify list looks exactly like a working one.

var when = time.Date(2026, 9, 20, 15, 4, 0, 0, time.UTC)

// --- the fakes ----------------------------------------------------------------

type forms struct {
	form formbus.Form
	err  error
}

func (f forms) ByID(types.Slug) (formbus.Form, error) { return f.form, f.err }

type submissions struct {
	sub submissionbus.Submission
	err error
}

func (s submissions) ByID(context.Context, types.ID) (submissionbus.Submission, error) {
	return s.sub, s.err
}

type grants struct {
	list []accessbus.Grant
	err  error
}

func (g grants) ForForm(context.Context, types.Slug) ([]accessbus.Grant, error) {
	return g.list, g.err
}

type accounts struct {
	users map[types.ID]userbus.User
	err   error
}

func (a accounts) ByID(_ context.Context, id types.ID) (userbus.User, error) {
	if a.err != nil {
		return userbus.User{}, a.err
	}

	u, ok := a.users[id]
	if !ok {
		return userbus.User{}, userbus.ErrNotFound
	}

	return u, nil
}

// --- the fixtures --------------------------------------------------------------

func mustSlug(t *testing.T, s string) types.Slug {
	t.Helper()

	slug, err := types.ParseSlug(s)
	if err != nil {
		t.Fatalf("ParseSlug(%q): %v", s, err)
	}

	return slug
}

func mustEmail(t *testing.T, s string) types.Email {
	t.Helper()

	e, err := types.ParseEmail(s)
	if err != nil {
		t.Fatalf("ParseEmail(%q): %v", s, err)
	}

	return e
}

// lunch is a form that sells a ticket and takes a donation, which is the shape
// the shrine's own form has.
func lunch(t *testing.T) formbus.Form {
	t.Helper()

	return formbus.Form{
		ID:           mustSlug(t, "feast-lunch-2026"),
		Title:        "Feast of Our Lady of Schoenstatt",
		Currency:     "usd",
		Confirmation: "Thank you. We look forward to seeing you on October 17th.",
		Items:        []formbus.Item{{ID: "ticket", Label: "Lunch ticket", Price: types.Money(1200)}},
	}
}

func order(t *testing.T, f formbus.Form, total types.Money, status submissionbus.Status) submissionbus.Submission {
	t.Helper()

	return submissionbus.Submission{
		ID:      types.NewID(),
		Form:    f.ID,
		Version: "abc123def456",
		Status:  status,
		Answers: formbus.Answers{
			FormID:   f.ID,
			Currency: "usd",
			Fields: []formbus.Answer{
				{Name: "name", Label: "Your name", Kind: formbus.KindText, Values: []string{"Maria O'Neill"}},
				{Name: "email", Label: "Email for your receipt", Kind: formbus.KindEmail, Values: []string{"maria@example.org"}},
				{Name: "donation", Label: "Donation for the Shrine", Kind: formbus.KindAmount, Values: []string{"10.00"}},
			},
			Lines: []formbus.Line{
				{ItemID: "ticket", Label: "Lunch ticket", Price: types.Money(1200), Qty: 2, Amount: types.Money(2400)},
			},
			Total: total,
		},
		Email:     mustEmail(t, "maria@example.org"),
		CreatedAt: when,
		UpdatedAt: when,
	}
}

// harness is a notifier over recorded mail and a captured log.
type harness struct {
	b     *notifybus.Business
	sent  *mail.Recorder
	lines *strings.Builder
}

func newHarness(t *testing.T, cfg notifybus.Config) harness {
	t.Helper()

	var lines strings.Builder

	sent := &mail.Recorder{}

	cfg.Log = slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg.Mail = sent

	b, err := notifybus.NewBusiness(cfg)
	if err != nil {
		t.Fatalf("NewBusiness: %v", err)
	}

	return harness{b: b, sent: sent, lines: &lines}
}

// to reports which addresses were written to, in order.
func (h harness) to() []string {
	out := make([]string, 0, len(h.sent.Sent))
	for _, m := range h.sent.Sent {
		out = append(out, m.To)
	}

	return out
}

func (h harness) messageTo(t *testing.T, address string) mail.Message {
	t.Helper()

	for _, m := range h.sent.Sent {
		if m.To == address {
			return m
		}
	}

	t.Fatalf("nothing was sent to %s; only %v", address, h.to())

	return mail.Message{}
}

// --- who is told ----------------------------------------------------------------

func TestTheSubmitterAndTheOfficeAreBothTold(t *testing.T) {
	f := lunch(t)
	sub := order(t, f, 0, submissionbus.StatusReceived)

	h := newHarness(t, notifybus.Config{
		Forms:       forms{form: f},
		Submissions: submissions{sub: sub},
		Office:      "office@schoenstatt.test",
	})

	h.b.Received(t.Context(), sub.ID)

	if got := len(h.sent.Sent); got != 2 {
		t.Fatalf("%d messages sent, want one to the submitter and one to the office: %v", got, h.to())
	}

	mine := h.messageTo(t, "maria@example.org")
	if !strings.Contains(mine.Subject, f.Title) {
		t.Errorf("the submitter's subject does not name the form: %q", mine.Subject)
	}
	if !strings.Contains(mine.Text, "Thank you. We look forward") {
		t.Errorf("the submitter was not sent the form's own confirmation:\n%s", mine.Text)
	}

	// Their own answers, because a form in a frame on somebody else's website
	// leaves them no other copy.
	if !strings.Contains(mine.Text, "Maria O'Neill") {
		t.Errorf("the submitter's message does not repeat what they sent:\n%s", mine.Text)
	}

	office := h.messageTo(t, "office@schoenstatt.test")
	if !strings.Contains(office.Subject, "New submission") {
		t.Errorf("the office subject is %q", office.Subject)
	}
	if !strings.Contains(office.Text, "Maria O'Neill") {
		t.Errorf("the office was not told who submitted:\n%s", office.Text)
	}
}

// Three sources of recipients and one message each, however many of them name
// the same person.
func TestEverybodyIsToldOnceAndOnlyOnce(t *testing.T) {
	f := lunch(t)

	// The kitchen is on the form's list *and* holds results, spelled
	// differently. That is one person and one message.
	f.Notify = []string{"kitchen@schoenstatt.test", "office@schoenstatt.test"}

	sub := order(t, f, 0, submissionbus.StatusReceived)

	reader := types.NewID()
	editor := types.NewID()
	gone := types.NewID()

	h := newHarness(t, notifybus.Config{
		Forms:       forms{form: f},
		Submissions: submissions{sub: sub},
		Office:      "office@schoenstatt.test",
		Grants: grants{list: []accessbus.Grant{
			{UserID: reader, Form: f.ID, Role: accessbus.RoleResults},
			{UserID: editor, Role: accessbus.RoleAdmin},
			{UserID: gone, Form: f.ID, Role: accessbus.RoleResults},
		}},
		Accounts: accounts{users: map[types.ID]userbus.User{
			reader: {ID: reader, Email: mustEmail(t, "KITCHEN@schoenstatt.test"), Enabled: true},
			editor: {ID: editor, Email: mustEmail(t, "pastor@schoenstatt.test"), Enabled: true},
			gone:   {ID: gone, Email: mustEmail(t, "left@schoenstatt.test"), Enabled: false},
		}},
	})

	h.b.Received(t.Context(), sub.ID)

	want := map[string]int{
		"maria@example.org":        1, // the submitter
		"office@schoenstatt.test":  1, // the configured address, named twice
		"kitchen@schoenstatt.test": 1, // the form's list and a grant, spelled two ways
		"pastor@schoenstatt.test":  1, // a site-wide admin, because admin includes results
	}

	got := map[string]int{}
	for _, to := range h.to() {
		got[strings.ToLower(to)]++
	}

	for address, n := range want {
		if got[address] != n {
			t.Errorf("%s got %d messages, want %d (all: %v)", address, got[address], n, h.to())
		}
	}

	// Somebody who has left keeps their grant until it is revoked, and mail to
	// them is mail nobody reads.
	if got["left@schoenstatt.test"] != 0 {
		t.Error("a disabled account was notified")
	}

	if len(h.to()) != len(want) {
		t.Errorf("%d messages in total, want %d: %v", len(h.to()), len(want), h.to())
	}
}

// The failure this step exists to prevent is quiet by nature: an empty list
// looks exactly like a working one.
func TestASubmissionNobodyHearsAboutIsAWarning(t *testing.T) {
	f := lunch(t)
	sub := order(t, f, 0, submissionbus.StatusReceived)
	sub.Email = types.Email{}

	h := newHarness(t, notifybus.Config{
		Forms:       forms{form: f},
		Submissions: submissions{sub: sub},
	})

	h.b.Received(t.Context(), sub.ID)

	if len(h.sent.Sent) != 0 {
		t.Fatalf("something was sent with nobody to send to: %v", h.to())
	}

	if !strings.Contains(h.lines.String(), "nobody was notified") {
		t.Errorf("nothing in the log says the submission reached nobody:\n%s", h.lines.String())
	}
}

// A form that did not ask for an address still notifies the office.
func TestAFormWithNoAddressStillTellsTheOffice(t *testing.T) {
	f := lunch(t)
	sub := order(t, f, 0, submissionbus.StatusReceived)
	sub.Email = types.Email{}

	h := newHarness(t, notifybus.Config{
		Forms:       forms{form: f},
		Submissions: submissions{sub: sub},
		Office:      "office@schoenstatt.test",
	})

	h.b.Received(t.Context(), sub.ID)

	if got := h.to(); len(got) != 1 || got[0] != "office@schoenstatt.test" {
		t.Errorf("sent to %v, want only the office", got)
	}
}

// Losing the grant lookup should cost the notifications it names and not the
// ones it does not.
func TestAFailedGrantLookupStillNotifiesTheRest(t *testing.T) {
	f := lunch(t)
	f.Notify = []string{"kitchen@schoenstatt.test"}

	sub := order(t, f, 0, submissionbus.StatusReceived)

	h := newHarness(t, notifybus.Config{
		Forms:       forms{form: f},
		Submissions: submissions{sub: sub},
		Office:      "office@schoenstatt.test",
		Grants:      grants{err: errors.New("the database is gone")},
		Accounts:    accounts{},
	})

	h.b.Received(t.Context(), sub.ID)

	if len(h.to()) != 3 {
		t.Errorf("sent to %v, want the submitter, the office and the kitchen", h.to())
	}
	if !strings.Contains(h.lines.String(), "could not be read") {
		t.Errorf("the lookup failure was not logged:\n%s", h.lines.String())
	}
}

// --- what they are told -----------------------------------------------------------

func TestAPaidOrderSaysSoAndAddsUp(t *testing.T) {
	f := lunch(t)
	sub := order(t, f, types.Money(3400), submissionbus.StatusPaid)

	h := newHarness(t, notifybus.Config{
		Forms:        forms{form: f},
		Submissions:  submissions{sub: sub},
		Office:       "office@schoenstatt.test",
		AdminBaseURL: "https://forms.test",
	})

	h.b.Paid(t.Context(), sub.ID)

	mine := h.messageTo(t, "maria@example.org")

	if !strings.Contains(mine.Subject, "payment is confirmed") {
		t.Errorf("the submitter's subject is %q", mine.Subject)
	}

	// The receipt has to carry the donation as well as the tickets. A line
	// list built from the priced items alone charges for the lunch and drops
	// the gift, which is a real bug this service has already had once.
	for _, want := range []string{"2 x Lunch ticket -- $24.00", "Donation for the Shrine -- $10.00", "Total: $34.00"} {
		if !strings.Contains(mine.Text, want) {
			t.Errorf("the receipt is missing %q:\n%s", want, mine.Text)
		}
	}

	office := h.messageTo(t, "office@schoenstatt.test")

	if !strings.Contains(office.Subject, "Payment received, $34.00") {
		t.Errorf("the office subject is %q; the amount belongs in it", office.Subject)
	}
	if !strings.Contains(office.Text, "(paid)") {
		t.Errorf("the office was not told the money arrived:\n%s", office.Text)
	}

	// And a link to the row itself, because a notification that says "go and
	// look" is one that gets read in a car park and forgotten.
	want := "https://forms.test/forms/feast-lunch-2026/submissions/" + sub.ID.String()
	if !strings.Contains(office.Text, want) {
		t.Errorf("the office message does not link to the submission:\n%s", office.Text)
	}
}

// With no admin address configured there is no link, and the message still
// says everything it has to say.
func TestWithNoAdminAddressThereIsNoLink(t *testing.T) {
	f := lunch(t)
	sub := order(t, f, 0, submissionbus.StatusReceived)

	h := newHarness(t, notifybus.Config{
		Forms:       forms{form: f},
		Submissions: submissions{sub: sub},
		Office:      "office@schoenstatt.test",
	})

	h.b.Received(t.Context(), sub.ID)

	office := h.messageTo(t, "office@schoenstatt.test")

	if strings.Contains(office.Text, "http") {
		t.Errorf("a link was written with no base URL configured:\n%s", office.Text)
	}
	if !strings.Contains(office.Text, "Maria O'Neill") {
		t.Errorf("the message lost its content along with the link:\n%s", office.Text)
	}
}

// --- what goes wrong -----------------------------------------------------------

// Nothing here fails a caller, because by the time it runs the submission is
// stored and, on the paid path, the money is collected.
func TestAFailureIsALogLineAndNothingElse(t *testing.T) {
	f := lunch(t)
	sub := order(t, f, 0, submissionbus.StatusReceived)

	for name, cfg := range map[string]notifybus.Config{
		"the submission cannot be read": {
			Forms:       forms{form: f},
			Submissions: submissions{err: errors.New("no such row")},
			Office:      "office@schoenstatt.test",
		},
		"the form cannot be read": {
			Forms:       forms{err: errors.New("no such form")},
			Submissions: submissions{sub: sub},
			Office:      "office@schoenstatt.test",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, cfg)

			h.b.Received(t.Context(), sub.ID)

			if len(h.sent.Sent) != 0 {
				t.Errorf("something was sent anyway: %v", h.to())
			}
			if !strings.Contains(h.lines.String(), "nobody could be told") {
				t.Errorf("the failure was not logged:\n%s", h.lines.String())
			}
		})
	}
}

// A relay that is down must not take the request with it.
func TestARelayThatRefusesIsLoggedPerMessage(t *testing.T) {
	f := lunch(t)
	sub := order(t, f, 0, submissionbus.StatusReceived)

	h := newHarness(t, notifybus.Config{
		Forms:       forms{form: f},
		Submissions: submissions{sub: sub},
		Office:      "office@schoenstatt.test",
	})

	// Replacing the recorder after construction, which is the one thing a test
	// can do that an operator cannot.
	broken := notifybus.Config{
		Log:         slog.New(slog.NewTextHandler(h.lines, &slog.HandlerOptions{Level: slog.LevelInfo})),
		Mail:        refuser{},
		Forms:       forms{form: f},
		Submissions: submissions{sub: sub},
		Office:      "office@schoenstatt.test",
	}

	b, err := notifybus.NewBusiness(broken)
	if err != nil {
		t.Fatalf("NewBusiness: %v", err)
	}

	b.Received(t.Context(), sub.ID)

	if got := strings.Count(h.lines.String(), "could not be sent"); got != 2 {
		t.Errorf("%d failures logged, want one per message:\n%s", got, h.lines.String())
	}
}

type refuser struct{}

func (refuser) Send(context.Context, mail.Message) error {
	return errors.New("the relay is not answering")
}

// A config that cannot work says so while the process is starting, rather than
// dereferencing nil at the end of the one path that takes somebody's money.
func TestAnUnusableConfigIsRefusedAtStartup(t *testing.T) {
	log := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))

	for name, cfg := range map[string]notifybus.Config{
		"no logger": {Mail: &mail.Recorder{}, Forms: forms{}, Submissions: submissions{}},
		"no mailer": {Log: log, Forms: forms{}, Submissions: submissions{}},
		"no forms":  {Log: log, Mail: &mail.Recorder{}, Submissions: submissions{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := notifybus.NewBusiness(cfg); err == nil {
				t.Error("it was accepted")
			}
		})
	}
}

// --- turning it off ------------------------------------------------------------

// mutes is a notifybus.Mutes in a map.
type mutes struct {
	off map[string]bool
	err error
}

func newMutes() *mutes { return &mutes{off: map[string]bool{}} }

func key(userID types.ID, form types.Slug) string { return userID.String() + "|" + form.String() }

func (m *mutes) Muted(_ context.Context, userID types.ID, form types.Slug) (bool, error) {
	if m.err != nil {
		return false, m.err
	}

	return m.off[key(userID, form)], nil
}

func (m *mutes) MutedForForm(_ context.Context, form types.Slug) ([]types.ID, error) {
	if m.err != nil {
		return nil, m.err
	}

	var out []types.ID

	for k, on := range m.off {
		if !on {
			continue
		}

		id, slug, _ := strings.Cut(k, "|")
		if slug != form.String() {
			continue
		}

		parsed, err := types.ParseID(id)
		if err != nil {
			continue
		}

		out = append(out, parsed)
	}

	return out, nil
}

func (m *mutes) MutedForUser(_ context.Context, userID types.ID) ([]types.Slug, error) {
	if m.err != nil {
		return nil, m.err
	}

	var out []types.Slug

	for k, on := range m.off {
		id, slug, _ := strings.Cut(k, "|")
		if !on || id != userID.String() {
			continue
		}

		parsed, err := types.ParseSlug(slug)
		if err != nil {
			continue
		}

		out = append(out, parsed)
	}

	return out, nil
}

func (m *mutes) Mute(_ context.Context, userID types.ID, form types.Slug, _ time.Time) error {
	if m.err != nil {
		return m.err
	}

	m.off[key(userID, form)] = true

	return nil
}

func (m *mutes) Unmute(_ context.Context, userID types.ID, form types.Slug) error {
	if m.err != nil {
		return m.err
	}

	delete(m.off, key(userID, form))

	return nil
}

// The whole feature, from the domain's side: somebody who has said no is not
// told, and everybody else still is.
func TestSomebodyWhoHasTurnedItOffIsNotTold(t *testing.T) {
	f := lunch(t)
	sub := order(t, f, 0, submissionbus.StatusReceived)

	quiet := types.NewID()
	loud := types.NewID()

	off := newMutes()
	off.off[key(quiet, f.ID)] = true

	h := newHarness(t, notifybus.Config{
		Forms:       forms{form: f},
		Submissions: submissions{sub: sub},
		Mutes:       off,
		Grants: grants{list: []accessbus.Grant{
			{UserID: quiet, Form: f.ID, Role: accessbus.RoleResults},
			{UserID: loud, Form: f.ID, Role: accessbus.RoleResults},
		}},
		Accounts: accounts{users: map[types.ID]userbus.User{
			quiet: {ID: quiet, Email: mustEmail(t, "quiet@schoenstatt.test"), Enabled: true},
			loud:  {ID: loud, Email: mustEmail(t, "loud@schoenstatt.test"), Enabled: true},
		}},
	})

	h.b.Received(t.Context(), sub.ID)

	for _, to := range h.to() {
		if to == "quiet@schoenstatt.test" {
			t.Errorf("somebody who turned this form off was emailed anyway: %v", h.to())
		}
	}

	h.messageTo(t, "loud@schoenstatt.test")
}

// A preference is per person and per form. Turning one form off does not turn
// another off, and does not turn anybody else's off.
func TestAPreferenceIsOnePersonAndOneForm(t *testing.T) {
	f := lunch(t)
	sub := order(t, f, 0, submissionbus.StatusReceived)

	reader := types.NewID()
	other := mustSlug(t, "another-form")

	off := newMutes()
	off.off[key(reader, other)] = true

	h := newHarness(t, notifybus.Config{
		Forms:       forms{form: f},
		Submissions: submissions{sub: sub},
		Mutes:       off,
		Grants:      grants{list: []accessbus.Grant{{UserID: reader, Form: f.ID, Role: accessbus.RoleResults}}},
		Accounts: accounts{users: map[types.ID]userbus.User{
			reader: {ID: reader, Email: mustEmail(t, "reader@schoenstatt.test"), Enabled: true},
		}},
	})

	h.b.Received(t.Context(), sub.ID)

	h.messageTo(t, "reader@schoenstatt.test")
}

// The direction a failure falls matters more here than most places: somebody
// getting an email they switched off is an annoyance, and an order nobody
// hears about is the failure this package exists to prevent.
func TestAPreferenceStoreThatIsDownTellsEverybody(t *testing.T) {
	f := lunch(t)
	sub := order(t, f, 0, submissionbus.StatusReceived)

	reader := types.NewID()

	broken := newMutes()
	broken.err = errors.New("the database is gone")

	h := newHarness(t, notifybus.Config{
		Forms:       forms{form: f},
		Submissions: submissions{sub: sub},
		Mutes:       broken,
		Grants:      grants{list: []accessbus.Grant{{UserID: reader, Form: f.ID, Role: accessbus.RoleResults}}},
		Accounts: accounts{users: map[types.ID]userbus.User{
			reader: {ID: reader, Email: mustEmail(t, "reader@schoenstatt.test"), Enabled: true},
		}},
	})

	h.b.Received(t.Context(), sub.ID)

	h.messageTo(t, "reader@schoenstatt.test")

	if !strings.Contains(h.lines.String(), "everybody who holds this form was told") {
		t.Errorf("the failure was not logged:\n%s", h.lines.String())
	}
}

// --- what the last line of the message says -------------------------------------

func TestOnlyAnAccountIsOfferedAWayOut(t *testing.T) {
	f := lunch(t)
	f.Notify = []string{"kitchen@schoenstatt.test"}

	sub := order(t, f, 0, submissionbus.StatusReceived)

	reader := types.NewID()

	muteKey, err := notifybus.ParseMuteKey("a-test-secret-long-enough-to-be-a-key")
	if err != nil {
		t.Fatalf("ParseMuteKey: %v", err)
	}

	h := newHarness(t, notifybus.Config{
		Forms:        forms{form: f},
		Submissions:  submissions{sub: sub},
		Mutes:        newMutes(),
		MuteKey:      muteKey,
		Office:       "office@schoenstatt.test",
		AdminBaseURL: "https://forms.test",
		Grants:       grants{list: []accessbus.Grant{{UserID: reader, Form: f.ID, Role: accessbus.RoleResults}}},
		Accounts: accounts{users: map[types.ID]userbus.User{
			reader: {ID: reader, Email: mustEmail(t, "reader@schoenstatt.test"), Enabled: true},
		}},
	})

	h.b.Received(t.Context(), sub.ID)

	// The account gets a link, and the link is theirs: it decodes to them and
	// to this form.
	mine := h.messageTo(t, "reader@schoenstatt.test")

	_, token, found := strings.Cut(mine.Text, "https://forms.test/notifications/"+f.ID.String()+"?t=")
	if !found {
		t.Fatalf("no unsubscribe link for an account that holds the form:\n%s", mine.Text)
	}

	token = strings.TrimSpace(token)

	user, form, err := notifybus.ReadMuteToken(muteKey, token)
	if err != nil {
		t.Fatalf("the link in the message does not verify: %v", err)
	}
	if user != reader || form != f.ID {
		t.Errorf("the link names %s on %s, want %s on %s", user, form, reader, f.ID)
	}

	// A configured address has nobody to unsubscribe, so it is told where the
	// decision lives instead of being offered a button that would silence a
	// shared mailbox for everybody.
	for _, address := range []string{"office@schoenstatt.test", "kitchen@schoenstatt.test"} {
		m := h.messageTo(t, address)

		if strings.Contains(m.Text, "/notifications/") {
			t.Errorf("%s was offered an unsubscribe link:\n%s", address, m.Text)
		}
		if !strings.Contains(m.Text, "on the notification list") {
			t.Errorf("%s was not told why it is getting this:\n%s", address, m.Text)
		}
	}
}

// With no key there is no link, and the message still says how to stop.
func TestWithNoKeyTheMessageStillSaysHowToStop(t *testing.T) {
	f := lunch(t)
	sub := order(t, f, 0, submissionbus.StatusReceived)

	reader := types.NewID()

	h := newHarness(t, notifybus.Config{
		Forms:        forms{form: f},
		Submissions:  submissions{sub: sub},
		Mutes:        newMutes(),
		AdminBaseURL: "https://forms.test",
		Grants:       grants{list: []accessbus.Grant{{UserID: reader, Form: f.ID, Role: accessbus.RoleResults}}},
		Accounts: accounts{users: map[types.ID]userbus.User{
			reader: {ID: reader, Email: mustEmail(t, "reader@schoenstatt.test"), Enabled: true},
		}},
	})

	h.b.Received(t.Context(), sub.ID)

	m := h.messageTo(t, "reader@schoenstatt.test")

	if strings.Contains(m.Text, "/notifications/") {
		t.Errorf("a link was offered with no key to sign it:\n%s", m.Text)
	}
	if !strings.Contains(m.Text, "sign in") {
		t.Errorf("the message does not say how to stop:\n%s", m.Text)
	}
}

// --- the token ---------------------------------------------------------------------

func TestAMuteTokenNamesItsOwnPairAndNothingElse(t *testing.T) {
	key, err := notifybus.ParseMuteKey("a-test-secret-long-enough-to-be-a-key")
	if err != nil {
		t.Fatalf("ParseMuteKey: %v", err)
	}

	user := types.NewID()
	form := mustSlug(t, "feast-lunch-2026")

	token, err := notifybus.MintMuteToken(key, user, form)
	if err != nil {
		t.Fatalf("MintMuteToken: %v", err)
	}

	gotUser, gotForm, err := notifybus.ReadMuteToken(key, token)
	if err != nil {
		t.Fatalf("ReadMuteToken: %v", err)
	}
	if gotUser != user || gotForm != form {
		t.Errorf("read back %s on %s, want %s on %s", gotUser, gotForm, user, form)
	}

	// Another key is another service. This is the property that stops somebody
	// who can construct a plausible link from silencing the person who reads
	// the orders.
	other, err := notifybus.ParseMuteKey("a-different-secret-also-long-enough!!")
	if err != nil {
		t.Fatalf("ParseMuteKey: %v", err)
	}

	if _, _, err := notifybus.ReadMuteToken(other, token); !errors.Is(err, notifybus.ErrBadMuteToken) {
		t.Errorf("a token signed with another key was accepted: %v", err)
	}

	// And the same secret used for something else is not this key. The label
	// is what makes sharing the configured grant secret safe.
	tampered := []string{
		token[:len(token)-1],
		token + "x",
		strings.Replace(token, ".", "", 1),
		"",
		"....",
	}

	for _, bad := range tampered {
		if _, _, err := notifybus.ReadMuteToken(key, bad); err == nil {
			t.Errorf("%q was accepted as a token", bad)
		}
	}
}

// The encoding is length-prefixed for the reason formbus's grant is: two
// variable-length strings concatenated let one pair produce another's bytes.
func TestTwoPairsCannotShareAToken(t *testing.T) {
	key, err := notifybus.ParseMuteKey("a-test-secret-long-enough-to-be-a-key")
	if err != nil {
		t.Fatalf("ParseMuteKey: %v", err)
	}

	user := types.NewID()

	first, err := notifybus.MintMuteToken(key, user, mustSlug(t, "feast-lunch"))
	if err != nil {
		t.Fatalf("MintMuteToken: %v", err)
	}

	second, err := notifybus.MintMuteToken(key, user, mustSlug(t, "feast"))
	if err != nil {
		t.Fatalf("MintMuteToken: %v", err)
	}

	if first == second {
		t.Error("two forms produced one token")
	}
}

// --- reading and writing the preference --------------------------------------------

func TestSetMutedGoesBothWays(t *testing.T) {
	f := lunch(t)
	off := newMutes()

	h := newHarness(t, notifybus.Config{
		Forms:       forms{form: f},
		Submissions: submissions{},
		Mutes:       off,
	})

	user := types.NewID()

	muted, err := h.b.Muted(t.Context(), user, f.ID)
	if err != nil {
		t.Fatalf("Muted: %v", err)
	}
	if muted {
		t.Fatal("a fresh account starts muted, so nobody is told about anything until they ask")
	}

	for _, want := range []bool{true, true, false, false} {
		if err := h.b.SetMuted(t.Context(), when, user, f.ID, want); err != nil {
			t.Fatalf("SetMuted(%v): %v", want, err)
		}

		got, err := h.b.Muted(t.Context(), user, f.ID)
		if err != nil {
			t.Fatalf("Muted: %v", err)
		}
		if got != want {
			t.Errorf("after SetMuted(%v), Muted is %v", want, got)
		}
	}

	// Which forms, for the page that lists them.
	if err := h.b.SetMuted(t.Context(), when, user, f.ID, true); err != nil {
		t.Fatalf("SetMuted: %v", err)
	}

	forms, err := h.b.MutedForms(t.Context(), user)
	if err != nil {
		t.Fatalf("MutedForms: %v", err)
	}
	if len(forms) != 1 || forms[0] != f.ID {
		t.Errorf("MutedForms = %v, want just %s", forms, f.ID)
	}
}

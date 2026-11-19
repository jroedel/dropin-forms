package notifybus_test

import (
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/notify/notifybus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// A carpool notice goes to the people who hear about the form's submissions,
// less anybody who turned the form off, and points at the carpool page.
func TestACarpoolNoticeGoesToTheOfficeOfThatForm(t *testing.T) {
	f := lunch(t)

	quiet, loud := types.NewID(), types.NewID()

	off := newMutes()
	off.off[key(quiet, f.ID)] = true

	h := newHarness(t, notifybus.Config{
		Forms:        forms{form: f},
		Submissions:  submissions{},
		Office:       "office@schoenstatt.test",
		AdminBaseURL: "https://forms.schoenstatt.test",
		Mutes:        off,
		Grants: grants{list: []accessbus.Grant{
			{UserID: quiet, Form: f.ID, Role: accessbus.RoleResults},
			{UserID: loud, Form: f.ID, Role: accessbus.RoleResults},
		}},
		Accounts: accounts{users: map[types.ID]userbus.User{
			quiet: {ID: quiet, Email: mustEmail(t, "quiet@schoenstatt.test"), Enabled: true},
			loud:  {ID: loud, Email: mustEmail(t, "loud@schoenstatt.test"), Enabled: true},
		}},
	})

	h.b.Carpool(t.Context(), f, "Ana needs a ride", "Every car is full, so this person is waiting:\n\n  Ana, (512) 555-0103\n")

	if got := strings.Join(h.to(), ","); got != "office@schoenstatt.test,loud@schoenstatt.test" {
		t.Fatalf("sent to %s, want the office and the reader who did not turn it off", got)
	}

	m := h.messageTo(t, "loud@schoenstatt.test")

	if m.Subject != f.Title+" carpool: Ana needs a ride" {
		t.Errorf("subject = %q", m.Subject)
	}

	for _, want := range []string{
		"Ana, (512) 555-0103",
		"https://forms.schoenstatt.test/forms/" + f.ID.String() + "/carpool",
		"To stop", // the way to turn it off, as on every message to an account
	} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("the message does not say %q:\n%s", want, m.Text)
		}
	}
}

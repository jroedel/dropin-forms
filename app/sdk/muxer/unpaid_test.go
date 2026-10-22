package muxer_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
)

// An order somebody started and never paid for, through the admin surface as
// it is mounted. It is an administrator's to see and nobody else's: not in a
// results reader's list, not at its own address, and in nobody's download.
func TestAnUnpaidOrderIsShownToAnAdministratorAlone(t *testing.T) {
	o := newOffice(t)

	o.sell(t, nil)

	kitchen, reading := o.reader(t, "kitchen@schoenstatt.test")
	o.grant(t, kitchen, theForm, accessbus.RoleResults)

	boss, administering := o.reader(t, "office@schoenstatt.test")
	o.grant(t, boss, theForm, accessbus.RoleAdmin)

	ids := o.idsOf(t, theForm, administering)
	if len(ids) != 1 {
		t.Fatalf("an administrator's list links to %d orders, want the unpaid one", len(ids))
	}

	list := o.read(t, "/forms/"+theForm+"/submissions", administering).Body.String()

	for _, want := range []string{
		`<label for="show-unpaid">Show the 1 not paid</label>`,
		`class="unpaid"`,
		"in checkout",
		"pay on Stripe&rsquo;s website",
		"for 2 hours",
		"<strong>0</strong> submissions",
	} {
		if !strings.Contains(list, want) {
			t.Errorf("an administrator's list does not say %q:\n%s", want, short(list))
		}
	}

	if got := o.idsOf(t, theForm, reading); len(got) != 0 {
		t.Errorf("a results reader's list links to %d orders, want none", len(got))
	}

	if body := o.read(t, "/forms/"+theForm+"/submissions", reading).Body.String(); strings.Contains(body, "show-unpaid") {
		t.Error("a results reader is offered the box that shows unpaid orders")
	}

	page := "/forms/" + theForm + "/submissions/" + ids[0]

	if w := o.read(t, page, reading); w.Code != http.StatusNotFound {
		t.Errorf("a results reader opening the unpaid order = %d, want 404", w.Code)
	}

	if w := o.read(t, page, administering); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "in checkout") {
		t.Errorf("an administrator opening the unpaid order = %d:\n%s", w.Code, short(w.Body.String()))
	}

	for _, cookie := range []string{reading, administering} {
		csv := o.read(t, "/forms/"+theForm+"/submissions.csv", cookie).Body.String()
		if lines := strings.Count(strings.TrimSpace(csv), "\n"); lines != 0 {
			t.Errorf("the download has %d rows under its header, want none:\n%s", lines, csv)
		}
	}
}

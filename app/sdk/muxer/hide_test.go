package muxer_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/app/sdk/mid"
	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
)

// Hiding a submission, through the admin surface as it is mounted.

func (o office) post(t *testing.T, target, cookie string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(url.Values{}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: mid.CookieName, Value: cookie})

	w := httptest.NewRecorder()
	o.admin.ServeHTTP(w, r)

	return w
}

// The journey: an administrator hides a test submission from its own page,
// it leaves the list, the totals and the download, is found again under
// "show hidden", and is put back.
func TestAnAdministratorHidesATestSubmissionAndPutsItBack(t *testing.T) {
	o := newOffice(t)

	o.sell(t, url.Values{"name": {"Real Person"}})
	o.sell(t, url.Values{"name": {"TEST DO NOT COUNT"}})

	boss, cookie := o.reader(t, "office@schoenstatt.test")
	o.grant(t, boss, theForm, accessbus.RoleAdmin)

	var test string
	for _, id := range o.idsOf(t, theForm, cookie) {
		if strings.Contains(o.read(t, "/forms/"+theForm+"/submissions/"+id, cookie).Body.String(), "TEST DO NOT COUNT") {
			test = id
		}
	}
	if test == "" {
		t.Fatal("the test submission is not in the list")
	}

	page := "/forms/" + theForm + "/submissions/" + test

	if body := o.read(t, page, cookie).Body.String(); !strings.Contains(body, `action="`+page+`/hide"`) {
		t.Fatalf("an administrator is not offered the button:\n%s", short(body))
	}

	w := o.post(t, page+"/hide", cookie)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != page {
		t.Fatalf("hiding = %d to %q, want 303 back to the submission", w.Code, w.Header().Get("Location"))
	}

	body := o.read(t, page, cookie).Body.String()
	if !strings.Contains(body, "office@schoenstatt.test took this out of every list") ||
		!strings.Contains(body, `action="`+page+`/unhide"`) {
		t.Errorf("the page does not say it is hidden, by whom, with a way back:\n%s", short(body))
	}

	if ids := o.idsOf(t, theForm, cookie); len(ids) != 1 {
		t.Errorf("the list has %d submissions, want the one not hidden", len(ids))
	}

	list := o.read(t, "/forms/"+theForm+"/submissions", cookie).Body.String()
	if !strings.Contains(list, "1 hidden submission is not") || !strings.Contains(list, "<strong>1</strong> submission<") {
		t.Errorf("the list does not count one and mention one hidden:\n%s", short(list))
	}

	if csv := o.read(t, "/forms/"+theForm+"/submissions.csv", cookie).Body.String(); strings.Contains(csv, "TEST DO NOT COUNT") {
		t.Error("the hidden submission is in the download")
	}

	hidden := o.read(t, "/forms/"+theForm+"/submissions?hidden=1", cookie).Body.String()
	if !strings.Contains(hidden, "/submissions/"+test+`"`) || strings.Contains(hidden, "Real Person") {
		t.Errorf("show hidden does not list exactly the hidden one:\n%s", short(hidden))
	}

	if w := o.post(t, page+"/unhide", cookie); w.Code != http.StatusSeeOther {
		t.Fatalf("unhiding = %d", w.Code)
	}

	if ids := o.idsOf(t, theForm, cookie); len(ids) != 2 {
		t.Errorf("after putting it back the list has %d, want 2", len(ids))
	}
}

// Somebody who reads the results cannot take a row out of them: no button,
// and the route refuses.
func TestAResultsReaderCannotHide(t *testing.T) {
	o := newOffice(t)

	o.sell(t, nil)

	reader, cookie := o.reader(t, "kitchen@schoenstatt.test")
	o.grant(t, reader, theForm, accessbus.RoleResults)

	id := o.idsOf(t, theForm, cookie)[0]
	page := "/forms/" + theForm + "/submissions/" + id

	if body := o.read(t, page, cookie).Body.String(); strings.Contains(body, "/hide") {
		t.Error("a results reader is offered the button")
	}

	if w := o.post(t, page+"/hide", cookie); w.Code != http.StatusForbidden {
		t.Errorf("a results reader hiding = %d, want 403", w.Code)
	}

	if ids := o.idsOf(t, theForm, cookie); len(ids) != 1 {
		t.Error("the refused hide happened anyway")
	}
}

// The gate checks the form in the path; the id is a separate wildcard. An
// administrator of one form must not reach a submission on another by
// editing the URL.
func TestASubmissionOnAnotherFormCannotBeHiddenThroughThisOne(t *testing.T) {
	o := newOffice(t)

	o.sell(t, nil)

	boss, cookie := o.reader(t, "office@schoenstatt.test")
	o.grant(t, boss, theForm, accessbus.RoleAdmin)
	o.grant(t, boss, otherForm, accessbus.RoleAdmin)

	id := o.idsOf(t, theForm, cookie)[0]

	if w := o.post(t, "/forms/"+otherForm+"/submissions/"+id+"/hide", cookie); w.Code != http.StatusNotFound {
		t.Errorf("hiding a submission through another form = %d, want 404", w.Code)
	}

	if ids := o.idsOf(t, theForm, cookie); len(ids) != 1 {
		t.Error("the submission was hidden through the wrong form")
	}
}

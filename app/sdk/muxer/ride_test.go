package muxer_test

import (
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

	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/business/domain/carpool/carpoolbus"
	"github.com/jroedel/dropin-forms/business/domain/carpool/stores/carpooldb"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

// rideHarness is the retreat form with a carpool, and the carpool behind its
// ride pages.
type rideHarness struct {
	retreatHarness
	carpool *carpoolbus.Business
	texts   *recordedTexts
}

func rideSurface(t *testing.T) rideHarness {
	t.Helper()

	rh := retreatSurface(t)
	carpooled(t, rh)

	db, err := sqldb.Open(filepath.Join(t.TempDir(), "rides.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if err := carpooldb.Init(t.Context(), db); err != nil {
		t.Fatal(err)
	}

	texts := &recordedTexts{}

	carpool, err := carpoolbus.NewBusiness(carpoolbus.Config{
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:       carpooldb.NewStore(db),
		Submissions: rh.subs,
		Forms:       formAt{rh.form},
		Texter:      texts,
		Office:      rh.cfg.Notify,
		RideKey:     carpoolbus.NewRideKey(answerKey),
		BaseURL:     "https://f.forms.test",
	})
	if err != nil {
		t.Fatal(err)
	}

	rh.cfg.Embed.Rides = carpool
	rh.h = embedOf(t, rh.cfg)

	return rideHarness{retreatHarness: rh, carpool: carpool, texts: texts}
}

var rideLinkPattern = regexp.MustCompile(`href="(/r/[A-Za-z0-9_-]+)"`)

// signUp answers the form with a carpool choice and returns the ride link
// from the thank-you page.
func (rh rideHarness) signUp(t *testing.T, name, number, choice, seats string) string {
	t.Helper()

	// A second apart, in the order called: who signed up first decides whose
	// car fills first, and the same clock reading for everybody would leave
	// that to the order of their random identifiers.
	*rh.now = rh.now.Add(time.Second)

	values := url.Values{
		embedapp.GrantField: {grantIn(t, getPage(t, rh.h, retreatPath).Body.String())},
		"name":              {name},
		"email":             {strings.ToLower(strings.Fields(name)[0]) + "@example.org"},
		"plans":             {"booked"},
		"mobile":            {number},
		"ride":              {choice},
	}
	if seats != "" {
		values.Set("ride.seats", seats)
	}

	w := postTo(t, rh.h, retreatPath, values)
	if w.Code != http.StatusOK {
		t.Fatalf("signing up %s = %d:\n%s", name, w.Code, short(w.Body.String()))
	}

	m := rideLinkPattern.FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatalf("the thank-you page has no ride link:\n%s", short(w.Body.String()))
	}

	return m[1]
}

func (rh rideHarness) arrange(t *testing.T) {
	t.Helper()

	if err := rh.carpool.Arrange(t.Context(), *rh.form); err != nil {
		t.Fatalf("Arrange: %v", err)
	}
}

// Everybody on a carpool form gets a ride page, somebody with their own way
// there included, and can ask for a ride from it.
func TestTheThankYouPageLinksToARidePageThatCanChangeTheRide(t *testing.T) {
	rh := rideSurface(t)

	link := rh.signUp(t, "Rosa", "5125550105", "have", "")

	page := getPage(t, rh.h, link)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "getting there your own way") {
		t.Fatalf("GET %s = %d:\n%s", link, page.Code, short(page.Body.String()))
	}

	w := postTo(t, rh.h, link, url.Values{"choice": {"need"}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Waiting for a car") {
		t.Errorf("asking for a ride = %d:\n%s", w.Code, short(w.Body.String()))
	}
}

// A rider sees the other cars with room and moves to one; the driver is
// texted, and the rider is shown where they are now.
func TestARiderChoosesACarOnTheirRidePage(t *testing.T) {
	rh := rideSurface(t)

	rh.signUp(t, "Damien", "5125550101", "drive", "1")
	rh.signUp(t, "Maria", "5125550102", "drive", "2")
	ana := rh.signUp(t, "Ana", "5125550103", "need", "")
	rh.arrange(t)

	page := getPage(t, rh.h, ana).Body.String()
	if !strings.Contains(page, "riding with Damien") || !strings.Contains(page, "Ride with Maria") {
		t.Fatalf("Ana's page:\n%s", short(page))
	}

	driver := regexp.MustCompile(`name="driver" value="([0-9a-f]+)"`).FindStringSubmatch(page)
	if driver == nil {
		t.Fatalf("no car to choose on the page:\n%s", short(page))
	}

	w := postTo(t, rh.h, ana, url.Values{"driver": {driver[1]}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "riding with Maria") {
		t.Fatalf("choosing Maria's car = %d:\n%s", w.Code, short(w.Body.String()))
	}

	// Maria has only her welcome: she hears about Ana in her next summary,
	// once her car has been quiet for ten minutes.
	if got := rh.texts.to("+15125550102"); len(got) != 1 || !strings.Contains(got[0], "thanks for driving") {
		t.Errorf("Maria was texted %q", got)
	}
}

// "I'm not coming" takes them out and emails the office, which is told the
// sign-up is still there.
func TestNotComingOnTheRidePageTellsTheOffice(t *testing.T) {
	rh := rideSurface(t)

	ana := rh.signUp(t, "Ana", "5125550103", "need", "")
	rh.arrange(t)

	w := postTo(t, rh.h, ana, url.Values{"choice": {carpoolbus.Out}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "told us you're not coming") {
		t.Fatalf("not coming = %d:\n%s", w.Code, short(w.Body.String()))
	}

	var told bool

	for _, m := range rh.mailTo("office@schoenstatt.test") {
		if strings.Contains(m.Subject, "Ana is not coming") && strings.Contains(m.Text, "still in the results") {
			told = true
		}
	}

	if !told {
		t.Errorf("the office was not told; it got %d messages", len(rh.mailTo("office@schoenstatt.test")))
	}
}

// A link that is not ours, or was cut short, leads to a page that says so
// and nothing else.
func TestARideLinkThatIsNotOursLeadsNowhere(t *testing.T) {
	rh := rideSurface(t)

	for _, link := range []string{"/r/not-a-token", "/r/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		w := getPage(t, rh.h, link)
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "does not lead to a ride") {
			t.Errorf("GET %s = %d:\n%s", link, w.Code, short(w.Body.String()))
		}
	}
}

// The token is the credential, so a change must still come from our own
// page: another site cannot post to a ride page it was handed the link of.
func TestAnotherSiteCannotChangeARide(t *testing.T) {
	rh := rideSurface(t)

	ana := rh.signUp(t, "Ana", "5125550103", "need", "")

	r := httptest.NewRequest(http.MethodPost, ana, strings.NewReader(url.Values{"choice": {carpoolbus.Out}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "cross-site")

	w := httptest.NewRecorder()
	rh.h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("a cross-site post = %d, want 403", w.Code)
	}

	if strings.Contains(getPage(t, rh.h, ana).Body.String(), "told us you're not coming") {
		t.Error("the cross-site post changed the ride")
	}
}

// The page that sends a lost link asks for the mobile number on a carpool
// form, and texts the ride link to it.
func TestALostRideLinkIsTextedToTheNumber(t *testing.T) {
	rh := rideSurface(t)

	ana := rh.signUp(t, "Ana", "5125550103", "need", "")

	page := getPage(t, rh.h, "/f/"+rh.form.ID.String()+"/link").Body.String()
	if !strings.Contains(page, `name="phone"`) {
		t.Fatalf("the link page does not ask for a number:\n%s", short(page))
	}

	w := postTo(t, rh.h, "/f/"+rh.form.ID.String()+"/link", url.Values{
		embedapp.GrantField: {grantIn(t, page)},
		"phone":             {"(512) 555-0103"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d:\n%s", w.Code, short(w.Body.String()))
	}

	if got := rh.texts.to("+15125550103"); len(got) != 1 || !strings.Contains(got[0], "https://f.forms.test"+ana) {
		t.Errorf("Ana was texted %q, want her ride link", got)
	}
}

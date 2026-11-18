package muxer_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
)

// The number an organiser is texted on is set by that person on their own
// account page, and only there: it is the consent to be texted.
func TestAPersonSetsAndClearsTheirOwnMobileNumber(t *testing.T) {
	a := newAdmin(t, "")

	u, err := a.users.Create(t.Context(), time.Now(), userbus.NewUser{
		Email: mustEmail(t, "organiser@schoenstatt.us"), Name: "Organiser",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cookie := signedIn(t, a, u)

	w := a.post(t, "/account/mobile", url.Values{"mobile": {"(512) 555-0100"}}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("saving a number = %d:\n%s", w.Code, w.Body)
	}

	if !strings.Contains(w.Body.String(), "(512) 555-0100") {
		t.Errorf("the page does not show the saved number:\n%s", w.Body)
	}

	if got, _ := a.users.ByID(t.Context(), u.ID); got.Mobile.String() != "+15125550100" {
		t.Errorf("stored Mobile = %q, want +15125550100", got.Mobile)
	}

	// A number that cannot be texted is refused, put back for correcting, and
	// the one already saved is left alone.
	w = a.post(t, "/account/mobile", url.Values{"mobile": {"555-0100"}}, cookie)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("a seven-digit number = %d, want 422", w.Code)
	}

	if !strings.Contains(w.Body.String(), `value="555-0100"`) {
		t.Errorf("the refused number is not put back in the box:\n%s", w.Body)
	}

	if got, _ := a.users.ByID(t.Context(), u.ID); got.Mobile.String() != "+15125550100" {
		t.Errorf("a refused number changed the stored one to %q", got.Mobile)
	}

	// Empty is how somebody stops being texted.
	if w := a.post(t, "/account/mobile", url.Values{"mobile": {""}}, cookie); w.Code != http.StatusOK {
		t.Fatalf("clearing = %d:\n%s", w.Code, w.Body)
	}

	if got, _ := a.users.ByID(t.Context(), u.ID); !got.Mobile.IsZero() {
		t.Errorf("after clearing, Mobile = %q, want none", got.Mobile)
	}
}

func TestSettingAMobileNumberNeedsASession(t *testing.T) {
	a := newAdmin(t, "")

	if w := a.post(t, "/account/mobile", url.Values{"mobile": {"5125550100"}}, ""); w.Code != http.StatusForbidden {
		t.Errorf("signed out = %d, want 403", w.Code)
	}
}

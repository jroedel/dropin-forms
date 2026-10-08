package userbus_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
)

// wrongCode is six digits that are not code.
func wrongCode(code string) string {
	if code == "000000" {
		return "111111"
	}

	return "000000"
}

func TestACodeSignsInOnce(t *testing.T) {
	b, store := newBusiness(t)
	u := mustCreate(t, b, "frjeff@schoenstatt.us")

	req, err := b.RequestSignIn(t.Context(), now, u.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}

	if len(req.Code) != 6 || strings.Trim(req.Code, "0123456789") != "" {
		t.Fatalf("code = %q, want six digits", req.Code)
	}

	// As somebody might type it off a phone: in two halves.
	typed := req.Code[:3] + " " + req.Code[3:]

	got, cookie, err := b.SignInWithCode(t.Context(), now, u.Email, typed)
	if err != nil {
		t.Fatalf("SignInWithCode: %v", err)
	}
	if got.ID != u.ID || cookie == "" {
		t.Fatalf("signed in as %q with %q", got.ID, cookie)
	}

	if _, _, err := b.SignInWithCode(t.Context(), now, u.Email, req.Code); !errors.Is(err, userbus.ErrDenied) {
		t.Errorf("a code worked twice: %v", err)
	}

	for _, stored := range store.storedSecrets() {
		if strings.Contains(stored, req.Code) {
			t.Error("a code was stored, not just its hash")
		}
	}
}

// Asking again retires the last code, so the guesses available do not grow
// with the number of mails sent.
func TestOnlyTheNewestCodeWorks(t *testing.T) {
	b, _ := newBusiness(t)
	u := mustCreate(t, b, "frjeff@schoenstatt.us")

	first, err := b.RequestSignIn(t.Context(), now, u.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}

	second, err := b.RequestSignIn(t.Context(), now.Add(time.Minute), u.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}

	if first.Code != second.Code {
		if _, _, err := b.SignInWithCode(t.Context(), now.Add(time.Minute), u.Email, first.Code); !errors.Is(err, userbus.ErrDenied) {
			t.Errorf("an older code worked: %v", err)
		}
	}

	if _, _, err := b.SignInWithCode(t.Context(), now.Add(time.Minute), u.Email, second.Code); err != nil {
		t.Errorf("the newest code: %v", err)
	}
}

func TestACodeRefuses(t *testing.T) {
	b, store := newBusiness(t)
	u := mustCreate(t, b, "frjeff@schoenstatt.us")
	gone := mustCreate(t, b, "gone@schoenstatt.us")

	req, err := b.RequestSignIn(t.Context(), now, u.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}

	goneReq, err := b.RequestSignIn(t.Context(), now, gone.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}

	gone.Enabled = false
	if err := store.UpdateUser(t.Context(), gone); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}

	for _, tt := range []struct {
		name  string
		email string
		code  string
		at    time.Time
	}{
		{"blank", "frjeff@schoenstatt.us", "", now},
		{"five digits", "frjeff@schoenstatt.us", req.Code[:5], now},
		{"seven digits", "frjeff@schoenstatt.us", req.Code + "1", now},
		{"letters", "frjeff@schoenstatt.us", "abcdef", now},
		{"the right code for nobody", "nobody@schoenstatt.us", req.Code, now},
		{"the right code for somebody else", "gone@schoenstatt.us", req.Code, now},
		{"a disabled account's own code", "gone@schoenstatt.us", goneReq.Code, now},
		{"the right code as it expires", "frjeff@schoenstatt.us", req.Code, now.Add(15 * time.Minute)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := b.SignInWithCode(t.Context(), tt.at, mustEmail(t, tt.email), tt.code); !errors.Is(err, userbus.ErrDenied) {
				t.Errorf("err = %v, want ErrDenied", err)
			}
		})
	}

	// None of those spent it.
	if _, _, err := b.SignInWithCode(t.Context(), now, u.Email, req.Code); err != nil {
		t.Errorf("the right code after the refusals: %v", err)
	}
}

func TestACodeAllowsFiveTries(t *testing.T) {
	b, _ := newBusiness(t)
	u := mustCreate(t, b, "frjeff@schoenstatt.us")

	req, err := b.RequestSignIn(t.Context(), now, u.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}

	for range 5 {
		if _, _, err := b.SignInWithCode(t.Context(), now, u.Email, wrongCode(req.Code)); !errors.Is(err, userbus.ErrDenied) {
			t.Fatalf("a wrong code = %v, want ErrDenied", err)
		}
	}

	if _, _, err := b.SignInWithCode(t.Context(), now, u.Email, req.Code); !errors.Is(err, userbus.ErrDenied) {
		t.Errorf("the right code worked after five wrong ones: %v", err)
	}

	// Asking again is the way on.
	again, err := b.RequestSignIn(t.Context(), now, u.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}

	if _, _, err := b.SignInWithCode(t.Context(), now, u.Email, again.Code); err != nil {
		t.Errorf("a fresh code after the last one ran out: %v", err)
	}
}

// Guesses arriving together cannot overshoot the five: each reserves its
// attempt before it is compared.
func TestACodesTriesHoldUnderConcurrency(t *testing.T) {
	b, _ := newBusiness(t)
	u := mustCreate(t, b, "frjeff@schoenstatt.us")

	req, err := b.RequestSignIn(t.Context(), now, u.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			b.SignInWithCode(t.Context(), now, u.Email, wrongCode(req.Code))
		})
	}
	wg.Wait()

	if _, _, err := b.SignInWithCode(t.Context(), now, u.Email, req.Code); !errors.Is(err, userbus.ErrDenied) {
		t.Errorf("the right code worked after twenty wrong ones at once: %v", err)
	}
}

// Asking for a fresh code every five wrong guesses runs into the account's
// hourly budget, which a right code does not draw on; and when the hour has
// passed, codes work again.
func TestAnAccountsWrongCodesAreBudgeted(t *testing.T) {
	b, _ := newBusiness(t)
	u := mustCreate(t, b, "frjeff@schoenstatt.us")

	// A right code first, which must not count.
	ok, err := b.RequestSignIn(t.Context(), now, u.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}
	if _, _, err := b.SignInWithCode(t.Context(), now, u.Email, ok.Code); err != nil {
		t.Fatalf("SignInWithCode: %v", err)
	}

	at := now
	for range 2 {
		at = at.Add(time.Minute)

		req, err := b.RequestSignIn(t.Context(), at, u.Email)
		if err != nil {
			t.Fatalf("RequestSignIn: %v", err)
		}

		for range 5 {
			b.SignInWithCode(t.Context(), at, u.Email, wrongCode(req.Code))
		}
	}

	at = at.Add(time.Minute)

	req, err := b.RequestSignIn(t.Context(), at, u.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}

	if _, _, err := b.SignInWithCode(t.Context(), at, u.Email, req.Code); !errors.Is(err, userbus.ErrDenied) {
		t.Errorf("a fresh right code worked after ten wrong ones this hour: %v", err)
	}

	// An hour after the wrong ones, codes work again.
	later := now.Add(2*time.Minute + userbus.CodeBudgetWindow)

	req, err = b.RequestSignIn(t.Context(), later, u.Email)
	if err != nil {
		t.Fatalf("RequestSignIn: %v", err)
	}

	if _, _, err := b.SignInWithCode(t.Context(), later, u.Email, req.Code); err != nil {
		t.Errorf("a code an hour later: %v", err)
	}
}

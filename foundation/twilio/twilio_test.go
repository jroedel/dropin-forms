package twilio_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/foundation/twilio"
)

// A Twilio account SID is "AC" and thirty-two hex characters, which is also
// the shape of an IBAN to a secret scanner. Assembled from pieces for that
// reason and no other, as in eumaeus.
const (
	accountSID = "AC" + "00000000000000000000" + "000000000001"
	authToken  = "an-auth-token"
	fromNumber = "+15550100999"
	recipient  = "+15550100123"
)

// serve starts a fake Twilio and returns a sender pointed at it.
func serve(t *testing.T, h http.HandlerFunc) *twilio.Sender {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	s, err := twilio.New(twilio.Config{
		AccountSID: accountSID,
		AuthToken:  authToken,
		From:       fromNumber,
		HTTP:       srv.Client(),
		BaseURL:    srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v, want a sender", err)
	}

	return s
}

func reply(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprint(w, body)
}

func TestASendPostsTheFormTwilioExpects(t *testing.T) {
	var (
		method, path, ctype, user, pass string
		form                            url.Values
		authed                          bool
	)

	s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		method, path, ctype = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		user, pass, authed = r.BasicAuth()

		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}

		form = r.PostForm

		reply(w, http.StatusCreated, `{"sid":"SM123","status":"queued","error_code":null}`)
	})

	sid, err := s.Send(t.Context(), recipient, "Ana is riding with you.")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if sid != "SM123" {
		t.Errorf("sid = %q, want SM123", sid)
	}

	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}

	if want := "/2010-04-01/Accounts/" + accountSID + "/Messages.json"; path != want {
		t.Errorf("path = %s, want %s", path, want)
	}

	if ctype != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", ctype)
	}

	if !authed || user != accountSID || pass != authToken {
		t.Errorf("basic auth = %q/%q (%v), want the account SID and token", user, pass, authed)
	}

	for k, want := range map[string]string{"To": recipient, "From": fromNumber, "Body": "Ana is riding with you."} {
		if got := form.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestAnOptedOutRecipientIsARefusalThatSaysSo(t *testing.T) {
	// The test that caught eumaeus's first version: a 400 carries "status":
	// 400, a number, and a single reply struct with a string status turned
	// every refusal into "not JSON" and hid the opt-out.
	s := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusBadRequest,
			`{"code":21610,"message":"Attempt to send to unsubscribed recipient","more_info":"https://www.twilio.com/docs/errors/21610","status":400}`)
	})

	_, err := s.Send(t.Context(), recipient, "hello")

	refused, ok := errors.AsType[twilio.Refused](err)
	if !ok {
		t.Fatalf("err = %v, want a Refused", err)
	}

	if !refused.IsOptedOut() {
		t.Errorf("IsOptedOut() = false for code %d", refused.Code)
	}
}

func TestAnAcceptedMessageCarryingAnErrorCodeIsARefusal(t *testing.T) {
	s := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusCreated,
			`{"sid":"SM9","status":"failed","error_code":21614,"error_message":"'To' number is not a valid mobile number"}`)
	})

	_, err := s.Send(t.Context(), recipient, "hello")

	refused, ok := errors.AsType[twilio.Refused](err)
	if !ok {
		t.Fatalf("err = %v, want a Refused", err)
	}

	if refused.Code != 21614 || refused.IsOptedOut() {
		t.Errorf("refused = %+v, want code 21614 and not an opt-out", refused)
	}
}

func TestAProviderFailureIsAnErrorRatherThanARefusal(t *testing.T) {
	s := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusServiceUnavailable, `{"code":20500,"message":"Internal  Server\nError","status":503}`)
	})

	_, err := s.Send(t.Context(), recipient, "hello")
	if err == nil {
		t.Fatal("err = nil, want one")
	}

	if _, ok := errors.AsType[twilio.Refused](err); ok {
		t.Errorf("err = %v is a Refused; a 5xx is Twilio's fault, not the recipient's", err)
	}

	if !strings.Contains(err.Error(), "Internal Server Error") {
		t.Errorf("err = %q, want the message collapsed onto one line", err)
	}
}

func TestAReplyThatIsNotJSONNamesTheStatusCode(t *testing.T) {
	s := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html>captive portal</html>")
	})

	_, err := s.Send(t.Context(), recipient, "hello")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want one naming 403", err)
	}
}

func TestAReplyWithNoMessageIsAnError(t *testing.T) {
	s := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusCreated, `{"status":"queued"}`)
	})

	if _, err := s.Send(t.Context(), recipient, "hello"); err == nil {
		t.Error("err = nil, want one: a reply with no SID is not a sent message")
	}
}

func TestAProviderThatNeverAnswersIsAnError(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	client := srv.Client()
	client.Timeout = 50 * time.Millisecond

	s, err := twilio.New(twilio.Config{
		AccountSID: accountSID, AuthToken: authToken, From: fromNumber,
		HTTP: client, BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Send(t.Context(), recipient, "hello"); err == nil {
		t.Error("err = nil, want a timeout")
	}
}

func TestASenderWithAnIncompleteConfigIsRefused(t *testing.T) {
	cases := map[string]twilio.Config{
		"no account SID":     {AuthToken: authToken, From: fromNumber},
		"not an account SID": {AccountSID: "SK123", AuthToken: authToken, From: fromNumber},
		"no auth token":      {AccountSID: accountSID, From: fromNumber},
		"no sending number":  {AccountSID: accountSID, AuthToken: authToken},
		"a number not E.164": {AccountSID: accountSID, AuthToken: authToken, From: "512-555-0100"},
	}

	for name, cfg := range cases {
		if _, err := twilio.New(cfg); err == nil {
			t.Errorf("%s: New succeeded, want a refusal", name)
		}
	}
}

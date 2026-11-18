// Package twilio sends one text message through Twilio's REST API.
//
// Ported from eumaeus (business/domain/comms/sources/twilio), which sends
// through the same Twilio account. What came across is the request, the two
// reply shapes and the reasons for both; what was left behind is eumaeus's
// delivery-status callback and its receipt type, because nothing here tracks a
// message after Twilio has accepted it. The carpool records "texted at" or the
// refusal, and a person looks at the office page for the rest.
//
// # What crosses the wire
//
// The recipient's number and the message body, to Twilio, over TLS. Twilio
// then holds both, as does the carrier, and Twilio's console shows the body
// for the account's retention period. There is no version of sending a text
// where that is not true, so a body should say no more than the recipient
// needs: a name and a number to call, not the rest of the submission.
//
// Nothing here knows what a form or a carpool is. The caller says which number
// and what text; this package says it to Twilio and sorts what comes back into
// three answers a caller acts on differently: accepted, refused by Twilio for
// a reason that is about the recipient ([Refused], with [Refused.OptedOut] for
// the one that has legal weight), or not answered at all (any other error).
//
// # No SDK
//
// Twilio publishes a Go helper library and this does not use it, for
// eumaeus's reason: the whole surface needed is one form-encoded POST and one
// JSON reply, and a vendor SDK with its own dependency tree would be a larger
// change to this repository's trust path than the forty lines it replaces.
package twilio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// apiBase is Twilio's REST endpoint. Overridable in [Config] only so a test
// can point it at an httptest server.
const apiBase = "https://api.twilio.com"

// timeout bounds one message. Ten seconds, as in eumaeus: long enough that a
// blip is not reported as a failure, short enough that a person waiting on a
// page that sends three texts is not kept for a minute.
const timeout = 10 * time.Second

// maxBody is the most read from one reply. A reply is a few hundred bytes of
// JSON; without a limit a misdirected request to something that streams would
// read until the process died.
const maxBody = 1 << 20

// OptedOut is Twilio's error code for a recipient who has replied STOP to this
// sending number. Twilio refuses every later message to them until they reply
// START, which is the opt-out working as the law expects, and the one refusal a
// caller must record rather than retry.
const OptedOut = 21610

// Config is the account and the number messages come from.
type Config struct {
	// AccountSID is the account, "AC…", on the console's front page. It is
	// in every request's path whichever credential signs it.
	AccountSID string

	// APIKey and APISecret are an API key made for this service alone,
	// "SK…" and the secret Twilio shows once when it is made. Preferred over
	// AuthToken: a key can be revoked without touching anything else on the
	// account, and the auth token is the whole account's -- shared here with
	// eumaeus, so changing it would stop both at once.
	APIKey    string
	APISecret string

	// AuthToken is the account's own token, the alternative to an API key.
	// One or the other, not both.
	AuthToken string

	// From is the sending number in E.164, "+1…". It must be a number on this
	// account that is registered to send to the United States (A2P 10DLC, or
	// a verified toll-free number); an unregistered number is accepted here
	// and then filtered by the carriers, which arrives as error 30007 long
	// after this package has returned.
	From string

	// HTTP is the client every request goes through. Nil means one with
	// [timeout].
	HTTP *http.Client

	// BaseURL overrides [apiBase], for tests.
	BaseURL string
}

// Sender sends messages.
type Sender struct {
	cfg  Config
	http *http.Client
	base string
}

// New makes a Sender, refusing a config it cannot work with.
func New(cfg Config) (*Sender, error) {
	key := cfg.APIKey != "" || cfg.APISecret != ""

	switch {
	case !strings.HasPrefix(cfg.AccountSID, "AC"):
		return nil, errors.New(`a Twilio account SID begins with "AC"`)
	case key && cfg.AuthToken != "":
		return nil, errors.New("a Twilio sender takes an API key or the auth token, not both")
	case key && !strings.HasPrefix(cfg.APIKey, "SK"):
		return nil, errors.New(`a Twilio API key's SID begins with "SK"`)
	case key && strings.TrimSpace(cfg.APISecret) == "":
		return nil, errors.New("a Twilio API key needs its secret")
	case !key && strings.TrimSpace(cfg.AuthToken) == "":
		return nil, errors.New("a Twilio sender needs an API key and its secret, or the account's auth token")
	case !strings.HasPrefix(cfg.From, "+"):
		return nil, errors.New(`a Twilio sending number is written in E.164, beginning with "+"`)
	}

	client := cfg.HTTP
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}

	base := cfg.BaseURL
	if base == "" {
		base = apiBase
	}

	return &Sender{cfg: cfg, http: client, base: strings.TrimRight(base, "/")}, nil
}

// Refused is Twilio declining a message for a reason that is about the
// recipient or the message rather than about this program: they opted out, the
// number is a landline, the number does not exist. Retrying will not help.
type Refused struct {
	Code    int
	Message string
}

func (r Refused) Error() string {
	return fmt.Sprintf("Twilio refused the message (%d): %s", r.Code, r.Message)
}

// IsOptedOut reports whether the recipient has replied STOP to this number.
func (r Refused) IsOptedOut() bool { return r.Code == OptedOut }

// Twilio's two reply shapes, and they cannot be one struct.
//
// Both use the key "status" and disagree about its type: an accepted message
// says "status": "queued", a refusal says "status": 400. Decoded into one
// struct with a string field, every refusal fails to unmarshal and comes back
// as "that reply was not JSON" -- which hides error 21610, the opt-out, the one
// refusal that matters. eumaeus found that with a test; the test came across
// with the types.
type accepted struct {
	SID    string `json:"sid"`
	Status string `json:"status"`

	// ErrorCode is set on a message Twilio took and immediately knew would
	// fail. Null when nothing is wrong, which is why it is a pointer.
	ErrorCode    *int   `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

type refused struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Send hands one message to Twilio and returns the message's SID.
//
// to is an E.164 number. The error is a [Refused] when Twilio declined the
// message, and anything else when Twilio could not be reached or did not
// answer in a form this understands. A 5xx is the second kind and might be
// worth trying again, but nothing here does that: a retried message that did
// in fact go is a second copy on a real person's phone.
func (s *Sender) Send(ctx context.Context, to, body string) (string, error) {
	form := url.Values{}
	form.Set("To", to)
	form.Set("From", s.cfg.From)
	form.Set("Body", body)

	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Messages.json",
		s.base, url.PathEscape(s.cfg.AccountSID))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("building the Twilio request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// An API key signs as itself; the account's token signs as the account.
	// The path names the account either way.
	if s.cfg.APIKey != "" {
		req.SetBasicAuth(s.cfg.APIKey, s.cfg.APISecret)
	} else {
		req.SetBasicAuth(s.cfg.AccountSID, s.cfg.AuthToken)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending to Twilio: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", fmt.Errorf("reading Twilio's reply: %w", err)
	}

	if resp.StatusCode >= 400 {
		var bad refused

		// A body that will not decode is reported by its status code: at this
		// point the likely cause is a proxy or a captive portal, not Twilio.
		if err := json.Unmarshal(raw, &bad); err != nil {
			return "", fmt.Errorf("the reply to a %d was not JSON, so it did not come from Twilio: %w",
				resp.StatusCode, err)
		}

		if resp.StatusCode >= 500 {
			return "", fmt.Errorf("twilio failed with %d: %s", resp.StatusCode, oneLine(bad.Message))
		}

		return "", Refused{Code: bad.Code, Message: oneLine(bad.Message)}
	}

	var m accepted

	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("the reply to a %d was not JSON, so it did not come from Twilio: %w",
			resp.StatusCode, err)
	}

	if m.ErrorCode != nil && *m.ErrorCode != 0 {
		return "", Refused{Code: *m.ErrorCode, Message: oneLine(m.ErrorMessage)}
	}

	if m.SID == "" {
		return "", errors.New("twilio's reply named no message, so it cannot be said to have been sent")
	}

	return m.SID, nil
}

// oneLine collapses a provider message so it cannot break a log line or a
// table cell.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

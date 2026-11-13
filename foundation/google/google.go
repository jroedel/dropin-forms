// Package google is the part of Google's APIs this service uses: signing a
// person in to their Google account with OAuth 2.0 to be allowed to keep a
// spreadsheet for them, and the handful of Sheets and Drive calls that keep
// it.
//
// Hand-written over net/http rather than golang.org/x/oauth2 and
// google.golang.org/api. The second is a very large module -- every Google
// API, generated -- for seven requests, in a repository with three direct
// dependencies; and a thin client of our own is one a test can point at an
// httptest server and read in an afternoon. What that costs is that the
// request shapes here are written from Google's reference documentation
// rather than generated from it, so each is named with the method it calls,
// for checking against the reference when Google changes something.
//
// Nothing here knows what a form is. The caller says which spreadsheet, which
// tab, which cells and which tags; this package says it to Google and sorts
// what comes back into the few answers a caller acts on differently: the
// person withdrew permission ([ErrRevoked]), the file is not there
// ([ErrGone]), Google refused ([ErrDenied]), or try again later ([ErrBusy]).
package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The four answers a caller does something different about.
var (
	// ErrRevoked is the person having withdrawn permission, or Google having
	// expired it: the refresh token is no longer any good, and nothing will
	// work until they connect again.
	ErrRevoked = errors.New("google no longer accepts this connection")

	// ErrGone is a spreadsheet that is not there: deleted, or in the bin.
	ErrGone = errors.New("the spreadsheet is not there")

	// ErrDenied is Google refusing a request this connection is not allowed
	// to make: a file it did not create, or an API not switched on.
	ErrDenied = errors.New("google refused the request")

	// ErrBusy is a rate limit or a fault at Google's end. Try again later.
	ErrBusy = errors.New("google is busy or unavailable")
)

// Busy carries how long Google asked to be left alone, when it said.
type Busy struct {
	RetryAfter time.Duration
	Message    string
}

func (b Busy) Error() string { return "google is busy or unavailable: " + b.Message }
func (b Busy) Unwrap() error { return ErrBusy }

// The endpoints. Variables in a Config so a test can point them at itself.
const (
	authURL   = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenURL  = "https://oauth2.googleapis.com/token"
	revokeURL = "https://oauth2.googleapis.com/revoke"
	sheetsURL = "https://sheets.googleapis.com/v4/spreadsheets"
	driveURL  = "https://www.googleapis.com/drive/v3/files"
)

// Scopes asked for. drive.file is access to the files this app created or
// was given, and to nothing else in the person's Drive; Google classes it as
// non-sensitive, which is what lets a public app use it without a security
// review. openid and email say which Google account was connected, so the
// page can show it.
const (
	ScopeDriveFile = "https://www.googleapis.com/auth/drive.file"
	scopes         = "openid email " + ScopeDriveFile
)

// Config is the OAuth client and where Google is.
type Config struct {
	ClientID     string
	ClientSecret string

	// RedirectURL is where Google sends the browser back to, exactly as
	// registered on the OAuth client in Google Cloud.
	RedirectURL string

	// HTTP is the client every request goes through. Nil means one with a
	// timeout: a request to Google that never answers must not hold a sync,
	// or a person waiting on a page, forever.
	HTTP *http.Client

	// The endpoints, empty for Google's own. Set by tests.
	AuthURL, TokenURL, RevokeURL, SheetsURL, DriveURL string
}

// Client talks to Google.
type Client struct {
	cfg Config
}

// requestTimeout bounds one request. Thirty seconds, because a batch update
// of a few hundred cells is not instant at Google's end either, and a sync is
// in the background: the cost of waiting is nobody's.
const requestTimeout = 30 * time.Second

// maxBody is the most read from one answer. A spreadsheet's worth of columns
// for a form of a few thousand rows is well inside it.
const maxBody = 16 << 20

// New makes a Client, refusing a config it cannot work with.
func New(cfg Config) (*Client, error) {
	switch {
	case cfg.ClientID == "" || cfg.ClientSecret == "":
		return nil, errors.New("a Google client needs its client ID and secret")
	case !strings.HasPrefix(cfg.RedirectURL, "https://") && !strings.HasPrefix(cfg.RedirectURL, "http://"):
		return nil, errors.New("a Google client needs the address Google sends people back to")
	}

	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: requestTimeout}
	}

	for _, d := range []struct {
		p   *string
		def string
	}{
		{&cfg.AuthURL, authURL}, {&cfg.TokenURL, tokenURL}, {&cfg.RevokeURL, revokeURL},
		{&cfg.SheetsURL, sheetsURL}, {&cfg.DriveURL, driveURL},
	} {
		if *d.p == "" {
			*d.p = d.def
		}
	}

	return &Client{cfg: cfg}, nil
}

// --- the requests -----------------------------------------------------------

// call sends one request and decodes a JSON answer into out, which may be nil.
func (c *Client) call(ctx context.Context, method, target, bearer string, body any, out any) error {
	var rdr io.Reader

	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding the request: %w", err)
		}

		rdr = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	return c.do(req, out)
}

// form sends a form-encoded POST, as the OAuth endpoints want.
func (c *Client) form(ctx context.Context, target string, v url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(v.Encode()))
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return req.Context().Err()
		}

		// No answer at all is Google unavailable as far as anybody here can
		// tell, and the next attempt is the remedy.
		return Busy{Message: err.Error()}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Busy{Message: "the answer was cut off: " + err.Error()}
	}

	if resp.StatusCode/100 != 2 {
		return classify(resp, raw)
	}

	if out == nil || len(raw) == 0 {
		return nil
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("reading Google's answer: %w", err)
	}

	return nil
}

// classify turns a refusal into one of the four answers.
//
// Two shapes of error body: the OAuth endpoints' {"error": "invalid_grant",
// "error_description": "..."} and the APIs' {"error": {"code": 404,
// "message": "...", "status": "NOT_FOUND"}}.
func classify(resp *http.Response, raw []byte) error {
	var oauth struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}

	var api struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}

	msg := strings.TrimSpace(string(raw))

	if json.Unmarshal(raw, &oauth) == nil && oauth.Error != "" {
		msg = oauth.Error + ": " + oauth.Description

		if oauth.Error == "invalid_grant" {
			return fmt.Errorf("%w: %s", ErrRevoked, msg)
		}
	} else if json.Unmarshal(raw, &api) == nil && api.Error.Message != "" {
		msg = api.Error.Status + ": " + api.Error.Message
	}

	switch code := resp.StatusCode; {
	case code == http.StatusUnauthorized:
		// An access token that has stopped working before its time: the
		// person signed out everywhere or withdrew permission.
		return fmt.Errorf("%w: %s", ErrRevoked, msg)

	case code == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrGone, msg)

	case code == http.StatusTooManyRequests || code >= 500:
		b := Busy{Message: msg}

		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			b.RetryAfter = time.Duration(s) * time.Second
		}

		return b

	case code == http.StatusForbidden:
		// Quota errors also arrive as 403, with a reason saying so.
		if strings.Contains(msg, "RATE_LIMIT") || strings.Contains(strings.ToLower(msg), "quota") {
			return Busy{Message: msg}
		}

		return fmt.Errorf("%w: %s", ErrDenied, msg)
	}

	return fmt.Errorf("google answered %d: %s", resp.StatusCode, msg)
}

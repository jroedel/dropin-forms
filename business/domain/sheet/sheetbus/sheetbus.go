// Package sheetbus keeps a Google Sheet of a form's submissions up to date for
// one person, in their own Google account.
//
// # Whose it is
//
// A connection belongs to the person who made it, not to the form. It is
// their Google account, their Drive and their spreadsheet: nobody else sees
// that it exists, and only they are emailed about it. Several people can each
// keep their own sheet of the same form. Anybody who may read the form's
// answers -- results or above, who can already download them -- may connect.
//
// The service's own rules still decide what flows. Every pass checks that the
// person still has results on the form and an enabled account, and that the
// form still exists; when one of those stops being true the sheet stops being
// updated and they are told why. What is already in their Drive stays there,
// as a download they saved would.
//
// # Finding the data in a sheet somebody has rearranged
//
// People sort, filter, recolour, rename the tab, move columns, add their own
// columns and type notes beside rows. None of that is damage. The tab is
// remembered by its id, which survives renaming; each of the form's columns
// carries a tag (developer metadata, invisible to people) that Google moves
// with the column; and rows are found by the Reference column, never by their
// position. So a sync reads where things are now, every time.
//
// # What they delete stays deleted
//
// Every row and column written is recorded. One that has been written and is
// no longer in the sheet was deleted by the person, and is not put back --
// not even when that submission changes later. New submissions and new
// questions still arrive. Refill brings back everything, for the day somebody
// deleted by mistake.
//
// What is beyond use -- the tab gone, or the Reference column gone, so that
// no row can be matched to a submission -- is not repaired in place. A new
// tab is added beside it, filled from scratch, and the person is emailed. The
// old tab is left exactly as it was: it may hold notes.
//
// # Cheap when nothing has changed
//
// A pass builds the rows the sheet should hold and hashes them. Equal to the
// last pass's hash, and the sheet read less than an hour ago, it stops
// without asking Google anything. Otherwise it reads the form's columns, works
// out the differences, and sends them as one batch of changed cells.
package sheetbus

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/access/accessbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/domain/user/userbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/google"
	"github.com/jroedel/dropin-forms/foundation/mail"
	"github.com/jroedel/dropin-forms/foundation/seal"
)

// Errors a page turns into a sentence.
var (
	ErrNotFound  = errors.New("no Google Sheet is connected")
	ErrBadState  = errors.New("the answer from Google does not belong to this sign-in")
	ErrNoFiles   = errors.New("permission to create the spreadsheet was not given")
	ErrNoAccess  = errors.New("you cannot read this form's answers")
	ErrReconnect = errors.New("google no longer accepts this connection; connect again")
	ErrGone      = errors.New("the spreadsheet can no longer be reached")
)

// State is where a connection stands.
type State string

const (
	// StateOK is kept up to date.
	StateOK State = "ok"

	// StateReconnect is Google refusing the connection: permission withdrawn
	// in the Google account, or expired. Only connecting again mends it.
	StateReconnect State = "reconnect"

	// StateGone is a spreadsheet deleted, in the bin, or out of reach.
	StateGone State = "gone"

	// StateStopped is the person no longer allowed the answers, or the form
	// deleted. Their access coming back resumes it.
	StateStopped State = "stopped"
)

// Connection is one person's sheet of one form.
type Connection struct {
	ID   types.ID
	User types.ID
	Form types.Slug

	// Account is the Google account it was connected with.
	Account string

	Spreadsheet string
	Sheet       int64
	URL         string

	// Token is the refresh token, sealed with the connection's id as its
	// purpose.
	Token []byte

	State   State
	Problem string

	// Told is the last thing the person was emailed about, so that a
	// connection failing every two minutes sends one email rather than a
	// thousand. Cleared by a pass that works.
	Told string

	// Digest is the hash of what the last pass wrote.
	Digest string

	CreatedAt    time.Time
	SyncedAt     time.Time
	CheckedAt    time.Time
	NextAt       time.Time
	FailingSince time.Time
}

// Event is something worth telling the person on their page.
type Event struct {
	At   time.Time
	Text string
}

// Storer is the storage this domain needs.
type Storer interface {
	Save(ctx context.Context, c Connection) error
	ByID(ctx context.Context, id types.ID) (Connection, error)
	Of(ctx context.Context, user types.ID, form types.Slug) (Connection, error)
	Due(ctx context.Context, now time.Time) ([]Connection, error)
	Delete(ctx context.Context, id types.ID) error

	Rows(ctx context.Context, id types.ID) (map[types.ID]bool, error)
	AddRows(ctx context.Context, id types.ID, rows []types.ID) error
	Columns(ctx context.Context, id types.ID) (map[string]bool, error)
	AddColumns(ctx context.Context, id types.ID, keys []string) error
	Forget(ctx context.Context, id types.ID) error

	Log(ctx context.Context, id types.ID, at time.Time, text string) error
	Events(ctx context.Context, id types.ID, limit int) ([]Event, error)
}

// Google is the part of foundation/google used here.
type Google interface {
	AuthURL(state, verifier, loginHint string) string
	Exchange(ctx context.Context, now time.Time, code, verifier string) (google.Grant, error)
	Refresh(ctx context.Context, now time.Time, refresh string) (google.Access, error)
	Revoke(ctx context.Context, token string) error

	Create(ctx context.Context, token, title, tab, locale, timeZone string) (google.Created, error)
	Tabs(ctx context.Context, token, spreadsheet string) (map[int64]string, error)
	Tagged(ctx context.Context, token, spreadsheet, key string) ([]google.Tag, error)
	ReadColumns(ctx context.Context, token, spreadsheet string, sheet int64, columns []int) ([][]any, error)
	FileOf(ctx context.Context, token, spreadsheet string) (google.File, error)
	Apply(ctx context.Context, token, spreadsheet string, edits []google.Edit) ([]int64, error)
}

// Forms reads definitions.
type Forms interface {
	ByID(slug types.Slug) (formbus.Form, error)
}

// Submissions reads what a sheet is built from.
type Submissions interface {
	ForSheet(ctx context.Context, form types.Slug) ([]submissionbus.Submission, map[types.ID]bool, error)
}

// Access says whether somebody may still read a form's answers.
type Access interface {
	Allowed(ctx context.Context, userID types.ID, form types.Slug, want accessbus.Role) (bool, error)
}

// Accounts reads the person behind a connection.
type Accounts interface {
	ByID(ctx context.Context, id types.ID) (userbus.User, error)
}

// Config is what the domain needs.
type Config struct {
	Log      *slog.Logger
	Store    Storer
	Google   Google
	Forms    Forms
	Subs     Submissions
	Access   Access
	Accounts Accounts
	Mail     mail.Sender

	// SecretBytes is the configured google.token_key: refresh tokens are
	// sealed under one key derived from it, and the state that carries a
	// sign-in to Google and back is signed with another.
	SecretBytes []byte

	// Zone is the time zone spreadsheets are made in and dates are written
	// in; ZoneName is its IANA name, which Google wants.
	Zone     *time.Location
	ZoneName string

	// AdminBaseURL makes the link to the page in an email.
	AdminBaseURL string
}

// Business keeps the sheets.
type Business struct {
	cfg      Config
	sealer   seal.Key
	stateKey []byte

	mu      sync.Mutex
	locks   map[types.ID]*sync.Mutex
	access  map[types.ID]google.Access
	backoff map[types.ID]time.Duration
}

// NewBusiness makes one.
func NewBusiness(cfg Config) (*Business, error) {
	switch {
	case cfg.Log == nil || cfg.Store == nil || cfg.Google == nil || cfg.Forms == nil ||
		cfg.Subs == nil || cfg.Access == nil || cfg.Accounts == nil || cfg.Mail == nil:
		return nil, errors.New("keeping Google Sheets needs every one of its dependencies")
	case len(cfg.SecretBytes) == 0:
		return nil, errors.New("keeping Google Sheets needs a key to seal the connections with")
	case cfg.Zone == nil || cfg.ZoneName == "":
		return nil, errors.New("keeping Google Sheets needs the time zone dates are written in")
	}

	sealer, err := seal.FromBytes(seal.Derive(cfg.SecretBytes, "dropin-forms/sheet-token/v1"))
	if err != nil {
		return nil, err
	}

	return &Business{
		cfg:      cfg,
		sealer:   sealer,
		stateKey: seal.Derive(cfg.SecretBytes, "dropin-forms/sheet-state/v1"),
		locks:    map[types.ID]*sync.Mutex{},
		access:   map[types.ID]google.Access{},
		backoff:  map[types.ID]time.Duration{},
	}, nil
}

// lock serialises everything done to one connection: the worker's pass and a
// person pressing Sync now must not both write.
func (b *Business) lock(id types.ID) func() {
	b.mu.Lock()

	l, ok := b.locks[id]
	if !ok {
		l = &sync.Mutex{}
		b.locks[id] = l
	}

	b.mu.Unlock()

	l.Lock()

	return l.Unlock
}

// --- connecting ----------------------------------------------------------------

// stateLife is how long somebody has on Google's consent screen.
const stateLife = 15 * time.Minute

// Begin is where to send somebody to connect a sheet of form, and the PKCE
// verifier the browser keeps until it comes back.
func (b *Business) Begin(ctx context.Context, now time.Time, user types.ID, form types.Slug, hint string) (string, string, error) {
	if ok, err := b.cfg.Access.Allowed(ctx, user, form, accessbus.RoleResults); err != nil {
		return "", "", fmt.Errorf("checking access: %w", err)
	} else if !ok {
		return "", "", ErrNoAccess
	}

	verifier := google.Verifier()

	return b.cfg.Google.AuthURL(b.mintState(now, user, form), verifier, hint), verifier, nil
}

// mintState signs who is connecting what, until when: the state parameter
// that comes back from Google with the code. A state from one person's
// sign-in cannot be finished by another, or for another form.
func (b *Business) mintState(now time.Time, user types.ID, form types.Slug) string {
	nonce := make([]byte, 12)
	_, _ = rand.Read(nonce) // crypto/rand.Read never fails, per its documentation.

	var exp [8]byte
	binary.BigEndian.PutUint64(exp[:], uint64(now.Add(stateLife).Unix()))

	payload := strings.Join([]string{user.String(), form.String(), base64.RawURLEncoding.EncodeToString(exp[:]), base64.RawURLEncoding.EncodeToString(nonce)}, "~")
	mac := hmac.New(sha256.New, b.stateKey)
	mac.Write([]byte(payload))

	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ReadState checks a state and says which form it is for, for the person now
// signed in.
func (b *Business) ReadState(now time.Time, user types.ID, state string) (types.Slug, error) {
	p, m, ok := strings.Cut(state, ".")
	if !ok {
		return types.Slug{}, ErrBadState
	}

	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	sig, err2 := base64.RawURLEncoding.DecodeString(m)
	if err1 != nil || err2 != nil {
		return types.Slug{}, ErrBadState
	}

	mac := hmac.New(sha256.New, b.stateKey)
	mac.Write(payload)

	if !hmac.Equal(sig, mac.Sum(nil)) {
		return types.Slug{}, ErrBadState
	}

	parts := strings.Split(string(payload), "~")
	if len(parts) != 4 || parts[0] != user.String() {
		return types.Slug{}, ErrBadState
	}

	exp, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(exp) != 8 || now.Unix() > int64(binary.BigEndian.Uint64(exp)) {
		return types.Slug{}, ErrBadState
	}

	form, err := types.ParseSlug(parts[1])
	if err != nil {
		return types.Slug{}, ErrBadState
	}

	return form, nil
}

// Finish completes a connection when Google sends the person back: the code
// for tokens, a spreadsheet made and filled, or -- reconnecting with the same
// Google account to a spreadsheet that is still there -- the same one carried
// on with.
func (b *Business) Finish(ctx context.Context, now time.Time, user types.ID, state, code, verifier string) (Connection, error) {
	form, err := b.ReadState(now, user, state)
	if err != nil {
		return Connection{}, err
	}

	f, err := b.cfg.Forms.ByID(form)
	if err != nil {
		return Connection{}, fmt.Errorf("reading the form: %w", err)
	}

	if ok, err := b.cfg.Access.Allowed(ctx, user, form, accessbus.RoleResults); err != nil {
		return Connection{}, fmt.Errorf("checking access: %w", err)
	} else if !ok {
		return Connection{}, ErrNoAccess
	}

	grant, err := b.cfg.Google.Exchange(ctx, now, code, verifier)
	if err != nil {
		return Connection{}, fmt.Errorf("finishing the sign-in with Google: %w", err)
	}

	if !grant.Files {
		// Revoked so the half-made connection does not sit in their Google
		// account's list of apps doing nothing.
		_ = b.cfg.Google.Revoke(ctx, grant.Refresh)

		return Connection{}, ErrNoFiles
	}

	was, err := b.cfg.Store.Of(ctx, user, form)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Connection{}, err
	}

	existing := err == nil

	c := was
	if !existing {
		c = Connection{ID: types.NewID(), User: user, Form: form, CreatedAt: now}
	}

	defer b.lock(c.ID)()

	oldToken := b.openToken(c)

	c.Token, err = b.sealer.Seal(grant.Refresh, c.ID.String())
	if err != nil {
		return Connection{}, err
	}

	b.remember(c.ID, google.Access{Token: grant.Access, Expires: grant.Expires})

	// The same Google account and a spreadsheet it can still reach: carry on
	// with it. Anything else is a new spreadsheet -- drive.file reaches only
	// the files made for that account, so another account cannot open the
	// old one at all.
	keep := existing && c.Spreadsheet != "" && strings.EqualFold(c.Account, grant.Email)
	if keep {
		if file, err := b.cfg.Google.FileOf(ctx, grant.Access, c.Spreadsheet); err != nil || file.Trashed {
			keep = false
		}
	}

	c.Account = grant.Email
	c.State, c.Problem, c.Told = StateOK, "", ""
	c.FailingSince, c.NextAt = time.Time{}, now

	if keep {
		c.Digest = ""

		if err := b.cfg.Store.Save(ctx, c); err != nil {
			return Connection{}, err
		}

		b.log(ctx, c, now, "Connected again as "+grant.Email+".")
	} else {
		if c, err = b.makeSpreadsheet(ctx, now, c, f, grant.Access); err != nil {
			return Connection{}, err
		}

		b.log(ctx, c, now, "Connected as "+grant.Email+"; made the spreadsheet.")
	}

	if oldToken != "" {
		// The one replaced, so it stops being listed in their account.
		_ = b.cfg.Google.Revoke(ctx, oldToken)
	}

	return b.sync(ctx, now, c, false)
}

// Remake makes a new spreadsheet for a connection whose old one is gone.
func (b *Business) Remake(ctx context.Context, now time.Time, user types.ID, form types.Slug) (Connection, error) {
	c, err := b.cfg.Store.Of(ctx, user, form)
	if err != nil {
		return Connection{}, err
	}

	defer b.lock(c.ID)()

	f, err := b.cfg.Forms.ByID(form)
	if err != nil {
		return Connection{}, fmt.Errorf("reading the form: %w", err)
	}

	token, err := b.token(ctx, now, c)
	if err != nil {
		return b.failed(ctx, now, c, err)
	}

	if c, err = b.makeSpreadsheet(ctx, now, c, f, token); err != nil {
		return b.failed(ctx, now, c, err)
	}

	c.State, c.Problem, c.Told = StateOK, "", ""
	b.log(ctx, c, now, "Made a new spreadsheet.")

	return b.sync(ctx, now, c, false)
}

// makeSpreadsheet makes and sets up an empty spreadsheet for c, forgetting
// what was written to any earlier one.
func (b *Business) makeSpreadsheet(ctx context.Context, now time.Time, c Connection, f formbus.Form, token string) (Connection, error) {
	made, err := b.cfg.Google.Create(ctx, token, f.Title, tabTitle, "en_US", b.cfg.ZoneName)
	if err != nil {
		return c, fmt.Errorf("making the spreadsheet: %w", err)
	}

	c.Spreadsheet, c.Sheet, c.URL = made.SpreadsheetID, made.SheetID, made.URL

	if err := b.cfg.Store.Forget(ctx, c.ID); err != nil {
		return c, err
	}

	// Filled straight away rather than left to a pass, which would find no
	// tagged columns in an empty tab and take it for one beyond use.
	want, err := b.rows(ctx, f, c)
	if err != nil {
		return c, err
	}

	if err := b.fill(ctx, c, f, want, token); err != nil {
		return c, fmt.Errorf("filling the spreadsheet: %w", err)
	}

	c.Digest, c.CheckedAt, c.SyncedAt = want.digest(), now, now

	if err := b.cfg.Store.Save(ctx, c); err != nil {
		return c, err
	}

	return c, nil
}

// Disconnect stops keeping somebody's sheet and withdraws the permission they
// gave. The spreadsheet stays in their Drive: it is theirs.
func (b *Business) Disconnect(ctx context.Context, user types.ID, form types.Slug) error {
	c, err := b.cfg.Store.Of(ctx, user, form)
	if err != nil {
		return err
	}

	defer b.lock(c.ID)()

	if t := b.openToken(c); t != "" {
		if err := b.cfg.Google.Revoke(ctx, t); err != nil {
			b.cfg.Log.Warn("a disconnected sheet's permission could not be withdrawn at Google",
				"connection", c.ID.String(), "error", err,
				"consequence", "it stays listed in their Google account until they remove it; nothing here uses it")
		}
	}

	b.mu.Lock()
	delete(b.access, c.ID)
	delete(b.backoff, c.ID)
	b.mu.Unlock()

	return b.cfg.Store.Delete(ctx, c.ID)
}

// Mine is a person's connection to a form and what has happened to it.
func (b *Business) Mine(ctx context.Context, user types.ID, form types.Slug) (Connection, []Event, error) {
	c, err := b.cfg.Store.Of(ctx, user, form)
	if err != nil {
		return Connection{}, nil, err
	}

	events, err := b.cfg.Store.Events(ctx, c.ID, 10)
	if err != nil {
		return c, nil, err
	}

	return c, events, nil
}

// --- syncing ---------------------------------------------------------------------

// Now syncs a person's sheet straight away -- the Sync now and Refill buttons.
// Refill puts back every row and column they deleted.
func (b *Business) Now(ctx context.Context, now time.Time, user types.ID, form types.Slug, refill bool) (Connection, error) {
	c, err := b.cfg.Store.Of(ctx, user, form)
	if err != nil {
		return Connection{}, err
	}

	defer b.lock(c.ID)()

	switch c.State {
	case StateReconnect:
		return c, ErrReconnect
	case StateGone:
		return c, ErrGone
	}

	if refill {
		b.log(ctx, c, now, "Refilled: rows and columns that had been deleted were put back.")
	}

	return b.sync(ctx, now, c, refill)
}

// Due is every connection the worker should sync now.
func (b *Business) Due(ctx context.Context, now time.Time) ([]Connection, error) {
	return b.cfg.Store.Due(ctx, now)
}

// Pass is the worker's sync of one connection.
func (b *Business) Pass(ctx context.Context, now time.Time, id types.ID) error {
	defer b.lock(id)()

	c, err := b.cfg.Store.ByID(ctx, id)
	if err != nil {
		return err
	}

	// A stopped one is looked at too: the person's access may have come
	// back, and the pass is what notices.
	if (c.State != StateOK && c.State != StateStopped) || now.Before(c.NextAt) {
		return nil
	}

	_, err = b.sync(ctx, now, c, false)

	return err
}

// interval is how soon after a pass the next is due. The worker ticks at the
// same rate; most passes stop at the digest without asking Google anything.
const interval = 2 * time.Minute

// recheck is how long a sheet goes unread when nothing here has changed. An
// hour: what somebody did to the sheet itself -- deleted a tab, edited one
// of the form's cells -- is noticed within it.
const recheck = time.Hour

// sync is one pass. Called with the connection's lock held.
func (b *Business) sync(ctx context.Context, now time.Time, c Connection, refill bool) (Connection, error) {
	f, err := b.cfg.Forms.ByID(c.Form)
	if errors.Is(err, formbus.ErrNotFound) {
		return b.stop(ctx, now, c, "deleted", "The form "+c.Form.String()+" was deleted, so this sheet is no longer kept up to date.")
	}

	if err != nil {
		return c, fmt.Errorf("reading the form: %w", err)
	}

	who, err := b.cfg.Accounts.ByID(ctx, c.User)
	if err != nil {
		return c, fmt.Errorf("reading the account: %w", err)
	}

	allowed, err := b.cfg.Access.Allowed(ctx, c.User, c.Form, accessbus.RoleResults)
	if err != nil {
		return c, fmt.Errorf("checking access: %w", err)
	}

	if !allowed || !who.Enabled {
		return b.stop(ctx, now, c, "access", "You no longer have access to the answers of "+f.Title+", so this sheet is no longer kept up to date.")
	}

	if c.State == StateStopped {
		// Access is back. Carry on, and read the sheet properly.
		c.State, c.Problem, c.Told, c.Digest = StateOK, "", "", ""
		b.log(ctx, c, now, "Access to the form is back; the sheet is kept up to date again.")
	}

	want, err := b.rows(ctx, f, c)
	if err != nil {
		return c, err
	}

	digest := want.digest()

	if !refill && digest == c.Digest && now.Sub(c.CheckedAt) < recheck {
		c.NextAt = now.Add(interval)

		return c, b.cfg.Store.Save(ctx, c)
	}

	token, err := b.token(ctx, now, c)
	if err != nil {
		return b.failed(ctx, now, c, err)
	}

	if c, err = b.reconcile(ctx, now, c, f, want, token, refill); err != nil {
		return b.failed(ctx, now, c, err)
	}

	c.Digest = digest
	c.CheckedAt, c.SyncedAt, c.NextAt = now, now, now.Add(interval)
	c.Problem, c.Told, c.FailingSince = "", "", time.Time{}

	b.mu.Lock()
	delete(b.backoff, c.ID)
	b.mu.Unlock()

	return c, b.cfg.Store.Save(ctx, c)
}

// token is a working access token for c, refreshed when the one held has
// less than a minute left.
func (b *Business) token(ctx context.Context, now time.Time, c Connection) (string, error) {
	b.mu.Lock()
	a, ok := b.access[c.ID]
	b.mu.Unlock()

	if ok && now.Add(time.Minute).Before(a.Expires) {
		return a.Token, nil
	}

	refresh := b.openToken(c)
	if refresh == "" {
		return "", google.ErrRevoked
	}

	a, err := b.cfg.Google.Refresh(ctx, now, refresh)
	if err != nil {
		return "", err
	}

	b.remember(c.ID, a)

	return a.Token, nil
}

func (b *Business) remember(id types.ID, a google.Access) {
	b.mu.Lock()
	b.access[id] = a
	b.mu.Unlock()
}

// openToken is c's refresh token, or empty when there is none to open.
func (b *Business) openToken(c Connection) string {
	if len(c.Token) == 0 {
		return ""
	}

	t, err := b.sealer.Open(c.Token, c.ID.String())
	if err != nil {
		b.cfg.Log.Error("a sheet connection's token does not open",
			"connection", c.ID.String(), "error", err,
			"consequence", "the person is asked to connect again; check google.token_key has not changed")

		return ""
	}

	return t
}

// stop is a connection the service's own rules no longer let flow.
func (b *Business) stop(ctx context.Context, now time.Time, c Connection, why, sentence string) (Connection, error) {
	c.State, c.Problem, c.NextAt = StateStopped, sentence, now.Add(interval)

	if c.Told != why {
		c.Told = why
		b.log(ctx, c, now, sentence)
		b.tell(ctx, c, "Your Google Sheet has stopped updating", sentence+
			"\n\nWhat is already in the spreadsheet stays there; it is yours. If your access comes back, it will start updating again by itself.")
	}

	return c, b.cfg.Store.Save(ctx, c)
}

// failed records a pass that did not work, and decides what it means.
func (b *Business) failed(ctx context.Context, now time.Time, c Connection, err error) (Connection, error) {
	switch {
	case errors.Is(err, google.ErrRevoked):
		c.State = StateReconnect
		c.Problem = "Google no longer lets this service reach your spreadsheet: the permission was withdrawn in your Google account, or it expired. Connect again to carry on."

		if c.Told != "reconnect" {
			c.Told = "reconnect"
			b.log(ctx, c, now, c.Problem)
			b.tell(ctx, c, "Reconnect your Google Sheet", c.Problem)
		}

	case errors.Is(err, google.ErrGone):
		c.State = StateGone
		c.Problem = "The spreadsheet has been deleted or moved to the bin, so it is no longer kept up to date. Make a new one from the page, or disconnect."

		if c.Told != "gone" {
			c.Told = "gone"
			b.log(ctx, c, now, c.Problem)
			b.tell(ctx, c, "Your Google Sheet can't be reached", c.Problem)
		}

	default:
		// Busy, refused, or anything else: try again, less often each time,
		// and say so once it has gone on for a day.
		if c.FailingSince.IsZero() {
			c.FailingSince = now
		}

		b.mu.Lock()
		wait := b.backoff[c.ID]*2 + interval
		wait = min(wait, time.Hour)

		if busy, ok := errors.AsType[google.Busy](err); ok && busy.RetryAfter > wait {
			wait = busy.RetryAfter
		}

		b.backoff[c.ID] = wait
		b.mu.Unlock()

		c.NextAt = now.Add(wait)
		c.Problem = "The last update did not work, and it is being tried again: " + plain(err)

		b.cfg.Log.Warn("a Google Sheet could not be updated",
			"connection", c.ID.String(), "form", c.Form.String(), "error", err, "next", c.NextAt)

		if now.Sub(c.FailingSince) > 24*time.Hour && c.Told != "failing" {
			c.Told = "failing"
			b.log(ctx, c, now, "Updates have not worked for a day.")
			b.tell(ctx, c, "Your Google Sheet has not updated for a day", c.Problem+
				"\n\nIt is still being tried. If you have deleted the spreadsheet or taken this service's permission away in your Google account, disconnect it on the page.")
		}
	}

	if serr := b.cfg.Store.Save(ctx, c); serr != nil {
		return c, serr
	}

	return c, err
}

// plain is an error as a sentence for the page, without the package words.
func plain(err error) string {
	s := err.Error()

	if i := strings.LastIndex(s, ": "); i >= 0 && len(s)-i > 12 {
		s = s[i+2:]
	}

	return s
}

func (b *Business) log(ctx context.Context, c Connection, now time.Time, text string) {
	if err := b.cfg.Store.Log(ctx, c.ID, now, text); err != nil {
		b.cfg.Log.Warn("a sheet event could not be recorded", "connection", c.ID.String(), "error", err)
	}
}

// --- what the sheet should hold -------------------------------------------------

// tabTitle is the tab a new spreadsheet's rows go in.
const tabTitle = "Responses"

// Tag keys on the tab and on each of the form's columns.
const (
	tagTab    = "dropin-forms.form"
	tagColumn = "dropin-forms.column"
)

// wanted is what the sheet should hold, worked out from the form and its
// submissions without asking Google anything.
type wanted struct {
	cols []submissionbus.Column
	rows []wantedRow
	seen map[types.ID]bool // in the ledger: written to this sheet before
	had  map[string]bool   // columns written before
}

type wantedRow struct {
	id    types.ID
	cells []google.Value
}

// rows builds wanted. A submission gets a row when the office's lists show
// it and it is not hidden, or when it already has one: once in, a row stays,
// and its Status cell says what has happened since.
func (b *Business) rows(ctx context.Context, f formbus.Form, c Connection) (wanted, error) {
	subs, hidden, err := b.cfg.Subs.ForSheet(ctx, f.ID)
	if err != nil {
		return wanted{}, err
	}

	seen, err := b.cfg.Store.Rows(ctx, c.ID)
	if err != nil {
		return wanted{}, err
	}

	had, err := b.cfg.Store.Columns(ctx, c.ID)
	if err != nil {
		return wanted{}, err
	}

	w := wanted{cols: submissionbus.Columns(f), seen: seen, had: had}

	for _, s := range subs {
		if !seen[s.ID] && (!s.Listed() || hidden[s.ID]) {
			continue
		}

		cells := submissionbus.Cells(f, s)
		row := make([]google.Value, len(cells))

		for i, cell := range cells {
			row[i] = b.value(cell)

			if w.cols[i].Key == "status" {
				words := s.Words()
				if hidden[s.ID] {
					words = "hidden"
				}

				row[i] = google.Value{Kind: google.Text, Text: words}
			}
		}

		w.rows = append(w.rows, wantedRow{id: s.ID, cells: row})
	}

	return w, nil
}

// value is a cell as the sheet writes it: numbers as numbers, times as dates
// in the spreadsheet's own time zone.
func (b *Business) value(c submissionbus.Cell) google.Value {
	if c.Empty {
		return google.Value{}
	}

	switch c.Kind {
	case submissionbus.CellCount:
		return google.Value{Kind: google.Number, Number: float64(c.Count)}
	case submissionbus.CellMoney:
		return google.Value{Kind: google.Money, Number: float64(c.Money) / 100}
	case submissionbus.CellTime:
		return google.Value{Kind: google.Date, Time: c.Time.In(b.cfg.Zone)}
	}

	if c.Text == "" {
		return google.Value{}
	}

	return google.Value{Kind: google.Text, Text: c.Text}
}

func (w wanted) digest() string {
	h := sha256.New()

	for _, c := range w.cols {
		fmt.Fprintf(h, "%s=%s\x00", c.Key, c.Label)
	}

	for _, r := range w.rows {
		fmt.Fprintf(h, "\x01%s", r.id)

		for _, v := range r.cells {
			fmt.Fprintf(h, "\x00%d:%s:%v:%d", v.Kind, v.Text, v.Number, v.Time.Unix())
		}
	}

	return hex.EncodeToString(h.Sum(nil))
}

// same says whether what a cell holds now is what it should.
func same(now any, want google.Value) bool {
	switch want.Kind {
	case google.Empty:
		return now == nil || now == ""
	case google.Text:
		s, ok := now.(string)
		return ok && s == want.Text
	case google.Number, google.Money:
		n, ok := now.(float64)
		return ok && math.Abs(n-want.Number) < 1e-6
	case google.Date:
		n, ok := now.(float64)
		return ok && math.Abs(n-google.Serial(want.Time)) < 0.5/86400
	}

	return false
}

// Package userbus holds accounts and how somebody proves they hold one.
//
// Sign-in is by emailed link, with backup codes for when mail is not working
// and a one-time bootstrap secret for when nothing is working yet. There is no
// password, which removes password storage, password reset, password reuse and
// password strength from this service entirely -- the mailbox is the factor,
// and it is a factor the person already maintains.
//
// # There is no CLI, and that decides the shape of this package
//
// Every operational thing about this service happens in a browser, which means
// reading submissions needs a session, which means sign-in has to work before
// anything else does. And sign-in by emailed link cannot work until outbound
// mail works. That circularity is the reason [Business.Bootstrap] exists: a
// one-time secret from the config file that produces a session without
// sending anything. Without it, a misconfigured mail relay is not an
// inconvenience, it is being locked out of your own service on the week of the
// event it was built for.
//
// # What this package refuses to tell anybody
//
// Every failed attempt returns [ErrDenied] and nothing else. Not "no such
// account", not "that link expired", not "wrong code". The app layer cannot
// accidentally leak which half was wrong, because it is never told. In
// particular [Business.RequestSignIn] succeeds for an address that has no
// account, so that the page after it reads the same either way -- a login form
// that answers differently is an account-enumeration oracle, and for a parish
// that means a list of who is involved.
package userbus

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jroedel/dropin-forms/business/types"
)

// How long each credential lasts.
const (
	// Long enough to walk to another device and open the mail, short enough
	// that a link left in an inbox is not a standing key. Mail delivery is
	// occasionally slow, so this is not tighter.
	signInLife = 15 * time.Minute

	// Absolute, with no sliding renewal. A session that renews on use never
	// ends for whoever is using it, which is exactly wrong if the person using
	// it is not the account holder.
	sessionLife = 14 * 24 * time.Hour
)

// The errors this package returns. Everything a stranger can provoke collapses
// into ErrDenied on purpose.
var (
	// ErrDenied is every failed attempt to prove an identity: an unknown
	// address, a disabled account, a token that expired, one already used, a
	// wrong secret, a wrong backup code, a bootstrap secret that has been
	// claimed. One error, so the difference cannot leak.
	ErrDenied = errors.New("that did not work")

	// ErrNotFound is for a lookup by identifier that the caller made, rather
	// than a credential a stranger presented.
	ErrNotFound = errors.New("no such account")

	// ErrEmailTaken is returned to an administrator creating an account, where
	// the collision is information they are entitled to.
	ErrEmailTaken = errors.New("an account already has that address")
)

// User is an account.
type User struct {
	ID        types.ID
	Email     types.Email
	Name      string
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Enabled is a positive field rather than a Disabled one, so that the zero
// User -- the thing returned alongside every error in this package -- is an
// account that cannot sign in and cannot do anything. A caller that ignores an
// error gets a useless value rather than a privileged one.

// NewUser is what an administrator supplies to create an account.
type NewUser struct {
	Email types.Email
	Name  string
}

// Token is a single-use credential delivered out of band: the emailed sign-in
// link, and nothing else so far.
//
// Only the hash is stored. A leaked database does not let anybody sign in as
// somebody else, which matters more here than for a password, because these
// arrive by mail and mail is archived forever.
type Token struct {
	ID        types.ID
	UserID    types.ID
	Hash      []byte
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    time.Time // zero means unused
}

// Session is a signed-in browser.
type Session struct {
	ID         types.ID
	UserID     types.ID
	Hash       []byte
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

// BackupCode is one of the codes issued for when mail is not working.
type BackupCode struct {
	ID        types.ID
	UserID    types.ID
	Hash      []byte
	CreatedAt time.Time
	UsedAt    time.Time // zero means unused
}

// Storer is what this package needs from storage.
//
// Three of these return a bool rather than an error for the "somebody else got
// there first" case, and that is the important part of this interface.
// UseToken, UseBackupCode and ClaimBootstrap must each be a single atomic
// claim -- an UPDATE with a WHERE that only matches an unused row, reporting
// whether it matched. A SELECT followed by an UPDATE would let two
// simultaneous requests both see an unused credential and both succeed, which
// for a single-use sign-in link is the whole property gone.
type Storer interface {
	CreateUser(ctx context.Context, u User) error
	UpdateUser(ctx context.Context, u User) error
	UserByID(ctx context.Context, id types.ID) (User, error)
	UserByEmail(ctx context.Context, email types.Email) (User, error)
	Users(ctx context.Context) ([]User, error)

	CreateToken(ctx context.Context, t Token) error
	TokenByID(ctx context.Context, id types.ID) (Token, error)
	UseToken(ctx context.Context, id types.ID, at time.Time) (bool, error)

	ReplaceBackupCodes(ctx context.Context, userID types.ID, codes []BackupCode) error
	BackupCodes(ctx context.Context, userID types.ID) ([]BackupCode, error)
	UseBackupCode(ctx context.Context, id types.ID, at time.Time) (bool, error)

	CreateSession(ctx context.Context, s Session) error
	SessionByID(ctx context.Context, id types.ID) (Session, error)
	TouchSession(ctx context.Context, id types.ID, at time.Time) error
	DeleteSession(ctx context.Context, id types.ID) error
	DeleteUserSessions(ctx context.Context, userID types.ID) error

	// ClaimBootstrap records that the bootstrap secret has been spent, and
	// reports false if it already had been.
	ClaimBootstrap(ctx context.Context, at time.Time) (bool, error)

	PruneExpired(ctx context.Context, before time.Time) error
}

// Business is the set of operations on accounts.
type Business struct {
	log   *slog.Logger
	store Storer
}

// NewBusiness constructs one.
func NewBusiness(log *slog.Logger, store Storer) *Business {
	return &Business{log: log, store: store}
}

// Create adds an account. Only an administrator reaches this; there is no
// self-registration, because an account here can read other people's
// submissions.
func (b *Business) Create(ctx context.Context, now time.Time, nu NewUser) (User, error) {
	if nu.Email.Zero() {
		return User{}, fmt.Errorf("an account needs an email address")
	}

	switch _, err := b.store.UserByEmail(ctx, nu.Email); {
	case err == nil:
		return User{}, ErrEmailTaken
	case !errors.Is(err, ErrNotFound):
		return User{}, fmt.Errorf("the accounts could not be read: %w", err)
	}

	u := User{
		ID:        types.NewID(),
		Email:     nu.Email,
		Name:      nu.Name,
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := b.store.CreateUser(ctx, u); err != nil {
		return User{}, fmt.Errorf("the account could not be saved: %w", err)
	}

	return u, nil
}

// ByID returns an account.
func (b *Business) ByID(ctx context.Context, id types.ID) (User, error) {
	return b.store.UserByID(ctx, id)
}

// ByEmail returns the account holding an address, or [ErrNotFound].
//
// This is the administrator's lookup -- "does this person already have an
// account" -- and it is deliberately not the one a sign-in uses.
// [Business.RequestSignIn] answers the same question and tells nobody, because
// there the asker is a stranger and the answer is whether an address is worth
// attacking. Here the asker is already signed in and holds admin on a form, so
// the collision is information they are entitled to, in the same way
// [ErrEmailTaken] is.
func (b *Business) ByEmail(ctx context.Context, email types.Email) (User, error) {
	return b.store.UserByEmail(ctx, email)
}

// All returns every account, for the administration screen.
func (b *Business) All(ctx context.Context) ([]User, error) {
	return b.store.Users(ctx)
}

// SignInRequest is the result of asking for a sign-in link.
//
// Secret is empty when there is no account to send to, and the caller must
// render the same page either way. That is the whole reason this returns a
// struct with an empty field rather than an error: an error would tempt a
// handler into saying something different, and saying something different is
// the enumeration oracle.
type SignInRequest struct {
	User   User
	Secret string // empty means: send nothing, say the same thing
}

// Sendable reports whether there is actually a link to mail.
func (r SignInRequest) Sendable() bool { return r.Secret != "" }

// RequestSignIn mints a sign-in link for an address, if an account holds it.
//
// It returns no error for an unknown or disabled account. The caller sends
// mail only when [SignInRequest.Sendable] is true and otherwise does nothing,
// and in both cases renders "check your email". The pause before that page is
// not quite identical in the two cases, since one of them sends a message, and
// a determined attacker can time it -- narrowing that is step 9's rate
// limiting, not something to fake here with a sleep.
func (b *Business) RequestSignIn(ctx context.Context, now time.Time, email types.Email) (SignInRequest, error) {
	u, err := b.store.UserByEmail(ctx, email)

	switch {
	case errors.Is(err, ErrNotFound):
		// Logged, because somebody trying addresses is worth seeing, and the
		// log is the one place it is safe to record.
		b.log.Info("sign-in requested for an address with no account", "email", email.String())

		return SignInRequest{}, nil

	case err != nil:
		return SignInRequest{}, fmt.Errorf("the accounts could not be read: %w", err)

	case !u.Enabled:
		b.log.Info("sign-in requested for a disabled account", "user_id", u.ID.String())

		return SignInRequest{}, nil
	}

	cred := mintCredential()

	t := Token{
		ID:        cred.id,
		UserID:    u.ID,
		Hash:      cred.hash,
		CreatedAt: now,
		ExpiresAt: now.Add(signInLife),
	}

	if err := b.store.CreateToken(ctx, t); err != nil {
		return SignInRequest{}, fmt.Errorf("the sign-in link could not be saved: %w", err)
	}

	return SignInRequest{User: u, Secret: cred.String()}, nil
}

// SignIn redeems a sign-in link and returns a session credential.
//
// The presented string is what came back from the sign-in *form*, not from the
// link being opened. That distinction is load-bearing and is enforced by the
// app layer: the emailed link is a GET that renders a page with a button,
// because mail scanners and link previewers fetch URLs in messages, and a
// single-use token redeemed on GET is a token spent by software before the
// person ever clicks.
func (b *Business) SignIn(ctx context.Context, now time.Time, presented string) (User, string, error) {
	id, secret, err := splitCredential(presented)
	if err != nil {
		return User{}, "", ErrDenied
	}

	t, err := b.store.TokenByID(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return User{}, "", ErrDenied
	case err != nil:
		return User{}, "", fmt.Errorf("the sign-in link could not be read: %w", err)
	}

	// The secret is checked before anything else is believed about the row,
	// so that a guessed identifier cannot learn whether a token expired or
	// was already used.
	if !verifySecret(t.Hash, secret) {
		return User{}, "", ErrDenied
	}

	switch {
	case !t.UsedAt.IsZero():
		b.log.Warn("a sign-in link was presented twice", "token_id", t.ID.String(), "user_id", t.UserID.String())

		return User{}, "", ErrDenied
	case !now.Before(t.ExpiresAt):
		return User{}, "", ErrDenied
	}

	// The atomic claim. Between the check above and here, another request
	// holding the same link may have spent it, and only the store can settle
	// that -- which is why this reports a bool rather than trusting the read.
	claimed, err := b.store.UseToken(ctx, t.ID, now)
	switch {
	case err != nil:
		return User{}, "", fmt.Errorf("the sign-in link could not be spent: %w", err)
	case !claimed:
		b.log.Warn("two requests raced for one sign-in link", "token_id", t.ID.String())

		return User{}, "", ErrDenied
	}

	return b.start(ctx, now, t.UserID)
}

// SignInWithBackupCode is the way in when mail is not working.
//
// The address is asked for as well as the code, because a code has no
// identifier in it -- see the comment in credential.go -- so there is nothing
// to look it up by.
func (b *Business) SignInWithBackupCode(ctx context.Context, now time.Time, email types.Email, typed string) (User, string, error) {
	normalised := normaliseBackupCode(typed)
	if len(normalised) != backupCodeLen {
		return User{}, "", ErrDenied
	}

	u, err := b.store.UserByEmail(ctx, email)
	switch {
	case errors.Is(err, ErrNotFound):
		return User{}, "", ErrDenied
	case err != nil:
		return User{}, "", fmt.Errorf("the accounts could not be read: %w", err)
	case !u.Enabled:
		return User{}, "", ErrDenied
	}

	codes, err := b.store.BackupCodes(ctx, u.ID)
	if err != nil {
		return User{}, "", fmt.Errorf("the backup codes could not be read: %w", err)
	}

	// Every code is checked even after a match is found, so that the time
	// taken does not reveal which position matched or how many codes remain.
	// Ten SHA-256 evaluations cost nothing; this is the reason the codes carry
	// 80 bits rather than relying on a key derivation function.
	var match BackupCode

	for _, c := range codes {
		if !c.UsedAt.IsZero() {
			continue
		}

		if verifySecret(c.Hash, normalised) && match.ID.Zero() {
			match = c
		}
	}

	if match.ID.Zero() {
		b.log.Warn("a backup code did not match", "user_id", u.ID.String())

		return User{}, "", ErrDenied
	}

	claimed, err := b.store.UseBackupCode(ctx, match.ID, now)
	switch {
	case err != nil:
		return User{}, "", fmt.Errorf("the backup code could not be spent: %w", err)
	case !claimed:
		return User{}, "", ErrDenied
	}

	b.log.Warn("signed in with a backup code", "user_id", u.ID.String())

	return b.start(ctx, now, u.ID)
}

// Bootstrap trades the one-time secret from the config file for a session,
// creating the account if it does not exist yet.
//
// This is what makes a mail misconfiguration recoverable rather than fatal. It
// works exactly once: the claim is recorded in storage, so restarting the
// process does not restore it, and re-reading the config file does not either.
// Rotating the secret in the config does not revive it, which is deliberate --
// the marker is the record that a founding session was created, and creating a
// second one silently is not something a config edit should be able to do.
func (b *Business) Bootstrap(ctx context.Context, now time.Time, configured, presented string, email types.Email) (User, string, error) {
	// Constant time, and length-checked first. The configured secret is not a
	// hash to compare against, it is the secret itself, so this is the one
	// place a direct comparison happens -- and ConstantTimeCompare returns 0
	// on a length mismatch, which would otherwise be a free length oracle.
	switch {
	case configured == "":
		return User{}, "", ErrDenied
	case len(presented) != len(configured):
		return User{}, "", ErrDenied
	case subtle.ConstantTimeCompare([]byte(presented), []byte(configured)) != 1:
		b.log.Warn("the bootstrap secret was presented incorrectly")

		return User{}, "", ErrDenied
	}

	claimed, err := b.store.ClaimBootstrap(ctx, now)
	switch {
	case err != nil:
		return User{}, "", fmt.Errorf("the bootstrap could not be recorded: %w", err)
	case !claimed:
		b.log.Warn("the bootstrap secret was presented after it had been spent")

		return User{}, "", ErrDenied
	}

	u, err := b.store.UserByEmail(ctx, email)

	switch {
	case errors.Is(err, ErrNotFound):
		if u, err = b.Create(ctx, now, NewUser{Email: email}); err != nil {
			return User{}, "", err
		}

		b.log.Warn("bootstrap created the first account", "user_id", u.ID.String(), "email", email.String())

	case err != nil:
		return User{}, "", fmt.Errorf("the accounts could not be read: %w", err)

	case !u.Enabled:
		// The bootstrap has already been spent by now, and that is the right
		// order: a secret presented for a disabled account is still a secret
		// that has been used, and leaving it live would let somebody retry
		// against a different address.
		return User{}, "", ErrDenied

	default:
		b.log.Warn("bootstrap signed in an existing account", "user_id", u.ID.String())
	}

	return b.start(ctx, now, u.ID)
}

// IssueBackupCodes replaces every code this account has and returns the new
// ones, which are shown exactly once.
//
// Replaces rather than adds: a person who has lost track of which codes they
// have needs one honest list, not a growing set they cannot audit. Any unused
// old code stops working, which is also what somebody who suspects a leak
// wants from this button.
func (b *Business) IssueBackupCodes(ctx context.Context, now time.Time, userID types.ID) ([]string, error) {
	shown := make([]string, 0, backupCodeCount)
	stored := make([]BackupCode, 0, backupCodeCount)

	for range backupCodeCount {
		code, hash := mintBackupCode()

		shown = append(shown, code)
		stored = append(stored, BackupCode{
			ID:        types.NewID(),
			UserID:    userID,
			Hash:      hash,
			CreatedAt: now,
		})
	}

	if err := b.store.ReplaceBackupCodes(ctx, userID, stored); err != nil {
		return nil, fmt.Errorf("the backup codes could not be saved: %w", err)
	}

	b.log.Info("issued backup codes", "user_id", userID.String(), "count", len(shown))

	return shown, nil
}

// Authenticate turns a session cookie into the account that holds it.
//
// Called on every request, so it is one indexed read and one constant-time
// comparison, and it never writes. Refreshing the last-seen time on every
// request would make a read-only page a write, which on a single-writer SQLite
// database is how a listing page starts blocking on a submission.
func (b *Business) Authenticate(ctx context.Context, now time.Time, presented string) (User, error) {
	id, secret, err := splitCredential(presented)
	if err != nil {
		return User{}, ErrDenied
	}

	s, err := b.store.SessionByID(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return User{}, ErrDenied
	case err != nil:
		return User{}, fmt.Errorf("the session could not be read: %w", err)
	}

	if !verifySecret(s.Hash, secret) {
		return User{}, ErrDenied
	}

	if !now.Before(s.ExpiresAt) {
		return User{}, ErrDenied
	}

	u, err := b.store.UserByID(ctx, s.UserID)
	switch {
	case errors.Is(err, ErrNotFound):
		return User{}, ErrDenied
	case err != nil:
		return User{}, fmt.Errorf("the account could not be read: %w", err)
	case !u.Enabled:
		// Checked here rather than only at sign-in, so that disabling an
		// account ends its sessions on the next request rather than in two
		// weeks.
		return User{}, ErrDenied
	}

	return u, nil
}

// SignOut ends one session. A malformed or unknown cookie is not an error:
// the caller's intention was to have no session, and it does not.
func (b *Business) SignOut(ctx context.Context, presented string) error {
	id, _, err := splitCredential(presented)
	if err != nil {
		return nil
	}

	if err := b.store.DeleteSession(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("the session could not be ended: %w", err)
	}

	return nil
}

// SignOutEverywhere ends every session an account has, for somebody who has
// lost a device.
func (b *Business) SignOutEverywhere(ctx context.Context, userID types.ID) error {
	if err := b.store.DeleteUserSessions(ctx, userID); err != nil {
		return fmt.Errorf("the sessions could not be ended: %w", err)
	}

	return nil
}

// Touch records that a session was used, for showing somebody their own device
// list. Called on a timer or from a write path, never from every read -- see
// [Business.Authenticate].
func (b *Business) Touch(ctx context.Context, now time.Time, id types.ID) error {
	if err := b.store.TouchSession(ctx, id, now); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("the session could not be updated: %w", err)
	}

	return nil
}

// Prune deletes spent and expired credentials.
//
// Sign-in tokens are recorded at mint, unlike submission grants, because there
// is an account behind every one of them -- so this table cannot be filled by
// a stranger and pruning is housekeeping rather than a defence.
func (b *Business) Prune(ctx context.Context, now time.Time) error {
	if err := b.store.PruneExpired(ctx, now); err != nil {
		return fmt.Errorf("the expired credentials could not be removed: %w", err)
	}

	return nil
}

// start issues a session for an account that has just proved itself. The
// returned string is the cookie value and exists only for this request.
func (b *Business) start(ctx context.Context, now time.Time, userID types.ID) (User, string, error) {
	u, err := b.store.UserByID(ctx, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		// A credential whose account has gone. Not ErrDenied by accident: the
		// credential was good, so this is worth a louder signal than a wrong
		// secret.
		b.log.Error("a valid credential named an account that does not exist", "user_id", userID.String())

		return User{}, "", ErrDenied
	case err != nil:
		return User{}, "", fmt.Errorf("the account could not be read: %w", err)
	case !u.Enabled:
		return User{}, "", ErrDenied
	}

	cred := mintCredential()

	s := Session{
		ID:         cred.id,
		UserID:     u.ID,
		Hash:       cred.hash,
		CreatedAt:  now,
		ExpiresAt:  now.Add(sessionLife),
		LastSeenAt: now,
	}

	if err := b.store.CreateSession(ctx, s); err != nil {
		return User{}, "", fmt.Errorf("the session could not be saved: %w", err)
	}

	b.log.Info("signed in", "user_id", u.ID.String(), "session_id", s.ID.String())

	return u, cred.String(), nil
}

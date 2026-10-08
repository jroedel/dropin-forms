package userbus

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/types"
)

// A sign-in code is the six digits printed in the sign-in mail beside the
// link, for somebody who would rather type than click.
//
// It exists because of where the mail is read. On a phone, the link opens in
// whichever browser the mail app chooses, which is often not the one the
// person was signing in on -- so they sign in somewhere they did not mean to,
// and are still signed out where they did. Six digits carry across devices in
// a glance, and Gmail recognises a message that reads like a verification
// code and offers a "Copy code" button on it. The wording of the mail is
// shaped for that; see authapp.send.
//
// # Why six digits is safe here, and what makes it so
//
// Six digits is a million possibilities, which is nothing next to the 128 bits
// of the link it travels with. Everything below exists to make that million
// enough:
//
//   - Only the newest code for an account works. Asking for another one
//     retires the last, so the guesses available never multiply with the
//     number of mails sent.
//   - Each code allows codeTries attempts, reserved before the code is
//     compared -- one statement that only succeeds while attempts remain -- so
//     concurrent guesses cannot overshoot the limit.
//   - An account allows codeBudget wrong codes in CodeBudgetWindow, across all
//     its codes. Without it, somebody could ask for a fresh code, guess five
//     times, and repeat; with it, they get fifteen guesses a day at a
//     one-in-a-million chance each, and every one of those days fills the
//     account holder's inbox with sign-in mail they did not ask for.
//   - Running out locks nobody out. When the budget is spent the code stops
//     working, and the link in the same mail and the backup codes still do.
//
// The code and the link are one credential: both redeem the same token, so
// signing in with either spends the other.
//
// # What the stored hash does not protect
//
// The code is stored as a SHA-256, like every other secret here, and unlike
// them that hash can be reversed by trying all million codes in well under a
// second. So somebody who could read this database during the fifteen minutes
// a code is live could sign in with it. That is accepted, not overlooked: the
// same reader already holds every submission, which is what an account would
// let them see, and a keyed hash would need a key kept somewhere the database
// is not -- a second secret to deploy for a fifteen-minute window. If one is
// ever added for another reason, this is the place to use it.

const (
	// codeDigits is how long a code is. Six because it is what every service
	// sends, so it is what a person expects and what a mail client recognises.
	codeDigits = 6

	// codeTries is how many attempts one code allows, right or wrong.
	codeTries = 5

	// codeBudget is how many wrong codes an account allows in
	// CodeBudgetWindow before codes stop working for it.
	codeBudget = 15

	// CodeBudgetWindow is how far back the wrong codes are counted. Exported
	// for storage, which keeps a code's row this long after it expires so
	// that the count survives the pruning of the link it came with.
	CodeBudgetWindow = 24 * time.Hour
)

// SignInCode is a sign-in code's record. The code itself is not in it.
type SignInCode struct {
	// TokenID is the sign-in link this code was sent with, and identifies the
	// code too: there is one per link.
	TokenID types.ID
	UserID  types.ID
	Hash    []byte

	// Tries is how many attempts have been made with it, the successful one
	// included.
	Tries int

	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    time.Time // zero unless somebody signed in with it
}

// mintCode returns a fresh code: codeDigits decimal digits, every value
// equally likely.
//
// rand.Int rather than reducing random bytes modulo a million, which would
// make the low codes slightly likelier than the high ones.
func mintCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		// crypto/rand does not fail on any platform Go supports; Read
		// documents that it crashes the program irrecoverably instead.
		panic("crypto/rand failed: " + err.Error())
	}

	return fmt.Sprintf("%0*d", codeDigits, n.Int64())
}

// codeSecret is what is hashed for a code: the code with the link it belongs
// to, so that two accounts given the same six digits do not store the same
// hash.
func codeSecret(tokenID types.ID, code string) string {
	return tokenID.String() + ":" + code
}

// normaliseCode accepts the code as somebody might type or paste it -- with
// spaces, or split by a dash -- and returns the digits, or empty when what is
// left is not codeDigits digits.
func normaliseCode(typed string) string {
	var b strings.Builder

	for _, r := range typed {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '\t':
		default:
			return ""
		}
	}

	if b.Len() != codeDigits {
		return ""
	}

	return b.String()
}

// SignInWithCode redeems the code from a sign-in mail.
//
// The address is asked for as well as the code, because six digits identify
// nothing: the address says whose newest code to compare against. Every
// refusal is ErrDenied, as everywhere in this package.
func (b *Business) SignInWithCode(ctx context.Context, now time.Time, email types.Email, typed string) (User, string, error) {
	code := normaliseCode(typed)
	if code == "" {
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

	wrong, err := b.store.SignInCodeFailures(ctx, u.ID, now.Add(-CodeBudgetWindow))
	switch {
	case err != nil:
		return User{}, "", fmt.Errorf("the sign-in codes could not be counted: %w", err)
	case wrong >= codeBudget:
		b.log.Warn("a sign-in code was refused because the account has had too many wrong ones today", "user_id", u.ID.String())

		return User{}, "", ErrDenied
	}

	c, err := b.store.LatestSignInCode(ctx, u.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return User{}, "", ErrDenied
	case err != nil:
		return User{}, "", fmt.Errorf("the sign-in code could not be read: %w", err)
	case !now.Before(c.ExpiresAt), !c.UsedAt.IsZero():
		return User{}, "", ErrDenied
	}

	// The attempt is reserved before the code is compared, so that the limit
	// holds however many guesses arrive at once.
	reserved, err := b.store.TrySignInCode(ctx, c.TokenID, codeTries)
	switch {
	case err != nil:
		return User{}, "", fmt.Errorf("the sign-in code attempt could not be recorded: %w", err)
	case !reserved:
		b.log.Warn("a sign-in code was tried after its attempts ran out", "user_id", u.ID.String(), "token_id", c.TokenID.String())

		return User{}, "", ErrDenied
	}

	if !verifySecret(c.Hash, codeSecret(c.TokenID, code)) {
		b.log.Warn("a sign-in code did not match", "user_id", u.ID.String(), "token_id", c.TokenID.String())

		return User{}, "", ErrDenied
	}

	// The link's own claim, because the code and the link are one credential:
	// whichever is used first spends both.
	claimed, err := b.store.UseToken(ctx, c.TokenID, now)
	switch {
	case err != nil:
		return User{}, "", fmt.Errorf("the sign-in link could not be spent: %w", err)
	case !claimed:
		return User{}, "", ErrDenied
	}

	// Recorded so that a right code is not counted against the budget. A
	// failure here costs the account one wrong code it did not make, which
	// is not worth refusing a sign-in that has already been earned.
	if err := b.store.UseSignInCode(ctx, c.TokenID, now); err != nil {
		b.log.Error("a used sign-in code could not be marked", "token_id", c.TokenID.String(), "error", err)
	}

	return b.start(ctx, now, u.ID)
}

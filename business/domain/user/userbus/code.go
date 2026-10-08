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

// A sign-in code is six digits sent by email, typed on the page that said to
// check for it. It is the only way in by mail: there is no sign-in link.
//
// # Why a code and not a link
//
// This service began with an emailed link, as most do, and the link was
// retired for three reasons, in the order they were felt:
//
//   - The mail is read on a phone, and a link opens in whichever browser the
//     mail app picks -- often not the one the person was signing in on. A
//     code carries across devices in a glance.
//   - Gmail recognises a message worded like a verification code and offers
//     a "Copy code" button on it. There is no markup for that, only the shape
//     of the message; authapp.send has the wording.
//   - A link has to survive mail scanners and link previewers, which fetch
//     every URL in a message before anybody clicks. That forced a page with a
//     button in front of every sign-in, and a code needs none of it: nothing
//     fetches six digits.
//
// It is the Schoenstatt Fathers apps' way of signing in from here on, and
// this file is the reference for the others.
//
// # Why six digits is safe here, and what makes it so
//
// Six digits is a million possibilities, which is nothing next to the 128
// bits a session carries. Everything below exists to make that million enough:
//
//   - Only the newest code for an account works. Asking for another retires
//     the last, so the guesses available never multiply with the number of
//     mails sent.
//   - Each code allows codeTries attempts, reserved before the code is
//     compared -- one statement that only succeeds while attempts remain -- so
//     concurrent guesses cannot overshoot the limit.
//   - An account allows codeBudget wrong codes in CodeBudgetWindow, across all
//     its codes. Without it, somebody could ask for a fresh code, guess five
//     times, and repeat; with it they get ten guesses an hour at a
//     one-in-a-million chance each, and every one of those hours fills the
//     account holder's inbox with codes they did not ask for.
//
// The window is an hour rather than a day, and that is the cost of there
// being no link. Somebody else's guessing can use up an account's budget, and
// until the window passes the account holder's own right code is refused too.
// An hour bounds how long a stranger can keep somebody out; the backup codes
// are the way in meanwhile, and are why they exist.
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
	codeBudget = 10

	// CodeBudgetWindow is how far back the wrong codes are counted. Exported
	// for storage, which keeps a code's row this long after it expires so
	// that the count does not forget a code the moment it dies.
	CodeBudgetWindow = time.Hour
)

// SignInCode is a sign-in code's record. The code itself is not in it.
type SignInCode struct {
	ID     types.ID
	UserID types.ID
	Hash   []byte

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

// codeSecret is what is hashed for a code: the code with its row's
// identifier, so that two accounts given the same six digits do not store the
// same hash.
func codeSecret(id types.ID, code string) string {
	return id.String() + ":" + code
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

// SignInWithCode redeems the code from a sign-in mail and returns a session
// credential.
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
		b.log.Warn("a sign-in code was refused because the account has had too many wrong ones this hour", "user_id", u.ID.String())

		return User{}, "", ErrDenied
	}

	c, err := b.store.LatestSignInCode(ctx, u.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return User{}, "", ErrDenied
	case err != nil:
		return User{}, "", fmt.Errorf("the sign-in code could not be read: %w", err)
	case !now.Before(c.ExpiresAt):
		return User{}, "", ErrDenied
	case !c.UsedAt.IsZero():
		b.log.Warn("a sign-in code was presented after it had been used", "user_id", u.ID.String(), "code_id", c.ID.String())

		return User{}, "", ErrDenied
	}

	// The attempt is reserved before the code is compared, so that the limit
	// holds however many guesses arrive at once.
	reserved, err := b.store.TrySignInCode(ctx, c.ID, codeTries)
	switch {
	case err != nil:
		return User{}, "", fmt.Errorf("the sign-in code attempt could not be recorded: %w", err)
	case !reserved:
		b.log.Warn("a sign-in code was tried after its attempts ran out", "user_id", u.ID.String(), "code_id", c.ID.String())

		return User{}, "", ErrDenied
	}

	if !verifySecret(c.Hash, codeSecret(c.ID, code)) {
		b.log.Warn("a sign-in code did not match", "user_id", u.ID.String(), "code_id", c.ID.String())

		return User{}, "", ErrDenied
	}

	// The atomic claim. Between the read above and here, another request
	// holding the same code may have spent it, and only the store can settle
	// that -- which is why this reports a bool rather than trusting the read.
	claimed, err := b.store.UseSignInCode(ctx, c.ID, now)
	switch {
	case err != nil:
		return User{}, "", fmt.Errorf("the sign-in code could not be spent: %w", err)
	case !claimed:
		b.log.Warn("two requests raced for one sign-in code", "code_id", c.ID.String())

		return User{}, "", ErrDenied
	}

	return b.start(ctx, now, u.ID)
}

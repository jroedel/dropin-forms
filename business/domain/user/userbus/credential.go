package userbus

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"

	"github.com/jroedel/dropin-forms/business/types"
)

// A credential in this package is always two parts: an identifier and a
// secret, presented together as "<id>.<secret>".
//
// The split is the whole design and it is worth stating before any of the code
// below.
//
// The identifier is what gets looked up. It is indexed, it is safe to log, and
// finding a row by it costs one indexed read. The secret is never stored --
// only a hash of it is -- and it is compared in constant time. Without the
// split, looking up a credential would mean either storing the secret in a
// searchable form, or scanning every unexpired row and hashing against each,
// which is both slow and a timing oracle for how many rows exist.
//
// It is also what makes a leaked log harmless. Our request logger never
// records a query string, but logs leak by other routes -- a stack trace, a
// proxy, somebody pasting a URL into a chat -- and an identifier on its own
// authorises nothing.

// ErrMalformed is returned when a presented credential is not even the right
// shape. It is deliberately not distinguished from a wrong secret anywhere a
// person can see, because the difference tells whoever is guessing which half
// they got right.
var ErrMalformed = errors.New("not a credential")

// secretHash is a SHA-256 digest of a presented secret.
//
// A plain hash rather than a password KDF, and that is a deliberate choice
// rather than an oversight. bcrypt, scrypt, argon2 and PBKDF2 exist to make
// guessing a *low-entropy human-chosen* secret expensive. Every secret in this
// package is machine-generated with at least 80 bits of entropy from
// crypto/rand, so there is nothing to guess: an attacker holding the hashes
// faces 2^80 or more SHA-256 evaluations per credential, which no iteration
// count meaningfully improves on. Adding a KDF would cost real time on every
// request in exchange for hardening a number that is already out of reach.
//
// crypto/pbkdf2 is in the standard library as of Go 1.24, so this stays a
// choice and not a constraint. If a human-chosen secret is ever introduced
// here -- a password, a PIN -- it must not use this function.
type secretHash []byte

func hashSecret(secret string) secretHash {
	sum := sha256.Sum256([]byte(secret))

	return sum[:]
}

// verifySecret compares a presented secret against a stored hash in constant
// time.
//
// subtle.ConstantTimeCompare rather than bytes.Equal. bytes.Equal returns as
// soon as two bytes differ, so the time it takes reveals how many leading
// bytes were right -- which is enough to reconstruct a hash one byte at a time
// given enough attempts. It also returns 0 for a length mismatch, so a
// truncated stored hash cannot compare equal to a prefix.
func verifySecret(stored secretHash, presented string) bool {
	return subtle.ConstantTimeCompare(stored, hashSecret(presented)) == 1
}

// mintSecret returns a fresh secret to hand out exactly once.
//
// crypto/rand.Text, which is documented to carry at least 128 bits of
// randomness and to return the standard base32 alphabet -- so it is safe in a
// URL, in an email that some client will helpfully linkify, and in a form
// field, with no escaping anywhere. Its length may grow in a future Go
// release; nothing here depends on it, which is the reason to use it rather
// than hand-rolling an encoding of a fixed byte count.
func mintSecret() string { return rand.Text() }

// credential is an identifier and a secret, ready to be handed out. The secret
// exists in memory for one request and is never persisted in this form.
type credential struct {
	id     types.ID
	secret string
	hash   secretHash
}

func mintCredential() credential {
	secret := mintSecret()

	return credential{
		id:     types.NewID(),
		secret: secret,
		hash:   hashSecret(secret),
	}
}

// String is what the holder is given: the identifier and the secret joined by
// a dot.
//
// A dot because it appears in neither hex nor base32, so splitting is
// unambiguous with no escaping. Deliberately not a method on a type that
// anything stores, so that this string cannot be written to a database by
// accident.
func (c credential) String() string { return c.id.String() + "." + c.secret }

// splitCredential takes apart a presented "<id>.<secret>".
//
// The identifier is parsed strictly, which means a malformed one is refused
// before any database work happens -- so a flood of junk costs no reads.
func splitCredential(presented string) (types.ID, string, error) {
	rawID, secret, found := strings.Cut(presented, ".")
	if !found {
		return types.ID{}, "", fmt.Errorf("%w: it has no separator", ErrMalformed)
	}

	id, err := types.ParseID(rawID)
	if err != nil {
		return types.ID{}, "", fmt.Errorf("%w: %w", ErrMalformed, err)
	}

	// A length floor rather than an exact match, because rand.Text may return
	// a longer string in a future Go release and credentials minted by an
	// older binary have to keep working across a deploy.
	if len(secret) < 20 {
		return types.ID{}, "", fmt.Errorf("%w: the secret is too short to be one", ErrMalformed)
	}

	return id, secret, nil
}

// Backup codes are the exception to the shape above: there is no identifier,
// because somebody typing one off a piece of paper has only the code.
//
// So they are found by checking a user's codes one at a time, which is why
// they are given more entropy than they would need with an identifier -- 80
// bits, in sixteen characters, grouped for typing. Ten cheap comparisons cost
// nothing; ten KDF evaluations would cost seconds.

const (
	backupCodeCount  = 10
	backupCodeLen    = 16
	backupCodeGroup  = 4
	backupCodeGroups = backupCodeLen / backupCodeGroup
)

// mintBackupCode returns one code in the form somebody reads off a page,
// "ABCD-EFGH-JKLM-NPQR", together with the hash to store.
func mintBackupCode() (string, secretHash) {
	// rand.Text promises at least 128 bits, so it is at least 26 base32
	// characters and taking 16 is always in range. Taking a prefix of a
	// uniformly random string is itself uniformly random.
	raw := rand.Text()[:backupCodeLen]

	var b strings.Builder
	for i := range backupCodeGroups {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(raw[i*backupCodeGroup : (i+1)*backupCodeGroup])
	}

	// The hash is over the normalised form, so that what gets compared does
	// not depend on whether the person typed the hyphens.
	return b.String(), hashSecret(raw)
}

// normaliseBackupCode turns what somebody typed into the form that was hashed.
//
// Everything here is about a code read off paper and typed by hand. The
// hyphens are decoration and may be omitted or duplicated. Case is folded,
// because the code is displayed in capitals and phones capitalise
// inconsistently. And three digits are mapped to the letters they are being
// mistaken for: the base32 alphabet is A-Z and 2-7, so 0, 1 and 8 cannot
// appear in a real code and can only be somebody reading O, I and B.
func normaliseBackupCode(typed string) string {
	var b strings.Builder
	b.Grow(len(typed))

	for _, r := range typed {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		case r >= 'A' && r <= 'Z', r >= '2' && r <= '7':
			b.WriteRune(r)
		case r == '0':
			b.WriteByte('O')
		case r == '1':
			b.WriteByte('I')
		case r == '8':
			b.WriteByte('B')
		default:
			// Hyphens, spaces and anything else are dropped rather than
			// refused. A code is either right or wrong; complaining about a
			// stray space helps nobody at the moment they have resorted to a
			// backup code.
		}
	}

	return b.String()
}

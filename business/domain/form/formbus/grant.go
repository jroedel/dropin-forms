package formbus

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/jroedel/dropin-forms/business/types"
)

// A submission grant is the thing a blank form page hands out and a submission
// hands back. It lives here, alongside the definition, because what it pins is
// [Form.Version] -- and a grant that could drift from the definition it was
// minted against would defeat the only mechanism this service has for changing
// a price safely.
//
// # It is not CSRF protection, and the name says so
//
// The embed surface never carries a cookie, so there is no ambient credential
// to forge and nothing for a cross-site request to abuse. Calling this a form
// token and seating it where a CSRF gate sits would tell the next reader it
// provides a defence it does not. What it actually buys, in order of how much
// it is worth:
//
//  1. Server-chosen state pinned into the submission -- which form, at which
//     version, valid until when. This is the part that matters. A tab opened
//     last week cannot submit against last week's prices, because the version
//     in the grant no longer matches the definition and the POST is
//     re-rendered at the current one instead.
//  2. Single-use replay limiting, which is the caller's half: [Redeem] returns
//     a nonce, and recording it is one INSERT that has to be in the same
//     transaction as the submission.
//  3. Proof that one round trip happened, which is a weak bot tax and no more.
//
// See docs/design/drop-in-forms.md sections 5.3 and 5.4.
//
// # Why the encoding is length-prefixed
//
// Plain concatenation of a variable-length slug is ambiguous:
// ("fall-retreat", "2026abc") and ("fall-retreat2", "026abc") produce the same
// bytes and therefore the same MAC. A grant minted for one form would
// authorise a submission to another -- another price list, possibly another
// owner's numbers. Every field here carries its own length, and [Redeem]
// checks the decoded form against the one being submitted to as well, rather
// than trusting the MAC to have bound it.

// ErrGrantRefused is every reason a presented grant is not usable: a bad MAC,
// a truncated token, an expiry in the past, a version that no longer matches
// the definition, a form that is not the one being submitted to.
//
// One error, and the app layer does one thing with it: re-render the form with
// a fresh grant and whatever the person had typed. The reasons are genuinely
// interchangeable from the reader's point of view -- they did nothing wrong
// and have no idea what a grant is -- so "your session for this form expired,
// here it is again" is the whole vocabulary needed.
var ErrGrantRefused = errors.New("that submission could not be accepted")

// ErrWeakGrantKey is a signing key too short to be one.
var ErrWeakGrantKey = errors.New("the grant signing key is too short")

// grantLife is how long a minted grant stays usable.
//
// Long enough to fill in a form slowly, with interruptions, on a phone. Not so
// long that a tab left open over a weekend is still holding a price list. The
// consequence of getting it wrong in either direction is the same and it is
// mild: the POST re-renders with a fresh grant and the answers preserved.
const grantLife = 2 * time.Hour

// grantKeyMinLen is the shortest secret accepted, in characters. Machine
// generated, so this is about 190 bits of base64 rather than a passphrase.
const grantKeyMinLen = 32

// grantFormat is the first byte of every payload, so that a future change of
// encoding is a refusal rather than a misparse.
const grantFormat = 1

// grantMACLen is how much of the HMAC is kept, in bytes. 128 bits, which is
// not a truncation worth worrying about for a two-hour token -- and it keeps
// the string short enough to sit in a hidden field without comment.
const grantMACLen = 16

// GrantKey is the server secret a grant is signed with.
//
// A distinct type rather than a []byte, so that a config string cannot be
// passed to [Mint] by accident: the only way to get one is through
// [ParseGrantKey], which is where the length is checked.
type GrantKey struct {
	k []byte
}

// ParseGrantKey turns the configured secret into a signing key.
//
// The secret is hashed rather than used directly, so that the HMAC key is
// always exactly a block's worth of uniformly distributed bytes whatever the
// configuration holds. SHA-256 rather than a password KDF: this is a machine
// generated secret with far more entropy than any amount of stretching would
// add, and the same reasoning is written out at length in
// userbus/credential.go.
func ParseGrantKey(secret string) (GrantKey, error) {
	if len(secret) < grantKeyMinLen {
		return GrantKey{}, fmt.Errorf("%w: %d characters, and at least %d are needed", ErrWeakGrantKey, len(secret), grantKeyMinLen)
	}

	sum := sha256.Sum256([]byte(secret))

	return GrantKey{k: sum[:]}, nil
}

// Zero reports whether this key was never set.
func (k GrantKey) Zero() bool { return len(k.k) == 0 }

// Grant is what a redeemed token decodes to.
type Grant struct {
	Form    types.Slug
	Version string

	// Nonce is what makes a grant single-use, and it is the caller's job to
	// make it so. Redeem does not record it: this package has no storage, and
	// the record has to be written in the same transaction as the submission
	// it belongs to, which only the caller can arrange.
	Nonce string

	ExpiresAt time.Time
}

// Mint issues a grant for a form.
//
// Nothing is recorded. Minting happens on an unauthenticated GET that anybody
// can repeat as fast as they can ask, so writing a row per mint is a way for a
// stranger to fill the disk from the other side of the world. The nonce is
// recorded when it is spent instead, which is the moment there is something
// worth remembering.
func Mint(key GrantKey, f Form, now time.Time) (string, error) {
	if key.Zero() {
		return "", errors.New("no grant signing key")
	}
	if f.ID.Zero() {
		return "", errors.New("a grant needs a form")
	}
	if f.Version == "" {
		return "", errors.New("a grant needs a stamped form; call Stamp first")
	}

	// rand.Text gives a 26-character base32 string from crypto/rand, which is
	// 130 bits -- more than enough that two mints never collide, and short
	// enough to keep the token compact.
	nonce := rand.Text()

	payload, err := encodeGrant(Grant{
		Form:      f.ID,
		Version:   f.Version,
		Nonce:     nonce,
		ExpiresAt: now.Add(grantLife),
	})
	if err != nil {
		return "", err
	}

	mac := sign(key, payload)

	enc := base64.RawURLEncoding

	return enc.EncodeToString(payload) + "." + enc.EncodeToString(mac), nil
}

// Redeem checks a presented grant against the form it is being submitted to.
//
// Everything is checked here rather than split between this and the caller,
// because the checks that are easy to forget are the ones that matter: that
// the form in the grant is this form, and that the version in it is still this
// version. A caller holding a verified-but-unexamined grant is the bug the
// length-prefixed encoding was chosen to prevent, and leaving the comparison
// outside would reintroduce it one call site at a time.
//
// The returned nonce has not been spent. Recording it is the caller's, in the
// transaction that writes the submission.
func Redeem(key GrantKey, presented string, f Form, now time.Time) (Grant, error) {
	if key.Zero() {
		return Grant{}, errors.New("no grant signing key")
	}

	raw, mac, ok := cut(presented)
	if !ok {
		return Grant{}, fmt.Errorf("%w: it is not in two parts", ErrGrantRefused)
	}

	// The MAC is checked before anything is decoded, so that no attacker
	// chosen bytes reach the decoder at all.
	if !hmac.Equal(mac, sign(key, raw)) {
		return Grant{}, fmt.Errorf("%w: the signature does not match", ErrGrantRefused)
	}

	g, err := decodeGrant(raw)
	if err != nil {
		// Unreachable for anything this package minted, since the MAC has
		// already matched. Refused rather than ignored so that a future change
		// to the encoding cannot silently produce a zero-valued grant.
		return Grant{}, fmt.Errorf("%w: %s", ErrGrantRefused, err)
	}

	switch {
	case g.Form != f.ID:
		// The check the design document is emphatic about: assert the decoded
		// form against the one in hand rather than trusting the MAC to have
		// bound it.
		return Grant{}, fmt.Errorf("%w: it was issued for a different form", ErrGrantRefused)

	case g.Version != f.Version:
		// An edit landed while this page was open. The price, the bounds or
		// the closing time are not what the person was shown, so the answers
		// are re-presented against the current definition instead.
		return Grant{}, fmt.Errorf("%w: the form has changed since this page was opened", ErrGrantRefused)

	case !now.Before(g.ExpiresAt):
		return Grant{}, fmt.Errorf("%w: it expired at %s", ErrGrantRefused, g.ExpiresAt.Format(time.RFC3339))
	}

	return g, nil
}

// sign is the MAC over an encoded payload.
func sign(key GrantKey, payload []byte) []byte {
	mac := hmac.New(sha256.New, key.k)
	mac.Write(payload)

	return mac.Sum(nil)[:grantMACLen]
}

// cut splits a presented token and decodes both halves.
func cut(presented string) (payload, mac []byte, ok bool) {
	enc := base64.RawURLEncoding

	before, after, found := splitOne(presented)
	if !found {
		return nil, nil, false
	}

	payload, err := enc.DecodeString(before)
	if err != nil {
		return nil, nil, false
	}

	mac, err = enc.DecodeString(after)
	if err != nil || len(mac) != grantMACLen {
		return nil, nil, false
	}

	return payload, mac, true
}

// splitOne cuts at the single separator, refusing a string with more than one.
// strings.Cut alone would accept "a.b.c" by treating "b.c" as the MAC, which
// then fails to decode -- this refuses it for the reason it is wrong.
func splitOne(s string) (before, after string, ok bool) {
	dot := -1
	for i := range len(s) {
		if s[i] != '.' {
			continue
		}

		if dot >= 0 {
			return "", "", false
		}

		dot = i
	}

	if dot <= 0 || dot == len(s)-1 {
		return "", "", false
	}

	return s[:dot], s[dot+1:], true
}

// encodeGrant writes the payload: a format byte, then each string with its own
// two-byte length, then the expiry as milliseconds.
func encodeGrant(g Grant) ([]byte, error) {
	out := []byte{grantFormat}

	for _, s := range []string{g.Form.String(), g.Version, g.Nonce} {
		if len(s) > 0xFFFF {
			return nil, fmt.Errorf("a grant field is %d bytes, which does not fit", len(s))
		}

		out = binary.BigEndian.AppendUint16(out, uint16(len(s)))
		out = append(out, s...)
	}

	return binary.BigEndian.AppendUint64(out, uint64(g.ExpiresAt.UTC().UnixMilli())), nil
}

// decodeGrant reads what encodeGrant wrote, and refuses anything else.
func decodeGrant(payload []byte) (Grant, error) {
	if len(payload) == 0 {
		return Grant{}, errors.New("it is empty")
	}
	if payload[0] != grantFormat {
		return Grant{}, fmt.Errorf("it is format %d and this binary writes %d", payload[0], grantFormat)
	}

	rest := payload[1:]

	var parts [3]string
	for i := range parts {
		if len(rest) < 2 {
			return Grant{}, errors.New("it ends in the middle of a field")
		}

		n := int(binary.BigEndian.Uint16(rest[:2]))
		rest = rest[2:]

		if len(rest) < n {
			return Grant{}, errors.New("a field is shorter than it claims")
		}

		parts[i] = string(rest[:n])
		rest = rest[n:]
	}

	if len(rest) != 8 {
		return Grant{}, errors.New("the expiry is missing or the payload has trailing bytes")
	}

	form, err := types.ParseSlug(parts[0])
	if err != nil {
		return Grant{}, fmt.Errorf("the form name is unreadable: %w", err)
	}

	if parts[1] == "" {
		return Grant{}, errors.New("it names no version")
	}
	if parts[2] == "" {
		return Grant{}, errors.New("it carries no nonce")
	}

	return Grant{
		Form:      form,
		Version:   parts[1],
		Nonce:     parts[2],
		ExpiresAt: time.UnixMilli(int64(binary.BigEndian.Uint64(rest))).UTC(),
	}, nil
}

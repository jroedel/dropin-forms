package submissionbus

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/jroedel/dropin-forms/business/types"
)

// The link in every message to somebody whose answers they may still change,
// and what makes it safe to put in an email.
//
// # Holding the email is the credential
//
// The people filling these forms in have no account here and never will. What
// they do have is a mailbox we have already written to, and the link is how a
// message from us becomes the way back to their own answers. So the token is
// a MAC over the submission it names: nobody can construct one for somebody
// else's row, and whoever holds one was sent it, or was shown it by somebody
// who was.
//
// # Why it does not expire, and what does
//
// A sign-in code expires because it grants a session. This grants one thing,
// changing one submission, and the form already says until when: its
// ChangeableUntil, checked on every use. Putting a second clock in the token
// would make the oldest message in somebody's inbox the one that has quietly
// stopped working, months before the form stops taking changes -- and the
// whole point of the feature is that a reply to the first email, sent in
// October, still works in January. One date, on the form, that the office can
// move.
//
// What revokes a single link is hiding its submission: a hidden row cannot be
// changed through any link. What revokes every link at once is rotating the
// configured secret, which also refuses every grant and unsubscribe link, and
// is the right size of hammer for "a key has leaked".
//
// # Why this is not the unsubscribe token with a different label
//
// For the reason notifybus/token.go gives for not sharing the grant's code: a
// MAC helper that serves two purposes is one somebody eventually uses for a
// third with the same key. Each token here derives its own key under its own
// label and has its own encoder, and the duplication is the price of being
// able to read each one without the others.

// ErrBadAnswerLink is a link that did not come from us, or did not survive
// being emailed.
//
// One error for every reason, because the page does one thing with it: say
// that the link did not work and offer to send a fresh one.
var ErrBadAnswerLink = errors.New("that link is not one of ours")

// answerLinkFormat is the first byte of every payload, so that a future
// change of encoding is a refusal rather than a misparse.
const answerLinkFormat = 1

// answerMACLen is how much of the HMAC is kept, in bytes. 128 bits: the token
// does not expire, but guessing one is still a search over a space that size
// for each row, one rate-limited request at a time.
const answerMACLen = 16

// answerKeyLabel separates this key from every other use of the same
// configured secret. See notifybus.ParseMuteKey for why the secret is shared
// and why hashing it with a label makes that safe.
const answerKeyLabel = "dropin-forms/answer-link/v1"

// AnswerKey signs the links to change a submission.
//
// A distinct type rather than a []byte so that a configuration string cannot
// reach [MintAnswerLink] by accident, the shape formbus.GrantKey has.
type AnswerKey struct {
	k []byte
}

// ParseAnswerKey derives the signing key from the configured secret.
func ParseAnswerKey(secret string) (AnswerKey, error) {
	if len(secret) < 32 {
		return AnswerKey{}, fmt.Errorf("the secret that signs answer links is %d characters, and at least 32 are needed", len(secret))
	}

	sum := sha256.Sum256([]byte(answerKeyLabel + "\x00" + secret))

	return AnswerKey{k: sum[:]}, nil
}

// Zero reports whether this key was never set, in which case no message
// carries a link and nobody can change an answer.
func (k AnswerKey) Zero() bool { return len(k.k) == 0 }

// MintAnswerLink issues the token for one submission on one form.
//
// Deterministic: the same submission always gets the same token, so every
// message we ever send about it carries a link that works, and none of them
// supersedes another.
func MintAnswerLink(key AnswerKey, form types.Slug, id types.ID) (string, error) {
	switch {
	case key.Zero():
		return "", errors.New("no signing key for answer links")
	case form.Zero():
		return "", errors.New("an answer link needs a form")
	case id.Zero():
		return "", errors.New("an answer link needs a submission")
	}

	payload := encodeAnswerLink(form.String(), id.String())
	enc := base64.RawURLEncoding

	return enc.EncodeToString(payload) + "." + enc.EncodeToString(signAnswerLink(key, payload)), nil
}

// ReadAnswerLink checks a presented token and says which submission it names.
//
// The form is returned rather than taken, and the caller compares it with the
// form in the address: a token for one form presented under another's path is
// a link somebody edited, and the caller answers it the way it answers any
// other link that is not ours.
func ReadAnswerLink(key AnswerKey, presented string) (types.Slug, types.ID, error) {
	if key.Zero() {
		return types.Slug{}, types.ID{}, errors.New("no signing key for answer links")
	}

	raw, mac, ok := cutAnswerLink(presented)
	if !ok {
		return types.Slug{}, types.ID{}, fmt.Errorf("%w: it is not in two parts", ErrBadAnswerLink)
	}

	// The MAC is checked before anything is decoded, so that no bytes a
	// stranger chose reach the decoder at all.
	if !hmac.Equal(mac, signAnswerLink(key, raw)) {
		return types.Slug{}, types.ID{}, fmt.Errorf("%w: the signature does not match", ErrBadAnswerLink)
	}

	form, sub, err := decodeAnswerLink(raw)
	if err != nil {
		return types.Slug{}, types.ID{}, fmt.Errorf("%w: %s", ErrBadAnswerLink, err)
	}

	slug, err := types.ParseSlug(form)
	if err != nil {
		return types.Slug{}, types.ID{}, fmt.Errorf("%w: %s", ErrBadAnswerLink, err)
	}

	id, err := types.ParseID(sub)
	if err != nil {
		return types.Slug{}, types.ID{}, fmt.Errorf("%w: %s", ErrBadAnswerLink, err)
	}

	return slug, id, nil
}

func signAnswerLink(key AnswerKey, payload []byte) []byte {
	mac := hmac.New(sha256.New, key.k)
	mac.Write(payload)

	return mac.Sum(nil)[:answerMACLen]
}

func encodeAnswerLink(parts ...string) []byte {
	out := []byte{answerLinkFormat}

	for _, p := range parts {
		out = binary.AppendUvarint(out, uint64(len(p)))
		out = append(out, p...)
	}

	return out
}

func decodeAnswerLink(raw []byte) (string, string, error) {
	if len(raw) == 0 || raw[0] != answerLinkFormat {
		return "", "", errors.New("it was written by a different version of this service")
	}

	rest := raw[1:]

	var out [2]string

	for i := range out {
		n, read := binary.Uvarint(rest)
		if read <= 0 || uint64(len(rest[read:])) < n {
			return "", "", errors.New("it is truncated")
		}

		out[i] = string(rest[read : read+int(n)])
		rest = rest[read+int(n):]
	}

	if len(rest) != 0 {
		return "", "", errors.New("it has bytes on the end")
	}

	return out[0], out[1], nil
}

// cutAnswerLink splits the token and decodes both halves, reporting failure
// rather than a partial result.
func cutAnswerLink(presented string) ([]byte, []byte, bool) {
	enc := base64.RawURLEncoding

	payload, mac, found := strings.Cut(presented, ".")
	if !found {
		return nil, nil, false
	}

	raw, err := enc.DecodeString(payload)
	if err != nil {
		return nil, nil, false
	}

	sum, err := enc.DecodeString(mac)
	if err != nil || len(sum) != answerMACLen {
		return nil, nil, false
	}

	return raw, sum, true
}

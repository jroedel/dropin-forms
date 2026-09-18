package notifybus

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

// The link at the bottom of every notification, and what makes it safe to put
// in an email.
//
// # Why it is signed at all
//
// The only thing this token can do is turn one person's email about one form
// off or on. That sounds small until you write down the failure: silencing the
// person who reads the orders is how a paid lunch goes unnoticed, and it is
// the exact failure the whole notification step exists to prevent. So a link
// anybody could construct for anybody else is not acceptable, and the token
// carries a MAC over the pair it names.
//
// # Why it does not expire
//
// A sign-in link expires because it grants a session. This grants nothing: it
// identifies whose preference is being changed, and the preference is
// reversible from the same page. An unsubscribe link that has quietly stopped
// working is a person who replies to ask somebody to turn it off for them,
// which is worse than the risk of an old message in a mailbox being able to do
// something its recipient could already do.
//
// # Why the encoding is length-prefixed
//
// The same ambiguity formbus's grant is length-prefixed to avoid: plain
// concatenation of two variable-length strings lets one pair produce the bytes
// of another. It is written again here rather than shared, because the one
// thing worth keeping out of the payment path is a refactor of the payment
// path -- and because a MAC helper that serves two purposes is a MAC helper
// somebody eventually uses for a third with the same key.

// ErrBadMuteToken is a link that did not come from us, or did not survive
// being emailed.
//
// One error for every reason, because the page does one thing with it: say
// that the link did not work and offer the way in that always does, which is
// signing in.
var ErrBadMuteToken = errors.New("that link is not one of ours")

// muteTokenFormat is the first byte of every payload, so that a future change
// of encoding is a refusal rather than a misparse.
const muteTokenFormat = 1

// muteMACLen is how much of the HMAC is kept, in bytes. 128 bits over a
// value that authorises nothing but a preference.
const muteMACLen = 16

// muteKeyLabel separates this key from every other use of the same configured
// secret.
//
// The secret is the one that signs submission grants, and reusing it is a
// decision rather than an economy: another key means another line in
// secrets.env, another render, and another thing that can be missing on the
// morning somebody needs to unsubscribe. Hashing it with a label distinct from
// formbus's use means a MAC from one is not a MAC for the other, which is the
// property that makes sharing the input safe.
const muteKeyLabel = "dropin-forms/notification-mute/v1"

// MuteKey signs the unsubscribe links.
//
// A distinct type rather than a []byte so that a configuration string cannot
// reach [MintMuteToken] by accident, the same shape formbus.GrantKey has.
type MuteKey struct {
	k []byte
}

// ParseMuteKey derives the signing key from the configured secret.
func ParseMuteKey(secret string) (MuteKey, error) {
	if len(secret) < 32 {
		return MuteKey{}, fmt.Errorf("the secret that signs unsubscribe links is %d characters, and at least 32 are needed", len(secret))
	}

	sum := sha256.Sum256([]byte(muteKeyLabel + "\x00" + secret))

	return MuteKey{k: sum[:]}, nil
}

// Zero reports whether this key was never set, in which case notifications
// carry no unsubscribe link and the management app is the only way to change
// the preference.
func (k MuteKey) Zero() bool { return len(k.k) == 0 }

// MintMuteToken issues the token that goes in one person's copy of one
// notification.
func MintMuteToken(key MuteKey, userID types.ID, form types.Slug) (string, error) {
	switch {
	case key.Zero():
		return "", errors.New("no signing key for unsubscribe links")
	case userID.Zero():
		return "", errors.New("an unsubscribe link needs an account")
	case form.Zero():
		return "", errors.New("an unsubscribe link needs a form")
	}

	payload := encodeMute(userID.String(), form.String())
	enc := base64.RawURLEncoding

	return enc.EncodeToString(payload) + "." + enc.EncodeToString(signMute(key, payload)), nil
}

// ReadMuteToken checks a presented token and says whose preference it names.
func ReadMuteToken(key MuteKey, presented string) (types.ID, types.Slug, error) {
	if key.Zero() {
		return types.ID{}, types.Slug{}, errors.New("no signing key for unsubscribe links")
	}

	raw, mac, ok := cutMute(presented)
	if !ok {
		return types.ID{}, types.Slug{}, fmt.Errorf("%w: it is not in two parts", ErrBadMuteToken)
	}

	// The MAC is checked before anything is decoded, so that no bytes a
	// stranger chose reach the decoder at all.
	if !hmac.Equal(mac, signMute(key, raw)) {
		return types.ID{}, types.Slug{}, fmt.Errorf("%w: the signature does not match", ErrBadMuteToken)
	}

	user, form, err := decodeMute(raw)
	if err != nil {
		return types.ID{}, types.Slug{}, fmt.Errorf("%w: %s", ErrBadMuteToken, err)
	}

	id, err := types.ParseID(user)
	if err != nil {
		return types.ID{}, types.Slug{}, fmt.Errorf("%w: %s", ErrBadMuteToken, err)
	}

	slug, err := types.ParseSlug(form)
	if err != nil {
		return types.ID{}, types.Slug{}, fmt.Errorf("%w: %s", ErrBadMuteToken, err)
	}

	return id, slug, nil
}

func signMute(key MuteKey, payload []byte) []byte {
	mac := hmac.New(sha256.New, key.k)
	mac.Write(payload)

	return mac.Sum(nil)[:muteMACLen]
}

func encodeMute(parts ...string) []byte {
	out := []byte{muteTokenFormat}

	for _, p := range parts {
		out = binary.AppendUvarint(out, uint64(len(p)))
		out = append(out, p...)
	}

	return out
}

func decodeMute(raw []byte) (string, string, error) {
	if len(raw) == 0 || raw[0] != muteTokenFormat {
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

// cutMute splits the token and decodes both halves, reporting failure rather
// than a partial result: a token that is not two decodable parts is not a
// token, and there is nothing useful to do with half of one.
func cutMute(presented string) ([]byte, []byte, bool) {
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
	if err != nil || len(sum) != muteMACLen {
		return nil, nil, false
	}

	return raw, sum, true
}

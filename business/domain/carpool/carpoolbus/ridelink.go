package carpoolbus

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"

	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// The ride link is how a driver or a rider reaches their own ride page: the
// address in their texts and on the page that thanked them for signing up.
//
// # Holding it is the credential
//
// As with the link to change one's answers (submissionbus/answerlink.go),
// holding the link is what lets somebody use it. That one is held by whoever
// holds the email it was sent to; this one by whoever holds the phone the
// text went to, or saw the thank-you page. Nothing stored: an HMAC over the
// submission's identifier, so the same submission always has the same link,
// and hiding or declining the submission is what stops it leading anywhere.
//
// # Short, because it goes in a text
//
// The answer link names its form as well as the submission, and is about a
// hundred characters long. That is three text segments' worth of address
// before a word of the message, and every segment is paid for. This one is the
// submission's 16 bytes and 12 bytes of MAC -- 38 characters -- and the form is
// found from the submission. 96 bits of MAC is a search no rate-limited
// endpoint will finish.
//
// Its own key, derived from the answer-link key under a label of its own, so
// a ride link cannot be presented as any other kind and needs nothing more in
// the configuration.

const rideKeyLabel = "dropin-forms/ride-link/v1"

// rideMACLen is how much of the HMAC is kept, in bytes.
const rideMACLen = 12

// idLen is a submission identifier's length in bytes.
const idLen = 16

// ErrBadRideLink is a ride link that did not come from us, or did not survive
// being copied.
var ErrBadRideLink = errors.New("that ride link is not one of ours")

// RideKey signs ride links. The zero RideKey signs nothing: there are then no
// ride links, and nothing offers one.
type RideKey struct {
	k []byte
}

// NewRideKey derives the ride-link key from the answer-link key.
func NewRideKey(answers submissionbus.AnswerKey) RideKey {
	return RideKey{k: answers.Derive(rideKeyLabel)}
}

// Zero reports whether this key signs nothing.
func (k RideKey) Zero() bool { return len(k.k) == 0 }

func (k RideKey) sign(id []byte) []byte {
	mac := hmac.New(sha256.New, k.k)
	mac.Write(id)

	return mac.Sum(nil)[:rideMACLen]
}

// MintRideLink is the token for one submission's ride page.
func MintRideLink(k RideKey, id types.ID) (string, error) {
	if k.Zero() {
		return "", errors.New("no key to sign a ride link with")
	}

	raw, err := hex.DecodeString(id.String())
	if err != nil || len(raw) != idLen {
		return "", errors.New("a ride link needs a submission")
	}

	return base64.RawURLEncoding.EncodeToString(append(raw, k.sign(raw)...)), nil
}

// ReadRideLink checks a token and says whose ride page it is.
func ReadRideLink(k RideKey, token string) (types.ID, error) {
	if k.Zero() {
		return types.ID{}, errors.New("no key to check a ride link with")
	}

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != idLen+rideMACLen {
		return types.ID{}, ErrBadRideLink
	}

	id, mac := raw[:idLen], raw[idLen:]

	if !hmac.Equal(mac, k.sign(id)) {
		return types.ID{}, ErrBadRideLink
	}

	parsed, err := types.ParseID(hex.EncodeToString(id))
	if err != nil {
		return types.ID{}, ErrBadRideLink
	}

	return parsed, nil
}

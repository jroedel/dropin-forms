package submissionbus

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/jroedel/dropin-forms/business/types"
)

// The link in the message that offers somebody a place they have to pay for.
//
// The answer link's argument holds here unchanged: the person has no account,
// they have a mailbox we wrote to, and the token is a MAC over the submission
// it names. It does not expire either, for a better reason than the answer
// link's: the offer it leads to does, at [Submission.OfferExpiresAt], checked
// on every use -- and the page a lapsed link opens says so, which a link that
// had simply stopped verifying could not.
//
// Its own key, derived from the answer-link key under a label of its own, so
// that a link for changing somebody's answers is never a link for paying for
// their place, or the other way round, though both name the same submission.
// Derived rather than configured, because every installation that can send
// one already has the other, and a second secret is a second thing to rotate.
// The byte layout is the answer link's, read by the same functions: what keeps
// the two apart is the key, and sharing the encoder costs nothing that the
// separate key does not already guard.

// ErrBadPayLink is a pay link that did not come from us, or did not survive
// being emailed.
var ErrBadPayLink = errors.New("that payment link is not one of ours")

const payKeyLabel = "dropin-forms/pay-link/v1"

func payKey(k AnswerKey) []byte {
	sum := sha256.Sum256(append([]byte(payKeyLabel+"\x00"), k.k...))

	return sum[:]
}

func signPayLink(k AnswerKey, payload []byte) []byte {
	mac := hmac.New(sha256.New, payKey(k))
	mac.Write(payload)

	return mac.Sum(nil)[:answerMACLen]
}

// MintPayLink issues the token for paying for one offered place.
func MintPayLink(key AnswerKey, form types.Slug, id types.ID) (string, error) {
	switch {
	case key.Zero():
		return "", errors.New("no signing key for payment links")
	case form.Zero():
		return "", errors.New("a payment link needs a form")
	case id.Zero():
		return "", errors.New("a payment link needs a submission")
	}

	payload := encodeAnswerLink(form.String(), id.String())
	enc := base64.RawURLEncoding

	return enc.EncodeToString(payload) + "." + enc.EncodeToString(signPayLink(key, payload)), nil
}

// ReadPayLink checks a presented token and says which submission it names.
// The caller compares the form with the one in the address, as it does for an
// answer link.
func ReadPayLink(key AnswerKey, presented string) (types.Slug, types.ID, error) {
	if key.Zero() {
		return types.Slug{}, types.ID{}, errors.New("no signing key for payment links")
	}

	raw, mac, ok := cutAnswerLink(presented)
	if !ok {
		return types.Slug{}, types.ID{}, fmt.Errorf("%w: it is not in two parts", ErrBadPayLink)
	}

	if !hmac.Equal(mac, signPayLink(key, raw)) {
		return types.Slug{}, types.ID{}, fmt.Errorf("%w: the signature does not match", ErrBadPayLink)
	}

	form, sub, err := decodeAnswerLink(raw)
	if err != nil {
		return types.Slug{}, types.ID{}, fmt.Errorf("%w: %s", ErrBadPayLink, err)
	}

	slug, err := types.ParseSlug(form)
	if err != nil {
		return types.Slug{}, types.ID{}, fmt.Errorf("%w: %s", ErrBadPayLink, err)
	}

	id, err := types.ParseID(sub)
	if err != nil {
		return types.Slug{}, types.ID{}, fmt.Errorf("%w: %s", ErrBadPayLink, err)
	}

	return slug, id, nil
}

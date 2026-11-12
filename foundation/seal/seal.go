// Package seal encrypts a small secret this service has to keep and use
// again later -- a Google refresh token -- so that a copy of the database
// alone does not hand it over.
//
// Everything else secret here is stored as a hash, because it only ever has
// to be recognised: a sign-in session, an API key. A refresh
// token is the exception, because the service presents it to Google itself;
// a hash of it would be useless. AES-256-GCM, a fresh random nonce per
// sealing, and the key from configuration rather than from the database, so
// the two have to leak together.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Key seals and opens. A distinct type so a configuration string cannot be
// used as one by accident.
type Key struct {
	aead cipher.AEAD
}

// ParseKey reads a key: 32 random bytes, base64 (standard or URL alphabet,
// padded or not), as `openssl rand -base64 32` prints.
func ParseKey(s string) (Key, error) {
	raw, err := DecodeKey(s)
	if err != nil {
		return Key{}, err
	}

	return newKey(raw)
}

// DecodeKey is ParseKey's decoding alone: the 32 bytes, for deriving keys for
// several purposes from one configured secret with [Derive].
func DecodeKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)

	var raw []byte

	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			raw = b

			break
		}
	}

	if len(raw) != 32 {
		return nil, fmt.Errorf("the key is %d bytes once decoded, and needs to be 32 random bytes in base64 (openssl rand -base64 32)", len(raw))
	}

	return raw, nil
}

// Derive is a key for one purpose from a longer-lived secret: SHA-256 of the
// label and the secret, so that keys derived for different labels from the
// same secret are unrelated.
func Derive(secret []byte, label string) []byte {
	sum := sha256.Sum256(append([]byte(label+"\x00"), secret...))

	return sum[:]
}

// Zero reports whether this key was never set.
func (k Key) Zero() bool { return k.aead == nil }

func newKey(raw []byte) (Key, error) {
	block, err := aes.NewCipher(raw)
	if err != nil {
		return Key{}, fmt.Errorf("making the cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Key{}, fmt.Errorf("making the cipher: %w", err)
	}

	return Key{aead: aead}, nil
}

// FromBytes makes a Key from exactly 32 bytes, such as one from [Derive].
func FromBytes(raw []byte) (Key, error) {
	if len(raw) != 32 {
		return Key{}, fmt.Errorf("a key is 32 bytes, not %d", len(raw))
	}

	return newKey(raw)
}

// ErrOpen is a sealed value that does not open with this key: altered, or
// sealed under a different one.
var ErrOpen = errors.New("the sealed value does not open with this key")

// Seal encrypts plain. what is bound in as associated data -- what the value
// is for, such as the connection it belongs to -- so a sealed value copied
// onto another row does not open there.
func (k Key) Seal(plain, what string) ([]byte, error) {
	if k.aead == nil {
		return nil, errors.New("no key to seal with")
	}

	nonce := make([]byte, k.aead.NonceSize())
	_, _ = rand.Read(nonce) // crypto/rand.Read never fails, per its documentation.

	return k.aead.Seal(nonce, nonce, []byte(plain), []byte(what)), nil
}

// Open decrypts what Seal made for the same what.
func (k Key) Open(sealed []byte, what string) (string, error) {
	if k.aead == nil {
		return "", errors.New("no key to open with")
	}

	n := k.aead.NonceSize()
	if len(sealed) < n {
		return "", ErrOpen
	}

	plain, err := k.aead.Open(nil, sealed[:n], sealed[n:], []byte(what))
	if err != nil {
		return "", ErrOpen
	}

	return string(plain), nil
}

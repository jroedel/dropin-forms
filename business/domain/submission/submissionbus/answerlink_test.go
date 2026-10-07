package submissionbus_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

const secret = "a-test-secret-that-is-at-least-32-characters-long"

func answerKey(t *testing.T, s string) submissionbus.AnswerKey {
	t.Helper()

	k, err := submissionbus.ParseAnswerKey(s)
	if err != nil {
		t.Fatalf("ParseAnswerKey: %v", err)
	}

	return k
}

func TestAnAnswerLinkNamesItsSubmissionAndIsStable(t *testing.T) {
	key := answerKey(t, secret)
	form := mustSlug(t, "ordination")
	id := types.NewID()

	tok, err := submissionbus.MintAnswerLink(key, form, id)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	again, _ := submissionbus.MintAnswerLink(key, form, id)
	if again != tok {
		t.Errorf("the same submission got two links; every message must carry the same one")
	}

	gotForm, gotID, err := submissionbus.ReadAnswerLink(key, tok)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if gotForm != form || gotID != id {
		t.Errorf("read %s/%s, want %s/%s", gotForm, gotID, form, id)
	}
}

func TestAnAnswerLinkThatIsNotOursIsRefused(t *testing.T) {
	key := answerKey(t, secret)
	tok, err := submissionbus.MintAnswerLink(key, mustSlug(t, "ordination"), types.NewID())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	payload, mac, _ := strings.Cut(tok, ".")
	other, _ := submissionbus.MintAnswerLink(key, mustSlug(t, "ordination"), types.NewID())
	otherPayload, _, _ := strings.Cut(other, ".")

	cases := map[string]struct {
		key submissionbus.AnswerKey
		tok string
	}{
		"empty":                 {key, ""},
		"one part":              {key, payload},
		"another row's payload": {key, otherPayload + "." + mac},
		// The first character rather than the last: the last base64
		// character can carry padding bits a lenient decoder ignores.
		"a flipped character":         {key, flip(payload[0]) + payload[1:] + "." + mac},
		"signed with a different key": {answerKey(t, strings.Repeat("z", 40)), tok},
		"padding on the end":          {key, tok + "AA"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := submissionbus.ReadAnswerLink(tc.key, tc.tok)
			if !errors.Is(err, submissionbus.ErrBadAnswerLink) {
				t.Fatalf("Read: %v, want ErrBadAnswerLink", err)
			}
		})
	}
}

func TestAnAnswerKeyMustBeLongAndSet(t *testing.T) {
	if _, err := submissionbus.ParseAnswerKey("short"); err == nil {
		t.Error("a short secret was accepted")
	}

	var zero submissionbus.AnswerKey
	if _, err := submissionbus.MintAnswerLink(zero, mustSlug(t, "xx"), types.NewID()); err == nil {
		t.Error("an unset key minted a link")
	}
}

func flip(b byte) string {
	if b == 'A' {
		return "B"
	}

	return "A"
}

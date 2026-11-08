package submissionbus_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// paidWaiting fills a one-place paid form and puts a second person on its
// waiting list, then gives the place back by hiding the first.
func paidWaiting(t *testing.T) (*submissionbus.Business, *memStore, submissionbus.Submission) {
	t.Helper()

	b, store := newBusiness()
	form := mustSlug(t, "retreat")
	one := []formbus.Claim{{Limit: 1}}

	first := answers(t, form, 15000)
	in, err := b.Accept(t.Context(), now, grantFor(first, "one"), submissionbus.New{Answers: first, Claims: one, Waitlist: true})
	if err != nil || in.Status != submissionbus.StatusPending {
		t.Fatalf("the first Accept: %v, %s", err, in.Status)
	}

	second := answers(t, form, 15000)
	waiting, err := b.Accept(t.Context(), now.Add(time.Minute), grantFor(second, "two"), submissionbus.New{Answers: second, Claims: one, Waitlist: true})
	if err != nil || waiting.Status != submissionbus.StatusWaiting {
		t.Fatalf("the second Accept: %v, %s", err, waiting.Status)
	}

	store.hidden[in.ID] = submissionbus.Hiding{SubmissionID: in.ID, Form: form}

	return b, store, waiting
}

func TestAPaidPlaceIsOfferedAndHeldForTwoDays(t *testing.T) {
	b, _, waiting := paidWaiting(t)
	form := waiting.Form

	_, after, err := b.Offer(t.Context(), now, submissionbus.Offer{ID: waiting.ID, Form: form, Claims: []formbus.Claim{{Limit: 1}}})
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}

	if after.Status != submissionbus.StatusOffered || !after.OfferExpiresAt.Equal(now.Add(submissionbus.OfferHold)) {
		t.Errorf("after the offer: %s until %v, want offered until two days on", after.Status, after.OfferExpiresAt)
	}

	taken, err := b.Taken(t.Context(), form)
	if err != nil || taken.Held() != 1 {
		t.Errorf("an offered place is not held: %d, %v", taken.Held(), err)
	}

	// A declined card leaves the place held: they are still on Stripe's page.
	if err := b.Settle(t.Context(), now.Add(time.Hour), waiting.ID, submissionbus.StatusFailed, "pi_1"); err != nil {
		t.Fatalf("Settle failed: %v", err)
	}

	if got, _ := b.ByID(t.Context(), waiting.ID); got.Status != submissionbus.StatusOffered {
		t.Errorf("a declined card on an offer made it %s, want still offered", got.Status)
	}

	// And another card goes through.
	if err := b.Settle(t.Context(), now.Add(2*time.Hour), waiting.ID, submissionbus.StatusPaid, "pi_1"); err != nil {
		t.Fatalf("Settle paid: %v", err)
	}

	if got, _ := b.ByID(t.Context(), waiting.ID); got.Status != submissionbus.StatusPaid {
		t.Errorf("a paid offer is %s, want paid", got.Status)
	}

	// The sweep, long after, does not touch a paid place.
	if _, err := b.Abandon(t.Context(), now.Add(30*24*time.Hour), time.Hour); err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	if got, _ := b.ByID(t.Context(), waiting.ID); got.Status != submissionbus.StatusPaid {
		t.Errorf("the sweep moved a paid offer to %s", got.Status)
	}
}

// Unpaid when the hold runs out, the place goes back for the office to offer
// again -- and not a moment before.
func TestAnUnpaidOfferLapsesAndGivesThePlaceBack(t *testing.T) {
	b, _, waiting := paidWaiting(t)
	form := waiting.Form

	if _, _, err := b.Offer(t.Context(), now, submissionbus.Offer{ID: waiting.ID, Form: form, Claims: []formbus.Claim{{Limit: 1}}}); err != nil {
		t.Fatalf("Offer: %v", err)
	}

	almost := now.Add(submissionbus.OfferHold - time.Minute)
	if _, err := b.Abandon(t.Context(), almost, time.Hour); err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	if got, _ := b.ByID(t.Context(), waiting.ID); got.Status != submissionbus.StatusOffered {
		t.Fatalf("the sweep let go of an offer still held: %s", got.Status)
	}

	if _, err := b.Abandon(t.Context(), now.Add(submissionbus.OfferHold+time.Minute), time.Hour); err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	if got, _ := b.ByID(t.Context(), waiting.ID); got.Status != submissionbus.StatusAbandoned {
		t.Errorf("a lapsed offer is %s, want abandoned", got.Status)
	}

	if taken, _ := b.Taken(t.Context(), form); taken.Held() != 0 {
		t.Errorf("a lapsed offer still holds its place: %d held", taken.Held())
	}
}

// Somebody sent to pay in the last hour of a hold keeps the place for as long
// as their payment page is open; a lapsed or paid offer cannot be held.
func TestHoldingAnOfferForCheckout(t *testing.T) {
	b, _, waiting := paidWaiting(t)
	form := waiting.Form

	if _, _, err := b.Offer(t.Context(), now, submissionbus.Offer{ID: waiting.ID, Form: form, Claims: []formbus.Claim{{Limit: 1}}}); err != nil {
		t.Fatalf("Offer: %v", err)
	}

	late := now.Add(submissionbus.OfferHold - 10*time.Minute)
	until := late.Add(2 * time.Hour)

	s, err := b.Hold(t.Context(), late, form, waiting.ID, until)
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	if !s.OfferExpiresAt.Equal(until) {
		t.Errorf("held until %v, want %v", s.OfferExpiresAt, until)
	}

	// Never shortened.
	if s, err := b.Hold(t.Context(), late, form, waiting.ID, late.Add(time.Minute)); err != nil || !s.OfferExpiresAt.Equal(until) {
		t.Errorf("a shorter hold moved it to %v, %v", s.OfferExpiresAt, err)
	}

	if _, err := b.Hold(t.Context(), until.Add(time.Minute), form, waiting.ID, until.Add(time.Hour)); !errors.Is(err, submissionbus.ErrOfferLapsed) {
		t.Errorf("holding a lapsed offer: %v, want ErrOfferLapsed", err)
	}

	if _, err := b.Hold(t.Context(), late, mustSlug(t, "elsewhere"), waiting.ID, until); !errors.Is(err, submissionbus.ErrNotFound) {
		t.Errorf("holding an offer through another form: %v, want ErrNotFound", err)
	}
}

// A pay link names the one submission, and a link for changing answers is not
// a link for paying, though both name the same row.
func TestAPayLinkIsItsOwnKind(t *testing.T) {
	key, err := submissionbus.ParseAnswerKey("a-test-secret-that-is-long-enough-to-sign")
	if err != nil {
		t.Fatalf("ParseAnswerKey: %v", err)
	}

	form := mustSlug(t, "retreat")
	id := types.NewID()

	pay, err := submissionbus.MintPayLink(key, form, id)
	if err != nil {
		t.Fatalf("MintPayLink: %v", err)
	}

	gotForm, gotID, err := submissionbus.ReadPayLink(key, pay)
	if err != nil || gotForm != form || gotID != id {
		t.Errorf("ReadPayLink = %s, %s, %v", gotForm, gotID, err)
	}

	answer, err := submissionbus.MintAnswerLink(key, form, id)
	if err != nil {
		t.Fatalf("MintAnswerLink: %v", err)
	}

	if _, _, err := submissionbus.ReadPayLink(key, answer); !errors.Is(err, submissionbus.ErrBadPayLink) {
		t.Errorf("an answer link was read as a pay link: %v", err)
	}

	if _, _, err := submissionbus.ReadAnswerLink(key, pay); !errors.Is(err, submissionbus.ErrBadAnswerLink) {
		t.Errorf("a pay link was read as an answer link: %v", err)
	}
}

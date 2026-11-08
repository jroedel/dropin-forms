package muxer_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/app/domain/embedapp"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// A registration fee, on both surfaces: set in the builder, and charged once
// per submission on the public form with no box to change it.

const adventPath = "/f/advent-retreat"

// adventRetreat is one person per submission at one price, with a gift on top
// for whoever wants to give one.
func adventRetreat(t *testing.T) formbus.Form {
	t.Helper()

	f := formbus.Form{
		ID:        mustSlug(t, "advent-retreat"),
		Title:     "Advent retreat",
		Currency:  "usd",
		ReturnURL: "https://example.test/retreat",
		Fields: []formbus.Field{
			{Name: "name", Label: "Your name", Kind: formbus.KindText, Required: true, MaxLen: 100},
			{Name: "email", Label: "Your email", Kind: formbus.KindEmail, Required: true, MaxLen: 254},
			{Name: "gift", Label: "A gift toward the house", Kind: formbus.KindAmount},
		},
		Fee:      15000,
		FeeLabel: "Retreat registration",
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("the retreat form does not pass Check: %v", err)
	}

	return f
}

func adventSurface(t *testing.T) (http.Handler, *submissionbus.Business) {
	t.Helper()

	cfg := newConfig(t, nil, nil)
	cfg.Embed.Now = func() time.Time { return beforeTheFeast }
	cfg.Embed.Forms = oneForm{adventRetreat(t)}

	subs, ok := cfg.Embed.Submissions.(*submissionbus.Business)
	if !ok {
		t.Fatalf("the test config holds a %T rather than the submission domain", cfg.Embed.Submissions)
	}

	return embedOf(t, cfg), subs
}

func TestAFormWithAFeeShowsItWithNoBoxAndChargesItOnce(t *testing.T) {
	h, subs := adventSurface(t)

	page := getPage(t, h, adventPath).Body.String()

	if !strings.Contains(page, "Retreat registration") || !strings.Contains(page, "$150.00") {
		t.Errorf("the form does not show the fee:\n%s", short(page))
	}

	// The gift is a number box of its own; what must not be here is a
	// quantity, which is the "how many" the fee exists to remove.
	if strings.Contains(page, `name="qty_`) {
		t.Errorf("a form with a fee and nothing for sale has a quantity box:\n%s", short(page))
	}

	// Somebody tries to register five people under the fee's name, and to
	// give twenty-five dollars.
	w := postTo(t, h, adventPath, url.Values{
		embedapp.GrantField:                      {grantIn(t, page)},
		"name":                                   {"Maria O'Neill"},
		"email":                                  {"maria@example.org"},
		"gift":                                   {"25"},
		formbus.QuantityField(formbus.FeeLineID): {"5"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("POST = %d:\n%s", w.Code, short(w.Body.String()))
	}

	if body := w.Body.String(); !strings.Contains(body, "$175.00") || !strings.Contains(body, "Retreat registration") {
		t.Errorf("the confirmation does not show one fee and the gift:\n%s", short(body))
	}

	stored, err := allOf(t.Context(), subs, mustSlug(t, "advent-retreat"))
	if err != nil {
		t.Fatalf("ByForm: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("%d submissions stored, want 1", len(stored))
	}

	sub := stored[0]

	if sub.Answers.Total != types.Money(17500) {
		t.Errorf("stored total = %s, want 175.00", sub.Answers.Total)
	}

	if len(sub.Answers.Lines) != 1 || sub.Answers.Lines[0].ItemID != formbus.FeeLineID || sub.Answers.Lines[0].Qty != 1 {
		t.Errorf("stored lines = %+v, want the fee once", sub.Answers.Lines)
	}

	if sub.Status != submissionbus.StatusPending {
		t.Errorf("stored status = %q, want pending: there is money to collect", sub.Status)
	}
}

// The builder's settings page sets the fee and its name, refuses one that is
// not a price, and says so on the summary.
func TestTheBuilderSetsARegistrationFee(t *testing.T) {
	a := newAdmin(t, "")

	cookie := sessionFor(t, a, siteBoss(t, a, "site@schoenstatt.test"))

	made(t, a, cookie, "advent-retreat", "Advent retreat")

	w := a.post(t, "/forms/advent-retreat/edit/settings", url.Values{
		"title":      {"Advent retreat"},
		"return_url": {"https://example.test/retreat"},
		"fee":        {"150.00"},
		"fee_label":  {"Retreat registration"},
	}, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("saving the settings = %d, want 200:\n%s", w.Code, w.Body)
	}

	s, err := a.catalogue.Draft(t.Context(), mustSlug(t, "advent-retreat"))
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}

	if s.Form.Fee != types.Money(15000) || s.Form.FeeLabel != "Retreat registration" {
		t.Errorf("fee = %s %q, want 150.00 \"Retreat registration\"", s.Form.Fee, s.Form.FeeLabel)
	}

	if page := a.get(t, "/forms/advent-retreat/edit", cookie).Body.String(); !strings.Contains(page, "Retreat registration, $150.00") {
		t.Errorf("the builder's summary does not show the fee:\n%s", page)
	}

	w = a.post(t, "/forms/advent-retreat/edit/settings", url.Values{
		"title": {"Advent retreat"},
		"fee":   {"a hundred and fifty"},
	}, cookie)
	if w.Code != http.StatusBadRequest {
		t.Errorf("a fee that is not a price = %d, want 400:\n%s", w.Code, w.Body)
	}
}

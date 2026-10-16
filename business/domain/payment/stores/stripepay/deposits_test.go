package stripepay_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v83"

	"github.com/jroedel/dropin-forms/business/domain/deposit/depositbus"
	"github.com/jroedel/dropin-forms/business/domain/payment/stores/stripepay"
)

// Deposits against a fake Stripe serving the JSON the real one documents, so
// that what is tested is the reading of Stripe's shapes -- the expanded
// source, the payout's own line, a refusal -- and not a fake of the reader.

func fakeStripe(t *testing.T, routes map[string]string, status int) (*stripepay.Deposits, *[]string) {
	t.Helper()

	var asked []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path+"?"+r.URL.Query().Encode())

		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	backends := stripe.NewBackendsWithConfig(&stripe.BackendConfig{
		URL:               stripe.String(srv.URL),
		HTTPClient:        srv.Client(),
		MaxNetworkRetries: stripe.Int64(0),
	})

	d, err := stripepay.NewDeposits("rk_test_not_a_real_key", stripe.WithBackends(backends))
	if err != nil {
		t.Fatalf("NewDeposits: %v", err)
	}

	return d, &asked
}

func TestPayoutsAreReadWithTheirStatus(t *testing.T) {
	d, asked := fakeStripe(t, map[string]string{"/v1/payouts": `{
  "object": "list", "has_more": false, "url": "/v1/payouts",
  "data": [
    {"id": "po_paid", "object": "payout", "amount": 43390, "currency": "usd", "created": 1760400000,
     "arrival_date": 1760572800, "automatic": true, "status": "paid", "reconciliation_status": "completed"},
    {"id": "po_transit", "object": "payout", "amount": 1000, "currency": "usd", "created": 1760500000,
     "arrival_date": 1760659200, "automatic": true, "status": "in_transit", "reconciliation_status": "in_progress"}
  ]}`}, http.StatusOK)

	since := time.Unix(1760000000, 0)

	got, err := d.Payouts(t.Context(), since)
	if err != nil {
		t.Fatalf("Payouts: %v", err)
	}

	want := []depositbus.Payout{
		{ID: "po_paid", Amount: 43390, Currency: "usd", Created: time.Unix(1760400000, 0).UTC(),
			ArrivalDate: time.Unix(1760572800, 0).UTC(), Automatic: true, Paid: true, Reconciled: true},
		{ID: "po_transit", Amount: 1000, Currency: "usd", Created: time.Unix(1760500000, 0).UTC(),
			ArrivalDate: time.Unix(1760659200, 0).UTC(), Automatic: true},
	}

	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Payouts =\n%+v\nwant\n%+v", got, want)
	}

	if len(*asked) != 1 || !strings.Contains((*asked)[0], url.Values{"created[gte]": {"1760000000"}}.Encode()) {
		t.Errorf("asked %v, want the list bounded by created >= since", *asked)
	}
}

func TestADepositsLinesAreTracedToTheirPayment(t *testing.T) {
	d, asked := fakeStripe(t, map[string]string{"/v1/balance_transactions": `{
  "object": "list", "has_more": false, "url": "/v1/balance_transactions",
  "data": [
    {"id": "txn_sale", "object": "balance_transaction", "amount": 4500, "fee": 161, "net": 4339,
     "created": 1760300000, "type": "charge", "reporting_category": "charge", "description": "feast-lunch",
     "source": {"id": "ch_1", "object": "charge", "payment_intent": "pi_sale"}},
    {"id": "txn_refund", "object": "balance_transaction", "amount": -1500, "fee": 0, "net": -1500,
     "created": 1760310000, "type": "refund", "reporting_category": "refund",
     "source": {"id": "re_1", "object": "refund", "payment_intent": "pi_refunded"}},
    {"id": "txn_dispute", "object": "balance_transaction", "amount": -2000, "fee": 1500, "net": -3500,
     "created": 1760320000, "type": "adjustment", "reporting_category": "dispute",
     "source": {"id": "du_1", "object": "dispute", "payment_intent": "pi_disputed"}},
    {"id": "txn_fee", "object": "balance_transaction", "amount": -200, "fee": 0, "net": -200,
     "created": 1760330000, "type": "stripe_fee", "reporting_category": "fee", "source": null},
    {"id": "txn_payout", "object": "balance_transaction", "amount": -43390, "fee": 0, "net": -43390,
     "created": 1760400000, "type": "payout", "reporting_category": "payout",
     "source": {"id": "po_1", "object": "payout"}}
  ]}`}, http.StatusOK)

	got, err := d.Lines(t.Context(), "po_1")
	if err != nil {
		t.Fatalf("Lines: %v", err)
	}

	want := []struct {
		id   string
		kind depositbus.Kind
		ref  string
		net  int64
	}{
		{"txn_sale", depositbus.KindSale, "pi_sale", 4339},
		{"txn_refund", depositbus.KindRefund, "pi_refunded", -1500},
		{"txn_dispute", depositbus.KindDispute, "pi_disputed", -3500},
		{"txn_fee", depositbus.KindOther, "", -200},
	}

	// The payout's own line is left out: it is the deposit, not a part of it.
	if len(got) != len(want) {
		t.Fatalf("Lines = %d lines, want %d: %+v", len(got), len(want), got)
	}

	for i, w := range want {
		if g := got[i]; g.ID != w.id || g.Kind != w.kind || g.PaymentRef != w.ref || int64(g.Net) != w.net {
			t.Errorf("line %d = %+v, want %+v", i, g, w)
		}
	}

	if got[0].Fee != 161 || got[0].Amount != 4500 || got[0].Type != "charge" || got[0].Description != "feast-lunch" {
		t.Errorf("the sale's details = %+v", got[0])
	}

	q := (*asked)[0]
	if !strings.Contains(q, "payout=po_1") || !strings.Contains(q, "=data.source") {
		t.Errorf("asked %q, want the payout's lines with their sources expanded", q)
	}
}

func TestAKeyWithoutThePermissionSaysWhichToGive(t *testing.T) {
	d, _ := fakeStripe(t, map[string]string{"/v1/payouts": `{"error": {
  "type": "invalid_request_error",
  "message": "The provided key 'rk_test_***' does not have the required permissions for this endpoint. Having the 'rak_payout_read' permission would allow this request to continue."
}}`}, http.StatusForbidden)

	_, err := d.Payouts(t.Context(), time.Time{})
	if err == nil {
		t.Fatal("Payouts succeeded with a refused key")
	}

	for _, want := range []string{"Payouts: Read", "rak_payout_read"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

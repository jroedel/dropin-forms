package submissionbus_test

import (
	"slices"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

func TestARowIsLaidOutFromTheDefinitionWithTypedCells(t *testing.T) {
	f := formbus.Form{
		ID:       mustSlug(t, "supper"),
		Currency: "usd",
		Fields: []formbus.Field{
			{Name: "name", Label: "Your name", Kind: formbus.KindText},
			{Name: "part", Label: "Part two", Kind: formbus.KindSection},
			{Name: "diet", Label: "Diet", Kind: formbus.KindChoices},
		},
		Items: []formbus.Item{{ID: "item_1", Label: "Lunch", Price: 1200}, {ID: "item_2", Label: "Shirt", Price: 2000}},
	}

	cols := submissionbus.Columns(f)

	var keys []string
	for _, c := range cols {
		keys = append(keys, c.Key)
	}

	// The section heading has no column; every question and item has one,
	// whether or not anybody answered it; refunded because it takes money.
	want := []string{"received", "ref", "status", "total", "currency", "q:name", "q:diet", "item:item_1", "item:item_2", "payment_ref", "refunded"}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys = %v\nwant %v", keys, want)
	}

	at := time.Date(2026, 11, 12, 9, 30, 0, 0, time.FixedZone("CST", -6*3600))
	s := submissionbus.Submission{
		ID:        types.NewID(),
		CreatedAt: at,
		Status:    submissionbus.StatusPaid,
		Refunded:  500,
		Answers: formbus.Answers{
			Currency: "usd",
			Fields:   []formbus.Answer{{Name: "diet", Kind: formbus.KindChoices, Values: []string{"vegan", "no nuts"}}},
			Lines:    []formbus.Line{{ItemID: "item_1", Qty: 2}},
			Total:    2400,
		},
		PaymentRef: "pi_1",
	}

	cells := submissionbus.Cells(f, s)
	if len(cells) != len(cols) {
		t.Fatalf("%d cells for %d columns", len(cells), len(cols))
	}

	var text []string
	for _, c := range cells {
		text = append(text, c.String())
	}

	wantText := []string{"2026-11-12T15:30:00Z", s.ID.String(), "paid", "24.00", "usd", "", "vegan, no nuts", "2", "", "pi_1", "5.00"}
	if !slices.Equal(text, wantText) {
		t.Errorf("as text = %q\nwant      %q", text, wantText)
	}

	if cells[0].Kind != submissionbus.CellTime || cells[3].Kind != submissionbus.CellMoney || cells[7].Kind != submissionbus.CellCount || !cells[8].Empty {
		t.Errorf("cells are not typed for a sheet: %+v", cells)
	}
}

func TestAStatusReadsInWordsAPersonUnderstands(t *testing.T) {
	cases := []struct {
		s    submissionbus.Submission
		want string
	}{
		{submissionbus.Submission{Status: submissionbus.StatusPending}, "in checkout"},
		{submissionbus.Submission{Status: submissionbus.StatusWaiting, Review: submissionbus.ReviewAwaiting}, "on the waiting list"},
		{submissionbus.Submission{Status: submissionbus.StatusReceived, Review: submissionbus.ReviewDeclined}, "declined"},
		{submissionbus.Submission{Status: submissionbus.StatusPaid, Refunded: 2500}, "paid, $25.00 refunded"},
	}

	for _, c := range cases {
		if got := c.s.WordsWithRefund("usd"); got != c.want {
			t.Errorf("%s/%s reads %q, want %q", c.s.Status, c.s.Review, got, c.want)
		}
	}
}

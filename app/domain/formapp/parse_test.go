package formapp

import (
	"slices"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
)

func TestOptionsAreReadWithAndWithoutALimit(t *testing.T) {
	got, err := parseOptions(strings.Join([]string{
		"yes",
		"a | Monday 9 am | 1",
		"b | Monday 10 am|2",
		"hall | Hall | upstairs",
		"c | | 3",
	}, "\n"))
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}

	want := []formbus.Option{
		{Value: "yes", Label: "yes"},
		{Value: "a", Label: "Monday 9 am", Limit: 1},
		{Value: "b", Label: "Monday 10 am", Limit: 2},
		{Value: "hall", Label: "Hall | upstairs"}, // a bar in a label is not a limit
		{Value: "c", Label: "c", Limit: 3},
	}

	if !slices.Equal(got, want) {
		t.Errorf("parseOptions =\n%+v\nwant\n%+v", got, want)
	}

	// Back into the box and out again, unchanged.
	again, err := parseOptions(writeOptions(got))
	if err != nil || !slices.Equal(again, got) {
		t.Errorf("a round trip through the box gave %+v, %v", again, err)
	}
}

func TestALineWithOneBarNeverHasALimit(t *testing.T) {
	got, err := parseOptions("2 | 2")
	if err != nil || len(got) != 1 || got[0].Limit != 0 || got[0].Label != "2" {
		t.Errorf("parseOptions(%q) = %+v, %v; one bar is a value and a label", "2 | 2", got, err)
	}
}

func TestALimitOfNoughtIsRefused(t *testing.T) {
	_, err := parseOptions("a | Monday 9 am | 0")
	if err == nil || !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("parseOptions with a limit of 0: %v, want a refusal naming the option", err)
	}
}

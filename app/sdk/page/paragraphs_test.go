package page_test

import (
	"slices"
	"testing"

	"github.com/jroedel/dropin-forms/app/sdk/page"
)

func TestTextIsSplitIntoParagraphsOfLines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want [][]string
	}{
		{"nothing", "", nil},
		{"only blank lines", "\n  \n\r\n", nil},
		{"one line", "Bring a sleeping bag.", [][]string{{"Bring a sleeping bag."}}},
		{
			"two lines, one paragraph",
			"Early registration: $35\r\nDay of the retreat: $40",
			[][]string{{"Early registration: $35", "Day of the retreat: $40"}},
		},
		{
			"a blank line between, however many",
			"  First.\n\n\n Second,\nwith more. \n",
			[][]string{{"First."}, {"Second,", "with more."}},
		},
	}

	for _, c := range cases {
		got := page.Paragraphs(c.in)
		if !slices.EqualFunc(got, c.want, slices.Equal) {
			t.Errorf("%s: Paragraphs(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

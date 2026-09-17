package types_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/business/types"
)

// The parse tests are a table rather than prose because the list of rejected
// shapes *is* the specification. Each rejected case is a string a browser can
// send, and for most of them strconv.ParseFloat would have returned a number.
func TestParseMoneyAccepts(t *testing.T) {
	tests := []struct {
		in   string
		want types.Money
	}{
		{"0", 0},
		{"12", 1200},
		{"12.50", 1250},
		{"12.5", 1250},
		{"0.99", 99},
		{"0.01", 1},
		{"0.1", 10},
		{"0.0", 0},
		{"1000", 100000},
		{"15", 1500},

		// Leading zeros are a browser quirk, not an attack, and mean what they
		// look like.
		{"012", 1200},
		{"00.50", 50},

		// The ceiling itself, to pin that the bound is inclusive.
		{"1000000", 100_000_000},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := types.ParseMoney(tt.in)
			if err != nil {
				t.Fatalf("ParseMoney(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseMoney(%q) = %d cents, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseMoneyRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"blank", ""},

		// Every one of these parses as a float.
		{"scientific notation", "1e5"},
		{"scientific notation capital", "1E5"},
		{"scientific negative exponent", "1e-2"},
		{"hexadecimal", "0x10"},
		{"hex float", "0x1p-2"},
		{"leading plus", "+5"},
		{"infinity", "Inf"},
		{"infinity long", "Infinity"},
		{"not a number", "NaN"},
		{"underscores", "1_000"},

		// Negative amounts. A refund is not a submission.
		{"negative", "-5"},
		{"negative cents", "-0.01"},

		// Shapes that would round rather than refuse.
		{"three decimal places", "5.001"},
		{"many decimal places", "1.23456"},

		// Malformed decimals.
		{"trailing point", "5."},
		{"leading point", ".99"},
		{"two points", "1.2.3"},
		{"only a point", "."},

		// Whitespace in any position.
		{"leading space", " 5"},
		{"trailing space", "5 "},
		{"inner space", "1 5"},
		{"tab", "\t5"},
		{"newline", "5\n"},

		// Formatted money, which is a person's mistake rather than an attack --
		// but still not something to guess at.
		{"dollar sign", "$12"},
		{"dollar sign with cents", "$12.50"},
		{"thousands separator", "1,000"},
		{"european decimal comma", "1,00"},

		// Not ASCII digits. unicode.IsDigit says yes to both of these.
		{"arabic-indic digits", "١٢"},
		{"devanagari digits", "१२"},
		{"fullwidth digits", "１２"},

		// Words.
		{"letters", "twelve"},
		{"trailing letters", "12usd"},
		{"leading letters", "usd12"},

		// Above the ceiling, including a value that would overflow int64 if the
		// whole part were multiplied by 100 without checking first.
		{"over the ceiling", "1000000.01"},
		{"far over the ceiling", "99999999"},
		{"would overflow int64 times 100", "92233720368547758"},
		{"far past int64", "999999999999999999999999"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := types.ParseMoney(tt.in)
			if err == nil {
				t.Fatalf("ParseMoney(%q) = %d cents, want an error", tt.in, got)
			}
			if !errors.Is(err, types.ErrNotAnAmount) {
				t.Errorf("ParseMoney(%q) error does not wrap ErrNotAnAmount: %v", tt.in, err)
			}
			if got != 0 {
				t.Errorf("ParseMoney(%q) returned %d cents alongside its error; a rejected amount must be zero", tt.in, got)
			}
		})
	}
}

// The error reaches a person, so it must not name a Go package or a Go type.
// House style, and cheap to assert once rather than remember at each site.
func TestParseMoneyMessagesAreForPeople(t *testing.T) {
	forbidden := []string{"strconv", "ParseInt", "ParseFloat", "types.", "int64", "Money"}

	for _, in := range []string{"", "1e5", "$12", "5.001", "-5", "99999999", "5."} {
		_, err := types.ParseMoney(in)
		if err == nil {
			t.Fatalf("ParseMoney(%q) unexpectedly succeeded", in)
		}

		for _, word := range forbidden {
			if strings.Contains(err.Error(), word) {
				t.Errorf("ParseMoney(%q) error names %q, which means nothing to a person: %v", in, word, err)
			}
		}
	}
}

func TestMoneyTimes(t *testing.T) {
	tests := []struct {
		name  string
		price types.Money
		qty   int
		want  types.Money
		fails bool
	}{
		{name: "one ticket", price: 1200, qty: 1, want: 1200},
		{name: "a table of eight", price: 1200, qty: 8, want: 9600},
		{name: "zero tickets", price: 1200, qty: 0, want: 0},
		{name: "a free item", price: 0, qty: 5, want: 0},
		{name: "free and none", price: 0, qty: 0, want: 0},

		// A quantity arrives from a browser, so the interesting cases are the
		// ones a browser can send.
		{name: "negative quantity", price: 1200, qty: -1, fails: true},
		{name: "quantity over the ceiling", price: 1200, qty: 1_000_000, fails: true},

		// The multiplication that would wrap int64 rather than merely exceed
		// the ceiling. This is the case a naive `price * qty` gets wrong: it
		// returns a *small positive* number, which then passes a maximum check.
		{name: "would overflow int64", price: 100_000_000, qty: 1 << 40, fails: true},

		{name: "exactly the ceiling", price: 1_000_000, qty: 100, want: 100_000_000},
		{name: "one cent over the ceiling", price: 1_000_001, qty: 100, fails: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.price.Times(tt.qty)

			switch {
			case tt.fails && err == nil:
				t.Fatalf("%s x %d = %d, want an error", tt.price, tt.qty, got)
			case !tt.fails && err != nil:
				t.Fatalf("%s x %d: %v", tt.price, tt.qty, err)
			case tt.fails:
				if !errors.Is(err, types.ErrNotAnAmount) {
					t.Errorf("error does not wrap ErrNotAnAmount: %v", err)
				}
				if got != 0 {
					t.Errorf("returned %d cents alongside its error", got)
				}

				return
			}

			if got != tt.want {
				t.Errorf("%s x %d = %d cents, want %d", tt.price, tt.qty, got, tt.want)
			}
		})
	}
}

func TestMoneyAdd(t *testing.T) {
	tests := []struct {
		name  string
		a, b  types.Money
		want  types.Money
		fails bool
	}{
		{name: "two lines", a: 1200, b: 2400, want: 3600},
		{name: "a free line", a: 1200, b: 0, want: 1200},
		{name: "both zero", a: 0, b: 0, want: 0},
		{name: "up to the ceiling", a: 99_999_999, b: 1, want: 100_000_000},
		{name: "one past the ceiling", a: 100_000_000, b: 1, fails: true},
		{name: "both at the ceiling", a: 100_000_000, b: 100_000_000, fails: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.a.Add(tt.b)

			switch {
			case tt.fails && err == nil:
				t.Fatalf("%s + %s = %d, want an error", tt.a, tt.b, got)
			case !tt.fails && err != nil:
				t.Fatalf("%s + %s: %v", tt.a, tt.b, err)
			case tt.fails:
				return
			}

			if got != tt.want {
				t.Errorf("%s + %s = %d cents, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestMoneyString(t *testing.T) {
	tests := []struct {
		in   types.Money
		want string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{10, "0.10"},
		{99, "0.99"},
		{100, "1.00"},
		{1200, "12.00"},
		{1250, "12.50"},
		{1500, "15.00"},
		{100_000_000, "1000000.00"},

		// Not reachable through ParseMoney, but a Money is an int64 and a
		// subtraction somewhere could make one. Formatting it as "-0.-1" would
		// be worse than formatting it correctly.
		{-1, "-0.01"},
		{-1250, "-12.50"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.in.String(); got != tt.want {
				t.Errorf("Money(%d).String() = %q, want %q", int64(tt.in), got, tt.want)
			}
		})
	}
}

// Whatever ParseMoney accepts must survive a round trip through String and
// parse back to the same number of cents. If it does not, then a price shown on
// a page is not the price that would be charged for it.
func TestMoneyRoundTrips(t *testing.T) {
	for _, in := range []string{"0", "0.01", "0.99", "12", "12.50", "12.5", "15", "1000", "1000000"} {
		cents, err := types.ParseMoney(in)
		if err != nil {
			t.Fatalf("ParseMoney(%q): %v", in, err)
		}

		again, err := types.ParseMoney(cents.String())
		if err != nil {
			t.Fatalf("ParseMoney(%q.String() = %q): %v", in, cents.String(), err)
		}

		if again != cents {
			t.Errorf("%q parsed to %d cents, formatted as %q, parsed back to %d", in, cents, cents.String(), again)
		}
	}
}

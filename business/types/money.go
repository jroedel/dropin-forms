package types

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Money is an amount in the currency's minor unit -- cents, for USD.
//
// An integer, because a price is a count of cents and not a measurement. The
// alternative costs real money: float64 cannot hold 0.1, so a total built by
// adding floats drifts, and a drift of one cent on a Stripe charge is a
// reconciliation somebody does by hand.
type Money int64

// ErrNotAnAmount is what every parse failure wraps, so a caller can recognise
// the class without matching on the sentence.
var ErrNotAnAmount = errors.New("not an amount")

// maxMoney is a ceiling on any single parsed amount: one million dollars in
// cents. Nothing this service sells comes close, so a value above it is a
// mistake or an attempt, and refusing early means no later multiplication has
// to worry about overflowing int64.
const maxMoney Money = 100_000_000

// MaxAmount is the largest amount this service will take, in minor units.
//
// Exported so that a form definition can be checked against it at load time
// rather than having a price that every submission refuses at the moment
// somebody tries to pay it.
func MaxAmount() Money { return maxMoney }

// ParseMoney reads a plain decimal amount in the major unit -- "12", "12.50",
// "0.99" -- and returns it in minor units.
//
// It is deliberately strict, and the list of things it refuses is the point of
// the function rather than incidental to it. strconv.ParseFloat accepts every
// one of these:
//
//	"1e5"     scientific notation: a hundred thousand where 1 was meant
//	"0x10"    hexadecimal
//	"+5"      a leading sign
//	"5."      a trailing point
//	" 5"      whitespace
//	"Inf"     infinity, which formats back as a number
//	"NaN"     which compares false against every bound you check
//
// and then rounds "5.001" to something plausible instead of refusing it. Every
// one of those is a way for a browser to submit an amount that passes a bounds
// check and charges the wrong number, so this parses by hand and accepts only
// digits, at most one point, and at most two places after it.
//
// The currency's own minimum is not checked here. That belongs to the form
// that is selling something, because it is a rule about this sale rather than
// about arithmetic.
func ParseMoney(s string) (Money, error) {
	if s == "" {
		return 0, fmt.Errorf("%w: it is blank", ErrNotAnAmount)
	}

	// No TrimSpace. Whitespace means the value did not come from the field we
	// rendered, and silently accepting it hides that.
	if strings.TrimSpace(s) != s {
		return 0, fmt.Errorf("%w: %q has a space in it", ErrNotAnAmount, s)
	}

	// A currency symbol or a thousands separator is a reasonable thing for a
	// person to type, so the message says what to do rather than only what is
	// wrong. The field is rendered with inputmode=decimal, so this is the
	// uncommon path.
	if strings.ContainsAny(s, "$,") {
		return 0, fmt.Errorf("%w: write %q as digits only, like 12 or 12.50", ErrNotAnAmount, s)
	}

	whole, frac, hasPoint := strings.Cut(s, ".")

	if whole == "" {
		return 0, fmt.Errorf("%w: %q has no digits before the decimal point; write 0.99 rather than .99", ErrNotAnAmount, s)
	}
	if !allDigits(whole) {
		return 0, fmt.Errorf("%w: %q is not a plain number", ErrNotAnAmount, s)
	}

	if hasPoint {
		switch {
		case frac == "":
			return 0, fmt.Errorf("%w: %q ends in a decimal point", ErrNotAnAmount, s)
		case !allDigits(frac):
			return 0, fmt.Errorf("%w: %q is not a plain number", ErrNotAnAmount, s)
		case len(frac) > 2:
			return 0, fmt.Errorf("%w: %q has more than two decimal places", ErrNotAnAmount, s)
		}
	}

	// Parsed rather than accumulated digit by digit, now that the shape is
	// known to be safe. ParseInt does the overflow check that a hand-rolled
	// loop would get wrong.
	dollars, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is too large", ErrNotAnAmount, s)
	}

	var cents int64
	switch len(frac) {
	case 0:
	case 1:
		cents, _ = strconv.ParseInt(frac, 10, 64)
		cents *= 10
	case 2:
		cents, _ = strconv.ParseInt(frac, 10, 64)
	}

	if dollars > int64(maxMoney)/100 {
		return 0, fmt.Errorf("%w: %q is larger than this service will take", ErrNotAnAmount, s)
	}

	total := Money(dollars*100 + cents)
	if total > maxMoney {
		return 0, fmt.Errorf("%w: %q is larger than this service will take", ErrNotAnAmount, s)
	}

	return total, nil
}

// Times multiplies an amount by a count, refusing anything that would overflow
// or exceed the ceiling.
//
// A separate method rather than a bare `price * qty` so that the check cannot
// be forgotten at a call site. qty arrives from a browser; price does not.
func (m Money) Times(qty int) (Money, error) {
	if qty < 0 {
		return 0, fmt.Errorf("%w: a quantity cannot be negative", ErrNotAnAmount)
	}
	if qty == 0 || m == 0 {
		return 0, nil
	}

	if int64(m) > int64(maxMoney)/int64(qty) {
		return 0, fmt.Errorf("%w: %d at %s each is more than this service will take", ErrNotAnAmount, qty, m)
	}

	return m * Money(qty), nil
}

// Add sums two amounts, refusing a total above the ceiling.
func (m Money) Add(n Money) (Money, error) {
	if m > maxMoney-n {
		return 0, fmt.Errorf("%w: the total is more than this service will take", ErrNotAnAmount)
	}

	return m + n, nil
}

// String formats as a plain decimal with two places, with no currency symbol.
// The symbol is a presentation decision and belongs to whatever is rendering,
// which knows the form's currency.
func (m Money) String() string {
	sign := ""
	v := int64(m)
	if v < 0 {
		sign = "-"
		v = -v
	}

	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}

	for _, r := range s {
		// Explicitly ASCII. unicode.IsDigit accepts Arabic-Indic and Devanagari
		// digits, which strconv.ParseInt then refuses -- and a value that
		// passes one check and fails the next is how a confusing 500 happens.
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

package submissionbus

import (
	"strconv"
	"strings"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// A submission laid out as one row of a spreadsheet: the download and the
// Google Sheet a person connects.
//
// Here rather than in either app, because the sheet is kept up to date by a
// worker in the business layer, which cannot import an app, and two copies of
// "which columns, in which order" are two answers that drift. The values are
// typed so that each reader writes them its own way: the download as text in
// the stored words a formula matches on, the sheet as numbers and dates a
// spreadsheet can add up and sort.

// Column is one column of the layout.
type Column struct {
	// Key names the column for as long as the form exists, whatever its
	// heading becomes: "ref", "received", "status", "total", "currency",
	// "q:<field name>", "item:<item id>", "payment_ref", "review",
	// "refunded". A sheet finds its columns by this, after somebody has
	// moved them about.
	Key string

	// Name is the download's heading: a fixed lower-case word for the
	// columns every form has, so that a formula built on one download works
	// on the next, and the question or item's own label for the rest.
	Name string

	// Label is the heading a person reads, in a sheet.
	Label string
}

// CellKind says which of a Cell's values is set.
type CellKind int

const (
	// CellText is Text: an answer, a reference, a word.
	CellText CellKind = iota

	// CellCount is Count, a quantity; Empty says there is none, which is not
	// zero -- an item nobody chose is blank, not 0.
	CellCount

	// CellMoney is Money, in the form's currency.
	CellMoney

	// CellTime is Time.
	CellTime
)

// Cell is one value of a row, lined up with Columns.
type Cell struct {
	Kind  CellKind
	Text  string
	Count int
	Money types.Money
	Time  time.Time

	// Empty is a cell with nothing in it, whatever its kind.
	Empty bool
}

// String is the cell as the download writes it: RFC 3339 in UTC for a time,
// because a spreadsheet full of local times with no offset is one nobody can
// compare against anything; minor-unit decimal for money.
func (c Cell) String() string {
	if c.Empty {
		return ""
	}

	switch c.Kind {
	case CellCount:
		return strconv.Itoa(c.Count)
	case CellMoney:
		return c.Money.String()
	case CellTime:
		return c.Time.UTC().Format(time.RFC3339)
	}

	return c.Text
}

// Columns is the layout of every row of a form, in order.
//
// From the definition rather than from the submissions, which is the whole
// reason formbus.Answers keeps its fields as an ordered slice. Derived from
// the rows instead, a form whose first submission skipped a conditional field
// would export a spreadsheet missing that column entirely, and two exports of
// the same form could have different columns.
//
// The review and refunded columns are only on a form that asks for approval
// or takes money, so that every other form's download keeps the columns a
// spreadsheet was already built on.
func Columns(f formbus.Form) []Column {
	out := []Column{
		{Key: "received", Name: "submitted_at", Label: "Received"},
		{Key: "ref", Name: "submission_id", Label: "Reference"},
		{Key: "status", Name: "status", Label: "Status"},
		{Key: "total", Name: "total", Label: "Total"},
		{Key: "currency", Name: "currency", Label: "Currency"},
	}

	for _, fld := range f.Questions() {
		out = append(out, Column{Key: "q:" + fld.Name, Name: fld.Label, Label: fld.Label})
	}

	for _, it := range f.Items {
		out = append(out, Column{Key: "item:" + it.ID, Name: it.Label, Label: it.Label})
	}

	out = append(out, Column{Key: "payment_ref", Name: "payment_reference", Label: "Payment reference"})

	if f.NeedsApproval {
		out = append(out, Column{Key: "review", Name: "review", Label: "Review"})
	}

	if f.Sells() {
		out = append(out, Column{Key: "refunded", Name: "refunded", Label: "Refunded"})
	}

	return out
}

// Cells is one submission's values, lined up with Columns. Status and review
// are the stored words; a reader that shows a person something friendlier
// replaces them by Key.
func Cells(f formbus.Form, s Submission) []Cell {
	out := []Cell{
		{Kind: CellTime, Time: s.CreatedAt},
		{Kind: CellText, Text: s.ID.String()},
		{Kind: CellText, Text: s.Status.String()},
		{Kind: CellMoney, Money: s.Answers.Total},
		{Kind: CellText, Text: s.Answers.Currency},
	}

	for _, fld := range f.Questions() {
		ans, ok := s.Answers.Field(fld.Name)
		if !ok {
			// Not asked, or left blank. Empty either way in a spreadsheet;
			// the detail page is where the difference is visible.
			out = append(out, Cell{Kind: CellText, Empty: true})

			continue
		}

		out = append(out, Cell{Kind: CellText, Text: strings.Join(ans.Values, ", ")})
	}

	for _, it := range f.Items {
		qty := Cell{Kind: CellCount, Empty: true}

		for _, l := range s.Answers.Lines {
			if l.ItemID == it.ID {
				qty = Cell{Kind: CellCount, Count: l.Qty}

				break
			}
		}

		out = append(out, qty)
	}

	out = append(out, Cell{Kind: CellText, Text: s.PaymentRef})

	if f.NeedsApproval {
		out = append(out, Cell{Kind: CellText, Text: s.Review.String()})
	}

	if f.Sells() {
		out = append(out, Cell{Kind: CellMoney, Money: s.Refunded})
	}

	return out
}

// Words is how a submission's standing reads to a person: where it is with
// the office, on a form that asks for approval, and otherwise where it is with
// its payment. The two never both have something to say, because a form that
// asks for approval sells nothing.
func (s Submission) Words() string {
	// The waiting list first: on a form that also asks for approval, nobody
	// decides anything about somebody who has no place yet.
	if s.Status == StatusWaiting {
		return s.Status.Words()
	}

	switch s.Review {
	case ReviewAwaiting:
		return "awaiting approval"
	case ReviewApproved:
		return "approved"
	case ReviewDeclined:
		return "declined"
	}

	return s.Status.Words()
}

// WordsWithRefund is Words, and how much has gone back when anything has:
// "paid, $25.00 refunded".
func (s Submission) WordsWithRefund(currency string) string {
	words := s.Words()

	if s.Refunded > 0 {
		words += ", " + formbus.Show(s.Refunded, currency) + " refunded"
	}

	return words
}

// Words is how a status reads to a person.
//
// Not the stored word, for the ones a person misreads. "pending" is what the
// database calls an order whose payment page is open, and people read it as
// something under way long after it had stopped being anything; "failed" is a
// declined card, which is not a failure of anything here and is still payable
// with another card until the page closes. The download and the API keep the
// stored words, which a formula or a program matches on.
func (s Status) Words() string {
	switch s {
	case StatusPending:
		return "in checkout"
	case StatusFailed:
		return "card declined"
	case StatusWaiting:
		return "on the waiting list"
	case StatusOffered:
		return "offered a place, not paid yet"
	}

	return s.String()
}

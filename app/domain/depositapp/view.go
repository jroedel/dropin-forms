package depositapp

import (
	"cmp"
	"strings"

	"github.com/jroedel/dropin-forms/business/domain/deposit/depositbus"
	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// pageView is the whole page.
type pageView struct {
	FormID string
	Title  string

	// The three numbers at the top: what this form has put into the bank so
	// far, what Stripe kept doing it, and what is paid and still on its way.
	Deposited string
	Fees      string
	Waiting   string

	Months []monthView

	// Undeposited is the orders paid and not yet in any deposit, by name, so
	// a deposit that looks short can be explained by somebody whose money is
	// still in Stripe's balance.
	Undeposited []orderView

	// Checked says when deposits were last read from Stripe.
	Checked string
}

// monthView is the deposits that arrived in one calendar month.
type monthView struct {
	Name string

	// Net is this form's share of the month's deposits, which is the number
	// that goes into the month's books for this form.
	Net string

	Deposits []depositView
}

// depositView is one deposit as this form sees it.
type depositView struct {
	Payout  string
	Arrived string

	// Amount is the bank line.
	Amount string

	Sales    string
	Refunds  string
	Disputes string
	Other    string
	Fees     string
	Net      string

	OtherForms   string
	NotFromAForm string

	// The rows of the breakdown drawn only when they are not zero, because a
	// refunds row reading 0.00 on every deposit is noise somebody has to read
	// past to find the one that is not.
	HasRefunds, HasDisputes, HasOther, HasOtherForms, HasNotFromAForm bool

	// To is the account it went to, "CHASE BANK ••••2013", or Stripe's
	// identifier for it when that is all there is.
	To    string
	Last4 string

	// Trace is the bank's reference for the transfer, or a sentence saying
	// why there is none yet.
	Trace   string
	TraceID bool

	// Mismatch is Stripe's lines not adding up to the deposit. Said in words
	// on the deposit, because a breakdown that does not match the bank is
	// worse than none if nobody is told.
	Mismatch bool

	Lines []lineView
}

// lineView is one movement of this form's money inside a deposit.
type lineView struct {
	When   string
	Kind   string
	Who    string
	Order  string
	Amount string
	Fee    string
	Net    string
}

// orderView is a paid order not yet deposited.
type orderView struct {
	Order  string
	Who    string
	When   string
	Amount string
}

// build turns the domain's answer into the page.
func build(f formbus.Form, shares []depositbus.Share, orders map[types.ID]submissionbus.Submission, waiting []submissionbus.Submission) pageView {
	cur := f.Currency

	view := pageView{FormID: f.ID.String(), Title: f.Title}

	var deposited, fees, owed types.Money

	// Each month's net, beside the months, so a month's total is the sum of
	// exactly the deposits listed under it.
	var nets []types.Money

	for _, s := range shares {
		deposited += s.Net
		fees += s.Fees

		month := s.Payout.ArrivalDate.Format("January 2006")

		if n := len(view.Months); n == 0 || view.Months[n-1].Name != month {
			view.Months = append(view.Months, monthView{Name: month})
			nets = append(nets, 0)
		}

		m := &view.Months[len(view.Months)-1]
		nets[len(nets)-1] += s.Net

		d := depositView{
			Payout:          s.Payout.ID,
			Arrived:         s.Payout.ArrivalDate.Format("Mon 2 Jan 2006"),
			Amount:          show(s.Payout.Amount, cur),
			Sales:           show(s.Sales, cur),
			Refunds:         show(s.Refunds, cur),
			Disputes:        show(s.Disputes, cur),
			Other:           show(s.Other, cur),
			Fees:            show(-s.Fees, cur),
			Net:             show(s.Net, cur),
			OtherForms:      show(s.OtherForms, cur),
			NotFromAForm:    show(s.NotFromAForm, cur),
			HasRefunds:      s.Refunds != 0,
			HasDisputes:     s.Disputes != 0,
			HasOther:        s.Other != 0,
			HasOtherForms:   s.OtherForms != 0,
			HasNotFromAForm: s.NotFromAForm != 0,
			Mismatch:        s.Payout.Mismatch,
			To:              cmp.Or(destination(s.Payout), "not yet known"),
			Last4:           s.Payout.Last4,
			TraceID:         s.Payout.TraceID != "",
		}
		d.Trace = trace(s.Payout)

		for _, l := range s.Lines {
			d.Lines = append(d.Lines, lineView{
				When:   l.Created.Local().Format("2 Jan 15:04"),
				Kind:   kindWord(l.Kind),
				Who:    nameOf(orders[l.SubmissionID]),
				Order:  l.SubmissionID.String(),
				Amount: show(l.Amount, cur),
				Fee:    show(-l.Fee, cur),
				Net:    show(l.Net, cur),
			})
		}

		m.Deposits = append(m.Deposits, d)
	}

	for i, net := range nets {
		view.Months[i].Net = show(net, cur)
	}

	for _, s := range waiting {
		owed += s.Answers.Total

		view.Undeposited = append(view.Undeposited, orderView{
			Order:  s.ID.String(),
			Who:    nameOf(s),
			When:   s.CreatedAt.Local().Format("2 Jan 2006"),
			Amount: show(s.Answers.Total, cur),
		})
	}

	view.Deposited = show(deposited, cur)
	view.Fees = show(fees, cur)
	view.Waiting = show(owed, cur)

	return view
}

// show is formbus.Show with the minus sign in front of the currency symbol,
// where somebody reading a ledger expects it: −$15.00, not $-15.00.
func show(m types.Money, currency string) string {
	if m < 0 {
		return "−" + formbus.Show(-m, currency)
	}

	return formbus.Show(m, currency)
}

// destination is where a payout went, as somebody looking at their bank
// accounts would name it, or empty when Stripe has not said.
func destination(p depositbus.Payout) string {
	switch {
	case p.Last4 != "":
		return strings.TrimSpace(p.Bank + " ••••" + p.Last4)
	default:
		return p.Destination
	}
}

// trace is the bank's reference for a payout, or why there is none.
func trace(p depositbus.Payout) string {
	switch {
	case p.TraceID != "":
		return p.TraceID
	case p.TraceStatus == depositbus.TraceUnsupported:
		return "not given by this bank"
	default:
		// Pending, or a payout kept before traces were recorded and not yet
		// described by a later sync. Either way Stripe may still say.
		return "not yet known; Stripe usually has it within a few days of the deposit"
	}
}

// kindWord is a line's kind as the page says it.
func kindWord(k depositbus.Kind) string {
	switch k {
	case depositbus.KindSale:
		return "Sale"
	case depositbus.KindRefund:
		return "Refund"
	case depositbus.KindDispute:
		return "Dispute"
	default:
		return "Other"
	}
}

// nameOf is who placed an order: the first answer that reads like a name,
// then the address, then a placeholder rather than an empty cell.
//
// The same rule the will-call table uses, and a copy of it rather than a
// shared function, because an app never imports another app and a rule this
// small about how a page labels a row does not belong in the business layer.
func nameOf(sub submissionbus.Submission) string {
	if sub.ID.Zero() {
		return ""
	}

	for _, ans := range sub.Answers.Fields {
		if !ans.Kind.Textual() || ans.Kind == formbus.KindEmail || ans.Kind == formbus.KindTel {
			continue
		}

		if v := strings.TrimSpace(strings.Join(ans.Values, " ")); v != "" {
			return v
		}
	}

	if sub.Email.String() != "" {
		return sub.Email.String()
	}

	return "somebody"
}

package muxer_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/submission/submissionbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// allOf is every submission a form has stored and not hidden, paid or not,
// newest first.
//
// ByForm is the settled ones alone, because that is what every page reads; a
// test asking "was this stored" means stored whether or not anybody has paid
// yet, and most submissions in these tests are orders nobody has.
func allOf(ctx context.Context, subs *submissionbus.Business, form types.Slug) ([]submissionbus.Submission, error) {
	settled, err := subs.ByForm(ctx, form)
	if err != nil {
		return nil, err
	}

	unsettled, err := subs.Unsettled(ctx, form)
	if err != nil {
		return nil, err
	}

	out := append(settled, unsettled...)
	slices.SortStableFunc(out, func(x, y submissionbus.Submission) int { return y.CreatedAt.Compare(x.CreatedAt) })

	return out, nil
}

// payFor marks every order on a form paid, as Stripe's webhook would.
//
// For the tests about reading results rather than about money: what a results
// reader is shown is the orders that were paid for, and an order posted
// through the public surface is not, until something says so.
func (o office) payFor(t *testing.T, form string) {
	t.Helper()

	subs, ok := o.cfg.Submissions.(*submissionbus.Business)
	if !ok {
		t.Fatalf("the admin surface reads %T, not the submission domain", o.cfg.Submissions)
	}

	open, err := subs.Unsettled(t.Context(), mustSlug(t, form))
	if err != nil {
		t.Fatalf("Unsettled: %v", err)
	}

	for _, s := range open {
		if err := subs.Settle(t.Context(), time.Now(), s.ID, submissionbus.StatusPaid, "cs_test_"+s.ID.String()); err != nil {
			t.Fatalf("Settle: %v", err)
		}
	}
}

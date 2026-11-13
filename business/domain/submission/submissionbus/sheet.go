package submissionbus

import (
	"context"
	"fmt"
	"slices"

	"github.com/jroedel/dropin-forms/business/types"
)

// ForSheet is every submission a form has, oldest first, and which of them
// are hidden: what a spreadsheet kept for somebody is built from.
//
// Every one rather than ByForm's settled few, because a sheet does not drop a
// row once it has one. Somebody who was in it and is then hidden, declined,
// cancelled or whose offered place lapsed stays, with the Status cell saying
// so -- the office's own notes typed beside that row are theirs, and a row
// that vanished would take them with it. Which submissions are worth a new
// row is the sheet's decision, made from this.
func (b *Business) ForSheet(ctx context.Context, form types.Slug) ([]Submission, map[types.ID]bool, error) {
	shown, err := b.store.ByForm(ctx, form)
	if err != nil {
		return nil, nil, fmt.Errorf("listing the submissions: %w", err)
	}

	hidden, err := b.store.Hidden(ctx, form)
	if err != nil {
		return nil, nil, fmt.Errorf("listing the hidden submissions: %w", err)
	}

	gone := make(map[types.ID]bool, len(hidden))
	for _, s := range hidden {
		gone[s.ID] = true
	}

	all := append(shown, hidden...)

	// Oldest first, which is the order rows are added to a sheet in; ties on
	// the identifier so two in the same millisecond keep one order.
	slices.SortStableFunc(all, func(a, b Submission) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}

		return compareID(a.ID, b.ID)
	})

	return all, gone, nil
}

func compareID(a, b types.ID) int {
	switch x, y := a.String(), b.String(); {
	case x < y:
		return -1
	case x > y:
		return 1
	}

	return 0
}

// Listed says whether a submission is one the office's lists show: settled
// and not declined, or on the waiting list, or offered a place. The rest --
// an order nobody paid for -- was never a submission anybody needed to read.
func (s Submission) Listed() bool {
	switch {
	case s.Status == StatusWaiting || s.Status == StatusOffered:
		return true
	case s.Review == ReviewDeclined:
		return false
	}

	return s.Status.Settled()
}

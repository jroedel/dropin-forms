package sheetbus

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/google"
)

// reconcile reads the sheet as it is now and makes it hold what it should,
// in one batch. Called with the connection's lock held.
func (b *Business) reconcile(ctx context.Context, now time.Time, c Connection, f formbus.Form, w wanted, token string, refill bool) (Connection, error) {
	file, err := b.cfg.Google.FileOf(ctx, token, c.Spreadsheet)
	if err != nil {
		return c, err
	}

	if file.Trashed {
		return c, fmt.Errorf("%w: it is in the bin", google.ErrGone)
	}

	tabs, err := b.cfg.Google.Tabs(ctx, token, c.Spreadsheet)
	if err != nil {
		return c, err
	}

	if _, ok := tabs[c.Sheet]; !ok {
		return b.rebuild(ctx, now, c, f, w, token, tabs, "the tab it was kept in has been deleted")
	}

	tags, err := b.cfg.Google.Tagged(ctx, token, c.Spreadsheet, tagColumn)
	if err != nil {
		return c, err
	}

	// Where each of the form's columns is now. A column somebody copied
	// may carry a copy of the tag: the first is the one used.
	at := map[string]int{}

	for _, t := range tags {
		if t.SheetID != c.Sheet || t.Column < 0 {
			continue
		}

		if _, dup := at[t.Value]; !dup || t.Column < at[t.Value] {
			at[t.Value] = t.Column
		}
	}

	if _, ok := at["ref"]; !ok {
		return b.rebuild(ctx, now, c, f, w, token, tabs, "its Reference column has been deleted, so no row can be matched to a submission")
	}

	keys := make([]string, 0, len(at))
	for k := range at {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	cols := make([]int, len(keys))
	for i, k := range keys {
		cols[i] = at[k]
	}

	read, err := b.cfg.Google.ReadColumns(ctx, token, c.Spreadsheet, c.Sheet, cols)
	if err != nil {
		return c, err
	}

	now2 := map[string][]any{}
	for i, k := range keys {
		now2[k] = read[i]
	}

	plan := planEdits(c.Sheet, f, w, at, now2, refill)

	if len(plan.edits) > 0 {
		if _, err := b.cfg.Google.Apply(ctx, token, c.Spreadsheet, plan.edits); err != nil {
			return c, err
		}
	}

	if err := b.cfg.Store.AddRows(ctx, c.ID, plan.wroteRows); err != nil {
		return c, err
	}

	if err := b.cfg.Store.AddColumns(ctx, c.ID, plan.wroteCols); err != nil {
		return c, err
	}

	for _, note := range plan.notes {
		b.log(ctx, c, now, note)
	}

	return c, nil
}

// plan is what one pass will send, and what it will then have written.
type plan struct {
	edits     []google.Edit
	wroteRows []types.ID
	wroteCols []string
	notes     []string
}

// planEdits works out the batch. Separate from reconcile so a test can read
// the reasoning without a sheet.
//
// Order matters, because Google applies a batch in order and every later
// position is a position after the earlier edits: rows inserted first, then
// columns, then the values.
func planEdits(sheet int64, f formbus.Form, w wanted, at map[string]int, cells map[string][]any, refill bool) plan {
	var p plan

	// Which rows hold which submission, by the Reference column. A cell
	// that is not a reference -- the heading, a row of the office's own --
	// is not ours and is left alone.
	refs := cells["ref"]
	rowsOf := map[types.ID][]int{}
	header := -1

	for r, v := range refs {
		s, _ := v.(string)

		if s == "Reference" && header < 0 {
			header = r
		}

		if id, err := types.ParseID(s); err == nil {
			rowsOf[id] = append(rowsOf[id], r)
		}
	}

	shiftRows := func(from int) {
		for id, rs := range rowsOf {
			for i := range rs {
				if rs[i] >= from {
					rs[i]++
				}
			}

			rowsOf[id] = rs
		}
	}

	if header < 0 {
		// The heading row is gone: somebody deleted it. Put one back at the
		// top rather than writing headings over somebody's data.
		p.edits = append(p.edits, google.InsertRows{Sheet: sheet, At: 0, Count: 1})
		shiftRows(0)
		header = 0

		for k := range cells {
			cells[k] = append([]any{nil}, cells[k]...)
		}

		p.notes = append(p.notes, "The heading row had been deleted; it was put back at the top.")
	}

	// Columns: every one the form has, where it is; a new question's put at
	// the end of the form's block; one deleted by the person left deleted.
	last := -1
	for _, col := range at {
		last = max(last, col)
	}

	var added []string

	for _, col := range w.cols {
		if _, ok := at[col.Key]; ok {
			continue
		}

		if w.had[col.Key] && !refill {
			continue
		}

		last++
		pos := last

		for k, c := range at {
			if c >= pos {
				at[k] = c + 1
			}
		}

		at[col.Key] = pos
		cells[col.Key] = nil

		p.edits = append(p.edits,
			google.InsertColumns{Sheet: sheet, At: pos, Count: 1},
			google.TagColumn{Sheet: sheet, Column: pos, Key: tagColumn, Value: col.Key},
			google.ProtectColumn{Sheet: sheet, Column: pos, Description: protectNote(f)},
		)

		p.wroteCols = append(p.wroteCols, col.Key)

		if w.had[col.Key] {
			added = append(added, col.Label+" (put back)")
		} else if len(w.had) > 0 {
			added = append(added, col.Label)
		}
	}

	if len(added) > 0 {
		p.notes = append(p.notes, "Added columns: "+joinList(added)+".")
	}

	// Columns already there that were never recorded -- a sheet made before
	// the ledger -- are recorded now.
	for _, col := range w.cols {
		if _, ok := at[col.Key]; ok && !w.had[col.Key] && !slices.Contains(p.wroteCols, col.Key) {
			p.wroteCols = append(p.wroteCols, col.Key)
		}
	}

	set := func(row, col int, v google.Value) {
		p.edits = append(p.edits, google.SetCells{Sheet: sheet, Row: row, Column: col, Rows: [][]google.Value{{v}}})
	}

	cellAt := func(key string, row int) any {
		vs := cells[key]
		if row < len(vs) {
			return vs[row]
		}

		return nil
	}

	// Headings: the form's current wording for each question.
	for _, col := range w.cols {
		pos, ok := at[col.Key]
		if !ok {
			continue
		}

		if s, _ := cellAt(col.Key, header).(string); s != col.Label {
			set(header, pos, google.Value{Kind: google.Text, Text: col.Label, Bold: true})
		}
	}

	// Rows: each in place where it is, wherever it has been sorted to; a
	// new one appended; one the person deleted left deleted.
	width := 0
	for _, pos := range at {
		width = max(width, pos+1)
	}

	var appended [][]google.Value

	putBack := 0

	for _, r := range w.rows {
		rows, there := rowsOf[r.id]

		if !there {
			if w.seen[r.id] && !refill {
				continue
			}

			if w.seen[r.id] {
				putBack++
			}

			line := make([]google.Value, width)

			for i, col := range w.cols {
				if pos, ok := at[col.Key]; ok {
					line[pos] = r.cells[i]
				}
			}

			appended = append(appended, line)
			p.wroteRows = append(p.wroteRows, r.id)

			continue
		}

		if !w.seen[r.id] {
			p.wroteRows = append(p.wroteRows, r.id)
		}

		for _, row := range rows {
			for i, col := range w.cols {
				pos, ok := at[col.Key]
				if !ok {
					continue
				}

				if !same(cellAt(col.Key, row), r.cells[i]) {
					set(row, pos, r.cells[i])
				}
			}
		}
	}

	if putBack > 0 {
		p.notes = append(p.notes, fmt.Sprintf("Put back %d deleted row%s.", putBack, plural(putBack)))
	}

	if len(appended) > 0 {
		p.edits = append(p.edits, google.AppendRows{Sheet: sheet, Rows: appended})
	}

	return p
}

// rebuild is a sheet beyond use: a new tab beside the old, filled from
// scratch, and the person told. The old tab is not touched.
func (b *Business) rebuild(ctx context.Context, now time.Time, c Connection, f formbus.Form, w wanted, token string, tabs map[int64]string, why string) (Connection, error) {
	title := fmt.Sprintf("%s (rebuilt %s)", tabTitle, now.In(b.cfg.Zone).Format("2 Jan"))

	taken := map[string]bool{}
	for _, t := range tabs {
		taken[t] = true
	}

	for n := 2; taken[title]; n++ {
		title = fmt.Sprintf("%s (rebuilt %s, %d)", tabTitle, now.In(b.cfg.Zone).Format("2 Jan"), n)
	}

	ids, err := b.cfg.Google.Apply(ctx, token, c.Spreadsheet, []google.Edit{google.AddTab{Title: title}})
	if err != nil {
		return c, err
	}

	if len(ids) != 1 {
		return c, errors.New("google added a tab and did not say which")
	}

	c.Sheet = ids[0]

	if err := b.cfg.Store.Forget(ctx, c.ID); err != nil {
		return c, err
	}

	// A fresh tab: every row the sheet should hold, the deleted ones too --
	// what was deleted was deleted from the tab that is no longer in use.
	w.seen, w.had = map[types.ID]bool{}, map[string]bool{}

	if err := b.fill(ctx, c, f, w, token); err != nil {
		return c, err
	}

	if err := b.cfg.Store.Save(ctx, c); err != nil {
		return c, err
	}

	sentence := fmt.Sprintf("The sheet could not be kept up to date where it was: %s. A new tab, %q, has been added beside it and filled in; the old tab has not been changed.", why, title)

	b.log(ctx, c, now, sentence)
	b.tell(ctx, c, "We rebuilt your Google Sheet's tab", sentence+
		"\n\nAnything you typed into the old tab is still there. Copy what you need across, then delete the old tab if you like.")

	return c, nil
}

// fill sets up an empty tab: the form's columns tagged and protected, the
// headings, and every row.
func (b *Business) fill(ctx context.Context, c Connection, f formbus.Form, w wanted, token string) error {
	edits := []google.Edit{google.TagTab{Sheet: c.Sheet, Key: tagTab, Value: f.ID.String()}}

	head := make([]google.Value, len(w.cols))
	keys := make([]string, len(w.cols))

	for i, col := range w.cols {
		keys[i] = col.Key
		head[i] = google.Value{Kind: google.Text, Text: col.Label, Bold: true}

		edits = append(edits,
			google.TagColumn{Sheet: c.Sheet, Column: i, Key: tagColumn, Value: col.Key},
			google.ProtectColumn{Sheet: c.Sheet, Column: i, Description: protectNote(f)},
		)
	}

	block := [][]google.Value{head}
	ids := make([]types.ID, 0, len(w.rows))

	for _, r := range w.rows {
		block = append(block, r.cells)
		ids = append(ids, r.id)
	}

	edits = append(edits, google.SetCells{Sheet: c.Sheet, Row: 0, Column: 0, Rows: block})

	if _, err := b.cfg.Google.Apply(ctx, token, c.Spreadsheet, edits); err != nil {
		return err
	}

	if err := b.cfg.Store.AddColumns(ctx, c.ID, keys); err != nil {
		return err
	}

	return b.cfg.Store.AddRows(ctx, c.ID, ids)
}

func protectNote(f formbus.Form) string {
	return "Filled in by the form “" + f.Title + "”. A change here is replaced at the next update; add your own columns beside these instead."
}

func plural(n int) string {
	if n == 1 {
		return ""
	}

	return "s"
}

func joinList(s []string) string {
	switch len(s) {
	case 0:
		return ""
	case 1:
		return s[0]
	}

	out := ""
	for i, v := range s {
		switch {
		case i == 0:
			out = v
		case i == len(s)-1:
			out += " and " + v
		default:
			out += ", " + v
		}
	}

	return out
}

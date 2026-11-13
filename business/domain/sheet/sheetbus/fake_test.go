package sheetbus_test

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/jroedel/dropin-forms/foundation/google"
)

// fakeGoogle is Google's side, in memory: spreadsheets as grids of cells with
// tags on tabs and columns that move with the columns, the way Google's
// developer metadata does. What people do to a sheet -- sort it, delete a
// column, delete the tab -- is done to it directly by the damage helpers.
type fakeGoogle struct {
	mu sync.Mutex

	books   map[string]*book
	revoked map[string]bool
	busy    error

	grant google.Grant

	applied int // batch updates sent
	asked   int // every call that reaches Google
}

type book struct {
	title   string
	trashed bool
	tabs    map[int64]*tab
	nextTab int64
}

type tab struct {
	title     string
	rows      [][]any
	colTags   []map[string]string
	tabTags   map[string]string
	protected int
}

func newFakeGoogle(email string) *fakeGoogle {
	return &fakeGoogle{
		books:   map[string]*book{},
		revoked: map[string]bool{},
		grant:   google.Grant{Refresh: "1//refresh", Access: "access", Email: email, Files: true},
	}
}

func (g *fakeGoogle) enter() error {
	g.mu.Lock()
	g.asked++

	return g.busy
}

func (g *fakeGoogle) AuthURL(state, verifier, hint string) string {
	return "https://google.test/auth?state=" + state
}

func (g *fakeGoogle) Exchange(_ context.Context, now time.Time, code, verifier string) (google.Grant, error) {
	defer g.mu.Unlock()
	if err := g.enter(); err != nil {
		return google.Grant{}, err
	}

	out := g.grant
	out.Expires = now.Add(time.Hour)

	return out, nil
}

func (g *fakeGoogle) Refresh(_ context.Context, now time.Time, refresh string) (google.Access, error) {
	defer g.mu.Unlock()
	if err := g.enter(); err != nil {
		return google.Access{}, err
	}

	if g.revoked[refresh] {
		return google.Access{}, fmt.Errorf("%w: invalid_grant", google.ErrRevoked)
	}

	return google.Access{Token: "access", Expires: now.Add(time.Hour)}, nil
}

func (g *fakeGoogle) Revoke(_ context.Context, token string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.revoked[token] = true

	return nil
}

func (g *fakeGoogle) Create(_ context.Context, token, title, tabTitle, locale, zone string) (google.Created, error) {
	defer g.mu.Unlock()
	if err := g.enter(); err != nil {
		return google.Created{}, err
	}

	id := fmt.Sprintf("ss%d", len(g.books)+1)
	g.books[id] = &book{title: title, tabs: map[int64]*tab{0: {title: tabTitle, tabTags: map[string]string{}}}, nextTab: 1}

	return google.Created{SpreadsheetID: id, SheetID: 0, URL: "https://docs.google.test/" + id}, nil
}

func (g *fakeGoogle) book(id string) (*book, error) {
	b, ok := g.books[id]
	if !ok {
		return nil, fmt.Errorf("%w: no spreadsheet %s", google.ErrGone, id)
	}

	return b, nil
}

func (g *fakeGoogle) Tabs(_ context.Context, token, id string) (map[int64]string, error) {
	defer g.mu.Unlock()
	if err := g.enter(); err != nil {
		return nil, err
	}

	b, err := g.book(id)
	if err != nil {
		return nil, err
	}

	out := map[int64]string{}
	for k, t := range b.tabs {
		out[k] = t.title
	}

	return out, nil
}

func (g *fakeGoogle) Tagged(_ context.Context, token, id, key string) ([]google.Tag, error) {
	defer g.mu.Unlock()
	if err := g.enter(); err != nil {
		return nil, err
	}

	b, err := g.book(id)
	if err != nil {
		return nil, err
	}

	var out []google.Tag

	for sid, t := range b.tabs {
		if v, ok := t.tabTags[key]; ok {
			out = append(out, google.Tag{Value: v, SheetID: sid, Column: -1})
		}

		for col, tags := range t.colTags {
			if v, ok := tags[key]; ok {
				out = append(out, google.Tag{Value: v, SheetID: sid, Column: col})
			}
		}
	}

	return out, nil
}

func (g *fakeGoogle) ReadColumns(_ context.Context, token, id string, sheet int64, cols []int) ([][]any, error) {
	defer g.mu.Unlock()
	if err := g.enter(); err != nil {
		return nil, err
	}

	b, err := g.book(id)
	if err != nil {
		return nil, err
	}

	t := b.tabs[sheet]
	out := make([][]any, len(cols))

	for i, c := range cols {
		var col []any

		for _, row := range t.rows {
			var v any
			if c < len(row) {
				v = row[c]
			}

			if v == nil {
				v = ""
			}

			col = append(col, v)
		}

		// Google leaves trailing empty cells off.
		for len(col) > 0 && col[len(col)-1] == "" {
			col = col[:len(col)-1]
		}

		out[i] = col
	}

	return out, nil
}

func (g *fakeGoogle) FileOf(_ context.Context, token, id string) (google.File, error) {
	defer g.mu.Unlock()
	if err := g.enter(); err != nil {
		return google.File{}, err
	}

	b, err := g.book(id)
	if err != nil {
		return google.File{}, err
	}

	return google.File{Trashed: b.trashed, Link: "https://docs.google.test/" + id}, nil
}

func (g *fakeGoogle) Apply(_ context.Context, token, id string, edits []google.Edit) ([]int64, error) {
	defer g.mu.Unlock()
	if err := g.enter(); err != nil {
		return nil, err
	}

	b, err := g.book(id)
	if err != nil {
		return nil, err
	}

	g.applied++

	var tabs []int64

	for _, e := range edits {
		switch e := e.(type) {
		case google.AddTab:
			sid := b.nextTab
			b.nextTab++
			b.tabs[sid] = &tab{title: e.Title, tabTags: map[string]string{}}
			tabs = append(tabs, sid)

		case google.TagTab:
			b.tabs[e.Sheet].tabTags[e.Key] = e.Value

		case google.TagColumn:
			t := b.tabs[e.Sheet]
			t.widen(e.Column + 1)
			t.colTags[e.Column][e.Key] = e.Value

		case google.InsertColumns:
			b.tabs[e.Sheet].insertColumn(e.At)

		case google.InsertRows:
			t := b.tabs[e.Sheet]
			t.rows = slices.Insert(t.rows, min(e.At, len(t.rows)), []any{})

		case google.SetCells:
			t := b.tabs[e.Sheet]
			for r, row := range e.Rows {
				for c, v := range row {
					t.set(e.Row+r, e.Column+c, cellValue(v))
				}
			}

		case google.AppendRows:
			t := b.tabs[e.Sheet]
			at := t.lastUsed() + 1
			for r, row := range e.Rows {
				for c, v := range row {
					t.set(at+r, c, cellValue(v))
				}
			}

		case google.ProtectColumn:
			b.tabs[e.Sheet].protected++

		default:
			return nil, fmt.Errorf("the fake does not know %T", e)
		}
	}

	return tabs, nil
}

func cellValue(v google.Value) any {
	switch v.Kind {
	case google.Text:
		return v.Text
	case google.Number, google.Money:
		return v.Number
	case google.Date:
		return google.Serial(v.Time)
	}

	return nil
}

func (t *tab) widen(n int) {
	for len(t.colTags) < n {
		t.colTags = append(t.colTags, map[string]string{})
	}
}

func (t *tab) set(r, c int, v any) {
	for len(t.rows) <= r {
		t.rows = append(t.rows, []any{})
	}

	for len(t.rows[r]) <= c {
		t.rows[r] = append(t.rows[r], nil)
	}

	t.rows[r][c] = v
}

func (t *tab) lastUsed() int {
	last := -1

	for r, row := range t.rows {
		for _, v := range row {
			if v != nil && v != "" {
				last = r
			}
		}
	}

	return last
}

func (t *tab) insertColumn(at int) {
	t.widen(at)
	t.colTags = slices.Insert(t.colTags, at, map[string]string{})

	for r, row := range t.rows {
		if at <= len(row) {
			t.rows[r] = slices.Insert(row, at, any(nil))
		}
	}
}

// --- what people do to sheets ---------------------------------------------------

func (g *fakeGoogle) only(t interface{ Fatalf(string, ...any) }) (*book, *tab) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if len(g.books) != 1 {
		t.Fatalf("%d spreadsheets, want 1", len(g.books))
	}

	for _, b := range g.books {
		var ids []int64
		for k := range b.tabs {
			ids = append(ids, k)
		}

		slices.Sort(ids)

		return b, b.tabs[ids[len(ids)-1]]
	}

	return nil, nil
}

// column finds a tagged column's index.
func (t *tab) column(key string) int {
	for i, tags := range t.colTags {
		if tags["dropin-forms.column"] == key {
			return i
		}
	}

	return -1
}

// rowOf finds the row holding a reference.
func (t *tab) rowOf(ref string) int {
	c := t.column("ref")

	for r, row := range t.rows {
		if c < len(row) && row[c] == ref {
			return r
		}
	}

	return -1
}

func (t *tab) cell(r, c int) any {
	if r < 0 || r >= len(t.rows) || c < 0 || c >= len(t.rows[r]) {
		return nil
	}

	return t.rows[r][c]
}

// sortBelowHeader sorts the data rows by a column, descending, as somebody
// pressing Sort Z→A would.
func (t *tab) sortBelowHeader(col int) {
	data := t.rows[1:]

	sort.SliceStable(data, func(i, j int) bool {
		return fmt.Sprint(cellOr(data[i], col)) > fmt.Sprint(cellOr(data[j], col))
	})
}

func cellOr(row []any, c int) any {
	if c < len(row) {
		return row[c]
	}

	return nil
}

func (t *tab) deleteColumn(c int) {
	t.colTags = slices.Delete(t.colTags, c, c+1)

	for r, row := range t.rows {
		if c < len(row) {
			t.rows[r] = slices.Delete(row, c, c+1)
		}
	}
}

func (t *tab) deleteRow(r int) {
	t.rows = slices.Delete(t.rows, r, r+1)
}

// moveColumn drags a column, tag and all, to a new position.
func (t *tab) moveColumn(from, to int) {
	width := len(t.colTags)
	for _, row := range t.rows {
		width = max(width, len(row))
	}

	t.widen(width)

	for r := range t.rows {
		for len(t.rows[r]) < width {
			t.rows[r] = append(t.rows[r], nil)
		}

		v := t.rows[r][from]
		t.rows[r] = slices.Insert(slices.Delete(t.rows[r], from, from+1), to, v)
	}

	tags := t.colTags[from]
	t.colTags = slices.Insert(slices.Delete(t.colTags, from, from+1), to, tags)
}

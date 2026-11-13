package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Spreadsheets: making one, finding the tabs and tagged columns in it, reading
// columns, and changing cells, all through the Sheets API v4 and one Drive
// call.
//
// Tagged means developer metadata, which is the thing that makes a sheet
// somebody has rearranged still readable: a key and value attached to a tab or
// a column, invisible to people, that Google moves along with the column when
// a column is inserted before it, dragged elsewhere or the tab is renamed.
// PROJECT visibility, so only this app's requests see or match it.

// Created is a spreadsheet just made.
type Created struct {
	SpreadsheetID string
	SheetID       int64
	URL           string
}

// Create makes a spreadsheet in the person's Drive with one tab, its first
// row frozen. spreadsheets.create.
func (c *Client) Create(ctx context.Context, token, title, tab, locale, timeZone string) (Created, error) {
	body := map[string]any{
		"properties": map[string]any{"title": title, "locale": locale, "timeZone": timeZone},
		"sheets": []any{map[string]any{"properties": map[string]any{
			"title":          tab,
			"gridProperties": map[string]any{"frozenRowCount": 1},
		}}},
	}

	var r struct {
		SpreadsheetID  string `json:"spreadsheetId"`
		SpreadsheetURL string `json:"spreadsheetUrl"`
		Sheets         []struct {
			Properties struct {
				SheetID int64 `json:"sheetId"`
			} `json:"properties"`
		} `json:"sheets"`
	}

	if err := c.call(ctx, http.MethodPost, c.cfg.SheetsURL, token, body, &r); err != nil {
		return Created{}, err
	}

	if r.SpreadsheetID == "" || len(r.Sheets) == 0 {
		return Created{}, errors.New("google made a spreadsheet and did not say which")
	}

	return Created{SpreadsheetID: r.SpreadsheetID, SheetID: r.Sheets[0].Properties.SheetID, URL: r.SpreadsheetURL}, nil
}

// Tabs lists a spreadsheet's tabs, by id, with their titles.
// spreadsheets.get with a field mask.
func (c *Client) Tabs(ctx context.Context, token, spreadsheet string) (map[int64]string, error) {
	var r struct {
		Sheets []struct {
			Properties struct {
				SheetID int64  `json:"sheetId"`
				Title   string `json:"title"`
			} `json:"properties"`
		} `json:"sheets"`
	}

	target := c.cfg.SheetsURL + "/" + url.PathEscape(spreadsheet) + "?fields=" + url.QueryEscape("sheets.properties(sheetId,title)")

	if err := c.call(ctx, http.MethodGet, target, token, nil, &r); err != nil {
		return nil, err
	}

	out := make(map[int64]string, len(r.Sheets))
	for _, s := range r.Sheets {
		out[s.Properties.SheetID] = s.Properties.Title
	}

	return out, nil
}

// Tag is one piece of developer metadata found, and where it is now.
type Tag struct {
	Value   string
	SheetID int64

	// Column is the column's index now, counting from 0, or -1 for a tag on
	// a whole tab.
	Column int
}

// Tagged finds every tag with this key in a spreadsheet.
// spreadsheets.developerMetadata.search.
func (c *Client) Tagged(ctx context.Context, token, spreadsheet, key string) ([]Tag, error) {
	body := map[string]any{"dataFilters": []any{map[string]any{
		"developerMetadataLookup": map[string]any{"metadataKey": key},
	}}}

	var r struct {
		Matched []struct {
			Meta struct {
				Value    string `json:"metadataValue"`
				Location struct {
					SheetID *int64 `json:"sheetId"`
					Range   *struct {
						SheetID    int64  `json:"sheetId"`
						Dimension  string `json:"dimension"`
						StartIndex int    `json:"startIndex"`
					} `json:"dimensionRange"`
				} `json:"location"`
			} `json:"developerMetadata"`
		} `json:"matchedDeveloperMetadata"`
	}

	target := c.cfg.SheetsURL + "/" + url.PathEscape(spreadsheet) + "/developerMetadata:search"

	if err := c.call(ctx, http.MethodPost, target, token, body, &r); err != nil {
		return nil, err
	}

	var out []Tag

	for _, m := range r.Matched {
		loc := m.Meta.Location

		switch {
		case loc.Range != nil && loc.Range.Dimension == "COLUMNS":
			out = append(out, Tag{Value: m.Meta.Value, SheetID: loc.Range.SheetID, Column: loc.Range.StartIndex})
		case loc.SheetID != nil:
			out = append(out, Tag{Value: m.Meta.Value, SheetID: *loc.SheetID, Column: -1})
		}
	}

	return out, nil
}

// ReadColumns reads whole columns of one tab, as values: a string, a float64
// (numbers, and dates as serial numbers) or a bool for each cell, from the
// first row, with trailing empty cells left off. One slice per column asked
// for, in the order asked. spreadsheets.values.batchGetByDataFilter.
func (c *Client) ReadColumns(ctx context.Context, token, spreadsheet string, sheet int64, columns []int) ([][]any, error) {
	if len(columns) == 0 {
		return nil, nil
	}

	filters := make([]any, len(columns))
	for i, col := range columns {
		filters[i] = map[string]any{"gridRange": map[string]any{
			"sheetId": sheet, "startColumnIndex": col, "endColumnIndex": col + 1,
		}}
	}

	body := map[string]any{
		"dataFilters":          filters,
		"majorDimension":       "COLUMNS",
		"valueRenderOption":    "UNFORMATTED_VALUE",
		"dateTimeRenderOption": "SERIAL_NUMBER",
	}

	var r struct {
		Ranges []struct {
			ValueRange struct {
				Values [][]any `json:"values"`
			} `json:"valueRange"`
		} `json:"valueRanges"`
	}

	target := c.cfg.SheetsURL + "/" + url.PathEscape(spreadsheet) + "/values:batchGetByDataFilter"

	if err := c.call(ctx, http.MethodPost, target, token, body, &r); err != nil {
		return nil, err
	}

	if len(r.Ranges) != len(columns) {
		return nil, fmt.Errorf("google answered %d columns for %d asked", len(r.Ranges), len(columns))
	}

	out := make([][]any, len(columns))
	for i, vr := range r.Ranges {
		if len(vr.ValueRange.Values) > 0 {
			out[i] = vr.ValueRange.Values[0]
		}
	}

	return out, nil
}

// File is what Drive says about a spreadsheet.
type File struct {
	Trashed bool
	Link    string
}

// FileOf asks Drive whether the spreadsheet is in the bin. files.get.
func (c *Client) FileOf(ctx context.Context, token, spreadsheet string) (File, error) {
	var r struct {
		Trashed bool   `json:"trashed"`
		Link    string `json:"webViewLink"`
	}

	target := c.cfg.DriveURL + "/" + url.PathEscape(spreadsheet) + "?fields=trashed,webViewLink"

	if err := c.call(ctx, http.MethodGet, target, token, nil, &r); err != nil {
		return File{}, err
	}

	return File{Trashed: r.Trashed, Link: r.Link}, nil
}

// --- changing cells -----------------------------------------------------------

// Edit is one request in a batch update.
type Edit interface {
	request() map[string]any
}

// Apply sends edits as one batch, which Google applies in order and all or
// none. spreadsheets.batchUpdate. The tab id of each AddTab, in order, is
// returned.
func (c *Client) Apply(ctx context.Context, token, spreadsheet string, edits []Edit) ([]int64, error) {
	if len(edits) == 0 {
		return nil, nil
	}

	reqs := make([]any, len(edits))
	for i, e := range edits {
		reqs[i] = e.request()
	}

	var r struct {
		Replies []struct {
			AddSheet *struct {
				Properties struct {
					SheetID int64 `json:"sheetId"`
				} `json:"properties"`
			} `json:"addSheet"`
		} `json:"replies"`
	}

	target := c.cfg.SheetsURL + "/" + url.PathEscape(spreadsheet) + ":batchUpdate"

	if err := c.call(ctx, http.MethodPost, target, token, map[string]any{"requests": reqs}, &r); err != nil {
		return nil, err
	}

	var tabs []int64

	for _, rep := range r.Replies {
		if rep.AddSheet != nil {
			tabs = append(tabs, rep.AddSheet.Properties.SheetID)
		}
	}

	return tabs, nil
}

// AddTab adds a tab with its first row frozen.
type AddTab struct{ Title string }

func (e AddTab) request() map[string]any {
	return map[string]any{"addSheet": map[string]any{"properties": map[string]any{
		"title":          e.Title,
		"gridProperties": map[string]any{"frozenRowCount": 1},
	}}}
}

// TagTab tags a whole tab.
type TagTab struct {
	Sheet      int64
	Key, Value string
}

func (e TagTab) request() map[string]any {
	return createMeta(e.Key, e.Value, map[string]any{"sheetId": e.Sheet})
}

// TagColumn tags one column.
type TagColumn struct {
	Sheet      int64
	Column     int
	Key, Value string
}

func (e TagColumn) request() map[string]any {
	return createMeta(e.Key, e.Value, map[string]any{"dimensionRange": map[string]any{
		"sheetId": e.Sheet, "dimension": "COLUMNS", "startIndex": e.Column, "endIndex": e.Column + 1,
	}})
}

func createMeta(key, value string, location map[string]any) map[string]any {
	return map[string]any{"createDeveloperMetadata": map[string]any{"developerMetadata": map[string]any{
		"metadataKey":   key,
		"metadataValue": value,
		"location":      location,
		"visibility":    "PROJECT",
	}}}
}

// InsertColumns inserts empty columns before At, moving everything from there
// to the right -- tags, protection and all.
type InsertColumns struct {
	Sheet     int64
	At, Count int
}

func (e InsertColumns) request() map[string]any {
	return insertDim(e.Sheet, "COLUMNS", e.At, e.Count)
}

// InsertRows inserts empty rows before At.
type InsertRows struct {
	Sheet     int64
	At, Count int
}

func (e InsertRows) request() map[string]any {
	return insertDim(e.Sheet, "ROWS", e.At, e.Count)
}

func insertDim(sheet int64, dim string, at, count int) map[string]any {
	return map[string]any{"insertDimension": map[string]any{
		"range": map[string]any{"sheetId": sheet, "dimension": dim, "startIndex": at, "endIndex": at + count},
		// Not inheriting from before: a new column next to a date column
		// would otherwise come formatted as dates.
		"inheritFromBefore": false,
	}}
}

// SetCells writes a block of cells starting at Row, Column. Each cell's value
// and number format are replaced; an Empty value clears both.
type SetCells struct {
	Sheet       int64
	Row, Column int
	Rows        [][]Value
}

func (e SetCells) request() map[string]any {
	return map[string]any{"updateCells": map[string]any{
		"rows":   rowsOf(e.Rows),
		"fields": cellFields,
		"start":  map[string]any{"sheetId": e.Sheet, "rowIndex": e.Row, "columnIndex": e.Column},
	}}
}

// AppendRows adds rows after the last row that has anything in it, in any
// column. Each row starts at the first column.
type AppendRows struct {
	Sheet int64
	Rows  [][]Value
}

func (e AppendRows) request() map[string]any {
	return map[string]any{"appendCells": map[string]any{
		"sheetId": e.Sheet,
		"rows":    rowsOf(e.Rows),
		"fields":  cellFields,
	}}
}

// ProtectColumn marks a column as one that warns anybody editing it. Not a
// lock: Google shows "you are editing a protected range" with the
// description, and lets them carry on.
type ProtectColumn struct {
	Sheet       int64
	Column      int
	Description string
}

func (e ProtectColumn) request() map[string]any {
	return map[string]any{"addProtectedRange": map[string]any{"protectedRange": map[string]any{
		"range":       map[string]any{"sheetId": e.Sheet, "startColumnIndex": e.Column, "endColumnIndex": e.Column + 1},
		"description": e.Description,
		"warningOnly": true,
	}}}
}

// cellFields is what a write replaces: the value, its number format, whether
// it is bold and where it links -- so that clearing a cell that held a date
// leaves no date format behind for whatever is typed there next, and a link
// goes with the value it was on.
const cellFields = "userEnteredValue,userEnteredFormat.numberFormat,userEnteredFormat.textFormat.bold,userEnteredFormat.textFormat.link"

// ValueKind says what a Value holds.
type ValueKind int

const (
	Empty ValueKind = iota
	Text
	Number
	Money
	Date
)

// Value is one cell's content.
type Value struct {
	Kind   ValueKind
	Text   string
	Number float64

	// Time is a Date's moment, written as the wall-clock time it reads in its
	// own location: the caller puts it in the spreadsheet's time zone.
	Time time.Time

	Bold bool

	// Link makes the whole cell a link to this address. It is a property of
	// the cell's format rather than a HYPERLINK formula, so the value stays
	// plain text: what is read back is what was written, and no formula is
	// ever put in somebody's spreadsheet.
	Link string
}

func rowsOf(rows [][]Value) []any {
	out := make([]any, len(rows))

	for i, row := range rows {
		cells := make([]any, len(row))
		for j, v := range row {
			cells[j] = cellOf(v)
		}

		out[i] = map[string]any{"values": cells}
	}

	return out
}

// cellOf is a CellData. Typed values -- stringValue, numberValue -- rather
// than text for Google to interpret, which is what keeps an answer that
// begins with "=" a piece of text rather than a formula that runs in the
// office's spreadsheet.
func cellOf(v Value) map[string]any {
	cell := map[string]any{}
	format := map[string]any{}

	switch v.Kind {
	case Text:
		cell["userEnteredValue"] = map[string]any{"stringValue": v.Text}
	case Number:
		cell["userEnteredValue"] = map[string]any{"numberValue": v.Number}
	case Money:
		cell["userEnteredValue"] = map[string]any{"numberValue": v.Number}
		format["numberFormat"] = map[string]any{"type": "NUMBER", "pattern": "#,##0.00"}
	case Date:
		cell["userEnteredValue"] = map[string]any{"numberValue": Serial(v.Time)}
		format["numberFormat"] = map[string]any{"type": "DATE_TIME", "pattern": "yyyy-mm-dd hh:mm"}
	}

	text := map[string]any{}

	if v.Bold {
		text["bold"] = true
	}

	if v.Link != "" {
		text["link"] = map[string]any{"uri": v.Link}
	}

	if len(text) > 0 {
		format["textFormat"] = text
	}

	if len(format) > 0 {
		cell["userEnteredFormat"] = format
	}

	return cell
}

// Serial is a time as a spreadsheet date: days since 30 December 1899, with
// the time of day as the fraction, read from t's own wall clock.
func Serial(t time.Time) float64 {
	wall := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
	epoch := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)

	return wall.Sub(epoch).Hours() / 24
}

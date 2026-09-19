package formdb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
	"github.com/jroedel/dropin-forms/business/types"
)

// definition is the stored JSON shape of a form.
//
// Its field names are a compatibility surface -- see the package comment --
// and it carries neither an id nor a version: the first is the row's primary
// key and the second is derived from the content by Stamp.
type definition struct {
	Title string `json:"title"`
	Intro string `json:"intro,omitempty"`

	OpensAt    time.Time `json:"opens_at,omitzero"`
	ClosesAt   time.Time `json:"closes_at,omitzero"`
	ClosedNote string    `json:"closed_note,omitempty"`

	Currency string   `json:"currency"`
	Origins  []string `json:"origins,omitempty"`

	ReturnURL string `json:"return_url,omitempty"`

	MinPerOrder int `json:"min_per_order,omitempty"`
	MaxPerOrder int `json:"max_per_order,omitempty"`

	// Whole minor units, so 500 is five dollars. See the package comment on
	// why this is a number here and a string in the TOML store.
	MinTotal int64 `json:"min_total,omitempty"`
	MaxTotal int64 `json:"max_total,omitempty"`

	PaymentRequired bool   `json:"payment_required,omitempty"`
	PaymentNote     string `json:"payment_note,omitempty"`

	DailyCap int `json:"daily_cap,omitempty"`

	Confirmation string   `json:"confirmation,omitempty"`
	Notify       []string `json:"notify,omitempty"`

	Fields []field `json:"fields,omitzero"`
	Items  []item  `json:"items,omitzero"`
}

type field struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Kind  string `json:"kind"`

	Required bool `json:"required,omitempty"`

	Help         string `json:"help,omitempty"`
	Placeholder  string `json:"placeholder,omitempty"`
	Autocomplete string `json:"autocomplete,omitempty"`

	MinLen int `json:"min_length,omitempty"`
	MaxLen int `json:"max_length,omitempty"`

	// Pointers, because a bound of zero and no bound at all are different
	// rules -- formbus.Field.Min says so for a donation with a floor. omitempty
	// on a pointer omits only nil, which is exactly the distinction wanted.
	Min *int64 `json:"min,omitempty"`
	Max *int64 `json:"max,omitempty"`

	Pattern     string `json:"pattern,omitempty"`
	PatternNote string `json:"pattern_note,omitempty"`

	Options []option `json:"options,omitzero"`

	ShowIf *condition `json:"show_if,omitempty"`
}

type option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type condition struct {
	Field string   `json:"field"`
	Is    []string `json:"is"`
}

type item struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Note  string `json:"note,omitempty"`
	Price int64  `json:"price"`
	Max   int    `json:"max,omitempty"`
}

// encode turns a definition into the bytes that go in the column.
func encode(f formbus.Form) (string, error) {
	w := definition{
		Title:           f.Title,
		Intro:           f.Intro,
		OpensAt:         f.OpensAt,
		ClosesAt:        f.ClosesAt,
		ClosedNote:      f.ClosedNote,
		Currency:        f.Currency,
		ReturnURL:       f.ReturnURL,
		MinPerOrder:     f.MinPerOrder,
		MaxPerOrder:     f.MaxPerOrder,
		MinTotal:        int64(f.MinTotal),
		MaxTotal:        int64(f.MaxTotal),
		PaymentRequired: f.PaymentRequired,
		PaymentNote:     f.PaymentNote,
		DailyCap:        f.DailyCap,
		Confirmation:    f.Confirmation,
		Notify:          f.Notify,
	}

	for _, o := range f.Origins {
		w.Origins = append(w.Origins, o.String())
	}

	for _, fld := range f.Fields {
		wf := field{
			Name:         fld.Name,
			Label:        fld.Label,
			Kind:         string(fld.Kind),
			Required:     fld.Required,
			Help:         fld.Help,
			Placeholder:  fld.Placeholder,
			Autocomplete: fld.Autocomplete,
			MinLen:       fld.MinLen,
			MaxLen:       fld.MaxLen,
			Min:          fld.Min,
			Max:          fld.Max,
			Pattern:      fld.Pattern,
			PatternNote:  fld.PatternNote,
		}

		for _, o := range fld.Options {
			wf.Options = append(wf.Options, option{Value: o.Value, Label: o.Label})
		}

		if fld.ShowIf != nil {
			wf.ShowIf = &condition{Field: fld.ShowIf.Field, Is: fld.ShowIf.Is}
		}

		w.Fields = append(w.Fields, wf)
	}

	for _, it := range f.Items {
		w.Items = append(w.Items, item{
			ID:    it.ID,
			Label: it.Label,
			Note:  it.Note,
			Price: int64(it.Price),
			Max:   it.Max,
		})
	}

	b, err := json.Marshal(w)
	if err != nil {
		return "", fmt.Errorf("the form %s could not be written: %w", f.ID, err)
	}

	return string(b), nil
}

// decode reads one back.
//
// Unknown keys are refused, and that is a decision with a cost worth naming.
// The case it is written for is a rollback: the previous binary reading a
// document the newer one wrote, with an attribute it has never heard of. The
// alternative -- ignore it -- is a rule that quietly does not apply, which is
// the failure formbus exists to make impossible and is far worse than a form
// that visibly stops being served. So the refusal is loud: formbus.refresh
// logs it and drops that one definition, and every other form on the service
// keeps working.
func decode(body string) (formbus.Form, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	dec.DisallowUnknownFields()

	var w definition

	if err := dec.Decode(&w); err != nil {
		return formbus.Form{}, err
	}

	f := formbus.Form{
		Title:           w.Title,
		Intro:           w.Intro,
		OpensAt:         w.OpensAt,
		ClosesAt:        w.ClosesAt,
		ClosedNote:      w.ClosedNote,
		Currency:        w.Currency,
		ReturnURL:       w.ReturnURL,
		MinPerOrder:     w.MinPerOrder,
		MaxPerOrder:     w.MaxPerOrder,
		MinTotal:        types.Money(w.MinTotal),
		MaxTotal:        types.Money(w.MaxTotal),
		PaymentRequired: w.PaymentRequired,
		PaymentNote:     w.PaymentNote,
		DailyCap:        w.DailyCap,
		Confirmation:    w.Confirmation,
		Notify:          w.Notify,
	}

	for _, raw := range w.Origins {
		o, err := types.ParseOrigin(raw)
		if err != nil {
			return formbus.Form{}, fmt.Errorf("origins: %w", err)
		}

		f.Origins = append(f.Origins, o)
	}

	for _, wf := range w.Fields {
		fld := formbus.Field{
			Name:         wf.Name,
			Label:        wf.Label,
			Kind:         formbus.Kind(wf.Kind),
			Required:     wf.Required,
			Help:         wf.Help,
			Placeholder:  wf.Placeholder,
			Autocomplete: wf.Autocomplete,
			MinLen:       wf.MinLen,
			MaxLen:       wf.MaxLen,
			Min:          wf.Min,
			Max:          wf.Max,
			Pattern:      wf.Pattern,
			PatternNote:  wf.PatternNote,
		}

		for _, o := range wf.Options {
			fld.Options = append(fld.Options, formbus.Option{Value: o.Value, Label: o.Label})
		}

		if wf.ShowIf != nil {
			fld.ShowIf = &formbus.Condition{Field: wf.ShowIf.Field, Is: wf.ShowIf.Is}
		}

		f.Fields = append(f.Fields, fld)
	}

	for _, wi := range w.Items {
		f.Items = append(f.Items, formbus.Item{
			ID:    wi.ID,
			Label: wi.Label,
			Note:  wi.Note,
			Price: types.Money(wi.Price),
			Max:   wi.Max,
		})
	}

	return f, nil
}

package formapp

import (
	"net/http"
	"slices"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
)

// itemView is one thing the form sells.
//
// Short, because an item is short: a name, a label, a note, a price and a cap.
// The price lives in the definition and is the only place a price exists --
// the browser never sends one and every total is recomputed from here, which
// is the rule [formbus] is built around.
type itemView struct {
	FormID string
	Live   bool

	// ID is not editable, for the same reason a field's name is not: it is
	// stored on every order already placed, and the quantity input is named
	// after it.
	ID string

	Label string
	Note  string
	Price string
	Max   string

	// Symbol is the currency's sign, for the box the price is typed into.
	Symbol string

	Problems []string
	Problem  string
}

// addItem appends something for sale.
//
// A price is required in the same breath as the name, and an explicit 0.00 is
// how something free is written. formtoml refuses a priceless item with the
// same sentence and for the same reason: an item with no price is far more
// likely to be a forgotten line than a deliberately free thing to order, and
// the cost of guessing wrong is selling tickets for nothing.
func (a app) addItem(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}

	if err := r.ParseForm(); err != nil {
		a.show(w, r, http.StatusBadRequest, s, buildView{
			Problem: "We could not read that. Please try again.",
		})

		return
	}

	it := formbus.Item{
		ID:    value(r, "id"),
		Label: value(r, "label"),
	}

	if _, taken := s.Form.Item(it.ID); taken {
		a.show(w, r, http.StatusConflict, s, buildView{
			Problem: "There is already something called " + it.ID + " for sale on this form.",
		})

		return
	}

	raw := value(r, "price")
	if raw == "" {
		a.show(w, r, http.StatusBadRequest, s, buildView{
			Problem: "Give it a price. Write 0.00 if it really is free.",
		})

		return
	}

	price, err := parseMoney("The price", raw)
	if err != nil {
		a.show(w, r, http.StatusBadRequest, s, buildView{Problem: err.Error()})

		return
	}

	it.Price = price

	f := s.Form
	f.Items = append(slices.Clone(f.Items), it)

	problems, ok := a.save(w, r, f, s.Live)
	if !ok {
		if problems == nil {
			return
		}

		a.show(w, r, http.StatusBadRequest, s, buildView{
			Problem:  "That was not added.",
			Problems: problems,
		})

		return
	}

	http.Redirect(w, r, "/forms/"+f.ID.String()+"/edit/items/"+it.ID, http.StatusSeeOther)
}

// item shows one item's page.
func (a app) item(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}

	it, at := findItem(s.Form, r.PathValue("id"))
	if at < 0 {
		a.notFound(w)

		return
	}

	a.cfg.Render.Render(w, r, http.StatusOK, "build-item", itemOf(s, it))
}

func itemOf(s formbus.Stored, it formbus.Item) itemView {
	v := itemView{
		FormID: s.Form.ID.String(),
		Live:   s.Live,
		ID:     it.ID,
		Label:  it.Label,
		Note:   it.Note,
		Price:  it.Price.String(),
		Max:    blankZero(it.Max),
		Symbol: formbus.Symbol(s.Form.Currency),
	}

	return v
}

// saveItem writes one back.
//
// Changing a price changes the fingerprint, which is the whole mechanism by
// which a tab left open at the old price is re-rendered at the new one rather
// than charged a number nobody agreed to. [formbus.Item] says so: there is no
// price schedule and no effective window, because the version is the
// mechanism.
func (a app) saveItem(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}

	was, at := findItem(s.Form, r.PathValue("id"))
	if at < 0 {
		a.notFound(w)

		return
	}

	if err := r.ParseForm(); err != nil {
		view := itemOf(s, was)
		view.Problem = "We could not read that. Please try again."
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "build-item", view)

		return
	}

	it := formbus.Item{
		ID:    was.ID,
		Label: value(r, "label"),
		Note:  value(r, "note"),
	}

	var problems []string

	price, err := parseMoney("The price", value(r, "price"))
	if err != nil {
		problems = append(problems, err.Error())
	}

	it.Price = price

	it.Max, err = parseCount("The most per order", value(r, "max"))
	if err != nil {
		problems = append(problems, err.Error())
	}

	said := itemOf(s, it)

	if len(problems) > 0 {
		said.Problems = problems
		said.Problem = "Nothing was saved."
		a.cfg.Render.Render(w, r, http.StatusBadRequest, "build-item", said)

		return
	}

	f := s.Form
	f.Items = slices.Clone(f.Items)
	f.Items[at] = it

	problems, ok = a.save(w, r, f, s.Live)
	if !ok {
		if problems == nil {
			return
		}

		said.Problems = problems
		said.Problem = refusal(s.Live)
		a.cfg.Render.Render(w, r, http.StatusConflict, "build-item", said)

		return
	}

	s.Form = f

	a.show(w, r, http.StatusOK, s, buildView{Done: it.Label + " is saved."})
}

// moveItem shifts one up or down. Unlike a field's position this changes
// nothing about what an order means -- there are no conditions between items
// -- but it is in the fingerprint all the same, because the order they are
// listed in is part of the page somebody agreed to.
func (a app) moveItem(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}

	_, at := findItem(s.Form, r.PathValue("id"))
	if at < 0 {
		a.notFound(w)

		return
	}

	if err := r.ParseForm(); err != nil {
		a.show(w, r, http.StatusBadRequest, s, buildView{
			Problem: "We could not read that. Please try again.",
		})

		return
	}

	f := s.Form
	f.Items = slices.Clone(f.Items)

	to, ok := moved(at, len(f.Items), r.PostFormValue("to"))
	if !ok {
		a.show(w, r, http.StatusOK, s, buildView{})

		return
	}

	swap := f.Items[at]
	f.Items = slices.Delete(f.Items, at, at+1)
	f.Items = slices.Insert(f.Items, to, swap)

	problems, ok := a.save(w, r, f, s.Live)
	if !ok {
		if problems == nil {
			return
		}

		a.show(w, r, http.StatusConflict, s, buildView{
			Problem:  "That was not moved.",
			Problems: problems,
		})

		return
	}

	s.Form = f

	a.show(w, r, http.StatusOK, s, buildView{Done: "The order is saved."})
}

// removeItem takes something off sale.
//
// The orders already placed for it keep its id and its price, because those
// are recorded on the submission rather than looked up -- so removing a sold
// item does not change what anybody was charged, and the submissions page can
// still say what they bought.
func (a app) removeItem(w http.ResponseWriter, r *http.Request) {
	s, ok := a.load(w, r)
	if !ok {
		return
	}

	it, at := findItem(s.Form, r.PathValue("id"))
	if at < 0 {
		a.notFound(w)

		return
	}

	f := s.Form
	f.Items = slices.Delete(slices.Clone(f.Items), at, at+1)

	problems, ok := a.save(w, r, f, s.Live)
	if !ok {
		if problems == nil {
			return
		}

		// The refusal this is written for: taking the last item off a form
		// that requires a payment, or one with a minimum order. Check names
		// both, which is what says which setting to change instead.
		a.show(w, r, http.StatusConflict, s, buildView{
			Problem:  "That was not removed.",
			Problems: problems,
		})

		return
	}

	s.Form = f

	a.show(w, r, http.StatusOK, s, buildView{
		Done: it.Label + " is no longer for sale. Orders already placed for it are untouched.",
	})
}

// findItem locates an item by id, returning its position or -1.
func findItem(f formbus.Form, id string) (formbus.Item, int) {
	at := slices.IndexFunc(f.Items, func(c formbus.Item) bool { return c.ID == id })
	if at < 0 {
		return formbus.Item{}, -1
	}

	return f.Items[at], at
}

package formbus_test

import (
	"strings"
	"testing"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
)

func pageant(t *testing.T) formbus.Form {
	t.Helper()

	f := formbus.Form{
		ID: mustSlug(t, "pageant"), Title: "Christmas pageant", Currency: "usd",
		Fields: []formbus.Field{
			{Name: "name", Label: "Your name", Kind: formbus.KindText, Required: true},
			{Name: "slot", Label: "Rehearsal", Kind: formbus.KindRadio, Options: []formbus.Option{
				{Value: "sat", Label: "Saturday", Limit: 10},
				{Value: "sun", Label: "Sunday"},
			}},
		},
		Capacity: 40,
		Waitlist: true,
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("the pageant form does not pass Check: %v", err)
	}

	return f
}

// The form's own place is claimed first, so that a form both full and out of
// the chosen slot is reported as full -- the answer that decides the waiting
// list.
func TestTheFormsOwnPlaceIsClaimedFirst(t *testing.T) {
	f := pageant(t)

	ans, err := f.Validate(now, formbus.Values{"name": {"Maria"}, "slot": {"sat"}})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	claims := f.Claims(ans)

	want := []formbus.Claim{{Limit: 40}, {Field: "slot", Value: "sat", Limit: 10}}
	if len(claims) != len(want) || claims[0] != want[0] || claims[1] != want[1] {
		t.Errorf("Claims = %+v, want %+v", claims, want)
	}

	if !f.Limited() {
		t.Error("a form with a number of places says there is nothing to count")
	}
}

func TestFullAndHeldReadTheWholeFormsCount(t *testing.T) {
	f := pageant(t)

	if f.Full(formbus.Taken{"": {"": 39}}) {
		t.Error("39 of 40 reads as full")
	}

	full := formbus.Taken{"": {"": 40}, "slot": {"sat": 3}}
	if !f.Full(full) || full.Held() != 40 {
		t.Errorf("40 of 40: Full = %v, Held = %d", f.Full(full), full.Held())
	}

	// Somebody already in, changing their answers, does not find the form
	// full to them -- and the count they were handed is left as it was.
	ans, err := f.Validate(now, formbus.Values{"name": {"Maria"}, "slot": {"sat"}})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	mine := full.Without(ans)
	if f.Full(mine) || mine.Count("slot", "sat") != 2 {
		t.Errorf("their own place was not given back: held %d, sat %d", mine.Held(), mine.Count("slot", "sat"))
	}

	if full.Held() != 40 {
		t.Error("Without changed the count it was given")
	}

	f.Capacity = 0
	if f.Full(full) {
		t.Error("a form with no number of places reads as full")
	}
}

func TestCheckRefusesAWaitingListItCannotKeep(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(*formbus.Form)
		want string
	}{
		{"negative places", func(f *formbus.Form) { f.Capacity = -1 }, "negative"},
		{"a waiting list with no number of places", func(f *formbus.Form) { f.Capacity = 0 }, "never full"},
		{"a waiting list on a paid form", func(f *formbus.Form) {
			f.Items = []formbus.Item{{ID: "ticket", Label: "Ticket", Price: 1500}}
			f.ReturnURL = "https://example.test/pageant"
		}, "not ready yet"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := pageant(t)
			tt.edit(&f)
			f.Stamp()

			if err := f.Check(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Check = %v, want a problem mentioning %q", err, tt.want)
			}
		})
	}
}

// Raising the number of places so one more can come must not throw away the
// forms people have half filled in.
func TestThePlacesAreNotInTheFingerprint(t *testing.T) {
	f := pageant(t)
	before := f.Version

	f.Capacity = 41
	f.Waitlist = false
	f.Stamp()

	if f.Version != before {
		t.Error("changing the number of places or the waiting list changed the version")
	}
}

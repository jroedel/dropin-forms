package formbus_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/form/formbus"
)

// rides is a student event with a carpool: a name and a mobile number first,
// then the carpool that texts by them.
func rides(t *testing.T) formbus.Form {
	t.Helper()

	f := base(t)
	f.Fields = []formbus.Field{
		{Name: "name", Label: "Your name", Kind: formbus.KindText, Required: true},
		{Name: "mobile", Label: "Mobile", Kind: formbus.KindTel},
		{Name: "ride", Label: "Getting there", Kind: formbus.KindCarpool, Required: true,
			Phone: "mobile", Who: "name",
			Options: []formbus.Option{
				{Value: formbus.CarpoolHave, Label: "I have a ride"},
				{Value: formbus.CarpoolDrive, Label: "I can drive"},
				{Value: formbus.CarpoolNeed, Label: "Please find me a driver"},
			}},
	}
	f.Stamp()

	if err := f.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	return f
}

var carpoolNow = time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)

func violationsOf(t *testing.T, err error) map[string]string {
	t.Helper()

	invalid, ok := errors.AsType[formbus.Invalid](err)
	if !ok {
		t.Fatalf("err = %v, want Invalid", err)
	}

	out := map[string]string{}
	for _, v := range invalid.Violations {
		out[v.Field] = v.Message
	}

	return out
}

func TestADriverIsKeptWithTheirSeats(t *testing.T) {
	f := rides(t)

	ans, err := f.Validate(carpoolNow, formbus.Values{
		"name": {"Damien"}, "mobile": {"(512) 555-0123"},
		"ride": {"drive"}, "ride.seats": {" 3 "},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	got, _ := ans.Field("ride")
	if strings.Join(got.Values, ",") != "drive,3" {
		t.Errorf("values = %q, want [drive 3]", got.Values)
	}

	if got.Readable() != "Can drive, 3 seats" {
		t.Errorf("Readable = %q", got.Readable())
	}

	c, ok := f.Carpooler(ans)
	if !ok || !c.Driving() || c.Seats != 3 || c.Name != "Damien" || c.Phone.String() != "+15125550123" {
		t.Errorf("Carpooler = %+v, %v", c, ok)
	}

	// And back into the form for changing: the seats into their own box.
	values := ans.Values()
	if strings.Join(values["ride"], ",") != "drive" || strings.Join(values["ride.seats"], ",") != "3" {
		t.Errorf("Values() = ride %q, seats %q", values["ride"], values["ride.seats"])
	}
}

func TestADriverMustSayHowManySeats(t *testing.T) {
	f := rides(t)

	for _, seats := range [][]string{nil, {""}, {"0"}, {"8"}, {"three"}} {
		_, err := f.Validate(carpoolNow, formbus.Values{
			"name": {"Damien"}, "mobile": {"5125550123"}, "ride": {"drive"}, "ride.seats": seats,
		})

		if msg := violationsOf(t, err)["ride"]; !strings.Contains(msg, "from 1 to 7") {
			t.Errorf("seats %q: violation %q, want one naming 1 to 7", seats, msg)
		}
	}
}

func TestSeatsSentWithAnyOtherChoiceAreIgnored(t *testing.T) {
	f := rides(t)

	ans, err := f.Validate(carpoolNow, formbus.Values{
		"name": {"Ana"}, "mobile": {"5125550101"}, "ride": {"need"}, "ride.seats": {"4"},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if got, _ := ans.Field("ride"); strings.Join(got.Values, ",") != "need" {
		t.Errorf("values = %q, want [need]", got.Values)
	}
}

func TestSomebodyInACarMustGiveANumberAndAName(t *testing.T) {
	f := rides(t)

	vs := violationsOf(t, func() error {
		_, err := f.Validate(carpoolNow, formbus.Values{"name": {"Ana"}, "ride": {"need"}})
		return err
	}())
	if !strings.Contains(vs["mobile"], "text you") {
		t.Errorf("no number: mobile violation %q", vs["mobile"])
	}

	vs = violationsOf(t, func() error {
		_, err := f.Validate(carpoolNow, formbus.Values{"name": {"Ana"}, "mobile": {"555-0101"}, "ride": {"drive"}, "ride.seats": {"2"}})
		return err
	}())
	if !strings.Contains(vs["mobile"], "area code") {
		t.Errorf("a number with no area code: mobile violation %q", vs["mobile"])
	}

	// Somebody with their own ride is asked for neither.
	if _, err := f.Validate(carpoolNow, formbus.Values{"name": {"Ana"}, "ride": {"have"}}); err != nil {
		t.Errorf("own ride, no number: %v", err)
	}
}

func TestCheckRefusesACarpoolItCannotUse(t *testing.T) {
	cases := map[string]struct {
		change func(f *formbus.Form)
		want   string
	}{
		"a missing choice":    {func(f *formbus.Form) { f.Fields[2].Options = f.Fields[2].Options[:2] }, "exactly"},
		"a renamed value":     {func(f *formbus.Form) { f.Fields[2].Options[1].Value = "car" }, "exactly"},
		"a limit on a choice": {func(f *formbus.Form) { f.Fields[2].Options[1].Limit = 3 }, "cannot have a limit"},
		"no phone":            {func(f *formbus.Form) { f.Fields[2].Phone = "" }, "needs phone"},
		"a phone that is not a telephone question": {
			func(f *formbus.Form) { f.Fields[2].Phone = "name" }, "needs to be a tel one",
		},
		"a phone after the carpool": {
			func(f *formbus.Form) { f.Fields[1], f.Fields[2] = f.Fields[2], f.Fields[1] },
			"comes after it",
		},
		"a second carpool": {
			func(f *formbus.Form) {
				second := f.Fields[2]
				second.Name = "back"
				f.Fields = append(f.Fields, second)
			},
			"at most one carpool",
		},
		"carpool settings on a text question": {func(f *formbus.Form) { f.Fields[0].SeatsLabel = "Seats?" }, "only a carpool"},
	}

	for name, c := range cases {
		f := rides(t)
		c.change(&f)
		f.Stamp()

		err := f.Check()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Check = %v, want it to mention %q", name, err, c.want)
		}
	}
}

func TestWhoIsTextedIsPartOfTheVersion(t *testing.T) {
	f := rides(t)
	before := f.Version

	f.Fields = append([]formbus.Field{{Name: "cell", Label: "Cell", Kind: formbus.KindTel}}, f.Fields...)
	f.Stamp()
	withField := f.Version

	f.Fields[3].Phone = "cell"
	f.Stamp()

	if f.Version == withField || withField == before {
		t.Error("pointing the carpool at another number did not change the version")
	}

	f.Fields[3].SeatsLabel = "Seats for others?"
	f.Stamp()

	if f.Version != f.Fingerprint() {
		t.Fatal("Stamp did not stamp")
	}
}

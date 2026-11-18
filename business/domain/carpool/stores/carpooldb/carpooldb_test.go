package carpooldb_test

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/business/domain/carpool/carpoolbus"
	"github.com/jroedel/dropin-forms/business/domain/carpool/stores/carpooldb"
	"github.com/jroedel/dropin-forms/business/types"
	"github.com/jroedel/dropin-forms/foundation/sqldb"
)

func open(t *testing.T) *carpooldb.Store {
	t.Helper()

	db, err := sqldb.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := carpooldb.Init(t.Context(), db); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := sqldb.CheckSchema(t.Context(), db, carpooldb.Expected); err != nil {
		t.Fatalf("CheckSchema: %v", err)
	}

	return carpooldb.NewStore(db)
}

var now = time.Date(2026, 11, 17, 18, 0, 0, 0, time.UTC)

var form = func() types.Slug {
	s, err := types.ParseSlug("youth-retreat")
	if err != nil {
		panic(err)
	}

	return s
}()

func TestARideIsWaitedPlacedTextedAndReleased(t *testing.T) {
	s := open(t)
	rider, driver := types.NewID(), types.NewID()

	fresh, err := s.Wait(t.Context(), form, rider, "Ana", now)
	if err != nil || !fresh {
		t.Fatalf("Wait = %v, %v; want a new row", fresh, err)
	}

	if again, _ := s.Wait(t.Context(), form, rider, "Ana", now.Add(time.Hour)); again {
		t.Error("waiting twice made a second new row")
	}

	if err := s.Place(t.Context(), form, rider, driver, "Ana", 2, now.Add(time.Minute), types.ID{}); err != nil {
		t.Fatalf("Place: %v", err)
	}

	if err := s.Texted(t.Context(), rider, now.Add(2*time.Minute), time.Time{}, "they replied STOP to our texts"); err != nil {
		t.Fatalf("Texted: %v", err)
	}

	rides, err := s.ByForm(t.Context(), form)
	if err != nil || len(rides) != 1 {
		t.Fatalf("ByForm = %d, %v", len(rides), err)
	}

	r := rides[0]
	switch {
	case r.Driver != driver, r.Waiting():
		t.Errorf("driver = %v, want %v", r.Driver, driver)
	case !r.Since.Equal(now):
		t.Errorf("since = %v, want when they first waited", r.Since)
	case r.RiderTexted.IsZero() || !r.DriverTexted.IsZero() || r.Problem == "":
		t.Errorf("texts = %v/%v %q", r.RiderTexted, r.DriverTexted, r.Problem)
	}

	if err := s.Release(t.Context(), rider); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if rides, _ := s.ByForm(t.Context(), form); !rides[0].Waiting() || rides[0].Problem != "" {
		t.Errorf("after Release: %+v", rides[0])
	}

	if err := s.Remove(t.Context(), rider); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if rides, _ := s.ByForm(t.Context(), form); len(rides) != 0 {
		t.Errorf("after Remove: %d rides", len(rides))
	}
}

// The last seat in a car, asked for by two at once, goes to one of them.
func TestTheLastSeatGoesToOneRider(t *testing.T) {
	s := open(t)
	driver := types.NewID()

	if err := s.Place(t.Context(), form, types.NewID(), driver, "Ana", 2, now, types.ID{}); err != nil {
		t.Fatalf("the first seat: %v", err)
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	for range 2 {
		wg.Go(func() {
			err := s.Place(t.Context(), form, types.NewID(), driver, "Ana", 2, now, types.ID{})

			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		})
	}

	wg.Wait()

	full := 0
	for _, err := range errs {
		switch {
		case errors.Is(err, carpoolbus.ErrCarFull):
			full++
		case err != nil:
			t.Errorf("Place: %v", err)
		}
	}

	if full != 1 {
		t.Errorf("%d of the two were refused, want exactly one", full)
	}
}

// Moving a rider who is already in the car does not count them twice.
func TestARiderIsNotCountedAgainstTheirOwnSeat(t *testing.T) {
	s := open(t)
	rider, driver := types.NewID(), types.NewID()

	if err := s.Place(t.Context(), form, rider, driver, "Ana", 1, now, types.ID{}); err != nil {
		t.Fatal(err)
	}

	if err := s.Place(t.Context(), form, rider, driver, "Ana", 1, now, types.NewID()); err != nil {
		t.Errorf("placing the same rider in the same one-seat car again: %v", err)
	}
}

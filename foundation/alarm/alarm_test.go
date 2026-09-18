package alarm_test

import (
	"testing"
	"time"

	"github.com/jroedel/dropin-forms/foundation/alarm"
)

var start = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

// The property the whole type exists for: one line per window, whatever
// happens after it. An alarm that fires per event during a flood is an alarm
// that hides the flood inside its own output.
func TestTheCeilingFiresOnceAndThenCountsQuietly(t *testing.T) {
	c := alarm.NewCeiling(3, time.Hour)

	for i := 1; i <= 3; i++ {
		if crossed, n := c.Count("feast", start); crossed {
			t.Fatalf("event %d crossed a ceiling of 3 (count %d)", i, n)
		}
	}

	crossed, n := c.Count("feast", start)
	if !crossed || n != 4 {
		t.Fatalf("the fourth event: crossed=%v count=%d, want true and 4", crossed, n)
	}

	for i := 5; i <= 8; i++ {
		crossed, n := c.Count("feast", start)
		if crossed {
			t.Errorf("event %d fired a second time in one window", i)
		}
		if n != i {
			t.Errorf("event %d was counted as %d", i, n)
		}
	}
}

// The count in the line is what somebody decides from, so it has to be the
// count at the moment of crossing rather than the limit.
func TestTheWindowStartsAgainAndCanFireAgain(t *testing.T) {
	c := alarm.NewCeiling(1, time.Hour)

	c.Count("feast", start)

	if crossed, _ := c.Count("feast", start.Add(30*time.Minute)); !crossed {
		t.Fatal("the second event inside the window did not cross a ceiling of 1")
	}

	// An hour later the window has elapsed, so the count starts again and the
	// first event is inside the ceiling once more.
	if crossed, n := c.Count("feast", start.Add(time.Hour)); crossed || n != 1 {
		t.Fatalf("after the window: crossed=%v count=%d, want false and 1", crossed, n)
	}

	if crossed, _ := c.Count("feast", start.Add(time.Hour)); !crossed {
		t.Error("the new window cannot fire, so anything after the first hour is silent")
	}
}

func TestKeysAreCountedApart(t *testing.T) {
	c := alarm.NewCeiling(1, time.Hour)

	c.Count("feast", start)
	c.Count("feast", start)

	if crossed, n := c.Count("raffle", start); crossed || n != 1 {
		t.Errorf("another key started at crossed=%v count=%d, want false and 1", crossed, n)
	}
}

// Switching it off has to be a value rather than a branch at every call site,
// because the call sites are inside request handling and an if there is an if
// somebody has to keep correct.
func TestAZeroCeilingNeverFires(t *testing.T) {
	for name, c := range map[string]*alarm.Ceiling{
		"no limit":  alarm.NewCeiling(0, time.Hour),
		"no window": alarm.NewCeiling(10, 0),
		"negative":  alarm.NewCeiling(-1, time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			for range 100 {
				if crossed, _ := c.Count("feast", start); crossed {
					t.Fatal("a switched-off ceiling fired")
				}
			}
		})
	}
}

// Elapsed windows are dropped, so a ceiling keyed by something that comes and
// goes does not keep every key it has ever seen.
func TestElapsedWindowsAreForgotten(t *testing.T) {
	c := alarm.NewCeiling(10, time.Minute)

	for i := range 50 {
		c.Count(string(rune('a'+i%26))+string(rune('0'+i/26)), start)
	}

	// One event an hour later sweeps every window that has elapsed, including
	// its own predecessors.
	c.Count("last", start.Add(time.Hour))

	if got := c.Len(); got != 1 {
		t.Errorf("%d windows are still held, want only the live one", got)
	}
}

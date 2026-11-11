package stats

import (
	"testing"
	"time"
)

// cutsDay sums rows against a plan of line L: GTFS trips G (direction 0) and C (direction 1),
// both on a 4000 m shape, and line M, a loop of OASA's daily schedule with trips R1 and R2.
func cutsDay(t *testing.T, rows ...string) map[string]TripCount {
	t.Helper()
	dir := t.TempDir()
	at := func(h, m int) time.Time { return local(2026, 10, 6, h, m, 0) }
	writeRows(t, dir, at(8, 0), rows...)
	plan := &Plan{GTFS: "g", Trips: []PlanTrip{
		{ID: "G", Line: "L", Start: at(8, 0).Unix(), End: at(8, 40).Unix(), ToM: 4000},
		{ID: "C", Line: "L", Dir: 1, Start: at(8, 30).Unix(), End: at(9, 10).Unix(), ToM: 4000},
		{ID: "R1", Line: "M", Start: at(8, 0).Unix(), End: at(8, 40).Unix(), ToM: 4000},
	}}
	plan.UseDaily([]DailyDir{{Line: "M", Dir: 0, Starts: []int64{at(8, 0).Unix(), at(8, 45).Unix()},
		Trips: map[string]Span{"R1": {0, 4000}, "R2": {0, 4000}}}})
	d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), plan)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]TripCount{"": d.Trips.TripCount}
	for _, l := range d.Trips.ByLine {
		out[l.Line] = l.TripCount
	}
	return out
}

func cuts(c TripCount) [2]int { return [2]int{c.ShortTurn, c.Vanished} }

func TestShortTurnIsARunLeftMidRouteForTheOtherDirection(t *testing.T) {
	at := func(h, m int) time.Time { return local(2026, 10, 6, h, m, 0) }
	got := cutsDay(t,
		vrow(at(8, 0), "L", "a", "G", 200, "g"), vrow(at(8, 10), "L", "a", "G", 1500, "g"),
		vrow(at(8, 20), "L", "a", "G", 2600, "g"), // 65%: it lets the riders off and turns back
		vrow(at(8, 24), "L", "a", "C", 1500, "g"), vrow(at(8, 40), "L", "a", "C", 3950, "g"),
	)
	if cuts(got["L"]) != [2]int{1, 0} || cuts(got[""]) != [2]int{1, 0} {
		t.Fatalf("L = %+v, all = %+v", got["L"], got[""])
	}
}

func TestShortTurnIsARunLeftMidRouteForTheStart(t *testing.T) {
	at := func(h, m int) time.Time { return local(2026, 10, 6, h, m, 0) }
	got := cutsDay(t,
		vrow(at(8, 0), "M", "a", "R1", 200, "g"), vrow(at(8, 10), "M", "a", "R1", 1500, "g"),
		vrow(at(8, 20), "M", "a", "R1", 2400, "g"),
		vrow(at(8, 26), "M", "a", "R2", 100, "g"), vrow(at(8, 40), "M", "a", "R2", 3950, "g"),
	)
	if cuts(got["M"]) != [2]int{1, 0} {
		t.Fatalf("M = %+v", got["M"])
	}
}

func TestVanishedIsABusSilentMidRoute(t *testing.T) {
	at := func(h, m int) time.Time { return local(2026, 10, 6, h, m, 0) }
	got := cutsDay(t,
		vrow(at(8, 0), "L", "a", "G", 200, "g"), vrow(at(8, 10), "L", "a", "G", 1500, "g"),
		vrow(at(8, 20), "L", "a", "G", 2000, "g"), // never seen again
		vrow(at(8, 0), "L", "b", "G", 200, "g"), vrow(at(8, 10), "L", "b", "G", 1500, "g"),
		vrow(at(8, 15), "L", "b", "G", 2000, "g"),
		vrow(at(8, 45), "L", "b", "C", 2000, "g"), // back 30 minutes later, on another run
	)
	if cuts(got["L"]) != [2]int{0, 2} {
		t.Fatalf("L = %+v", got["L"])
	}
}

// A bus that goes on reporting without a match (or without a position) has not vanished.
func TestUnmatchedFixesAreNoSilence(t *testing.T) {
	at := func(h, m int) time.Time { return local(2026, 10, 6, h, m, 0) }
	got := cutsDay(t,
		vrow(at(8, 0), "L", "a", "G", 200, "g"), vrow(at(8, 10), "L", "a", "G", 1500, "g"),
		vrow(at(8, 20), "L", "a", "G", 2000, "g"),
		vrow(at(8, 25), "L", "a", "", -1, "g"), vrow(at(8, 32), "L", "a", "G", -1, "g"),
		vrow(at(8, 50), "L", "a", "C", 2000, "g"), vrow(at(9, 5), "L", "a", "C", 3950, "g"),
	)
	if cuts(got["L"]) != [2]int{1, 0} {
		t.Fatalf("L = %+v", got["L"])
	}
}

// A GPS gap shorter than TripSlack, after which the bus goes on along the same run, cuts nothing;
// neither does a matcher switch between the trips of one daily direction (#131), a run that
// reaches the last 10% of its shape, nor a run seen at one fix only.
func TestNoCutForAGapAFullRunOrASwitchInADirection(t *testing.T) {
	at := func(h, m int) time.Time { return local(2026, 10, 6, h, m, 0) }
	got := cutsDay(t,
		vrow(at(8, 0), "L", "a", "G", 200, "g"), vrow(at(8, 10), "L", "a", "G", 1500, "g"),
		vrow(at(8, 30), "L", "a", "G", 2500, "g"), vrow(at(8, 45), "L", "a", "G", 3700, "g"), // 92.5%
		vrow(at(8, 50), "L", "a", "C", 100, "g"), vrow(at(9, 10), "L", "a", "C", 3950, "g"),
		vrow(at(8, 0), "M", "b", "R1", 200, "g"), vrow(at(8, 10), "M", "b", "R2", 1500, "g"),
		vrow(at(8, 20), "M", "b", "R1", 3950, "g"),
		vrow(at(8, 0), "L", "c", "C", 2000, "g"), // one stray fix
		vrow(at(8, 2), "L", "c", "G", 2000, "g"), vrow(at(8, 20), "L", "c", "G", 3950, "g"),
	)
	if cuts(got[""]) != [2]int{0, 0} {
		t.Fatalf("all = %+v", got[""])
	}
}

// A loop line: the bus reaches the end of its shape, which is its first stop, and the next run
// starts at 0 m on the same daily direction.
func TestNoCutOnALoopLine(t *testing.T) {
	at := func(h, m int) time.Time { return local(2026, 10, 6, h, m, 0) }
	got := cutsDay(t,
		vrow(at(8, 0), "M", "a", "R1", 50, "g"), vrow(at(8, 20), "M", "a", "R1", 2000, "g"),
		vrow(at(8, 40), "M", "a", "R1", 3900, "g"),
		vrow(at(8, 45), "M", "a", "R2", 30, "g"), vrow(at(9, 5), "M", "a", "R2", 2000, "g"),
		vrow(at(9, 25), "M", "a", "R2", 3950, "g"),
	)
	if cuts(got["M"]) != [2]int{0, 0} {
		t.Fatalf("M = %+v", got["M"])
	}
}

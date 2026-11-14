package stats

import (
	"reflect"
	"testing"
	"time"
)

// The backend collects no fixes during an outage, so two consecutive passes either side of it
// are not a headway. These tests drive tripCounter.noteFix with the city-wide fixes of every
// minute (the rows of any line) and check which headways survive.

type outageDay struct {
	loc   *time.Location
	start time.Time
	plan  *Plan
}

func newOutageDay(t *testing.T) outageDay {
	t.Helper()
	loc, err := time.LoadLocation(TZ)
	if err != nil {
		t.Fatal(err)
	}
	start, _, _ := Bounds("2026-11-02", loc)
	plan := &Plan{GTFS: "v1"}
	plan.UseDaily([]DailyDir{{Line: "L", Dir: 0, Trips: map[string]Span{"T": {0, 1000}}}})
	return outageDay{loc, start, plan}
}

func (d outageDay) at(h, m, s int) int64 {
	return time.Date(2026, 11, 2, h, m, s, 0, d.loc).Unix()
}

// counter has one bus passing mid-route at each of the slots (clock minutes since midnight) plus
// 30 s, and perMin(minute since midnight) city-wide fixes in every minute from 06:00 to 22:00
// (nil: the counter hears of no city-wide fixes at all, as before this rule).
func (d outageDay) counter(slots []int, perMin func(min int) int) *tripCounter {
	c := newTripCounter(d.plan)
	for i, s := range slots {
		veh := string(rune('A'+i%26)) + string(rune('a'+i/26))
		t0 := d.at(0, s, 0)
		c.add(t0, veh, "T", "v1", 400)
		c.add(t0+60, veh, "T", "v1", 600)
	}
	if perMin != nil {
		for m := 6 * 60; m < 22*60; m++ {
			for k := 0; k < perMin(m); k++ {
				c.noteFix(d.at(0, m, k%60))
			}
		}
	}
	return c
}

// every is the slots from..to (inclusive, minutes since midnight) every step minutes, without
// those in [skipFrom, skipTo).
func every(from, to, step, skipFrom, skipTo int) []int {
	var out []int
	for s := from; s <= to; s += step {
		if s >= skipFrom && s < skipTo {
			continue
		}
		out = append(out, s)
	}
	return out
}

const h = 60 // minutes

// full is 100 fixes a minute, except `rate` a minute in [from, to) minutes since midnight.
func full(from, to, rate int) func(int) int {
	return func(m int) int {
		if m >= from && m < to {
			return rate
		}
		return 100
	}
}

func lineOf(t *testing.T, hw *Headways) LineHeadway {
	t.Helper()
	if hw == nil || len(hw.ByLine) != 1 {
		t.Fatalf("want one line, got %+v", hw)
	}
	return hw.ByLine[0]
}

// (a) 30 min with no fixes at all: the two passes either side are not a headway.
func TestHeadwaysDropAcrossOutage(t *testing.T) {
	d := newOutageDay(t)
	// buses every 5 min 08:00-11:55, none in the outage 10:00-10:30
	slots := every(8*h, 11*h+55, 5, 10*h, 10*h+30)
	got := lineOf(t, d.counter(slots, full(10*h, 10*h+30, 0)).headways(d.start, d.loc))
	// 42 passes, 41 headways, one of them (09:55:30 to 10:30:30, 2100 s) spans the outage
	if got.N != 40 || got.MeanS != 300 || got.CVh != 0 || got.LOS != "A" {
		t.Errorf("got %+v, want 40 headways of 300 s", got)
	}
}

// The outage is only degraded (13:20-13:50 in Thessaloniki on 2026-11-13 kept 12-30 % of the
// usual fixes): 20 % of the usual rate counts as an outage, 40 % does not.
func TestHeadwaysOutageThreshold(t *testing.T) {
	d := newOutageDay(t)
	slots := every(8*h, 11*h+55, 5, 10*h, 10*h+30)
	got := lineOf(t, d.counter(slots, full(10*h, 10*h+30, 20)).headways(d.start, d.loc))
	if got.N != 40 {
		t.Errorf("20 %% of the usual fixes: N %d, want 40 (headway across it dropped)", got.N)
	}
	got = lineOf(t, d.counter(slots, full(10*h, 10*h+30, 40)).headways(d.start, d.loc))
	if got.N != 41 {
		t.Errorf("40 %% of the usual fixes: N %d, want 41 (kept)", got.N)
	}
}

// (b) A normal day: knowing the city-wide fixes changes nothing, also with an uneven line.
func TestHeadwaysNormalDayUnchanged(t *testing.T) {
	d := newOutageDay(t)
	var slots []int
	for s, i := 8*h, 0; s < 12*h; i++ { // headways 3, 7, 3, 7 ... min
		slots = append(slots, s)
		s += 3 + 4*(i%2)
	}
	base := d.counter(slots, nil).headways(d.start, d.loc)
	if base == nil {
		t.Fatal("no headways without the rule")
	}
	// a normal day still has a quiet minute or two (fewer fixes, not an outage)
	quiet := func(m int) int {
		if m%97 == 0 {
			return 30
		}
		return 100
	}
	if got := d.counter(slots, quiet).headways(d.start, d.loc); !reflect.DeepEqual(got, base) {
		t.Errorf("got %+v, want %+v", got, base)
	}
}

// (c) 4 empty minutes are not an outage; 5 are (the HeadwayMaxGap of one fix pair).
func TestHeadwaysOutageNeedsFiveMinutes(t *testing.T) {
	d := newOutageDay(t)
	slots := every(8*h, 11*h+55, 5, 10*h, 10*h+5) // the bus of 10:00 is missing
	got := lineOf(t, d.counter(slots, full(10*h, 10*h+4, 0)).headways(d.start, d.loc))
	if got.N != 46 { // 47 passes
		t.Errorf("4 empty minutes: N %d, want 46", got.N)
	}
	got = lineOf(t, d.counter(slots, full(10*h, 10*h+5, 0)).headways(d.start, d.loc))
	if got.N != 45 {
		t.Errorf("5 empty minutes: N %d, want 45", got.N)
	}
}

// (d) The outage runs to the edge of the daytime window (07:00 and 21:00): no headway is lost
// that does not span it, and the minute at each edge is counted.
func TestHeadwaysOutageAtWindowEdge(t *testing.T) {
	d := newOutageDay(t)
	// start: buses at 07:00, 07:10 and then every 5 min to 08:30; minutes 07:01-07:09 empty
	slots := append([]int{7 * h}, every(7*h+10, 8*h+30, 5, 0, 0)...)
	got := lineOf(t, d.counter(slots, full(7*h+1, 7*h+10, 0)).headways(d.start, d.loc))
	if got.N != 16 { // 18 passes, 17 headways, the first spans the outage
		t.Errorf("start edge: N %d, want 16", got.N)
	}
	// end: buses every 5 min 19:00-20:40, one at 20:55; minutes 20:41-20:59 empty
	slots = append(every(19*h, 20*h+40, 5, 0, 0), 20*h+55)
	got = lineOf(t, d.counter(slots, full(20*h+41, 21*h, 0)).headways(d.start, d.loc))
	if got.N != 20 { // 22 passes, 21 headways, the last spans the outage
		t.Errorf("end edge: N %d, want 20", got.N)
	}
}

// (e) Every headway of a direction spans an outage: the line is left out.
func TestHeadwaysAllDropped(t *testing.T) {
	d := newOutageDay(t)
	slots := every(8*h, 10*h+12, 12, 0, 0) // 12 passes, 11 headways
	perMin := func(m int) int {
		if k := m % 12; m >= 8*h && m < 10*h+24 && k >= 3 && k <= 7 { // 5 empty minutes between every two passes
			return 0
		}
		return 100
	}
	if got := d.counter(slots, perMin).headways(d.start, d.loc); got != nil {
		t.Errorf("want nil, got %+v", got)
	}
}

// With no city-wide fix count (a test counter, or a day without rows) nothing is an outage.
func TestHeadwaysNoFixCountsNoOutage(t *testing.T) {
	d := newOutageDay(t)
	slots := every(8*h, 11*h+55, 5, 10*h, 10*h+30)
	got := lineOf(t, d.counter(slots, nil).headways(d.start, d.loc))
	if got.N != 41 {
		t.Errorf("N %d, want 41", got.N)
	}
}

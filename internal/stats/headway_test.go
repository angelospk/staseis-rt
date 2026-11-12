package stats

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"time"
)

func TestHeadwayStats(t *testing.T) {
	even := []float64{300, 300, 300, 300, 300, 300, 300, 300}
	// Alternating 200/400: mean 300, stddev 100, no headway off by more than 150.
	alt := []float64{200, 400, 200, 400, 200, 400, 200, 400}
	// Bunched pairs 60/540: mean 300, stddev 240, every headway off by more than 150.
	bunched := []float64{60, 540, 60, 540, 60, 540, 60, 540}
	// Median 100, mean 325: cv_h divides by the mean (TCQSM), not the median.
	skew := []float64{100, 100, 100, 100, 100, 700, 700, 700}
	for _, tc := range []struct {
		name string
		hs   []float64
		ok   bool
		want HeadwayStat
	}{
		{"seven headways are skipped", even[:7], false, HeadwayStat{}},
		{"regular", even, true, HeadwayStat{Mean: 300, Median: 300, WaitMin: 2.5}},
		{"alternating", alt, true, HeadwayStat{Mean: 300, Median: 300, CV: 1.0 / 3,
			EWTMin: 0.5 * 300 / 9 / 60, WaitMin: 0.5 * 300 * (1 + 1.0/9) / 60}},
		{"bunched", bunched, true, HeadwayStat{Mean: 300, Median: 300, CV: 0.8,
			EWTMin: 0.5 * 300 * 0.64 / 60, WaitMin: 0.5 * 300 * 1.64 / 60, OffShare: 1}},
		{"skewed", skew, true, HeadwayStat{Mean: 325, Median: 100, CV: math.Sqrt(84375) / 325,
			EWTMin: 0.5 * 84375 / 325 / 60, WaitMin: (0.5*325 + 0.5*84375/325) / 60, OffShare: 1}},
		// odd count: median is the middle value; |h-500| > 250 for 100, 200, 800, 900
		{"odd", []float64{100, 200, 300, 400, 500, 600, 700, 800, 900}, true, HeadwayStat{Mean: 500, Median: 500,
			CV: math.Sqrt(200000.0/3) / 500, EWTMin: 0.5 * (200000.0 / 3) / 500 / 60,
			WaitMin: (250 + 0.5*(200000.0/3)/500) / 60, OffShare: 4.0 / 9}},
	} {
		got, ok := HeadwayStats(tc.hs)
		if ok != tc.ok {
			t.Fatalf("%s: ok %v, want %v", tc.name, ok, tc.ok)
		}
		w := tc.want
		for _, f := range [][3]float64{{got.Mean, w.Mean}, {got.Median, w.Median}, {got.CV, w.CV},
			{got.EWTMin, w.EWTMin}, {got.WaitMin, w.WaitMin}, {got.OffShare, w.OffShare}} {
			if math.Abs(f[0]-f[1]) > 1e-9 {
				t.Errorf("%s: got %+v, want %+v", tc.name, got, w)
				break
			}
		}
	}
}

func TestLOS(t *testing.T) {
	for _, tc := range []struct {
		cv   float64
		want string
	}{
		{0, "A"}, {0.21, "A"}, {0.2101, "B"}, {0.30, "B"}, {0.31, "C"}, {0.39, "C"},
		{0.40, "D"}, {0.52, "D"}, {0.53, "E"}, {0.74, "E"}, {0.7401, "F"}, {2, "F"},
	} {
		if got := LOS(tc.cv); got != tc.want {
			t.Errorf("LOS(%v) = %s, want %s", tc.cv, got, tc.want)
		}
	}
}

// Buses every 5 min cross mid-route; the pass is interpolated between two fixes.
func TestTripCounterHeadways(t *testing.T) {
	loc, _ := time.LoadLocation(TZ)
	start, _, _ := Bounds("2026-11-02", loc)
	at := func(h, m, s int) int64 { return time.Date(2026, 11, 2, h, m, s, 0, loc).Unix() }
	plan := &Plan{GTFS: "v1"}
	plan.UseDaily([]DailyDir{{Line: "L", Dir: 0, Trips: map[string]Span{"T": {0, 1000}}}})
	mk := func(n int) *tripCounter {
		c := newTripCounter(plan)
		for i := 0; i < n; i++ {
			veh := string(rune('a' + i))
			t0 := at(8, 5*i, 0)
			c.add(t0, veh, "T", "v1", 400)    // share 0.4
			c.add(t0+60, veh, "T", "v1", 600) // share 0.6: passes 0.5 at t0+30
		}
		c.add(at(6, 0, 0), "early", "T", "v1", 400) // before 07:00: not counted
		c.add(at(6, 0, 30), "early", "T", "v1", 600)
		return c
	}
	if h := mk(8).headways(start, loc); h != nil {
		t.Fatalf("7 headways: want nil, got %+v", h)
	}
	h := mk(9).headways(start, loc)
	if h == nil || len(h.ByLine) != 1 {
		t.Fatalf("got %+v", h)
	}
	if l := h.ByLine[0]; l.N != 8 || l.MedianS != 300 || l.MeanS != 300 || l.CVh != 0 || l.EWTMin != 0 ||
		l.WaitMin != 2.5 || l.OffShare != 0 || l.LOS != "A" {
		t.Errorf("got %+v", l)
	}
	for _, b := range mk(9).buses {
		if b.key.veh == "a" && b.pass != at(8, 0, 30) {
			t.Errorf("pass %d, want %d", b.pass, at(8, 0, 30))
		}
	}
}

// A direction with a mean headway over 10 min is not high-frequency (TCQSM Exhibit 5-9).
func TestHeadwaysLowFrequency(t *testing.T) {
	loc, _ := time.LoadLocation(TZ)
	start, _, _ := Bounds("2026-11-02", loc)
	plan := &Plan{GTFS: "v1"}
	plan.UseDaily([]DailyDir{{Line: "L", Dir: 0, Trips: map[string]Span{"T": {0, 1000}}}})
	c := newTripCounter(plan)
	for i := 0; i < 10; i++ {
		t0 := time.Date(2026, 11, 2, 8, 0, 0, 0, loc).Unix() + int64(i)*11*60
		veh := string(rune('a' + i))
		c.add(t0, veh, "T", "v1", 400)
		c.add(t0+60, veh, "T", "v1", 600)
	}
	if h := c.headways(start, loc); h != nil {
		t.Errorf("want nil, got %+v", h)
	}
}

// gtfsHeadwayPlan is a GTFS plan of line L: n trips of dir 1 every gap from 08:00 (ids G0, G1,
// ...) and n of dir 0 every 4 min (ids H0, ...), each on the stop span 1000-2000 m of its shape.
func gtfsHeadwayPlan(loc *time.Location, n int, gap time.Duration) *Plan {
	p := &Plan{GTFS: "v1", GTFSHeadways: true}
	for i := 0; i < n; i++ {
		s := time.Date(2026, 11, 2, 8, 0, 0, 0, loc).Add(time.Duration(i) * gap).Unix()
		p.Trips = append(p.Trips, PlanTrip{ID: fmt.Sprintf("G%d", i), Line: "L", Dir: 1, Start: s, End: s + 3600, FromM: 1000, ToM: 2000})
		s = time.Date(2026, 11, 2, 8, 0, 0, 0, loc).Add(time.Duration(i) * 4 * time.Minute).Unix()
		p.Trips = append(p.Trips, PlanTrip{ID: fmt.Sprintf("H%d", i), Line: "L", Dir: 0, Start: s, End: s + 3600, FromM: 1000, ToM: 2000})
	}
	return p
}

// runGTFS adds two fixes of one bus on trip id: at 40% of its span at t0 and 60% at t0+60.
func runGTFS(c *tripCounter, veh, id string, t0 int64) {
	c.add(t0, veh, id, "v1", 1400)
	c.add(t0+60, veh, id, "v1", 1600)
}

// A GTFS plan that opts in (Thessaloniki) measures each trip's mid-route pass, by line and GTFS
// direction_id.
func TestGTFSHeadways(t *testing.T) {
	loc, _ := time.LoadLocation(TZ)
	start, _, _ := Bounds("2026-11-02", loc)
	plan := gtfsHeadwayPlan(loc, 9, 5*time.Minute)
	c := newTripCounter(plan)
	for _, pt := range plan.Trips {
		runGTFS(c, "v"+pt.ID, pt.ID, pt.Start+600)
	}
	h := c.headways(start, loc)
	if h == nil || len(h.ByLine) != 2 {
		t.Fatalf("got %+v", h)
	}
	if l := h.ByLine[0]; l.Line != "L" || l.Dir != 0 || l.N != 8 || l.MeanS != 240 || l.CVh != 0 || l.LOS != "A" {
		t.Errorf("dir 0: got %+v", l)
	}
	if l := h.ByLine[1]; l.Line != "L" || l.Dir != 1 || l.N != 8 || l.MeanS != 300 || l.WaitMin != 2.5 {
		t.Errorf("dir 1: got %+v", l)
	}

	// Without the opt-in (Athens, citybus) the GTFS trips give no headways.
	plan.GTFSHeadways = false
	c = newTripCounter(plan)
	for _, pt := range plan.Trips {
		runGTFS(c, "v"+pt.ID, pt.ID, pt.Start+600)
	}
	if h := c.headways(start, loc); h != nil {
		t.Errorf("no opt-in: want nil, got %+v", h)
	}
}

// One pass per trip: a second bus on the same trip id (a matcher slip) adds no headway, and the
// earlier pass counts. Fixes more than HeadwayMaxGap apart place no pass.
func TestGTFSHeadwaysOnePassPerTrip(t *testing.T) {
	loc, _ := time.LoadLocation(TZ)
	start, _, _ := Bounds("2026-11-02", loc)
	plan := gtfsHeadwayPlan(loc, 9, 5*time.Minute)
	plan.Trips = slices.DeleteFunc(plan.Trips, func(pt PlanTrip) bool { return pt.Dir == 0 })
	c := newTripCounter(plan)
	for i, pt := range plan.Trips {
		runGTFS(c, "v"+pt.ID, pt.ID, pt.Start+600)
		if i == 3 {
			runGTFS(c, "late", pt.ID, pt.Start+780) // the same trip again, 3 min later
		}
	}
	h := c.headways(start, loc)
	if h == nil || len(h.ByLine) != 1 {
		t.Fatalf("got %+v", h)
	}
	if l := h.ByLine[0]; l.N != 8 || l.MeanS != 300 || l.CVh != 0 {
		t.Errorf("got %+v", l)
	}

	c = newTripCounter(plan)
	for i, pt := range plan.Trips {
		if i == 0 {
			c.add(pt.Start+600, "v0", pt.ID, "v1", 1400)
			c.add(pt.Start+600+int64(HeadwayMaxGap/time.Second)+1, "v0", pt.ID, "v1", 1600) // gap too long
			continue
		}
		runGTFS(c, "v"+pt.ID, pt.ID, pt.Start+600)
	}
	if h := c.headways(start, loc); h != nil {
		t.Errorf("8 passes, 7 headways: want nil, got %+v", h)
	}
}

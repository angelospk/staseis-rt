package stats

import (
	"math"
	"testing"
	"time"
)

func TestHeadwayStats(t *testing.T) {
	even := []float64{300, 300, 300, 300, 300, 300, 300, 300}
	// Alternating 200/400: mean 300, median 300, stddev 100.
	alt := []float64{200, 400, 200, 400, 200, 400, 200, 400}
	for _, tc := range []struct {
		name         string
		hs           []float64
		ok           bool
		ref, cv, ewt float64
	}{
		{"seven headways are skipped", even[:7], false, 0, 0, 0},
		{"regular", even, true, 300, 0, 0},
		// EWT = sum(h^2)/(2 sum(h)) - 150 = 800000/4800 - 150 s
		{"alternating", alt, true, 300, 100.0 / 300, (100000.0/600 - 150) / 60},
		// odd count: median is the middle value
		{"odd", []float64{100, 200, 300, 400, 500, 600, 700, 800, 900}, true, 500, math.Sqrt(200000.0/3) / 500,
			(2850000.0/9000 - 250) / 60},
	} {
		ref, cv, ewt, ok := HeadwayStats(tc.hs)
		if ok != tc.ok {
			t.Fatalf("%s: ok %v, want %v", tc.name, ok, tc.ok)
		}
		if math.Abs(ref-tc.ref) > 1e-9 || math.Abs(cv-tc.cv) > 1e-9 || math.Abs(ewt-tc.ewt) > 1e-9 {
			t.Errorf("%s: ref %v cv %v ewt %v, want %v %v %v", tc.name, ref, cv, ewt, tc.ref, tc.cv, tc.ewt)
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
	if l := h.ByLine[0]; l.N != 8 || l.RefMedS != 300 || l.CVh != 0 || l.EWTMin != 0 || l.LOS != "A" {
		t.Errorf("got %+v", l)
	}
	for _, b := range mk(9).buses {
		if b.key.veh == "a" && b.pass != at(8, 0, 30) {
			t.Errorf("pass %d, want %d", b.pass, at(8, 0, 30))
		}
	}
}

// A direction with a median headway over 12 min is not high-frequency.
func TestHeadwaysLowFrequency(t *testing.T) {
	loc, _ := time.LoadLocation(TZ)
	start, _, _ := Bounds("2026-11-02", loc)
	plan := &Plan{GTFS: "v1"}
	plan.UseDaily([]DailyDir{{Line: "L", Dir: 0, Trips: map[string]Span{"T": {0, 1000}}}})
	c := newTripCounter(plan)
	for i := 0; i < 10; i++ {
		t0 := time.Date(2026, 11, 2, 8, 0, 0, 0, loc).Unix() + int64(i)*15*60
		veh := string(rune('a' + i))
		c.add(t0, veh, "T", "v1", 400)
		c.add(t0+60, veh, "T", "v1", 600)
	}
	if h := c.headways(start, loc); h != nil {
		t.Errorf("want nil, got %+v", h)
	}
}

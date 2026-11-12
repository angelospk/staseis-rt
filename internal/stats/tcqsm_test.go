package stats

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/history"
)

// TCQSM 4th ed. Exhibit 5-6: the band of a mean headway, in whole minutes (rounded).
func TestFrequencyBand(t *testing.T) {
	for _, c := range []struct {
		min  float64
		want string
	}{
		{1, "<=5"}, {5.4, "<=5"}, {5.6, "6-10"}, {10.4, "6-10"}, {10.6, "11-15"}, {15, "11-15"},
		{15.6, "16-30"}, {30.4, "16-30"}, {30.6, "31-59"}, {59.4, "31-59"}, {59.6, "60"}, {60.4, "60"},
		{60.6, ">60"}, {240, ">60"},
	} {
		if got := FrequencyBand(c.min); got != c.want {
			t.Errorf("FrequencyBand(%v) = %q, want %q", c.min, got, c.want)
		}
	}
}

// TCQSM 4th ed. Exhibit 5-8: the band of an on-time share, in whole percent (rounded down).
func TestOTPBand(t *testing.T) {
	for _, c := range []struct {
		on, n int
		want  string
	}{
		{100, 100, "95-100"}, {95, 100, "95-100"}, {949, 1000, "90-94"}, {90, 100, "90-94"},
		{899, 1000, "80-89"}, {80, 100, "80-89"}, {79, 100, "70-79"}, {70, 100, "70-79"},
		{699, 1000, "<70"}, {0, 5, "<70"},
	} {
		if got := OTPBand(c.on, c.n); got != c.want {
			t.Errorf("OTPBand(%d, %d) = %q, want %q", c.on, c.n, got, c.want)
		}
	}
}

// A plan that opts in (a citybus.gr city) gets the frequency of each line and direction from its
// trips: those leaving in [07:00, 21:00) local, one per departure time, at least FreqMinTrips.
func TestComputeFrequency(t *testing.T) {
	at := func(h, m int) int64 { return local(2026, 10, 6, h, m, 0).Unix() }
	trip := func(line string, dir int, h, m int) PlanTrip {
		return PlanTrip{ID: fmt.Sprintf("%s%d-%02d%02d", line, dir, h, m), Line: line, Dir: dir, Start: at(h, m), End: at(h, m) + 1800}
	}
	plan := &Plan{GTFS: "v", Source: SourceTimetable, Frequency: true}
	// A/0: 07:00 to 09:00 every 30 min; 06:30 and 21:00 are outside the day window, and 07:30
	// twice (two routes of the line) is one departure.
	for _, hm := range [][2]int{{6, 30}, {7, 0}, {7, 30}, {7, 30}, {8, 0}, {8, 30}, {9, 0}, {21, 0}} {
		plan.Trips = append(plan.Trips, trip("A", 0, hm[0], hm[1]))
	}
	// A/1: 3 departures, too few for a frequency.
	for _, h := range []int{7, 12, 18} {
		plan.Trips = append(plan.Trips, trip("A", 1, h, 0))
	}
	// B/0: hourly, 07:00 to 10:00; B/1: every 7 min.
	for h := 7; h <= 10; h++ {
		plan.Trips = append(plan.Trips, trip("B", 0, h, 0))
	}
	for i := 0; i < 6; i++ {
		plan.Trips = append(plan.Trips, trip("B", 1, 8, 7*i))
	}
	dir := t.TempDir()
	writeRaw(t, dir, hourName(local(2026, 10, 6, 9, 0, 0)), history.Header,
		fmt.Sprintf("%d,A,R,x,37980000,23700000,v,,,-1,,,\n", at(9, 10)))

	d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), plan)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(d.Frequency)
	want := `{"from_h":7,"to_h":21,"min_trips":4,"by_line":[` +
		`{"line":"A","dir":0,"trips":5,"headway_min":30,"band":"16-30"},` +
		`{"line":"B","dir":0,"trips":4,"headway_min":60,"band":"60"},` +
		`{"line":"B","dir":1,"trips":6,"headway_min":7,"band":"6-10"}]}`
	if string(got) != want {
		t.Fatalf("frequency\n got %s\nwant %s", got, want)
	}

	// No line with enough departures (a city with no timetable that day): no block at all.
	plan.Trips = plan.Trips[8:11] // A/1 only
	if d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), plan); err != nil || d.Frequency != nil {
		t.Fatalf("too few departures: %+v, %v", d.Frequency, err)
	}
	plan.Trips = []PlanTrip{}
	if d, err = Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), plan); err != nil || d.Frequency != nil {
		t.Fatalf("no trips: %+v, %v", d.Frequency, err)
	}
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), `"frequency"`) || strings.Contains(string(b), `"origin_otp"`) {
		t.Fatalf("empty blocks are in the day: %s", b)
	}
}

// otpRow is a history row of vehicle veh on line at sm metres along its shape.
func otpRow(at time.Time, line, veh string, sm int) string {
	return fmt.Sprintf("%d,%s,R,%s,37980000,23700000,v,,,%d,,,\n", at.Unix(), line, veh, sm)
}

// On-time performance at the origin: the run's scheduled departure comes from its vehicle code,
// the actual one is the crossing of shape metre OTPAtM, interpolated between the last sighting
// before it and the first after it when they are at most OTPMaxGap apart, and within
// [-OTPEarlyWindow, +OTPLateWindow] of the schedule. On time is 0 to OTPLateS late; early is
// reported apart and is not on time. A run with no such bracket counts, unjudged. Runs count for
// the day of their scheduled departure.
func TestComputeOriginOTP(t *testing.T) {
	at := func(h, m, s int) time.Time { return local(2026, 10, 6, h, m, s) }
	sched := map[string]time.Time{}
	run := func(name string, s time.Time) string { sched["c:"+name] = s; return "c:" + name }
	r1 := run("r1", at(10, 0, 0))    // crosses at 10:00:00: on time (0 s)
	r2 := run("r2", at(10, 10, 0))   // 10:15:00: on time (300 s)
	r3 := run("r3", at(10, 20, 0))   // 10:26:00: late
	r4 := run("r4", at(10, 30, 0))   // 10:29:10: early
	r5 := run("r5", at(10, 40, 0))   // sightings 120 s apart: unmeasured
	r6 := run("r6", at(10, 50, 0))   // first seen mid-route: unmeasured
	r7 := run("r7", at(11, 0, 0))    // crosses 90 min early: unmeasured
	r8 := run("r8", at(11, 10, 0))   // no shape metre: unmeasured
	r9 := run("r9", at(11, 20, 0))   // crosses at 11:20:10 (on time), back, and again at 11:27:10
	r10 := run("r10", at(23, 58, 0)) // crosses at 00:01:10 the next day: on time, for the 6th
	prev := run("prev", local(2026, 10, 5, 23, 50, 0))
	plan := &Plan{GTFS: "v", Source: SourceTimetable, Trips: []PlanTrip{},
		RunDeparture: func(veh string) (int64, bool) {
			s, ok := sched[veh]
			return s.Unix(), ok
		}}
	dir := t.TempDir()
	writeRaw(t, dir, hourName(at(0, 0, 0)), history.Header,
		otpRow(at(0, 1, 0), "c:2", prev, 50), otpRow(at(0, 1, 20), "c:2", prev, 150))
	writeRaw(t, dir, hourName(at(9, 0, 0)), history.Header,
		otpRow(at(9, 29, 50), "c:1", r7, 50), otpRow(at(9, 30, 10), "c:1", r7, 150))
	writeRaw(t, dir, hourName(at(10, 0, 0)), history.Header,
		otpRow(at(10, 0, 30), "c:1", r1, 180), otpRow(at(9, 59, 30), "c:1", r1, 20), // out of order
		otpRow(at(9, 58, 0), "c:1", r1, 0),
		otpRow(at(10, 14, 40), "c:1", r2, 50), otpRow(at(10, 15, 20), "c:1", r2, 150),
		otpRow(at(10, 25, 40), "c:1", r3, 50), otpRow(at(10, 26, 20), "c:1", r3, 150),
		otpRow(at(10, 29, 0), "c:1", r4, 0), otpRow(at(10, 29, 20), "c:1", r4, 200),
		otpRow(at(10, 41, 0), "c:1", r5, 50), otpRow(at(10, 43, 0), "c:1", r5, 150),
		otpRow(at(10, 59, 0), "c:1", r6, 800), otpRow(at(10, 59, 30), "c:1", r6, 1000),
		otpRow(at(10, 30, 0), "c:1", "c:device", 50), otpRow(at(10, 30, 20), "c:1", "c:device", 150),
	)
	writeRaw(t, dir, hourName(at(11, 0, 0)), history.Header,
		otpRow(at(11, 9, 0), "c:3", r8, -1), otpRow(at(11, 12, 0), "c:3", r8, -1),
		otpRow(at(11, 20, 0), "c:2", r9, 50), otpRow(at(11, 20, 20), "c:2", r9, 150),
		otpRow(at(11, 27, 0), "c:2", r9, 50), otpRow(at(11, 27, 20), "c:2", r9, 150),
	)
	next := local(2026, 10, 7, 0, 0, 0)
	writeRaw(t, dir, hourName(next), history.Header,
		otpRow(next.Add(60*time.Second), "c:2", r10, 50), otpRow(next.Add(80*time.Second), "c:2", r10, 150))

	d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), plan)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(d.OriginOTP)
	want := `{"at_m":100,"max_gap_s":60,"late_s":300,"runs":10,"measured":6,"on_time":4,"early":1,"late":1,"share":0.667,"band":"\u003c70","by_line":[` +
		`{"line":"c:1","runs":7,"measured":4,"on_time":2,"early":1,"late":1,"share":0.5},` +
		`{"line":"c:2","runs":2,"measured":2,"on_time":2,"early":0,"late":0,"share":1},` +
		`{"line":"c:3","runs":1,"measured":0,"on_time":0,"early":0,"late":0}]}`
	if string(got) != want {
		t.Fatalf("origin_otp\n got %s\nwant %s", got, want)
	}

	// No run code in the rows: no block.
	plan.RunDeparture = func(string) (int64, bool) { return 0, false }
	if d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), plan); err != nil || d.OriginOTP != nil {
		t.Fatalf("no runs: %+v, %v", d.OriginOTP, err)
	}
}

package gtfs

import (
	"reflect"
	"testing"
	"time"
)

func load(t *testing.T) *Feed {
	t.Helper()
	f, err := LoadZip(writeZip(t, sampleFiles()), LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestLoadZipBasics(t *testing.T) {
	f := load(t)
	if len(f.Trips) != 3 {
		t.Fatalf("trips = %d, want 3 (single-stop trip dropped)", len(f.Trips))
	}
	if got := f.Stops[f.StopIndex("A")].Name; got != "ΠΕΙΡΑΙΑΣ" {
		t.Fatalf("BOM header not handled: stop A name %q", got)
	}
	// stop_code is the code on the stop sign («ΚΣ» in OASA notices), not the stop_id.
	if got := f.Stops[f.StopIndex("B")].Code; got != "040054" {
		t.Fatalf("stop B code %q", got)
	}
	if got := f.StopsByCode("040054"); !reflect.DeepEqual(got, []int32{f.StopIndex("B")}) {
		t.Fatalf("StopsByCode(040054) = %v", got)
	}
	if got := f.StopsByCode("01"); !reflect.DeepEqual(got, []int32{f.StopIndex("A"), f.StopIndex("C")}) {
		t.Fatalf("a code shared by two stops: %v", got)
	}
	if f.StopsByCode("B") != nil || f.StopsByCode("") != nil {
		t.Fatal("a stop_id or an empty code is no stop code")
	}
	t1 := f.Trip(f.TripIndex("t1"))
	if f.Headsign(t1) != "ΣΥΝΤΑΓΜΑ" {
		t.Fatalf("headsign %q not trimmed", f.Headsign(t1))
	}
	var ids []string
	for i := 0; i < f.NumStops(t1); i++ {
		st := f.StopTime(t1, i)
		ids = append(ids, f.Stops[st.Stop].ID)
	}
	if ids[0] != "A" || ids[1] != "B" || ids[2] != "C" {
		t.Fatalf("stop order %v, want A B C", ids)
	}
	if st := f.StopTime(t1, 0); st.Arr != 36000 || st.Dep != 36060 || st.Seq != 1 {
		t.Fatalf("first stop time %+v", st)
	}
	if f.Start(t1) != 36060 || f.End(t1) != 36600 {
		t.Fatalf("start/end %d %d", f.Start(t1), f.End(t1))
	}
	n1 := f.Trip(f.TripIndex("n1"))
	if f.Start(n1) != 24*3600+20*60 {
		t.Fatalf("24:20:00 parsed as %d", f.Start(n1))
	}
	// Shape points sorted by sequence.
	s := f.Shapes[f.ShapeIndex("S1")]
	if s.Points[0][0] != 37.94 || s.Points[2][0] != 37.96 {
		t.Fatalf("shape order %v", s.Points)
	}
	r := f.Routes[t1.Route]
	if r.ShortName != "040" || r.Type != 3 || r.Color != "153CE0" {
		t.Fatalf("route %+v", r)
	}
}

func TestPatternAndProfileDedup(t *testing.T) {
	f := load(t)
	t1, t2 := f.Trip(f.TripIndex("t1")), f.Trip(f.TripIndex("t2"))
	if t1.Pattern != t2.Pattern {
		t.Fatal("same stops should share a pattern")
	}
	if t1.Profile != t2.Profile {
		t.Fatal("same relative times should share a profile")
	}
	if len(f.Patterns) != 2 {
		t.Fatalf("patterns = %d, want 2", len(f.Patterns))
	}
}

func TestServiceActive(t *testing.T) {
	f := load(t)
	svc := f.Trip(f.TripIndex("t1")).Service
	day := func(s string) time.Time {
		d, _ := time.ParseInLocation("2006-01-02", s, Athens)
		return d
	}
	cases := map[string]bool{
		"2026-10-05": true,  // Monday
		"2026-10-06": false, // Tuesday
		"2026-07-13": false, // Monday removed by exception
		"2026-07-15": true,  // Wednesday added by exception
		"2026-10-12": true,  // Monday after end_date: an expired feed still runs by weekday
		"2026-10-13": false, // Tuesday after end_date
		"2026-06-29": true,  // Monday before start_date
	}
	for d, want := range cases {
		if got := f.ServiceActive(svc, day(d)); got != want {
			t.Errorf("%s: active=%v want %v", d, got, want)
		}
	}
	if got := f.FeedEnd().Format("2006-01-02"); got != "2026-10-06" {
		t.Errorf("feed end %s", got)
	}
}

// OSETH (Thessaloniki) publishes feeds with an empty calendar.txt: every service is a list of
// dates in calendar_dates.txt. When such a feed expires, the same weekday inside it must still
// say which lines run (as for a feed with a calendar), or no bus is polled until the next feed.
func TestServiceActiveCalendarLess(t *testing.T) {
	files := sampleFiles()
	files["calendar.txt"] = "service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date\n"
	files["calendar_dates.txt"] = "service_id,date,exception_type\n" +
		"day_1,20261005,1\n" + // Monday
		"day_1,20261012,1\n" + // Monday
		"day_1,20261014,1\n" + // Wednesday
		"day_1,20261013,2\n" // Tuesday removed (no effect: not added)
	f, err := LoadZip(writeZip(t, files), LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	svc := f.Trip(f.TripIndex("t1")).Service
	day := func(s string) time.Time {
		d, _ := time.ParseInLocation("2006-01-02", s, Athens)
		return d
	}
	cases := map[string]bool{
		"2026-10-05": true,  // Monday listed
		"2026-10-06": false, // Tuesday inside the feed, not listed
		"2026-10-14": true,  // Wednesday listed
		"2026-10-19": true,  // Monday after the feed: the Monday of 12 Oct stands in
		"2026-10-26": true,  // Monday two weeks after
		"2026-10-20": false, // Tuesday after the feed: 13 Oct has no service
		"2026-10-21": true,  // Wednesday after the feed: 14 Oct
		"2026-09-28": true,  // Monday before the feed: 5 Oct
	}
	for d, want := range cases {
		if got := f.ServiceActive(svc, day(d)); got != want {
			t.Errorf("%s: active=%v want %v", d, got, want)
		}
	}
	if got := f.FeedStart().Format("2006-01-02"); got != "2026-10-05" {
		t.Errorf("feed start %s, want the first listed date", got)
	}
	if got := f.FeedEnd().Format("2006-01-02"); got != "2026-10-14" {
		t.Errorf("feed end %s, want the last listed date", got)
	}
	if !f.Expired(day("2026-10-19").Add(12 * time.Hour)) {
		t.Error("19 Oct is after the feed: Expired must be true")
	}
}

func TestCandidateTrips(t *testing.T) {
	f := load(t)
	// Tuesday 00:30: n1 belongs to Monday's service day and runs 24:20-24:40.
	now := time.Date(2026, 10, 6, 0, 30, 0, 0, Athens)
	var got []string
	for _, c := range f.CandidateTrips("2", now, 15*60, 60*60) {
		got = append(got, f.Trip(c.Trip).ID+"@"+c.Day.Format("0102"))
	}
	if len(got) != 1 || got[0] != "n1@1005" {
		t.Fatalf("candidates %v, want [n1@1005]", got)
	}
	// Monday 10:30: t1 (10:01-10:10, +60 min after) and t2 (11:01, -15 min before? no: 10:46).
	now = time.Date(2026, 10, 5, 10, 30, 0, 0, Athens)
	got = nil
	for _, c := range f.CandidateTrips("040", now, 15*60, 60*60) {
		got = append(got, f.Trip(c.Trip).ID)
	}
	if len(got) != 1 || got[0] != "t1" {
		t.Fatalf("candidates %v, want [t1]", got)
	}
}

func TestLinesFilter(t *testing.T) {
	f, err := LoadZip(writeZip(t, sampleFiles()), LoadOptions{Lines: map[string]bool{"2": true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Trips) != 1 || f.Trip(0).ID != "n1" {
		t.Fatalf("filtered trips %d", len(f.Trips))
	}
}

// GTFS stop times count from "noon minus 12 h" of the service day, so on DST change days a
// 10:00:00 trip still runs at 10:00 on the wall clock (as upstream's wall-clock arithmetic does).
func TestServiceDayAcrossDST(t *testing.T) {
	f, err := LoadZip(writeZip(t, map[string]string{
		"stops.txt":  "stop_id,stop_name,stop_lat,stop_lon\nA,A,37.94,23.64\nB,B,37.95,23.65\n",
		"routes.txt": "route_id,route_short_name,route_long_name,route_type\nR,D,A - B,3\n",
		"trips.txt":  "route_id,service_id,trip_id\nR,sun,d1\nR,sun,n1\n",
		"stop_times.txt": "trip_id,arrival_time,departure_time,stop_id,stop_sequence\n" +
			"d1,10:00:00,10:00:00,A,1\nd1,10:10:00,10:10:00,B,2\n" +
			"n1,23:50:00,23:50:00,A,1\nn1,24:20:00,24:20:00,B,2\n",
		"calendar.txt": "service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date\n" +
			"sun,0,0,0,0,0,0,1,20260101,20271231\n",
	}), LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		now      time.Time
		trip     string
		date     string
		secsWant int
	}{
		{time.Date(2026, 10, 25, 10, 5, 0, 0, Athens), "d1", "20261025", 10*3600 + 300}, // EEST -> EET
		{time.Date(2027, 3, 28, 10, 5, 0, 0, Athens), "d1", "20270328", 10*3600 + 300},  // EET -> EEST
		{time.Date(2026, 10, 26, 0, 10, 0, 0, Athens), "n1", "20261025", 24*3600 + 600}, // after midnight
		{time.Date(2027, 3, 29, 0, 10, 0, 0, Athens), "n1", "20270328", 24*3600 + 600},
	} {
		cands := f.CandidateTrips("D", tc.now, 15*60, 0)
		if len(cands) != 1 || f.Trip(cands[0].Trip).ID != tc.trip {
			t.Fatalf("%v: %d candidates", tc.now, len(cands))
		}
		c := cands[0]
		if got := ServiceDate(c.Day); got != tc.date {
			t.Errorf("%v: service date %s, want %s", tc.now, got, tc.date)
		}
		if secs := int(tc.now.Sub(c.Day) / time.Second); secs != tc.secsWant {
			t.Errorf("%v: %d s into the service day, want %d", tc.now, secs, tc.secsWant)
		}
	}
}

// ShapesAround ignores the service day: a weekly variant the GTFS puts on the wrong weekday
// still counts at its time of day (#20). t1 (shape S1) runs Mondays 10:00-10:10.
func TestShapesAround(t *testing.T) {
	f := load(t)
	s1 := int32(-1)
	for i := range f.Shapes {
		if f.Shapes[i].ID == "S1" {
			s1 = int32(i)
		}
	}
	for _, c := range []struct {
		line string
		now  time.Time
		want bool
	}{
		{"040", time.Date(2026, 10, 7, 10, 30, 0, 0, Athens), true}, // Wednesday, t1's time of day
		{"040", time.Date(2026, 10, 7, 14, 0, 0, 0, Athens), false}, // Wednesday, long after t1
		{"040", time.Date(2026, 10, 7, 9, 30, 0, 0, Athens), false}, // before t1's padded start
		{"2", time.Date(2026, 10, 8, 0, 30, 0, 0, Athens), true},    // Thursday 00:30: n1 runs 24:20-24:40
		{"2", time.Date(2026, 10, 7, 22, 0, 0, 0, Athens), false},   // Wednesday 22:00
	} {
		if got := f.ShapesAround(c.line, c.now, 15*60, 60*60)[s1]; got != c.want {
			t.Errorf("%s at %s: %v, want %v", c.line, c.now.Format("Mon 15:04"), got, c.want)
		}
	}
}

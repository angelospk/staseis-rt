package stats

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/gtfs/gtfstest"
)

// trow is one matched history row of trip (vehicle "v"+trip) at sm metres along its shape.
func trow(at time.Time, line, trip string, sm int, gtfsVer string) string {
	return fmt.Sprintf("%d,%s,R,v%s,37980000,23700000,%s,%s,SH,%d,0\n", at.Unix(), line, trip, gtfsVer, trip, sm)
}

func writeRows(t *testing.T, dir string, hour time.Time, rows ...string) {
	t.Helper()
	out, err := os.Create(filepath.Join(dir, hourName(hour)+".csv.gz"))
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(out)
	zw.Write([]byte(oldHeader + strings.Join(rows, "")))
	zw.Close()
	out.Close()
}

func tripIDs(p *Plan) string {
	var ids []string
	for _, tr := range p.Trips {
		ids = append(ids, tr.ID)
	}
	return strings.Join(ids, ",")
}

func TestNewPlanTakesTripsStartingInTheDay(t *testing.T) {
	f := gtfstest.StraightLine{Stops: 5, Trips: 3, First: 8 * 3600, Headway: 60, Leg: 2}.Feed(t)
	p, err := NewPlan(f, "v1", "2026-09-28", athens) // a Monday inside the feed
	if err != nil {
		t.Fatal(err)
	}
	if p.GTFS != "v1" || p.Expired || tripIDs(p) != "T00,T01,T02" {
		t.Fatalf("plan = %+v", p)
	}
	tr := p.Trips[0]
	if tr.Line != "L" || tr.Start != local(2026, 9, 28, 8, 0, 0).Unix() || tr.End != local(2026, 9, 28, 8, 8, 0).Unix() {
		t.Fatalf("trip = %+v", tr)
	}
	// Four legs of 0.01 deg of longitude at 37.98 N: about 3,506 m of shape between the end stops.
	if tr.FromM > 1 || tr.ToM < 3450 || tr.ToM > 3560 {
		t.Fatalf("shape span = %v..%v", tr.FromM, tr.ToM)
	}
	if p, err := NewPlan(f, "v1", "2026-09-29", athens); err != nil || len(p.Trips) != 0 {
		t.Fatalf("Tuesday: %+v, %v", p, err)
	}
	// After the feed's end the same weekday inside it decides.
	p, err = NewPlan(f, "v1", "2026-10-26", athens)
	if err != nil || !p.Expired || tripIDs(p) != "T00,T01,T02" || p.Trips[0].Start != local(2026, 10, 26, 8, 0, 0).Unix() {
		t.Fatalf("expired: %+v, %v", p, err)
	}
	if _, err := NewPlan(f, "v1", "2026-9-28", athens); err == nil {
		t.Fatal("bad date accepted")
	}
}

func TestNewPlanPutsTripsAfterMidnightOnTheNextDay(t *testing.T) {
	// T00 at 23:50 and T01 at 24:10, both on Monday's service.
	f := gtfstest.StraightLine{Stops: 3, Trips: 2, First: 23*3600 + 50*60, Headway: 20, Leg: 5}.Feed(t)
	mon, err := NewPlan(f, "v", "2026-09-28", athens)
	if err != nil || tripIDs(mon) != "T00" {
		t.Fatalf("Monday: %+v, %v", mon, err)
	}
	tue, err := NewPlan(f, "v", "2026-09-29", athens)
	if err != nil || tripIDs(tue) != "T01" || tue.Trips[0].Start != local(2026, 9, 29, 0, 10, 0).Unix() {
		t.Fatalf("Tuesday: %+v, %v", tue, err)
	}
}

// tripsDay writes the history of 2026-10-06 used by the Compute tests below.
func tripsDay(t *testing.T) (dir string, plan *Plan) {
	dir = t.TempDir()
	at := func(h, m int) time.Time { return local(2026, 10, 6, h, m, 0) }
	writeRows(t, dir, at(9, 0), trow(at(9, 0), "L", "", -1, "g")) // unmatched
	writeRows(t, dir, at(10, 0),
		trow(at(10, 5), "L", "B", 0, "g"),
		trow(at(10, 10), "L", "B", 1000, "g"),
		trow(at(10, 15), "L", "B", 3900, "old")) // another feed version: ignored
	writeRows(t, dir, at(12, 0), trow(at(12, 5), "L", "D", -1, "g"))
	writeRows(t, dir, at(13, 0), trow(at(13, 0), "L", "C", 2000, "g")) // later than TripSlack after C's end
	writeRows(t, dir, at(23, 0), trow(at(23, 40), "L", "A", 100, "g"))
	next := local(2026, 10, 7, 0, 10, 0)
	writeRows(t, dir, next, trow(next, "L", "A", 5000, "g")) // past the shape's end: counts to ToM
	trip := func(id, line string, h, m int, from, to float64) PlanTrip {
		return PlanTrip{ID: id, Line: line, Start: at(h, m).Unix(), End: at(h, m+25).Unix(), FromM: from, ToM: to}
	}
	plan = &Plan{GTFS: "g", Expired: true, Trips: []PlanTrip{
		trip("B", "L", 10, 0, 0, 4000),
		trip("C", "L", 11, 0, 0, 4000),
		trip("D", "L", 12, 0, 0, 0), // no usable shape
		trip("E", "M", 14, 0, 0, 4000),
		trip("A", "L", 23, 30, 0, 4000),
	}}
	return dir, plan
}

func TestComputeCountsTripsByCoverage(t *testing.T) {
	dir, plan := tripsDay(t)
	d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), plan)
	if err != nil {
		t.Fatal(err)
	}
	// The row after midnight is A's, not a fix of the 6th.
	if d.Fixes != 7 || d.Files != 6 {
		t.Fatalf("fixes = %d, files = %d", d.Fixes, d.Files)
	}
	want := &Trips{GTFS: "g", GTFSExpired: true, ScheduleSource: SourceGTFS, MinCoverage: MinCoverage,
		TripCount: TripCount{Planned: 5, Run: 1, Partial: 1, Missed: 2, Unmeasured: 1, Vanished: 1},
		ByLine: []TripLine{
			{Line: "L", ScheduleSource: SourceGTFS, TripCount: TripCount{Planned: 4, Run: 1, Partial: 1, Missed: 1, Unmeasured: 1, Vanished: 1}, MissedTrips: []string{"C"}},
			{Line: "M", ScheduleSource: SourceGTFS, TripCount: TripCount{Planned: 1, Missed: 1}, MissedTrips: []string{"E"}},
		}}
	if !reflect.DeepEqual(d.Trips, want) {
		got, _ := json.Marshal(d.Trips)
		t.Fatalf("trips = %s", got)
	}
}

func TestComputeTripsNeedTheFeedOfTheRows(t *testing.T) {
	dir, plan := tripsDay(t)
	plan.GTFS = "new" // most matched rows are of feed "g"
	d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), plan)
	if err != nil || d.Trips != nil {
		t.Fatalf("trips = %+v, err %v", d.Trips, err)
	}
	d, err = Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), nil)
	if err != nil || d.Trips != nil {
		t.Fatalf("no plan: trips = %+v, err %v", d.Trips, err)
	}
	if b, _ := json.Marshal(d); strings.Contains(string(b), `"trips"`) {
		t.Fatalf("day without a plan has trips: %s", b)
	}
}

func TestRunAddsTripsAndRetriesWithoutAPlan(t *testing.T) {
	hist, out := t.TempDir(), t.TempDir()
	t0 := local(2026, 10, 6, 12, 0, 0)
	writeRows(t, hist, t0, trow(t0, "L", "T", 0, "g"), trow(t0.Add(10*time.Minute), "L", "T", 900, "g"))
	now := local(2026, 10, 7, 5, 0, 0)
	noFeed := func(string) (*Plan, error) { return nil, errors.New("no feed yet") }
	if got, err := Run(hist, out, athens, now, false, noFeed); err == nil || len(got) != 0 {
		t.Fatalf("without a plan: wrote %v, err %v", got, err)
	}
	plan := func(date string) (*Plan, error) {
		if date != "2026-10-06" {
			t.Errorf("plan for %s", date)
		}
		return &Plan{GTFS: "g", Trips: []PlanTrip{{ID: "T", Line: "L", Start: t0.Unix(),
			End: t0.Add(20 * time.Minute).Unix(), ToM: 1000}}}, nil
	}
	if got, err := Run(hist, out, athens, now, false, plan); err != nil || len(got) != 1 {
		t.Fatalf("with a plan: wrote %v, err %v", got, err)
	}
	var d Day
	readJSON(t, filepath.Join(out, "days", "2026-10-06.json"), &d)
	if d.Trips == nil || d.Trips.Planned != 1 || d.Trips.Run != 1 {
		t.Fatalf("trips = %+v", d.Trips)
	}
}

func TestSettleWaitsForTheLastTrips(t *testing.T) {
	if Settle < TripsAfter+time.Hour {
		t.Fatalf("Settle %v leaves no hour to gzip the files TripsAfter %v reads", Settle, TripsAfter)
	}
}

func TestNewPlanKeepsTripsAcrossSpringForward(t *testing.T) {
	// On 2026-03-29 (Sunday) clocks go from 03:00 to 04:00, so GTFS times of Sunday's service
	// count from 23:00 on Saturday: a trip at 00:30 starts at 23:30 on Saturday.
	files := gtfstest.StraightLine{Stops: 3, Trips: 1, First: 30 * 60, Headway: 10, Leg: 5}.Files()
	files["calendar.txt"] = "service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date\n" +
		"sun,0,0,0,0,0,0,1,20260301,20260430\n"
	files["trips.txt"] = strings.ReplaceAll(files["trips.txt"], "day_1", "sun")
	f, err := gtfs.LoadZip(gtfstest.Zip(t, files), gtfs.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sat, err := NewPlan(f, "v", "2026-03-28", athens)
	if err != nil || tripIDs(sat) != "T00" || sat.Trips[0].Start != local(2026, 3, 28, 23, 30, 0).Unix() {
		t.Fatalf("Saturday: %+v, %v", sat, err)
	}
	if sun, err := NewPlan(f, "v", "2026-03-29", athens); err != nil || len(sun.Trips) != 0 {
		t.Fatalf("Sunday: %+v, %v", sun, err)
	}
}

func TestComputeTripsOfADayWithNoMatchedRow(t *testing.T) {
	// Every vehicle unmatched, but the rows are of the plan's feed: no trip ran, so 0 of 1.
	dir := t.TempDir()
	t0 := local(2026, 10, 6, 12, 0, 0)
	writeRows(t, dir, t0, trow(t0, "L", "", -1, "g"))
	// Matched rows of another feed after midnight do not decide the day's feed.
	next := local(2026, 10, 7, 0, 30, 0)
	writeRows(t, dir, next, trow(next, "L", "X", 0, "old"), trow(next.Add(time.Minute), "L", "X", 10, "old"))
	plan := &Plan{GTFS: "g", Trips: []PlanTrip{{ID: "T", Line: "L", Start: t0.Unix(), End: t0.Add(20 * time.Minute).Unix(), ToM: 1000}}}
	d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), plan)
	if err != nil || d.Trips == nil || d.Trips.Planned != 1 || d.Trips.Missed != 1 {
		t.Fatalf("trips = %+v, err %v", d.Trips, err)
	}
}

// vrow is a matched history row of vehicle veh on GTFS trip at sm metres along its shape.
func vrow(at time.Time, line, veh, trip string, sm int, gtfsVer string) string {
	return fmt.Sprintf("%d,%s,R,%s,37980000,23700000,%s,%s,SH,%d,0\n", at.Unix(), line, veh, gtfsVer, trip, sm)
}

// The winter timetable of OASA's daily schedule differs from the summer GTFS: line L leaves at
// 10:00 and 10:40 (the GTFS has 10:20 and 11:00). The bus the matcher put on the GTFS trip of
// 10:20 left at 10:41: it ran the 10:40 departure, which the GTFS lacks. The GTFS trip of 11:00,
// which OASA no longer schedules, is not missed: it is not planned at all.
func TestComputeCountsTheDailyScheduleRuns(t *testing.T) {
	dir := t.TempDir()
	at := func(h, m int) time.Time { return local(2026, 10, 6, h, m, 0) }
	writeRows(t, dir, at(10, 0),
		vrow(at(10, 30), "L", "v1", "S1020", 50, "g"),  // waiting at the stand
		vrow(at(10, 41), "L", "v1", "S1020", 200, "g"), // the last fix near the start: it leaves
		vrow(at(10, 50), "L", "v1", "S1020", 2500, "g"),
		vrow(at(10, 55), "L", "v2", "M1", 3000, "g"), // line M keeps its GTFS plan
	)
	plan := &Plan{GTFS: "g", Expired: true, Trips: []PlanTrip{
		{ID: "S1020", Line: "L", Start: at(10, 20).Unix(), End: at(10, 45).Unix(), ToM: 4000},
		{ID: "S1100", Line: "L", Start: at(11, 0).Unix(), End: at(11, 25).Unix(), ToM: 4000},
		{ID: "M1", Line: "M", Start: at(10, 30).Unix(), End: at(11, 0).Unix(), ToM: 4000},
	}}
	plan.UseDaily([]DailyDir{{Line: "L", Dir: 0, Starts: []int64{at(10, 0).Unix(), at(10, 40).Unix()},
		Trips: map[string]Span{"S1020": {0, 4000}, "S1100": {0, 4000}}}})
	d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), plan)
	if err != nil {
		t.Fatal(err)
	}
	want := &Trips{GTFS: "g", GTFSExpired: true, ScheduleSource: SourceMixed, MinCoverage: MinCoverage,
		TripCount: TripCount{Planned: 3, Run: 1, Partial: 1, Missed: 1, Vanished: 1},
		ByLine: []TripLine{
			{Line: "L", ScheduleSource: SourceDaily, TripCount: TripCount{Planned: 2, Run: 1, Missed: 1, Vanished: 1}, MissedTrips: []string{"go 10:00"}},
			{Line: "M", ScheduleSource: SourceGTFS, TripCount: TripCount{Planned: 1, Partial: 1}},
		}}
	if !reflect.DeepEqual(d.Trips, want) {
		got, _ := json.Marshal(d.Trips)
		t.Fatalf("trips = %s", got)
	}
}

// Pairing: each run takes, in time order, the first unused bus that left from RunEarly before to
// RunLate after it; a bus seen only mid-route left at its first fix. Runs of a direction without
// any GTFS shape are unmeasured; a run whose bus has no usable shape too.
func TestDailyRunsPairWithBusesInTimeOrder(t *testing.T) {
	dir := t.TempDir()
	at := func(h, m int) time.Time { return local(2026, 10, 6, h, m, 0) }
	writeRows(t, dir, at(8, 0),
		vrow(at(8, 2), "L", "a", "T1", 100, "g"), // left 08:02: the 08:00 run
		vrow(at(8, 20), "L", "a", "T1", 3900, "g"),
		vrow(at(8, 25), "L", "b", "T2", 2000, "g"), // first seen mid-route at 08:25: the 08:10 run (late)
		vrow(at(8, 35), "L", "b", "T2", 2100, "g"), // covers little: partial
		vrow(at(8, 51), "L", "c", "T3", 100, "g"),  // 08:51 is past RunLate of 08:20: not the 08:20 run
		vrow(at(9, 5), "L", "c", "T3", 3900, "g"),
		vrow(at(8, 30), "L", "d", "X9", 100, "g"), // a trip of no daily direction: ignored
		vrow(at(8, 40), "L", "d", "X9", 3900, "g"),
		vrow(at(8, 40), "N", "e", "U1", 100, "g"), // a bus on a trip without a shape
	)
	plan := &Plan{GTFS: "g", Expired: true, Trips: []PlanTrip{}}
	span := Span{0, 4000}
	plan.UseDaily([]DailyDir{
		{Line: "L", Dir: 0, Starts: []int64{at(8, 0).Unix(), at(8, 10).Unix(), at(8, 20).Unix()},
			Trips: map[string]Span{"T1": span, "T2": span, "T3": span}},
		{Line: "L", Dir: 1, Starts: []int64{at(9, 0).Unix()}}, // no shape matches this direction
		{Line: "N", Dir: 0, Starts: []int64{at(8, 40).Unix()}, Trips: map[string]Span{"U1": {}}},
	})
	d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), plan)
	if err != nil {
		t.Fatal(err)
	}
	want := &Trips{GTFS: "g", GTFSExpired: true, ScheduleSource: SourceDaily, MinCoverage: MinCoverage,
		TripCount: TripCount{Planned: 5, Run: 1, Partial: 1, Missed: 1, Unmeasured: 2, Vanished: 1},
		ByLine: []TripLine{
			{Line: "L", ScheduleSource: SourceDaily, TripCount: TripCount{Planned: 4, Run: 1, Partial: 1, Missed: 1, Unmeasured: 1, Vanished: 1}, MissedTrips: []string{"go 08:20"}},
			{Line: "N", ScheduleSource: SourceDaily, TripCount: TripCount{Planned: 1, Unmeasured: 1}},
		}}
	if !reflect.DeepEqual(d.Trips, want) {
		got, _ := json.Marshal(d.Trips)
		t.Fatalf("trips = %s", got)
	}
}

// A GTFS trip in two directions (a loop whose Go and Come both match every shape) cannot tell
// which run its bus did: it pairs with none, and a direction with no other usable trip is
// unmeasured, not missed. Trips without a usable span do not make a direction measurable.
func TestDailyAmbiguousOrShapelessTripsAreUnmeasured(t *testing.T) {
	dir := t.TempDir()
	at := func(h, m int) time.Time { return local(2026, 10, 6, h, m, 0) }
	writeRows(t, dir, at(8, 0), vrow(at(8, 2), "L", "a", "T1", 100, "g"), vrow(at(8, 20), "L", "a", "T1", 3900, "g"))
	plan := &Plan{GTFS: "g", Expired: true, Trips: []PlanTrip{}}
	span := Span{0, 4000}
	plan.UseDaily([]DailyDir{
		{Line: "L", Dir: 0, Starts: []int64{at(8, 0).Unix()}, Trips: map[string]Span{"T1": span}},
		{Line: "L", Dir: 1, Starts: []int64{at(9, 0).Unix()}, Trips: map[string]Span{"T1": span}},
		{Line: "N", Dir: 0, Starts: []int64{at(9, 0).Unix()}, Trips: map[string]Span{"U1": {}}},
	})
	d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), plan)
	if err != nil {
		t.Fatal(err)
	}
	if d.Trips.Planned != 3 || d.Trips.Unmeasured != 3 {
		got, _ := json.Marshal(d.Trips)
		t.Fatalf("trips = %s", got)
	}
}

// Real rows of bus 44552 on line 550 on 2026-10-27 (thinned; trip ids without "959_day_2_").
// At the stand and on the way the matcher moves the bus between trips of the same direction,
// so one bus run has rows on many trip ids (#131). It left at 08:04 (the 08:00 go run, shape 139)
// and covered the whole route; at 09:17 it was at the first stop of the come direction (shape
// 245) for the 09:30 come run: it left at 09:40 and was seen up to 3.5 km.
func TestDailyRunSpansTheTripIdsOfOneBus(t *testing.T) {
	dir := t.TempDir()
	at := func(hms string) time.Time {
		var h, m, s int
		fmt.Sscanf(hms, "%d:%d:%d", &h, &m, &s)
		return local(2026, 10, 27, h, m, s)
	}
	rows := []struct {
		at, trip string
		sm       int
	}{
		{"07:28:50", "1546_0740", 0}, {"07:33:14", "1546_0740", 21}, {"07:39:42", "1546_0740", 22},
		{"07:40:23", "1553_0750", 41}, {"07:45:48", "1553_0750", 40}, {"07:52:16", "1553_0750", 41},
		{"07:53:39", "1554_0800", 47}, {"07:59:27", "1554_0800", 44},
		{"08:00:28", "1547_0815", 45}, {"08:03:53", "1547_0815", 62},
		{"08:04:36", "1554_0800", 239}, {"08:06:16", "1554_0800", 842}, {"08:13:02", "1554_0800", 2746},
		{"08:22:26", "1554_0800", 5279}, {"08:29:41", "1554_0800", 8042}, {"08:39:42", "1554_0800", 10722},
		{"08:49:26", "1554_0800", 12877}, {"08:51:09", "1550_0715", 13537}, {"08:53:22", "1550_0715", 13820},
		{"08:54:31", "1546_0740", 14398}, {"08:55:22", "1546_0740", 14511}, {"08:57:10", "1554_0800", 14685},
		{"09:04:00", "1554_0800", 16159}, {"09:11:34", "1554_0800", 18429}, {"09:15:12", "1554_0800", 20252},
		{"09:17:42", "1560_0821", 70}, {"09:19:24", "1560_0821", 29},
		{"09:20:46", "1546_0925", 28}, {"09:25:51", "1546_0925", 0}, {"09:29:36", "1546_0925", 0},
		{"09:30:17", "1553_0935", 0}, {"09:34:39", "1553_0935", 39}, {"09:39:47", "1553_0935", 78},
		{"09:40:40", "1553_0935", 236}, {"09:41:39", "1553_0935", 503}, {"09:44:36", "1553_0935", 1846},
		{"09:49:40", "1553_0935", 3536},
	}
	var csv []string
	for _, r := range rows {
		csv = append(csv, vrow(at(r.at), "550", "44552", r.trip, r.sm, "g"))
	}
	writeRows(t, dir, at("07:00:00"), csv...)
	s139, s245 := Span{23.2, 20781.3}, Span{0, 20798.3}
	plan := &Plan{GTFS: "g", Expired: true, Trips: []PlanTrip{}}
	plan.UseDaily([]DailyDir{
		{Line: "550", Dir: 0, Starts: []int64{at("08:00:00").Unix()}, Trips: map[string]Span{"1546_0740": s139,
			"1550_0715": s139, "1553_0750": s139, "1554_0800": s139, "1547_0815": s139}},
		{Line: "550", Dir: 1, Starts: []int64{at("09:30:00").Unix()}, Trips: map[string]Span{"1560_0821": s245,
			"1546_0925": s245, "1553_0935": s245}},
	})
	d, err := Compute("2026-10-27", athens, inputs(t, dir, "2026-10-27"), time.Unix(0, 0), plan)
	if err != nil {
		t.Fatal(err)
	}
	// The rows end with the bus 17% along its 09:30 run: vanished.
	if got := d.Trips.TripCount; got != (TripCount{Planned: 2, Run: 1, Partial: 1, Vanished: 1}) {
		t.Fatalf("trips = %+v, want the 08:00 run and the 09:30 partial", got)
	}
}

// A bus first seen mid-route twice in a day, the second time not far behind its last fix, made
// two runs: more than TripSlack without a fix of it on the direction ends a run.
func TestDailyRunEndsAfterALongGap(t *testing.T) {
	dir := t.TempDir()
	at := func(h, m int) time.Time { return local(2026, 10, 6, h, m, 0) }
	writeRows(t, dir, at(8, 0),
		vrow(at(8, 10), "L", "a", "T1", 1000, "g"), vrow(at(8, 40), "L", "a", "T1", 3900, "g"),
		vrow(at(10, 10), "L", "a", "T2", 2500, "g"), vrow(at(10, 40), "L", "a", "T2", 3900, "g"),
	)
	plan := &Plan{GTFS: "g", Expired: true, Trips: []PlanTrip{}}
	span := Span{0, 4000}
	plan.UseDaily([]DailyDir{{Line: "L", Dir: 0, Starts: []int64{at(8, 0).Unix(), at(10, 0).Unix()},
		Trips: map[string]Span{"T1": span, "T2": span}}})
	d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Trips.TripCount; got != (TripCount{Planned: 2, Run: 1, Partial: 1}) {
		t.Fatalf("trips = %+v", got)
	}
}

// A bus that runs go, come and go again on a short line, with less than TripSlack between its go
// fixes, made two go runs: it was back at the first stop after it had left it.
func TestDailyRunEndsBackAtTheFirstStop(t *testing.T) {
	dir := t.TempDir()
	at := func(h, m int) time.Time { return local(2026, 10, 6, h, m, 0) }
	writeRows(t, dir, at(8, 0),
		vrow(at(8, 0), "L", "a", "G1", 50, "g"), vrow(at(8, 2), "L", "a", "G1", 400, "g"), vrow(at(8, 18), "L", "a", "G1", 3900, "g"),
		vrow(at(8, 24), "L", "a", "C1", 50, "g"), vrow(at(8, 40), "L", "a", "C1", 3900, "g"),
		vrow(at(8, 48), "L", "a", "G2", 50, "g"), vrow(at(8, 51), "L", "a", "G1", 400, "g"), vrow(at(9, 8), "L", "a", "G2", 3900, "g"),
	)
	plan := &Plan{GTFS: "g", Expired: true, Trips: []PlanTrip{}}
	span := Span{0, 4000}
	plan.UseDaily([]DailyDir{
		{Line: "L", Dir: 0, Starts: []int64{at(8, 0).Unix(), at(8, 50).Unix()}, Trips: map[string]Span{"G1": span, "G2": span}},
		{Line: "L", Dir: 1, Starts: []int64{at(8, 25).Unix()}, Trips: map[string]Span{"C1": span}},
	})
	d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Trips.TripCount; got != (TripCount{Planned: 3, Run: 3}) {
		t.Fatalf("trips = %+v", got)
	}
}

// A bus on a loop passed the first stop with no fix near it: a fix far behind the previous one
// started a new run.
func TestDailyRunEndsWhenTheBusJumpsBack(t *testing.T) {
	dir := t.TempDir()
	at := func(h, m int) time.Time { return local(2026, 10, 6, h, m, 0) }
	writeRows(t, dir, at(8, 0),
		vrow(at(8, 0), "L", "a", "G1", 50, "g"), vrow(at(8, 10), "L", "a", "G1", 2000, "g"), vrow(at(8, 20), "L", "a", "G1", 3800, "g"),
		vrow(at(8, 22), "L", "a", "G2", 500, "g"), vrow(at(8, 30), "L", "a", "G2", 2000, "g"), vrow(at(8, 40), "L", "a", "G1", 3900, "g"),
	)
	plan := &Plan{GTFS: "g", Expired: true, Trips: []PlanTrip{}}
	span := Span{0, 4000}
	plan.UseDaily([]DailyDir{{Line: "L", Dir: 0, Starts: []int64{at(8, 0).Unix(), at(8, 20).Unix()},
		Trips: map[string]Span{"G1": span, "G2": span}}})
	d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Trips.TripCount; got != (TripCount{Planned: 2, Run: 2}) {
		t.Fatalf("trips = %+v", got)
	}
}

// Shares of the route are clamped to [0, 1], and a late copy of a fix of an earlier run (a
// restart) is dropped. Line A: a fix before the first stop counts as the start, so the bus
// covered half the route. Line B: a fix past the last stop is the end, not 1.2 of the route, so
// the next fix at 0.71 is no jump back and makes no extra run to take the 08:30 start from bus b.
// Line C: the copy of the 08:20 fix, read after the 09:00 run began, does not extend that run.
func TestDailyRunSharesAreClampedAndOldRowsDropped(t *testing.T) {
	dir := t.TempDir()
	at := func(h, m int) time.Time { return local(2026, 10, 6, h, m, 0) }
	writeRows(t, dir, at(8, 0),
		vrow(at(8, 0), "A", "a", "A1", 0, "g"), vrow(at(8, 10), "A", "a", "A1", 2100, "g"),
		vrow(at(8, 0), "B", "b1", "B1", 100, "g"), vrow(at(8, 15), "B", "b1", "B1", 5000, "g"),
		vrow(at(8, 27), "B", "b1", "B1", 2950, "g"),
		vrow(at(8, 31), "B", "b2", "B1", 100, "g"), vrow(at(8, 50), "B", "b2", "B1", 4100, "g"),
		vrow(at(8, 0), "C", "c", "C1", 100, "g"), vrow(at(8, 20), "C", "c", "C1", 4100, "g"),
		vrow(at(9, 0), "C", "c", "C1", 100, "g"), vrow(at(9, 10), "C", "c", "C1", 1000, "g"),
		vrow(at(8, 20), "C", "c", "C1", 4100, "g"),
	)
	plan := &Plan{GTFS: "g", Expired: true, Trips: []PlanTrip{}}
	span := Span{100, 4100}
	plan.UseDaily([]DailyDir{
		{Line: "A", Dir: 0, Starts: []int64{at(8, 0).Unix()}, Trips: map[string]Span{"A1": span}},
		{Line: "B", Dir: 0, Starts: []int64{at(8, 0).Unix(), at(8, 30).Unix()}, Trips: map[string]Span{"B1": span}},
		{Line: "C", Dir: 0, Starts: []int64{at(8, 0).Unix(), at(9, 0).Unix()}, Trips: map[string]Span{"C1": span}},
	})
	d, err := Compute("2026-10-06", athens, inputs(t, dir, "2026-10-06"), time.Unix(0, 0), plan)
	if err != nil {
		t.Fatal(err)
	}
	// A and C's last rows stop mid-route: vanished; B's first bus reached the end before its
	// fix behind it.
	want := map[string]TripCount{"A": {Planned: 1, Run: 1, Vanished: 1}, "B": {Planned: 2, Run: 2},
		"C": {Planned: 2, Run: 1, Partial: 1, Vanished: 1}}
	for _, l := range d.Trips.ByLine {
		if l.TripCount != want[l.Line] {
			t.Errorf("line %s: %+v, want %+v", l.Line, l.TripCount, want[l.Line])
		}
	}
}

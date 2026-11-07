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
		TripCount: TripCount{Planned: 5, Run: 1, Partial: 1, Missed: 2, Unmeasured: 1},
		ByLine: []TripLine{
			{Line: "L", ScheduleSource: SourceGTFS, TripCount: TripCount{Planned: 4, Run: 1, Partial: 1, Missed: 1, Unmeasured: 1}, MissedTrips: []string{"C"}},
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
		TripCount: TripCount{Planned: 3, Run: 1, Partial: 1, Missed: 1},
		ByLine: []TripLine{
			{Line: "L", ScheduleSource: SourceDaily, TripCount: TripCount{Planned: 2, Run: 1, Missed: 1}, MissedTrips: []string{"go 10:00"}},
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
		TripCount: TripCount{Planned: 5, Run: 1, Partial: 1, Missed: 1, Unmeasured: 2},
		ByLine: []TripLine{
			{Line: "L", ScheduleSource: SourceDaily, TripCount: TripCount{Planned: 4, Run: 1, Partial: 1, Missed: 1, Unmeasured: 1}, MissedTrips: []string{"go 08:20"}},
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

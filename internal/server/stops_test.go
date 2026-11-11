package server

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/geo"
	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/gtfs/gtfstest"
	"github.com/angelospk/athens-transit-rt/internal/livespeed"
	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/sched"
)

func stopBody(t *testing.T, a *App, id string) (StopResponse, int) {
	t.Helper()
	res, body := get(t, a, "/v1/stops/"+id)
	if res.StatusCode != 200 {
		t.Fatalf("%s: %d %s", id, res.StatusCode, body)
	}
	var sr StopResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		t.Fatal(err)
	}
	return sr, maxAge(t, res)
}

// liveRows are the arrivals with a bus.
func liveRows(sr StopResponse) []StopArrival {
	var out []StopArrival
	for _, r := range sr.Arrivals {
		if r.Source == "live" {
			out = append(out, r)
		}
	}
	return out
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// The test line: S0..S4 every ~880 m due east, trips every 10 min from 10:00, 5 min a stop.
// Vehicle 44548 is between S2 and S3 at 10:19:50, matched to T00 (next stop S3), 440 s late;
// vehicle 9 is far away. At S3 it arrives after half a stop's scheduled running time (10:22:20,
// 140 s after 10:20); the trips that have not left yet follow, from the timetable, three rows
// per variant in all.
func TestStopArrivals(t *testing.T) {
	a := newApp(t, monday1020)
	poll(a, monday1020)

	t.Run("the buses on their way, then the trips that have not left", func(t *testing.T) {
		sr, ma := stopBody(t, a, "S3")
		if ma != stopMaxAge || sr.Stop.ID != "S3" || sr.Stop.Name != "STOP 3" || strings.Join(sr.Lines, ",") != "L" ||
			sr.UpdatedAt != monday1020.Unix() || len(sr.Arrivals) != 3 {
			t.Fatalf("max-age %d, %s", ma, asJSON(sr))
		}
		live := sr.Arrivals[0]
		fix := monday1020.Add(-10 * time.Second).Unix()
		if live.Source != "live" || live.Line != "L" || live.Vehicle != "44548" || *live.TripID != "T00" || live.Variant != "SH" ||
			live.Headsign != "EAST" || live.DelayS != 440 || live.PositionAt != fix || *live.StopsAway != 1 ||
			math.Abs(float64(*live.DistanceM)-440) > 10 ||
			*live.Scheduled != time.Date(2026, 10, 5, 10, 15, 0, 0, gtfs.Athens).Unix() ||
			math.Abs(float64(live.At-fix-150)) > 3 || live.EtaS != int(live.At-monday1020.Unix()) {
			t.Fatalf("live row %s", asJSON(live))
		}
		// T01 left at 10:10 with no bus: not shown. T02 leaves now, T03 at 10:30.
		for i, want := range []string{"T02", "T03"} {
			r := sr.Arrivals[i+1]
			at := time.Date(2026, 10, 5, 10, 35+10*i, 0, 0, gtfs.Athens).Unix()
			if r.Source != "schedule" || *r.TripID != want || r.At != at || *r.Scheduled != at || r.Vehicle != "" ||
				r.PositionAt != 0 || r.StopsAway != nil || r.DistanceM != nil || r.Variant != "SH" {
				t.Fatalf("schedule row %d %s", i, asJSON(r))
			}
		}
		if !a.sched.IsWatched("L") {
			t.Error("a stop with a bus on the way must mark its line watched")
		}
	})

	t.Run("a stop the bus has passed: its lines, the timetable only", func(t *testing.T) {
		sr, _ := stopBody(t, a, "S0")
		if strings.Join(sr.Lines, ",") != "L" || len(sr.Arrivals) != 3 || len(liveRows(sr)) != 0 {
			t.Fatalf("got %s", asJSON(sr))
		}
	})

	t.Run("every trip ends at S4: its line, nobody gets on there", func(t *testing.T) {
		sr, _ := stopBody(t, a, "S4")
		if strings.Join(sr.Lines, ",") != "L" || sr.Arrivals == nil || len(sr.Arrivals) != 0 {
			t.Fatalf("got %s", asJSON(sr))
		}
	})

	t.Run("an unknown stop", func(t *testing.T) {
		res, body := get(t, a, "/v1/stops/NOPE")
		if res.StatusCode != 404 || !strings.Contains(string(body), `"unknown_stop"`) {
			t.Fatalf("%d %s", res.StatusCode, body)
		}
	})
}

func TestStopArrivalsLateBusIsDueNow(t *testing.T) {
	// 4 minutes on, the bus is still before S3 by its last fix (due 10:22:20): due now, never a negative wait.
	a := newApp(t, monday1020)
	poll(a, monday1020)
	a.now = func() time.Time { return monday1020.Add(4 * time.Minute) }
	sr, _ := stopBody(t, a, "S3")
	if l := liveRows(sr); len(l) != 1 || l[0].EtaS != 0 || l[0].At != monday1020.Add(4*time.Minute).Unix() {
		t.Fatalf("got %s", asJSON(sr))
	}
}

func TestStopArrivalsDropOldFixes(t *testing.T) {
	// A bus not heard of for longer than staleFix may be gone: not promised.
	a := newAppWith(t, sched.DefaultConfig(), func() time.Time { return monday1020.Add(staleFix) })
	poll(a, monday1020)
	if sr, _ := stopBody(t, a, "S3"); len(liveRows(sr)) != 0 {
		t.Fatalf("got %s", asJSON(sr))
	}
}

func TestStopArrivalsBusThatChangedLines(t *testing.T) {
	// 44548 reports on another line after its fix on L: its L result is old news.
	a := newApp(t, monday1020)
	poll(a, monday1020)
	a.mu.Lock()
	a.lines["Α1"] = &lineData{updated: monday1020, feed: a.w.feed, results: []match.Result{{VehicleID: "44548", Line: "Α1", Trip: -1, Time: monday1020}}}
	a.mu.Unlock()
	if sr, _ := stopBody(t, a, "S3"); len(liveRows(sr)) != 0 {
		t.Fatalf("got %s", asJSON(sr))
	}
}

func TestStopWatchesOnlyLinesWithABusOnTheWay(t *testing.T) {
	// S0 is behind the only bus of L: nothing comes but the timetable, so L is not asked of OASA more often.
	a := newApp(t, monday1020)
	poll(a, monday1020)
	get(t, a, "/v1/stops/S0")
	if a.sched.IsWatched("L") {
		t.Fatal("a stop with no bus on the way must not mark its lines watched")
	}
	get(t, a, "/v1/stops/S3")
	if !a.sched.IsWatched("L") {
		t.Fatal("a stop with a bus on the way must mark its line watched")
	}
}

// With typical speeds (5 m/s over the shape) they replace the timetable's running time.
func TestStopBoardTypicalSpeeds(t *testing.T) {
	a := newApp(t, monday1020)
	a.segs.Store(typicalTable(t, monday1020, a.gtfsVersion()))
	poll(a, monday1020)
	sr, _ := stopBody(t, a, "S3")
	if r := sr.Arrivals[0]; r.Source != "live" || math.Abs(float64(r.At-r.PositionAt)-float64(*r.DistanceM)/5) > 3 {
		t.Fatalf("live row %s", asJSON(r))
	}
}

// A matched bus waiting at its first stop leaves at its scheduled departure: T03 at 10:30,
// at S2 ten minutes later.
func TestStopBoardWaitingAtFirstStop(t *testing.T) {
	now := time.Date(2026, 10, 5, 10, 25, 0, 0, gtfs.Athens)
	a := newApp(t, now)
	pollOne(a, now, "w", 23.7001, now.Add(-10*time.Second))
	sr, _ := stopBody(t, a, "S2")
	l := liveRows(sr)
	at := time.Date(2026, 10, 5, 10, 40, 0, 0, gtfs.Athens).Unix()
	if len(l) != 1 || *l[0].TripID != "T03" || l[0].At != at || *l[0].Scheduled != at || *l[0].StopsAway != 3 {
		t.Fatalf("arrivals %s", asJSON(sr.Arrivals))
	}
	for _, x := range sr.Arrivals {
		if x.TripID != nil && *x.TripID == "T03" && x.Source == "schedule" {
			t.Fatal("T03 listed twice: its bus and its timetable")
		}
	}
}

// At night no trip fits, so the bus is unmatched: placed on its shape, it comes at the default
// speed with no trip; standing at its first stop, it has no time and is left out.
func TestStopBoardUnmatched(t *testing.T) {
	now := time.Date(2026, 10, 5, 3, 0, 0, 0, gtfs.Athens)
	a := newApp(t, now)
	pollOne(a, now, "u", 23.715, now.Add(-10*time.Second))
	sr, _ := stopBody(t, a, "S3")
	if len(sr.Arrivals) != 1 {
		t.Fatalf("arrivals %s", asJSON(sr.Arrivals))
	}
	r := sr.Arrivals[0]
	if r.TripID != nil || r.Scheduled != nil || r.Vehicle != "u" || r.DelayS != 0 || *r.StopsAway != 2 ||
		math.Abs(float64(r.At-r.PositionAt)-float64(*r.DistanceM)/boardDefaultMS) > 2 {
		t.Fatalf("unmatched row %s", asJSON(r))
	}
	// Standing at S0 over two polls (speed 0).
	a2 := newApp(t, now)
	pollOne(a2, now, "s", 23.7001, now.Add(-60*time.Second))
	pollOne(a2, now, "s", 23.7001, now.Add(-10*time.Second))
	if sr, _ = stopBody(t, a2, "S2"); len(sr.Arrivals) != 0 {
		t.Fatalf("waiting unmatched %s", asJSON(sr.Arrivals))
	}
}

// After the timetable expired, buses still match (PlanningTime) and come; the timetable's own
// trips are not listed, their times may not be OASA's any more.
func TestStopBoardExpiredTimetable(t *testing.T) {
	after := time.Date(2026, 10, 12, 10, 20, 0, 0, gtfs.Athens) // Monday; feed ended 2026-10-06
	a := newApp(t, after)
	poll(a, after)
	sr, _ := stopBody(t, a, "S3")
	if len(sr.Arrivals) != 1 || sr.Arrivals[0].Source != "live" || sr.Arrivals[0].TripID == nil {
		t.Fatalf("arrivals %s", asJSON(sr.Arrivals))
	}
}

// A matched bus standing at the stop (its next stop already the one after) is due now.
func TestStopBoardAtTheStop(t *testing.T) {
	a := newApp(t, monday1020)
	pollOne(a, monday1020, "at", 23.7201, monday1020.Add(-10*time.Second)) // ~9 m past S2
	sr, _ := stopBody(t, a, "S2")
	l := liveRows(sr)
	if len(l) != 1 || l[0].TripID == nil || *l[0].TripID != "T01" || *l[0].StopsAway != 1 || *l[0].DistanceM != 0 ||
		l[0].EtaS != 0 || l[0].At != monday1020.Unix() {
		t.Fatalf("arrivals %s", asJSON(sr.Arrivals))
	}
}

func TestStopBoardErrors(t *testing.T) {
	a := newApp(t, monday1020)
	for _, p := range []string{"/v1/stops/NOPE", "/v1/stops/S3/x", "/v1/stops/..%2FS3", "/v1/stops/" + strings.Repeat("9", 2048), "/v1/stops/"} {
		res, body := get(t, a, p)
		if res.StatusCode != 404 || !json.Valid(body) || maxAge(t, res) > 60 {
			t.Fatalf("%.40s: %d %s", p, res.StatusCode, body)
		}
	}
	a.mu.Lock()
	a.w = nil
	a.mu.Unlock()
	if res, body := get(t, a, "/v1/stops/S3"); res.StatusCode != 503 || !strings.Contains(string(body), "warming_up") {
		t.Fatalf("no feed: %d %s", res.StatusCode, body)
	}
}

// The JSON field names must be exactly those of the contract fixture.
func TestStopJSONMatchesFixture(t *testing.T) {
	a := newApp(t, monday1020)
	poll(a, monday1020)
	_, body := get(t, a, "/v1/stops/S3")
	fix, err := os.ReadFile("../../docs/fixtures/stop-61000.json")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := keys(t, body), keys(t, fix); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys %v, fixture %v", got, want)
	}
}

// The timetable's running time to the stop ends at its arrival, not after its wait there; the
// waits at the stops on the way count.
func TestStopBoardTravelEndsAtArrival(t *testing.T) {
	a := newApp(t, monday1020)
	a.SetFeed(gtfstest.StraightLine{Stops: 5, Trips: 6, First: 10 * 3600, Headway: 10, Leg: 5, Dwell: 60}.Feed(t), nil)
	w := a.world()
	f := w.feed
	trip := f.Trip(0)
	g := w.matcher.Geometry(trip)
	if got := boardTravel(f, g, f.ShapeID(trip), nil, nil, 0, 0, trip, g.StopAlong[2], g.StopAlong[3], nil); math.Abs(got-240) > 1 {
		t.Fatalf("S2 to S3: %.0f s, want 240 (leaves 10:11, due 10:15)", got)
	}
	if got := boardTravel(f, g, f.ShapeID(trip), nil, nil, 0, 0, trip, g.StopAlong[1], g.StopAlong[3], nil); math.Abs(got-540) > 1 {
		t.Fatalf("S1 to S3: %.0f s, want 540 (the wait at S2 counts)", got)
	}
}

// A bus whose fix is stale, or that went on to another line, no longer runs its trip: the
// trip's timetable row comes back.
func TestStopBoardGoneBusLeavesItsTripToTheTimetable(t *testing.T) {
	now := time.Date(2026, 10, 5, 10, 25, 0, 0, gtfs.Athens)
	isT03 := func(sr StopResponse) (live, sched bool) {
		for _, r := range sr.Arrivals {
			if r.TripID != nil && *r.TripID == "T03" {
				live, sched = live || r.Source == "live", sched || r.Source == "schedule"
			}
		}
		return
	}
	a := newAppWith(t, sched.DefaultConfig(), func() time.Time { return now.Add(staleFix) })
	pollOne(a, now, "w", 23.7001, now.Add(-10*time.Second)) // waiting at S0 for T03 (10:30)
	if live, sch := isT03(func() StopResponse { sr, _ := stopBody(t, a, "S2"); return sr }()); live || !sch {
		t.Fatalf("stale bus: live %v, schedule %v", live, sch)
	}
	b := newApp(t, now)
	pollOne(b, now, "w", 23.7001, now.Add(-10*time.Second))
	b.mu.Lock()
	b.lines["Α1"] = &lineData{updated: now, feed: b.w.feed, results: []match.Result{{VehicleID: "w", Line: "Α1", Trip: -1, Time: now}}}
	b.mu.Unlock()
	if live, sch := isT03(func() StopResponse { sr, _ := stopBody(t, b, "S2"); return sr }()); live || !sch {
		t.Fatalf("bus on another line: live %v, schedule %v", live, sch)
	}
}

// At 23:50 on Sunday the trips of Monday's service from 00:10 are within the hour.
func TestStopBoardScheduleAcrossMidnight(t *testing.T) {
	now := time.Date(2026, 10, 4, 23, 50, 0, 0, gtfs.Athens)
	a := newApp(t, now)
	a.SetFeed(gtfstest.StraightLine{Stops: 5, Trips: 6, First: 10 * 60, Headway: 10, Leg: 5}.Feed(t), nil)
	sr, _ := stopBody(t, a, "S2")
	at := time.Date(2026, 10, 5, 0, 20, 0, 0, gtfs.Athens).Unix()
	if len(sr.Arrivals) == 0 || sr.Arrivals[0].Source != "schedule" || *sr.Arrivals[0].TripID != "T00" || sr.Arrivals[0].At != at {
		t.Fatalf("arrivals %s", asJSON(sr.Arrivals))
	}
}

// Today's traffic counts on the stop board as on the path ahead of a bus: two buses of any
// line that just crawled over a stretch make the ETA over it longer than the timetable says,
// and a stretch no bus crossed lately keeps the timetable's time.
func TestStopBoardTravelFollowsLiveSpeeds(t *testing.T) {
	a := newApp(t, monday1020)
	a.SetFeed(gtfstest.StraightLine{Stops: 5, Trips: 6, First: 10 * 3600, Headway: 10, Leg: 5, Dwell: 60}.Feed(t), nil)
	w := a.world()
	f := w.feed
	trip := f.Trip(0)
	g := w.matcher.Geometry(trip)
	g = &geo.Geometry{Line: g.Line, StopAlong: g.StopAlong, FromShape: true}
	now := monday1020.Unix()
	roads := livespeed.New()
	from, to := g.StopAlong[2], g.StopAlong[3]
	// Two buses crossed the whole stretch at 1 m/s, 100 m between fixes, ending a minute ago.
	for _, id := range []string{"A", "B"} {
		for s0 := from; s0 < to; s0 += 100 {
			s1 := math.Min(s0+100, to)
			t1 := now - 60 - int64(to-s1)
			roads.Pass(id, g.Line, s0, s1, t1-int64(s1-s0), t1)
		}
	}
	base := boardTravel(f, g, f.ShapeID(trip), nil, nil, now, 0, trip, from, to, nil)
	live := boardTravel(f, g, f.ShapeID(trip), nil, roads, now, 0, trip, from, to, nil)
	if want := to - from; math.Abs(live-want) > 1 {
		t.Fatalf("with live speeds: %.0f s, want %.0f (1 m/s over %.0f m), timetable alone %.0f", live, want, to-from, base)
	}
	// A stretch nobody crossed: as before.
	if got := boardTravel(f, g, f.ShapeID(trip), nil, roads, now, 0, trip, g.StopAlong[0], g.StopAlong[1], nil); math.Abs(got-base) > 1 {
		t.Fatalf("no live speeds: %.0f s, want %.0f", got, base)
	}
}

// A bus's own speed now counts over the first 500 m: crawling at 1 m/s where the timetable
// says 3.65 m/s, it is later than the timetable for the next stop; standing, it adds a short
// wait; further than 500 m on, its speed no longer matters.
func TestStopBoardTravelStartsAtTheBussOwnSpeed(t *testing.T) {
	a := newApp(t, monday1020)
	a.SetFeed(gtfstest.StraightLine{Stops: 5, Trips: 6, First: 10 * 3600, Headway: 10, Leg: 5, Dwell: 60}.Feed(t), nil)
	w := a.world()
	f := w.feed
	trip := f.Trip(0)
	g := w.matcher.Geometry(trip)
	from, to := g.StopAlong[2], g.StopAlong[3] // 876 m, 240 s by the timetable
	base := boardTravel(f, g, f.ShapeID(trip), nil, nil, 0, 0, trip, from, to, nil)
	slow := boardTravel(f, g, f.ShapeID(trip), nil, nil, 0, 0, trip, from, to, ptr(1.0))
	if slow <= base+80 || slow >= base+300 {
		t.Fatalf("crawling: %.0f s, timetable %.0f: want between +80 and +300", slow, base)
	}
	fast := boardTravel(f, g, f.ShapeID(trip), nil, nil, 0, 0, trip, from, to, ptr(10.0))
	if fast >= base {
		t.Fatalf("fast: %.0f s, want under the timetable's %.0f", fast, base)
	}
	stand := boardTravel(f, g, f.ShapeID(trip), nil, nil, 0, 0, trip, from, to, ptr(0.0))
	if math.Abs(stand-base-boardStandS) > 1 {
		t.Fatalf("standing: %.0f s, want %.0f + %.0f", stand, base, boardStandS)
	}
	// Far down the line the bus's speed is forgotten: the last stretch is the timetable's.
	far := boardTravel(f, g, f.ShapeID(trip), nil, nil, 0, 0, trip, g.StopAlong[0], g.StopAlong[4], ptr(1.0))
	farBase := boardTravel(f, g, f.ShapeID(trip), nil, nil, 0, 0, trip, g.StopAlong[0], g.StopAlong[4], nil)
	if far-farBase > 400 {
		t.Fatalf("whole line crawling at first: +%.0f s, want the first 500 m only", far-farBase)
	}
}

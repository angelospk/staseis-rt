package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/gtfs/gtfstest"
	"github.com/angelospk/athens-transit-rt/internal/model"
	"github.com/angelospk/athens-transit-rt/internal/plan"
	"github.com/angelospk/athens-transit-rt/internal/sched"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

// /v2/plan (contract §9) on the test feed: line L runs due east through S0..S4 (0.01 deg apart,
// ~880 m), trips T00..T05 leave S0 every 10 min from 10:00, 5 min between stops, Mondays only,
// 2026-07-06..2026-10-06.

type planPlace struct {
	Stop *string `json:"stop"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
}

type planLeg struct {
	Kind      string       `json:"kind"`
	Line      string       `json:"line"`
	Mode      string       `json:"mode"`
	Run       string       `json:"run"`
	From      planPlace    `json:"from"`
	To        planPlace    `json:"to"`
	DepartAt  int64        `json:"depart_at"`
	ArriveAt  int64        `json:"arrive_at"`
	DistanceM int          `json:"distance_m"`
	Hops      int          `json:"hops"`
	Shape     [][2]float64 `json:"shape"`
}

type planItin struct {
	DepartAt  int64     `json:"depart_at"`
	ArriveAt  int64     `json:"arrive_at"`
	Transfers int       `json:"transfers"`
	Modes     []string  `json:"modes"`
	Legs      []planLeg `json:"legs"`
}

type v2Plan struct {
	ServiceDay  string                `json:"service_day"`
	After       int64                 `json:"after"`
	Last        bool                  `json:"last"`
	Itineraries []planItin            `json:"itineraries"`
	WalkOnly    *plan.WalkOnly        `json:"walk_only"`
	Lines       map[string]model.Line `json:"lines"`
	Stops       map[string]model.Stop `json:"stops"`
	Sources     []model.SourceState   `json:"sources"`
}

// planAt is an App on the test feed whose clock reads *clock, polled once and with its planner
// built.
func planAt(t *testing.T, clock *time.Time, edit ...func(*App)) *App {
	t.Helper()
	a := oasaOnly(newAppWith(t, sched.DefaultConfig(), func() time.Time { return *clock }))
	poll(a, *clock)
	for _, f := range edit {
		f(a)
	}
	if err := a.buildPlan(*clock); err != nil {
		t.Fatal(err)
	}
	return a
}

const (
	atS0 = "37.98,23.70"
	atS3 = "37.98,23.73"
)

func TestV2PlanDirect(t *testing.T) {
	now := monday1020
	a := planAt(t, &now)
	var p v2Plan
	code, ma := v2get(t, a, "/v2/plan?from="+atS0+"&to="+atS3, &p)
	if code != 200 || ma != 60 {
		t.Fatalf("%d max-age %d", code, ma)
	}
	if p.ServiceDay != "2026-10-05" || p.After != now.Unix() || p.Last || len(p.Itineraries) != 3 || p.WalkOnly != nil {
		t.Fatalf("plan %s", asJSON(p))
	}
	var runs []string
	for _, it := range p.Itineraries {
		if len(it.Legs) != 3 || it.Transfers != 0 || !slices.Equal(it.Modes, []string{"bus"}) {
			t.Fatalf("itinerary %s", asJSON(it))
		}
		runs = append(runs, it.Legs[1].Run)
	}
	if want := []string{"oasa:run:T02:2026-10-05", "oasa:run:T03:2026-10-05", "oasa:run:T04:2026-10-05"}; !slices.Equal(runs, want) {
		t.Fatalf("runs %v", runs)
	}
	ride := p.Itineraries[0].Legs[1]
	dep := time.Date(2026, 10, 5, 10, 20, 0, 0, gtfs.Athens).Unix()
	if ride.Line != "oasa:L" || ride.Mode != "bus" || *ride.From.Stop != "oasa:S0" || *ride.To.Stop != "oasa:S3" ||
		ride.DepartAt != dep || ride.ArriveAt != dep+15*60 || ride.Hops != 3 || len(ride.Shape) < 2 {
		t.Fatalf("ride %s", asJSON(ride))
	}
	if walk := p.Itineraries[0].Legs[0]; walk.DistanceM != 0 || walk.DepartAt != dep || walk.From.Stop != nil {
		t.Fatalf("first walk %s", asJSON(walk))
	}
	if _, ok := p.Lines["oasa:L"]; !ok || len(p.Lines) != 1 {
		t.Errorf("lines %s", asJSON(p.Lines))
	}
	if _, ok := p.Stops["oasa:S0"]; !ok || len(p.Stops) != 2 {
		t.Errorf("stops %s", asJSON(p.Stops))
	}
	if stateOf(p.Sources, "oasa") != model.OK {
		t.Errorf("sources %s", asJSON(p.Sources))
	}
	// Every ride's run opens.
	if code, _ := v2get(t, a, "/v2/runs/"+esc(ride.Run), nil); code != 200 {
		t.Errorf("/v2/runs/%s: %d", ride.Run, code)
	}
	// last=true: the last 3 of the day; n=1.
	v2get(t, a, "/v2/plan?from="+atS0+"&to="+atS3+"&last=true&n=1", &p)
	if len(p.Itineraries) != 1 || p.Itineraries[0].Legs[1].Run != "oasa:run:T05:2026-10-05" || !p.Last {
		t.Fatalf("last=true n=1: %s", asJSON(p.Itineraries))
	}
	// Points 880 m apart: walk_only.
	v2get(t, a, "/v2/plan?from="+atS0+"&to=37.98,23.71", &p)
	if p.WalkOnly == nil || p.WalkOnly.DistanceM < 1100 || p.WalkOnly.DistanceM > 1160 {
		t.Errorf("walk_only %s", asJSON(p.WalkOnly))
	}
}

func TestV2PlanErrors(t *testing.T) {
	now := monday1020
	a := planAt(t, &now)
	svcEnd := time.Date(2026, 10, 6, 3, 0, 0, 0, gtfs.Athens).Unix()
	for _, tc := range []struct{ query, code string }{
		{"to=" + atS3, "bad_from"},
		{"from=x&to=" + atS3, "bad_from"},
		{"from=37.98&to=" + atS3, "bad_from"},
		{"from=37.98,23.7,1&to=" + atS3, "bad_from"},
		{"from=NaN,23.7&to=" + atS3, "bad_from"},
		{"from=" + atS0, "bad_to"},
		{"from=" + atS0 + "&to=37.98,", "bad_to"},
		{"from=40.0,23.7&to=" + atS3, "outside_area"},
		{"from=" + atS0 + "&to=37.98,24.3", "outside_area"},
		{"from=" + atS0 + "&to=" + atS3 + "&after=soon", "bad_after"},
		{"from=" + atS0 + "&to=" + atS3 + "&after=" + fmt.Sprint(svcEnd), "bad_after"},
		{"from=" + atS0 + "&to=" + atS3 + "&after=" + fmt.Sprint(time.Date(2026, 10, 5, 2, 0, 0, 0, gtfs.Athens).Unix()), "bad_after"},
		{"from=" + atS0 + "&to=" + atS3 + "&n=0", "bad_n"},
		{"from=" + atS0 + "&to=" + atS3 + "&n=6", "bad_n"},
		{"from=" + atS0 + "&to=" + atS3 + "&n=two", "bad_n"},
	} {
		res, body := get(t, a, "/v2/plan?"+tc.query)
		if res.StatusCode != 400 || string(body) != `{"error":"`+tc.code+`"}` {
			t.Errorf("%s: %d %s, want 400 %s", tc.query, res.StatusCode, body, tc.code)
		}
	}
	// The end of the service day minus a second, and earlier today, are fine.
	for _, after := range []int64{svcEnd - 1, time.Date(2026, 10, 5, 8, 0, 0, 0, gtfs.Athens).Unix()} {
		if code, _ := v2get(t, a, "/v2/plan?from="+atS0+"&to="+atS3+"&after="+fmt.Sprint(after), nil); code != 200 {
			t.Errorf("after %d: %d", after, code)
		}
	}
	// At 03:30 the new service day has begun; 02:45 is within the hour before now.
	now = time.Date(2026, 10, 6, 3, 30, 0, 0, gtfs.Athens)
	if code, _ := v2get(t, a, "/v2/plan?from="+atS0+"&to="+atS3+"&after="+fmt.Sprint(now.Unix()-45*60), nil); code != 200 {
		t.Errorf("after 02:45 at 03:30: %d", code)
	}
}

func TestV2PlanBusy(t *testing.T) {
	now := monday1020
	a := planAt(t, &now)
	a.planner.wait = 10 * time.Millisecond
	a.planner.sem <- struct{}{}
	a.planner.sem <- struct{}{}
	res, body := get(t, a, "/v2/plan?from="+atS0+"&to="+atS3)
	if res.StatusCode != 429 || string(body) != `{"error":"busy"}` || res.Header.Get("Retry-After") != "2" {
		t.Fatalf("%d %s Retry-After %q", res.StatusCode, body, res.Header.Get("Retry-After"))
	}
	<-a.planner.sem
	if code, _ := v2get(t, a, "/v2/plan?from="+atS0+"&to="+atS3, nil); code != 200 {
		t.Fatalf("with a free slot: %d", code)
	}
}

// All three Sources are listed; one warming up caches the plan 5 s.
func TestV2PlanSources(t *testing.T) {
	now := monday1020
	a := newAppWith(t, sched.DefaultConfig(), func() time.Time { return now })
	poll(a, now)
	if err := a.buildPlan(now); err != nil {
		t.Fatal(err)
	}
	var p v2Plan
	_, ma := v2get(t, a, "/v2/plan?from="+atS0+"&to="+atS3, &p)
	var ids []string
	for _, s := range p.Sources {
		ids = append(ids, s.ID)
	}
	if !slices.Equal(ids, []string{"oasa", "stasy", "proastiakos"}) || stateOf(p.Sources, "stasy") != model.WarmingUp || ma != 5 {
		t.Fatalf("sources %s, max-age %d", asJSON(p.Sources), ma)
	}
	if len(p.Itineraries) != 3 {
		t.Errorf("itineraries %d", len(p.Itineraries))
	}
}

// Before the first build: 200, no options, cached 5 s. Never 503.
func TestV2PlanNotBuilt(t *testing.T) {
	now := monday1020
	a := oasaOnly(newAppWith(t, sched.DefaultConfig(), func() time.Time { return now }))
	poll(a, now)
	var p v2Plan
	code, ma := v2get(t, a, "/v2/plan?from="+atS0+"&to="+atS3, &p)
	if code != 200 || ma != 5 || p.Itineraries == nil || len(p.Itineraries) != 0 {
		t.Fatalf("%d max-age %d %s", code, ma, asJSON(p))
	}
	// walk_only does not need the timetable.
	v2get(t, a, "/v2/plan?from="+atS0+"&to=37.98,23.71", &p)
	if p.WalkOnly == nil {
		t.Errorf("walk_only %s before the first build", asJSON(p.WalkOnly))
	}
}

// An expired GTFS: oasa is warming_up until a daily schedule of the service day is cached; then
// the rides are OASA's daily runs.
func TestV2PlanExpiredGTFS(t *testing.T) {
	now := expiredMonday
	a := planAt(t, &now)
	var p v2Plan
	_, ma := v2get(t, a, "/v2/plan?from="+atS0+"&to="+atS3, &p)
	if stateOf(p.Sources, "oasa") != model.WarmingUp || ma != 5 || len(p.Itineraries) != 0 {
		t.Fatalf("no daily schedule: max-age %d %s", ma, asJSON(p))
	}

	a = planAt(t, &now, withDaily(t, "10:45", "11:30", "23:50", "24:30"))
	_, ma = v2get(t, a, "/v2/plan?from="+atS0+"&to="+atS3+"&n=5", &p)
	if stateOf(p.Sources, "oasa") == model.WarmingUp || ma != 60 || len(p.Itineraries) != 2 { // 23:50 and 24:30 are past the 3 h window
		t.Fatalf("with a daily schedule: max-age %d %s", ma, asJSON(p))
	}
	ride := p.Itineraries[0].Legs[1]
	dep := time.Date(2026, 10, 12, 10, 45, 0, 0, gtfs.Athens).Unix()
	if !strings.HasSuffix(ride.Run, "@1045:2026-10-12") || ride.DepartAt != dep || ride.ArriveAt != dep+15*60 {
		t.Fatalf("daily ride %s", asJSON(ride))
	}
}

// A city without a daily schedule (Thessaloniki: OASTH has none) never waits for one: on an
// expired GTFS the plan keeps the rides of the same weekday inside the feed, and oasa is not
// warming_up.
func TestV2PlanExpiredGTFSNoDailySchedule(t *testing.T) {
	now := expiredMonday
	a := planAt(t, &now, func(a *App) {
		c, err := CityByID("thessaloniki")
		if err != nil {
			t.Fatal(err)
		}
		a.city = c
	})
	var p v2Plan
	_, ma := v2get(t, a, "/v2/plan?from="+atS0+"&to="+atS3+"&n=5", &p)
	if stateOf(p.Sources, "oasa") == model.WarmingUp || len(p.Itineraries) == 0 {
		t.Fatalf("max-age %d %s", ma, asJSON(p))
	}
	if run := p.Itineraries[0].Legs[1].Run; run != "oasa:run:T02:2026-10-12" {
		t.Errorf("first ride %s, want the GTFS trip T02 of the same weekday", run)
	}
}

func withDaily(t *testing.T, deps ...string) func(*App) {
	return func(a *App) {
		a.daily = func(context.Context, string) (telematics.DailySchedule, error) {
			return telematics.DailySchedule{Descr: "WEST - EAST", Go: deps}, nil
		}
		if _, ok := a.dailyOfLine(context.Background(), "L", gtfs.Midnight(a.now())); !ok {
			t.Fatal("daily schedule not cached")
		}
	}
}

// Design test 11: a plan's OASA daily Run id opens with /v2/runs/{id} at 01:30 of the next calendar
// day, with the service day's times.
func TestV2PlanDailyRunAfterMidnight(t *testing.T) {
	now := time.Date(2026, 10, 12, 23, 0, 0, 0, gtfs.Athens)
	a := planAt(t, &now, withDaily(t, "10:45", "23:50", "24:30"))
	var p v2Plan
	v2get(t, a, "/v2/plan?from="+atS0+"&to="+atS3+"&last=true&n=1", &p)
	if len(p.Itineraries) != 1 {
		t.Fatalf("%s", asJSON(p))
	}
	id := p.Itineraries[0].Legs[1].Run
	if !strings.HasSuffix(id, "@2430:2026-10-12") {
		t.Fatalf("last run %s", id)
	}
	now = time.Date(2026, 10, 13, 1, 30, 0, 0, gtfs.Athens)
	a.dailyCache = nil // the line's entry may be replaced by the next day's
	var r struct {
		Run *model.Run `json:"run"`
	}
	if code, _ := v2get(t, a, "/v2/runs/"+esc(id), &r); code != 200 || r.Run == nil {
		t.Fatalf("/v2/runs/%s at 01:30: %d", id, code)
	}
	want := time.Date(2026, 10, 13, 0, 30, 0, 0, gtfs.Athens).Unix()
	if r.Run.ID != id || r.Run.Line != "oasa:L" || len(r.Run.Stops) != 5 || r.Run.Stops[0].At != want ||
		r.Run.Stops[3].At != want+15*60 || r.Run.Stops[0].Stop != "oasa:S0" {
		t.Fatalf("run %s", asJSON(r.Run))
	}
	// At 03:00 the service day is over: the id no longer opens.
	now = time.Date(2026, 10, 13, 3, 0, 0, 0, gtfs.Athens)
	if code, _ := v2get(t, a, "/v2/runs/"+esc(id), nil); code != 404 {
		t.Errorf("at 03:00: %d, want 404", code)
	}
}

// Design test 6, Sources side: a STASY trip of D+1 that starts at 02:30 is in D's timetable; one of
// D that starts at 02:30 is not (it belongs to the day before). D's 24:30 trip is in.
func TestPlanInputSTASYWindow(t *testing.T) {
	now := time.Date(2026, 10, 28, 22, 0, 0, 0, gtfs.Athens)
	a := stasyAppEdit(t, now, func(dir string) {
		appendFile(t, filepath.Join(dir, "trips.txt"), "R2,wd,M2-7,ΕΛΛΗΝΙΚΟ,0\n")
		appendFile(t, filepath.Join(dir, "stop_times.txt"), "M2-7,02:30:00,02:30:00,P1,1\nM2-7,02:32:00,02:32:00,O1,2\n")
	})
	if err := a.buildPlan(now); err != nil {
		t.Fatal(err)
	}
	tt := a.planner.snap().tt
	if tt.Day() != "2026-10-28" {
		t.Fatalf("day %s", tt.Day())
	}
	for id, want := range map[string]bool{
		"stasy:run:M2-7:2026-10-29": true, "stasy:run:M2-7:2026-10-28": false,
		"stasy:run:M2-6:2026-10-28": true, "stasy:run:M2-6:2026-10-29": false, "stasy:run:M2-1:2026-10-28": true,
	} {
		run, ok := tt.Run(id)
		if ok != want {
			t.Errorf("%s in the timetable: %v, want %v", id, ok, want)
		}
		if ok && id == "stasy:run:M2-7:2026-10-29" {
			if at := time.Date(2026, 10, 29, 2, 30, 0, 0, gtfs.Athens).Unix(); run.Stops[0].Dep != at || run.Stops[0].Stop != syntagma {
				t.Errorf("M2-7 %+v", run)
			}
		}
	}
	// Συνταγμα is a metro station: the walk into it takes 120 s more.
	var p v2Plan
	v2get(t, a, "/v2/plan?from=37.975,23.7355&to=37.984,23.728&after="+fmt.Sprint(time.Date(2026, 10, 29, 2, 0, 0, 0, gtfs.Athens).Unix()), &p)
	if len(p.Itineraries) != 1 || p.Itineraries[0].Legs[1].Run != "stasy:run:M2-7:2026-10-29" ||
		p.Itineraries[0].Legs[0].ArriveAt-p.Itineraries[0].Legs[0].DepartAt < 120 {
		t.Fatalf("%s", asJSON(p))
	}
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// Suburban rides come from the cached boards of D and D+1, never a fetch; a train on both boards
// is in once.
func TestPlanInputSuburban(t *testing.T) {
	a, fb, _ := proastiakosApp(t, wed1000)
	if err := a.buildPlan(wed1000); err != nil {
		t.Fatal(err)
	}
	if fb.fetches != 0 {
		t.Errorf("the build fetched the board %d times", fb.fetches)
	}
	tt := a.planner.snap().tt
	if _, ok := tt.Run(subRunID("1500", wed1000)); !ok {
		t.Error("train 1500 not in the timetable")
	}
	if _, ok := tt.Run(subRunID("1600", wed1000)); ok {
		t.Error("cancelled train 1600 in the timetable")
	}
	if _, ok := tt.Run(subRunID("1500", wed1000.AddDate(0, 0, 1))); ok {
		t.Error("train 1500 twice (the board of D+1 repeats it)")
	}
	run, _ := tt.Run(subRunID("1500", wed1000))
	if len(run.Stops) != 3 || run.Stops[0].Dep != athens(10, 30).Unix() || run.Stops[2].Arr != athens(10, 55).Unix() ||
		run.Line != "proastiakos:KIATO" || run.Pattern == "" {
		t.Errorf("1500 %+v", run)
	}
}

// The fixture docs/fixtures/v2/plan.json is this response (PLAN_FIXTURE=1 rewrites it).
func TestV2PlanFixture(t *testing.T) {
	now := monday1020
	a := planAt(t, &now)
	_, body := get(t, a, "/v2/plan?from=37.98012,23.69950&to=37.98060,23.73100&n=2")
	var buf bytes.Buffer
	if err := json.Indent(&buf, body, "", "  "); err != nil {
		t.Fatal(err)
	}
	buf.WriteByte('\n')
	path := filepath.Join("..", "..", "docs", "fixtures", "v2", "plan.json")
	if os.Getenv("PLAN_FIXTURE") == "1" {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(old, buf.Bytes()) {
		t.Fatalf("%s differs from the response (PLAN_FIXTURE=1 go test -run TestV2PlanFixture rewrites it): %v", path, err)
	}
}

// Review round 1: after the service day's calendar midnight the daily cache moves on to the next
// day, so a rebuild would drop D's runs: none happens before 03:00. Before midnight a new or a
// completed (partial to full) daily schedule rebuilds after 10 min; a lost one does not.
func TestPlanRebuildRules(t *testing.T) {
	now := time.Date(2026, 10, 12, 22, 0, 0, 0, gtfs.Athens)
	a := planAt(t, &now, withDaily(t, "10:45", "23:50", "24:30"))
	id := "oasa:run:T05@2430:2026-10-12"
	first := a.planner.snap()
	if _, ok := first.tt.Run(id); !ok {
		t.Fatalf("%s not in the timetable", id)
	}
	setEntry := func(f func(*dailyEntry)) {
		a.dailyMu.Lock()
		e := a.dailyCache["L"]
		f(&e)
		a.dailyCache["L"] = e
		a.dailyMu.Unlock()
	}
	// Partial then full, 20 min later: rebuilt.
	setEntry(func(e *dailyEntry) { e.partial = true })
	if err := a.buildPlan(now); err != nil {
		t.Fatal(err)
	}
	built := a.planner.snap()
	setEntry(func(e *dailyEntry) { e.partial = false })
	now = now.Add(20 * time.Minute)
	a.planTick(now)
	if a.planner.snap() == built {
		t.Error("a daily schedule completed (partial to full): no rebuild")
	}
	// Lost (a failed retry): no rebuild.
	built = a.planner.snap()
	setEntry(func(e *dailyEntry) { e.ok = false })
	now = now.Add(20 * time.Minute)
	a.planTick(now)
	if a.planner.snap() != built {
		t.Error("a daily schedule lost: rebuilt without it")
	}
	setEntry(func(e *dailyEntry) { e.ok = true })
	// After midnight the entry holds the next day's runs.
	now = time.Date(2026, 10, 13, 0, 40, 0, 0, gtfs.Athens)
	setEntry(func(e *dailyEntry) { e.day = "2026-10-13" })
	a.planTick(now)
	if s := a.planner.snap(); s != built {
		t.Error("rebuilt after midnight")
	}
	if _, ok := a.planner.snap().tt.Run(id); !ok {
		t.Errorf("%s gone after midnight", id)
	}
}

// The service day changes at 03:00 on DST days too, the same instant for planNoon and planBounds.
func TestPlanDayBoundsDST(t *testing.T) {
	for _, from := range []time.Time{time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC), time.Date(2027, 3, 27, 0, 0, 0, 0, time.UTC)} {
		for at := from; at.Before(from.Add(72 * time.Hour)); at = at.Add(5 * time.Minute) {
			start, end := planBounds(planNoon(at))
			if at.Before(start) || !at.Before(end) {
				t.Fatalf("%v (%v): service day %v..%v", at, at.In(gtfs.Athens), start, end)
			}
			if _, prevEnd := planBounds(planNoon(start.Add(-time.Second))); !prevEnd.Equal(start) {
				t.Fatalf("day before %v ends %v", start, prevEnd)
			}
		}
	}
}

// A start after midnight builds before the STASY timetable has loaded: it still comes in before
// 03:00 (the one rebuild of the same day after midnight).
func TestPlanSTASYAfterMidnightStart(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 10, 0, 0, gtfs.Athens)
	a := newAppConfig(t, now, Config{STASYURL: "http://127.0.0.1:1/none"})
	a.SetFeed(gtfstest.StraightLine{Stops: 5, Trips: 6, First: 10 * 3600, Headway: 10, Leg: 5}.Feed(t), nil)
	if err := os.CopyFS(a.stasy.Dir, os.DirFS(filepath.Join("..", "rail", "testdata", "stasy"))); err != nil {
		t.Fatal(err)
	}
	a.planTick(now)
	first := a.planner.snap()
	if first == nil || first.key.stasy != nil {
		t.Fatalf("first build %+v", first)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.stasy.Run(ctx)
	a.planTick(now.Add(time.Minute))
	if _, ok := a.planner.snap().tt.Run("stasy:run:M2-6:2026-10-28"); !ok {
		t.Error("the STASY timetable that loaded after midnight is not in the planner")
	}
}

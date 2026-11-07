package server

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/gtfs/gtfstest"
	"github.com/angelospk/athens-transit-rt/internal/stats"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

// The prefetched daily schedules of the day go to <state>/daily/<date>.json.gz; a save merges
// with the file, so lines saved before a restart stay, and an unchanged cache writes nothing.
func TestDailyArchiveSavesAndMerges(t *testing.T) {
	a := appWithLines(t, func() time.Time { return monday1020 }, []telematics.Line{{LineCode: "R1", LineID: "L"}},
		map[string][]telematics.Route{"R1": {{RouteCode: "SH"}}})
	a.dailyBg = func(context.Context, string) (telematics.DailySchedule, error) {
		return telematics.DailySchedule{Descr: "WEST - EAST", Go: []string{"10:00", "10:40"}, Come: []string{"11:00"}}, nil
	}
	dir := a.dailyArchiveDir()
	if err := writeDailyArchive(dir, &dailyArchive{Version: 1, Date: "2026-10-05",
		Lines: map[string]archivedDaily{"X": {Descr: "P - Q", Go: []string{"07:00"}}}}); err != nil {
		t.Fatal(err)
	}
	if !a.prefetchDailyOnce(context.Background()) {
		t.Fatal("prefetch did not finish")
	}
	today := gtfs.Midnight(monday1020)
	if changed, err := a.saveDailyArchive(today); err != nil || !changed {
		t.Fatalf("save: changed %v, err %v", changed, err)
	}
	got, err := loadDailyArchive(dir, "2026-10-05")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]archivedDaily{
		"X": {Descr: "P - Q", Go: []string{"07:00"}},
		"L": {Descr: "WEST - EAST", Go: []string{"10:00", "10:40"}, Come: []string{"11:00"}},
	}
	if got.Version != 1 || got.Date != "2026-10-05" || !reflect.DeepEqual(got.Lines, want) {
		t.Fatalf("archive = %+v", got)
	}
	if changed, err := a.saveDailyArchive(today); err != nil || changed {
		t.Fatalf("second save: changed %v, err %v", changed, err)
	}
	// After a restart one line code failed: the partial schedule does not replace the full one.
	a.dailyMu.Lock()
	e := a.dailyCache["L"]
	e.partial, e.d.Come = true, nil
	a.dailyCache["L"] = e
	a.dailyMu.Unlock()
	if changed, err := a.saveDailyArchive(today); err != nil || changed {
		t.Fatalf("partial save: changed %v, err %v", changed, err)
	}
	if _, err := loadDailyArchive(dir, "2026-10-06"); err == nil {
		t.Fatal("an archive for a day never saved")
	}
}

// With the GTFS expired, the stats plan takes a line's runs from the archive of the day, and the
// night runs (24:00 and later) from the archive of the day before; a direction gets the GTFS
// trips whose shape ends where it does. A valid feed, or a day without an archive, keeps the
// GTFS plan.
func TestDailyPlanFromTheArchive(t *testing.T) {
	f := gtfstest.StraightLine{Stops: 5, Trips: 2, First: 10 * 3600, Headway: 20, Leg: 5}.Feed(t) // headsign EAST
	dir := t.TempDir()
	for _, arch := range []*dailyArchive{
		{Version: 1, Date: "2026-10-12", Lines: map[string]archivedDaily{
			"L": {Descr: "WEST - EAST", Go: []string{"10:00", "10:40", "24:30"}, Come: []string{"11:00"}}}},
		{Version: 1, Date: "2026-10-11", Lines: map[string]archivedDaily{
			"L": {Descr: "WEST - EAST", Go: []string{"23:00", "25:15"}, Come: []string{"24:05"}},
			"Z": {Descr: "P - Q", Go: []string{"24:20"}}}},
		{Version: 1, Date: "2026-10-05", Lines: map[string]archivedDaily{
			"L": {Descr: "WEST - EAST", Go: []string{"10:00"}}}},
	} {
		if err := writeDailyArchive(dir, arch); err != nil {
			t.Fatal(err)
		}
	}
	at := func(d, h, m int) int64 { return time.Date(2026, 10, d, h, m, 0, 0, gtfs.Athens).Unix() }

	p, err := DailyPlan(f, "v", "2026-10-12", gtfs.Athens, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Expired || len(p.Trips) != 0 || len(p.Daily) != 3 {
		t.Fatalf("plan = %+v", p)
	}
	goDir, come := p.Daily[0], p.Daily[1]
	if goDir.Line != "L" || goDir.Dir != 0 || !reflect.DeepEqual(goDir.Starts, []int64{at(12, 1, 15), at(12, 10, 0), at(12, 10, 40)}) {
		t.Fatalf("go = %+v", goDir)
	}
	if len(goDir.Trips) != 2 || goDir.Trips["T00"].ToM < 3450 || goDir.Trips["T00"].FromM > 1 {
		t.Fatalf("go trips = %+v", goDir.Trips)
	}
	if come.Dir != 1 || !reflect.DeepEqual(come.Starts, []int64{at(12, 0, 5), at(12, 11, 0)}) || len(come.Trips) != 0 {
		t.Fatalf("come = %+v", come) // no GTFS shape ends at WEST
	}
	if z := p.Daily[2]; z.Line != "Z" || !reflect.DeepEqual(z.Starts, []int64{at(12, 0, 20)}) {
		t.Fatalf("night run of a line not in today's archive: %+v", z) // OASA has no runs of Z today
	}

	for _, date := range []string{"2026-10-05", "2026-10-19"} { // feed valid; no archive
		p, err := DailyPlan(f, "v", date, gtfs.Athens, dir)
		if err != nil || p.Daily != nil || len(p.Trips) != 2 {
			t.Fatalf("%s: plan = %+v, err %v", date, p, err)
		}
	}
	var _ stats.PlanFunc = func(date string) (*stats.Plan, error) { return DailyPlan(f, "v", date, gtfs.Athens, dir) }
}

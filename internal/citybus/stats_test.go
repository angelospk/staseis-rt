package citybus

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/history"
	"github.com/angelospk/athens-transit-rt/internal/stats"
)

// ---- daily stats per city (staseis-rt#151) ----

func TestStatsPlanListsTheDayTrips(t *testing.T) {
	c, _, today, base, _ := runCity(t, t.TempDir(), nil)
	p, err := c.statsPlan(today)
	if err != nil {
		t.Fatal(err)
	}
	r := c.model.Routes["10"]
	if p.GTFS != c.model.Version || p.GTFS == "" || p.Source != stats.SourceTimetable {
		t.Fatalf("plan gtfs %q source %q, model %q", p.GTFS, p.Source, c.model.Version)
	}
	want := []stats.PlanTrip{
		{ID: "7", Line: "t:1", Start: base + t1720, End: base + t1720 + 6*60, FromM: r.StopM[0], ToM: r.StopM[3]},
		{ID: "8", Line: "t:1", Start: base + t1840, End: base + t1840 + 6*60, FromM: r.StopM[0], ToM: r.StopM[3]},
	}
	if fmt.Sprint(p.Trips) != fmt.Sprint(want) {
		t.Fatalf("trips\n got %+v\nwant %+v", p.Trips, want)
	}
	tomorrow := time.Unix(base+36*3600, 0).In(athensLoc).Format("2006-01-02")
	if p, err := c.statsPlan(tomorrow); err != nil || len(p.Trips) != 0 {
		t.Fatalf("another weekday has no run of runRaw: %+v, %v", p, err)
	}
}

// statsHour writes one gzipped history hour with rows of the citybus history format.
func statsHour(t *testing.T, dir string, at int64, rows ...string) {
	t.Helper()
	out, err := os.Create(filepath.Join(dir, time.Unix(at, 0).UTC().Format("2006-01-02T15")+".csv.gz"))
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(out)
	zw.Write([]byte(history.Header + strings.Join(rows, "")))
	zw.Close()
	out.Close()
}

// statsRow: fix_t,line,route_code,veh,lat,lon,gtfs,trip_id,shape_id,s_m,delay_s,run_start,start_delay_s
func statsRow(at int64, line, veh, gtfs, trip string, lon float64, sm int) string {
	return fmt.Sprintf("%d,%s,10,%s,35300000,%d,%s,%s,,%d,,,\n", at, line, veh, int64(lon*1e6), gtfs, trip, sm)
}

func TestRunStatsWritesTheCityDay(t *testing.T) {
	c, _, today, base, _ := runCity(t, t.TempDir(), nil)
	hdir := t.TempDir()
	r, v := c.model.Routes["10"], c.model.Version
	at := base + t1720
	statsHour(t, hdir, at,
		statsRow(at, "t:1", "t:x", v, "7", 25.1, int(r.StopM[0])),
		statsRow(at+60, "t:1", "t:x", v, "7", 25.11, int(r.StopM[1])),
		statsRow(at+240, "t:1", "t:x", v, "7", 25.13, int(r.StopM[3])),
		statsRow(at+60, "u:1", "u:y", "other", "7", 25.2, 0),
	)
	days, err := c.runStats(hdir, time.Unix(base+30*3600, 0))
	if err != nil || len(days) != 1 || days[0] != today {
		t.Fatalf("days %v, %v", days, err)
	}
	var d stats.Day
	readStats(t, filepath.Join(c.dir, "stats", "days", today+".json"), &d)
	if d.Fixes != 3 || len(d.ByLine) != 1 || d.ByLine[0].Line != "t:1" {
		t.Fatalf("fixes %d by_line %+v", d.Fixes, d.ByLine)
	}
	if d.Trips == nil || d.Trips.Planned != 2 || d.Trips.Run != 1 || d.Trips.Missed != 1 || d.Trips.ScheduleSource != stats.SourceTimetable {
		t.Fatalf("trips %+v", d.Trips)
	}
}

func readStats(t *testing.T, p string, v any) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func TestStatsEndpoints(t *testing.T) {
	c, s, _, _, _ := runCity(t, t.TempDir(), nil)
	sdir := filepath.Join(c.dir, "stats")
	os.MkdirAll(filepath.Join(sdir, "days"), 0o755)
	os.WriteFile(filepath.Join(sdir, "index.json"), []byte(`{"version":1,"latest":"2026-11-12","days":["2026-11-12"]}`), 0o644)
	os.WriteFile(filepath.Join(sdir, "days", "2026-11-12.json"), []byte(`{"version":1,"date":"2026-11-12"}`), 0o644)
	if code, b := getBody(t, s, "/t/v1/stats"); code != 200 || b["latest"] != "2026-11-12" {
		t.Fatalf("index: %d %v", code, b)
	}
	if code, b := getBody(t, s, "/t/v1/stats/days/2026-11-12"); code != 200 || b["date"] != "2026-11-12" {
		t.Fatalf("day: %d %v", code, b)
	}
	if code, _ := getBody(t, s, "/t/v1/stats/days/2026-11-13"); code != 404 {
		t.Fatalf("missing day: %d", code)
	}
	if code, _ := getBody(t, s, "/t/v1/stats/days/2026-13-40"); code != 400 {
		t.Fatalf("bad date: %d", code)
	}
	if code, _ := getBody(t, s, "/t/v1/stats/days/..%2Fhealth"); code != 400 && code != 404 {
		t.Fatalf("path out of the stats dir: %d", code)
	}
}

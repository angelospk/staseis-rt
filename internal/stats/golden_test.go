package stats

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/history"
)

var update = flag.Bool("update", false, "rewrite the golden files of testdata/")

// The day files of Athens (OASA's daily schedule and GTFS) and of Thessaloniki (GTFS headways)
// stay byte for byte what they were before the citybus.gr blocks (staseis-rt#167). The goldens
// were written by the code before that change. The vehicles carry codes of the citybus.gr run
// form ("20261006_4_0100000_10_00"), and pass shape metre 100 near that time: a plan without the
// opt-in must still read nothing from them.
func TestAthensAndThessalonikiDaysUnchanged(t *testing.T) {
	at := func(h, m, s int) time.Time { return local(2026, 10, 6, h, m, s) }
	code := func(h, m int) string { return fmt.Sprintf("20261006_4_0100000_%02d_%02d", h, m) }
	row := func(t time.Time, line, veh, trip string, sm int, run time.Time, delay int) string {
		r, d := "", ""
		if !run.IsZero() {
			r, d = fmt.Sprint(run.Unix()), fmt.Sprint(delay)
		}
		return fmt.Sprintf("%d,%s,R,%s,37980000,23700000,g,%s,SH,%d,0,%s,%s\n", t.Unix(), line, veh, trip, sm, r, d)
	}

	// Athens: line L from OASA's daily schedule, line M from the GTFS.
	athDir := t.TempDir()
	writeRaw(t, athDir, hourName(at(10, 0, 0)), history.Header,
		row(at(10, 0, 10), "L", code(10, 0), "S1000", 50, time.Time{}, 0),
		row(at(10, 0, 40), "L", code(10, 0), "S1000", 180, at(10, 0, 0), 40),
		row(at(10, 20, 0), "L", code(10, 0), "S1000", 2500, at(10, 0, 0), 40),
		row(at(10, 30, 0), "M", code(10, 30), "M1", 60, time.Time{}, 0),
		row(at(10, 30, 50), "M", code(10, 30), "M1", 400, at(10, 30, 0), 50),
		row(at(10, 50, 0), "M", code(10, 30), "M1", 3000, at(10, 30, 0), 50),
	)
	athPlan := &Plan{GTFS: "g", Trips: []PlanTrip{
		{ID: "S1000", Line: "L", Start: at(10, 0, 0).Unix(), End: at(10, 25, 0).Unix(), ToM: 4000},
		{ID: "M1", Line: "M", Start: at(10, 30, 0).Unix(), End: at(11, 0, 0).Unix(), ToM: 4000},
	}}
	athPlan.UseDaily([]DailyDir{{Line: "L", Dir: 0, Starts: []int64{at(10, 0, 0).Unix()},
		Trips: map[string]Span{"S1000": {0, 4000}}}})

	// Thessaloniki: line T, 9 GTFS trips of direction 0 every 5 min, with headways by GTFS trip.
	theDir := t.TempDir()
	thePlan := &Plan{GTFS: "g", GTFSHeadways: true}
	var rows []string
	for i := 0; i < 9; i++ {
		s := at(8, 5*i, 0)
		id := fmt.Sprintf("T%d", i)
		thePlan.Trips = append(thePlan.Trips, PlanTrip{ID: id, Line: "T", Start: s.Unix(), End: s.Add(time.Hour).Unix(), FromM: 0, ToM: 2000})
		veh := code(8, 5*i)
		rows = append(rows,
			row(s.Add(-30*time.Second), "T", veh, id, 60, time.Time{}, 0),
			row(s.Add(30*time.Second), "T", veh, id, 300, time.Time{}, 0),
			row(s.Add(10*time.Minute), "T", veh, id, 800, time.Time{}, 0),
			row(s.Add(11*time.Minute), "T", veh, id, 1200, time.Time{}, 0))
	}
	writeRaw(t, theDir, hourName(at(8, 0, 0)), history.Header, rows...)

	for _, c := range []struct {
		name string
		dir  string
		plan *Plan
	}{{"athens", athDir, athPlan}, {"thessaloniki", theDir, thePlan}} {
		d, err := Compute("2026-10-06", athens, inputs(t, c.dir, "2026-10-06"), time.Unix(0, 0), c.plan)
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.MarshalIndent(d, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join("testdata", c.name+"-day.golden.json")
		if *update {
			os.MkdirAll("testdata", 0o755)
			if err := os.WriteFile(p, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s day changed:\n got %s\nwant %s", c.name, got, want)
		}
	}
}

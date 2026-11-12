package stats

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The citybus.gr cities share one history; their lines and vehicles carry the city code
// ("pat:601"). Filter{Prefix} sums one city (staseis-rt#151).
func TestComputeFilterKeepsOneCity(t *testing.T) {
	dir := t.TempDir()
	at := local(2026, 11, 12, 10, 0, 0)
	writeHour(t, dir, hourName(at), ".csv.gz",
		fix{at, "pat:1", "pat:a", 38.2, 21.7},
		fix{at.Add(30 * time.Second), "pat:1", "pat:a", north(38.2, 200), 21.7},
		fix{at, "pat:2", "pat:b", 38.3, 21.7},
		fix{at, "cfu:1", "cfu:a", 39.6, 19.9},
		fix{at.Add(30 * time.Second), "cfu:1", "cfu:a", north(39.6, 200), 19.9},
	)
	d, err := ComputeFiltered("2026-11-12", athens, inputs(t, dir, "2026-11-12"), at, nil, Filter{Prefix: "pat:"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Fixes != 3 || d.Vehicles != 2 || d.Lines != 2 || len(d.ByLine) != 2 {
		t.Fatalf("fixes %d vehicles %d lines %d by_line %+v", d.Fixes, d.Vehicles, d.Lines, d.ByLine)
	}
	for _, l := range d.ByLine {
		if l.Line != "pat:1" && l.Line != "pat:2" {
			t.Fatalf("line of another city: %+v", l)
		}
	}
	if h := d.Hours[10]; h.Fixes != 3 || !near(h.DistM, 200) || h.TimeS != 30 {
		t.Fatalf("hour 10: %+v", h)
	}
	all, err := Compute("2026-11-12", athens, inputs(t, dir, "2026-11-12"), at, nil)
	if err != nil || all.Fixes != 5 {
		t.Fatalf("Compute without a filter keeps every row: %d, %v", all.Fixes, err)
	}
}

// A citybus row's time is when a board was read, not the GPS time, so a bus standing in the
// rows may only have a stale GPS place. MovedOnly drops the pairs that did not move.
func TestComputeMovedOnlySkipsStillPairs(t *testing.T) {
	dir := t.TempDir()
	at := local(2026, 11, 12, 10, 0, 0)
	writeHour(t, dir, hourName(at), ".csv.gz",
		fix{at, "pat:1", "pat:a", 38.2, 21.7},
		fix{at.Add(60 * time.Second), "pat:1", "pat:a", 38.2, 21.7},
		fix{at.Add(90 * time.Second), "pat:1", "pat:a", north(38.2, 300), 21.7},
	)
	files := inputs(t, dir, "2026-11-12")
	still, err := ComputeFiltered("2026-11-12", athens, files, at, nil, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if l := byLine(still, "pat:1"); !near(l.DistM, 300) || l.TimeS != 90 {
		t.Fatalf("without MovedOnly: %+v", l)
	}
	moved, err := ComputeFiltered("2026-11-12", athens, files, at, nil, Filter{MovedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if l := byLine(moved, "pat:1"); !near(l.DistM, 300) || l.TimeS != 30 || l.Fixes != 3 {
		t.Fatalf("MovedOnly: %+v", l)
	}
}

func TestRunFilteredWritesOnlyDaysOfTheCity(t *testing.T) {
	hist, out := t.TempDir(), t.TempDir()
	at := local(2026, 11, 12, 10, 0, 0)
	writeHour(t, hist, hourName(at), ".csv.gz", fix{at, "pat:1", "pat:a", 38.2, 21.7})
	now := local(2026, 11, 13, 6, 0, 0)
	days, err := RunFiltered(hist, out, athens, now, false, nil, Filter{Prefix: "cfu:"})
	if err != nil || len(days) != 0 {
		t.Fatalf("a city with no row of the day: %v, %v", days, err)
	}
	var idx Index
	readJSON(t, filepath.Join(out, "index.json"), &idx)
	if len(idx.Days) != 0 {
		t.Fatalf("index %+v", idx)
	}
	pat := t.TempDir() // every city has its own folder
	days, err = RunFiltered(hist, pat, athens, now, false, nil, Filter{Prefix: "pat:"})
	if err != nil || len(days) != 1 || days[0] != "2026-11-12" {
		t.Fatalf("days %v, %v", days, err)
	}
	if _, err := os.Stat(filepath.Join(pat, "days", "2026-11-12.json")); err != nil {
		t.Fatal(err)
	}
}

// The trips of one city are counted against the rows of that city only: the rows of another
// city, with another feed version, must not make the day look like another feed's.
func TestTripsOfOneCityIgnoreOtherCitiesRows(t *testing.T) {
	dir := t.TempDir()
	at := local(2026, 11, 12, 10, 0, 0)
	rows := []string{trow(at, "pat:1", "7", 0, "p1"), trow(at.Add(5*time.Minute), "pat:1", "7", 1000, "p1")}
	for i := 0; i < 10; i++ {
		rows = append(rows, trow(at.Add(time.Duration(i)*time.Minute), "cfu:1", "9", 100*i, "c1"))
	}
	writeRows(t, dir, at, rows...)
	plan := &Plan{GTFS: "p1", Source: SourceTimetable, Trips: []PlanTrip{
		{ID: "7", Line: "pat:1", Start: at.Unix(), End: at.Add(10 * time.Minute).Unix(), FromM: 0, ToM: 1000},
		{ID: "8", Line: "pat:1", Start: at.Add(time.Hour).Unix(), End: at.Add(70 * time.Minute).Unix(), FromM: 0, ToM: 1000},
	}}
	d, err := ComputeFiltered("2026-11-12", athens, inputs(t, dir, "2026-11-12"), at, plan, Filter{Prefix: "pat:"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Trips == nil {
		t.Fatal("no trips: the other city's rows counted as another feed")
	}
	if d.Trips.Planned != 2 || d.Trips.Run != 1 || d.Trips.Missed != 1 {
		t.Fatalf("trips %+v", d.Trips.TripCount)
	}
	if d.Trips.ScheduleSource != SourceTimetable || len(d.Trips.ByLine) != 1 || d.Trips.ByLine[0].ScheduleSource != SourceTimetable {
		t.Fatalf("schedule source %q, by line %+v", d.Trips.ScheduleSource, d.Trips.ByLine)
	}
}

// A city with no row in a day (a timetable-only city) writes no day file; a marker keeps the
// hourly run from reading that day's files again (28 cities share them).
func TestRunFilteredMarksAnEmptyDayOnce(t *testing.T) {
	hist, out := t.TempDir(), t.TempDir()
	at := local(2026, 11, 12, 10, 0, 0)
	writeHour(t, hist, hourName(at), ".csv.gz", fix{at, "pat:1", "pat:a", 38.2, 21.7})
	now := local(2026, 11, 13, 6, 0, 0)
	f := Filter{Prefix: "cfu:"}
	if _, err := RunFiltered(hist, out, athens, now, false, nil, f); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "days", "2026-11-12.none")); err != nil {
		t.Fatal("no marker for the empty day:", err)
	}
	// The day is not read again: a broken file now would fail a run that read it.
	if err := os.WriteFile(filepath.Join(hist, hourName(at)+".csv.gz"), []byte("not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if days, err := RunFiltered(hist, out, athens, now, false, nil, f); err != nil || len(days) != 0 {
		t.Fatalf("the empty day was read again: %v, %v", days, err)
	}
	var idx Index
	readJSON(t, filepath.Join(out, "index.json"), &idx)
	if len(idx.Days) != 0 {
		t.Fatalf("index %+v", idx)
	}
	// Athens (no prefix) keeps its old way: no marker.
	if _, err := Run(hist, t.TempDir(), athens, now, false, nil); err == nil {
		t.Fatal("Run without a filter must still read the day")
	}
}

// A city with a timetable but no live bus that day (Drama, Kastoria) still gets its day, for the
// frequency (staseis-rt#167); with no frequency either it gets the marker.
func TestRunFilteredKeepsATimetableOnlyDay(t *testing.T) {
	hist, out := t.TempDir(), t.TempDir()
	at := local(2026, 11, 12, 10, 0, 0)
	writeHour(t, hist, hourName(at), ".csv.gz", fix{at, "pat:1", "pat:a", 38.2, 21.7})
	now := local(2026, 11, 13, 6, 0, 0)
	plan := func(date string) (*Plan, error) {
		p := &Plan{GTFS: "v", Source: SourceTimetable, Frequency: true, Trips: []PlanTrip{}}
		if date != "2026-11-12" {
			return p, nil
		}
		for h := 8; h < 12; h++ {
			s := local(2026, 11, 12, h, 0, 0).Unix()
			p.Trips = append(p.Trips, PlanTrip{ID: fmt.Sprint(h), Line: "dra:1", Start: s, End: s + 1800})
		}
		return p, nil
	}
	days, err := RunFiltered(hist, out, athens, now, false, plan, Filter{Prefix: "dra:"})
	if err != nil || fmt.Sprint(days) != "[2026-11-12]" {
		t.Fatalf("days %v, %v", days, err)
	}
	var d Day
	readJSON(t, filepath.Join(out, "days", "2026-11-12.json"), &d)
	if d.Fixes != 0 || d.Frequency == nil || len(d.Frequency.ByLine) != 1 || d.Frequency.ByLine[0].Band != "60" {
		t.Fatalf("day %+v, frequency %+v", d, d.Frequency)
	}
}

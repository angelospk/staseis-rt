package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/model"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

func archiveL(t *testing.T, a *App, date string, deps ...string) {
	t.Helper()
	if err := writeDailyArchive(a.dailyArchiveDir(), &dailyArchive{Version: dailyArchiveVersion, Date: date,
		Lines: map[string]archivedDaily{"L": {Descr: "WEST - EAST", Go: deps}}}); err != nil {
		t.Fatal(err)
	}
}

// A restart at 00:20 empties the daily cache, and the prefetch does not ask OASA for the service
// day that still runs (it waits for 03:00 and the next day). With the GTFS expired the planner
// had no bus runs until then; the day's archive now fills the cache before the first build.
func TestRestoreDailyAfterMidnight(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 20, 0, 0, gtfs.Athens)
	a := planAt(t, &now, func(a *App) {
		archiveL(t, a, "2026-10-12", "10:45", "23:50", "24:30")
		if n, err := a.restoreDaily(); err != nil || n != 1 {
			t.Fatalf("restored %d lines, err %v", n, err)
		}
	})
	var p v2Plan
	_, ma := v2get(t, a, "/v2/plan?from="+atS0+"&to="+atS3, &p)
	if stateOf(p.Sources, "oasa") == model.WarmingUp || ma != 60 || len(p.Itineraries) != 1 {
		t.Fatalf("max-age %d %s", ma, asJSON(p))
	}
	if run := p.Itineraries[0].Legs[1].Run; !strings.HasSuffix(run, "@2430:2026-10-12") {
		t.Fatalf("run %s", run)
	}
	// Nothing asks OASA for that day any more: the restored lines count as complete.
	if e, ok := a.dailyEntryOf("L", gtfs.Midnight(time.Date(2026, 10, 12, 12, 0, 0, 0, gtfs.Athens))); !ok || e.partial {
		t.Fatalf("entry %+v, %v", e, ok)
	}
}

// A restart during the day: the archive fills the cache at once, and the prefetch still asks
// OASA for every line, as it did with an empty cache. A failed answer keeps the archived runs; a
// good one replaces them.
func TestRestoreDailyDuringTheDay(t *testing.T) {
	now := expiredMonday
	a := planAt(t, &now)
	archiveL(t, a, "2026-10-12", "10:45", "11:30")
	if n, err := a.restoreDaily(); err != nil || n != 1 {
		t.Fatalf("restored %d lines, err %v", n, err)
	}
	today := gtfs.Midnight(now)
	if e, ok := a.dailyEntryOf("L", today); !ok || !e.partial || len(e.d.Go) != 2 {
		t.Fatalf("restored entry %+v, %v", e, ok)
	}
	if !a.anyDailyCached(today) {
		t.Fatal("anyDailyCached false after a restore")
	}

	a.dailyBg = func(context.Context, string) (telematics.DailySchedule, error) {
		return telematics.DailySchedule{}, errors.New("oasa down")
	}
	a.prefetchDailyOnce(context.Background())
	e, ok := a.dailyEntryOf("L", today)
	if !ok || len(e.d.Go) != 2 || !e.partial || !now.Before(e.retry) {
		t.Fatalf("after a failed fetch %+v, %v", e, ok)
	}

	now = now.Add(dailyNegativeTTL)
	a.dailyBg = func(context.Context, string) (telematics.DailySchedule, error) {
		return telematics.DailySchedule{Descr: "WEST - EAST", Go: []string{"10:45", "11:30", "12:15"}}, nil
	}
	a.prefetchDailyOnce(context.Background())
	if e, ok := a.dailyEntryOf("L", today); !ok || e.partial || len(e.d.Go) != 3 {
		t.Fatalf("after a good fetch %+v, %v", e, ok)
	}
}

// No archive for the day (a new server, or a day the prefetch never finished): nothing restored.
// An archive of another day is not the running service day's.
func TestRestoreDailyNoArchive(t *testing.T) {
	now := expiredMonday
	a := planAt(t, &now)
	archiveL(t, a, "2026-10-11", "10:45")
	if n, err := a.restoreDaily(); err != nil || n != 0 {
		t.Fatalf("restored %d lines, err %v", n, err)
	}
	if a.anyDailyCached(gtfs.Midnight(now)) {
		t.Fatal("a line cached from another day's archive")
	}
}

// A line with two line codes, restored from the archive: when one code answers and the other
// fails, the partial answer may miss the failed code's runs, so the cache keeps its runs, adds the
// new ones and asks again after the pause. An answer from both codes replaces them and completes
// the line.
func TestPartialFetchKeepsCachedRuns(t *testing.T) {
	now := monday1020
	a := appWithLines(t, func() time.Time { return now }, []telematics.Line{{LineCode: "R1", LineID: "608"}, {LineCode: "R2", LineID: "608"}},
		map[string][]telematics.Route{"R1": {{RouteCode: "SH"}}, "R2": {{RouteCode: "SH2"}}})
	today := gtfs.Midnight(now)
	line := a.canonLine("608")
	restored := telematics.DailySchedule{Descr: "A - B", Go: []string{"09:00", "09:30"}, Come: []string{"09:15", "09:45"}}
	a.dailyMu.Lock()
	a.dailyCache = map[string]dailyEntry{line: {day: today.Format(time.DateOnly), ok: true, partial: true, retry: now, d: restored}}
	a.dailyMu.Unlock()

	a.dailyBg = func(_ context.Context, code string) (telematics.DailySchedule, error) {
		if code == "R2" {
			return telematics.DailySchedule{}, errors.New("timeout")
		}
		return telematics.DailySchedule{Descr: "A - B", Go: []string{"09:00", "09:30", "10:00", "10:30"}}, nil
	}
	a.prefetchDailyOnce(context.Background())
	e, ok := a.dailyEntryOf(line, today)
	if !ok || !slices.Equal(e.d.Go, []string{"09:00", "09:30", "10:00", "10:30"}) || !slices.Equal(e.d.Come, restored.Come) ||
		!e.partial || !now.Before(e.retry) {
		t.Fatalf("after a partial fetch %+v, %v", e, ok)
	}

	now = now.Add(dailyNegativeTTL)
	a.dailyBg = func(_ context.Context, code string) (telematics.DailySchedule, error) {
		if code == "R2" {
			return telematics.DailySchedule{Descr: "A - B", Come: []string{"09:15"}}, nil
		}
		return telematics.DailySchedule{Descr: "A - B", Go: []string{"09:00", "10:00"}}, nil
	}
	a.prefetchDailyOnce(context.Background())
	if e, ok := a.dailyEntryOf(line, today); !ok || e.partial || len(e.d.Go) != 2 || len(e.d.Come) != 1 {
		t.Fatalf("after a full fetch %+v, %v", e, ok)
	}
}

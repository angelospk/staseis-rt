package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

// Between 00:00 and 03:00 the planner plans the service day that still runs, while /schedule
// asks OASA for the calendar day. The calendar day's answer must not evict the running service
// day from the cache (#128): both stay readable, and the second request asks OASA no more.
func TestScheduleAfterMidnightKeepsTheServiceDay(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 20, 0, 0, gtfs.Athens)
	a := planAt(t, &now)
	service := gtfs.Midnight(planNoon(now))
	a.dailyMu.Lock()
	a.dailyCache = map[string]dailyEntry{"L": {day: service.Format(time.DateOnly), ok: true,
		d: telematics.DailySchedule{Descr: "WEST - EAST", Go: []string{"10:45", "24:30"}}}}
	a.dailyMu.Unlock()
	var calls atomic.Int32
	a.daily = func(context.Context, string) (telematics.DailySchedule, error) {
		calls.Add(1)
		return telematics.DailySchedule{Descr: "WEST - EAST", Go: []string{"06:00", "07:00", "08:00"}}, nil
	}

	for range 2 {
		if res, body := get(t, a, "/v1/lines/L/schedule"); res.StatusCode != 200 {
			t.Fatalf("%d %s", res.StatusCode, body)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("OASA calls %d, want 1", calls.Load())
	}
	if e, ok := a.dailyEntryOf("L", service); !ok || len(e.d.Go) != 2 {
		t.Fatalf("service day %+v, %v", e, ok)
	}
	if !a.anyDailyCached(service) || a.planKeyOf(planNoon(now)).daily != 1 {
		t.Fatal("the planner no longer sees the service day")
	}
	if e, ok := a.dailyEntryOf("L", gtfs.Midnight(now)); !ok || len(e.d.Go) != 3 {
		t.Fatalf("calendar day %+v, %v", e, ok)
	}
}

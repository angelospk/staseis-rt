package server

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/model"
	"github.com/angelospk/athens-transit-rt/internal/sched"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
	"github.com/angelospk/athens-transit-rt/internal/usual"
)

// usualEvery10: on three weekdays before monday1020, line L (EAST) passed S2 and S3 every 10
// min from 09:00 to 12:00: a usual headway of 10 min.
func usualEvery10(t *testing.T) *usual.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "usual")
	for _, d := range []time.Time{date(2026, 9, 28), date(2026, 9, 29), date(2026, 9, 30)} {
		day := usual.NewDay(d)
		for m := 9 * 60; m <= 12*60; m += 10 {
			at := time.Date(d.Year(), d.Month(), d.Day(), m/60, m%60, 0, 0, gtfs.Athens).Unix()
			day.Add("S2", "L", "EAST", at)
			day.Add("S3", "L", "EAST", at+300)
		}
		if err := usual.WriteDay(dir, day); err != nil {
			t.Fatal(err)
		}
	}
	st, err := usual.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// onSchedule is the longitude of a bus of the test line that left S0 at start, on time at time
// at (one stop of 0.01° every 5 min), held at the line's two ends.
func onSchedule(start, at time.Time) float64 {
	p := at.Sub(start).Seconds() / 300
	return 23.70 + 0.01*math.Min(math.Max(p, 0), 4)
}

// TestLiveQuality (contract rev 23): two buses 10 min apart on a line whose usual headway is
// 10 min. The second one gets a headway level from its gap at the stops it passed; the segment
// they both crossed at 2.9 m/s, usually 5 m/s, is a little worse (ratio 0.59).
func TestLiveQuality(t *testing.T) {
	now := monday1020.Add(-4 * time.Minute)
	a := newAppWith(t, sched.DefaultConfig(), func() time.Time { return now })
	a.segs.Store(typicalTable(t, monday1020, a.gtfsVersion()))
	a.usual.Store(usualEvery10(t))
	t01 := monday1020.Add(-10 * time.Minute) // T01 leaves S0 at 10:10, T02 at 10:20
	for ; !now.After(monday1020.Add(16 * time.Minute)); now = now.Add(30 * time.Second) {
		fix := now.Add(-5 * time.Second)
		a.onLine(sched.LinePoll{Line: "L", Started: now, Done: now, Routes: 1, Obs: []match.Obs{
			{Vehicle: telematics.Vehicle{VehNo: "a", RouteCode: "SH", Lat: 37.98, Lon: onSchedule(t01, fix), Heading: 90, Time: fix}, LineCode: "R1"},
			{Vehicle: telematics.Vehicle{VehNo: "b", RouteCode: "SH", Lat: 37.98, Lon: onSchedule(monday1020, fix), Heading: 90, Time: fix}, LineCode: "R1"},
		}})
	}
	now = now.Add(-30 * time.Second)
	var body struct {
		Vehicles []model.Vehicle `json:"vehicles"`
		Segments []model.Segment `json:"segments"`
	}
	v2get(t, a, "/v2/lines/"+esc("oasa:L")+"/vehicles", &body)
	var b *model.Vehicle
	for i := range body.Vehicles {
		if body.Vehicles[i].ID == "oasa:b" {
			b = &body.Vehicles[i]
		}
	}
	if b == nil || b.Quality == nil {
		t.Fatalf("no quality for b: %s", asJSON(body))
	}
	q := b.Quality
	if q.Level != "normal" || q.Basis == nil || *q.Basis != "headway" || q.UsualHeadwayS == nil || *q.UsualHeadwayS != 600 ||
		q.GapS == nil || math.Abs(float64(*q.GapS-600)) > 30 || q.Stop == nil || q.Bunched || q.Early {
		t.Errorf("b quality %s", asJSON(q))
	}
	t.Logf("b %s", asJSON(q))
	var seg *model.Segment
	for i := range body.Segments {
		if s := &body.Segments[i]; s.From == "oasa:S2" && s.To == "oasa:S3" {
			seg = s
		}
	}
	if seg == nil {
		t.Fatalf("no S2-S3 segment: %s", asJSON(body.Segments))
	}
	if seg.Pattern != "oasa:SH" || seg.N != 2 || seg.Level != "worse" || seg.Ratio == nil || math.Abs(*seg.Ratio-0.59) > 0.02 ||
		seg.UsualSpeed == nil || *seg.UsualSpeed != 5 || math.Abs(seg.Speed-2.9) > 0.1 {
		t.Errorf("S2-S3 %s", asJSON(seg))
	}
	t.Logf("segments %s", asJSON(body.Segments))
	// Without a usual store nothing is rated; segments still list their speed, unknown with no table.
	a.usual.Store(nil)
	a.segs.Store(nil)
	a.usualRows = usualCache{}
	now = now.Add(30 * time.Second)
	fix := now.Add(-5 * time.Second)
	a.onLine(sched.LinePoll{Line: "L", Started: now, Done: now, Routes: 1, Obs: []match.Obs{
		{Vehicle: telematics.Vehicle{VehNo: "b", RouteCode: "SH", Lat: 37.98, Lon: onSchedule(monday1020, fix), Heading: 90, Time: fix}, LineCode: "R1"},
	}})
	v2get(t, a, "/v2/lines/"+esc("oasa:L")+"/vehicles", &body)
	if len(body.Vehicles) != 1 || body.Vehicles[0].Quality == nil || body.Vehicles[0].Quality.Level != "unknown" {
		t.Errorf("no store: %s", asJSON(body.Vehicles))
	}
	for _, s := range body.Segments {
		if s.Level != "unknown" || s.Ratio != nil || s.UsualSpeed != nil {
			t.Errorf("no table: %s", asJSON(s))
		}
	}
}

package server

import (
	"encoding/json"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/model"
	"github.com/angelospk/athens-transit-rt/internal/sched"
)

func itoa(n int) string { return strconv.Itoa(n) }

func v2get(t *testing.T, a *App, path string, into any) (int, int) {
	t.Helper()
	res, body := get(t, a, path)
	if into != nil && res.StatusCode == 200 {
		if err := json.Unmarshal(body, into); err != nil {
			t.Fatalf("%s: %v %s", path, err, body)
		}
	}
	if res.StatusCode != 200 {
		return res.StatusCode, 0
	}
	return res.StatusCode, maxAge(t, res)
}

func esc(id string) string { return url.PathEscape(id) }

type v2Lines struct {
	Lines   []model.Line        `json:"lines"`
	Sources []model.SourceState `json:"sources"`
}

type v2LineDetail struct {
	Line     *model.Line           `json:"line"`
	Patterns []model.Pattern       `json:"patterns"`
	Stops    map[string]model.Stop `json:"stops"`
	Sources  []model.SourceState   `json:"sources"`
}

type v2Stop struct {
	Stop      *model.Stop         `json:"stop"`
	Lines     []model.Line        `json:"lines"`
	Arrivals  []model.Arrival     `json:"arrivals"`
	UpdatedAt int64               `json:"updated_at"`
	Sources   []model.SourceState `json:"sources"`
}

type v2Vehicles struct {
	Line         string              `json:"line"`
	UpdatedAt    int64               `json:"updated_at"`
	NextUpdateAt int64               `json:"next_update_at"`
	Vehicles     []model.Vehicle     `json:"vehicles"`
	Sources      []model.SourceState `json:"sources"`
}

func stateOf(src []model.SourceState, id string) string {
	for _, s := range src {
		if s.ID == id {
			return s.State
		}
	}
	return ""
}

// oasaOnly drops the other adapters: their timetables are not loaded in these tests, and a list
// with a Source warming up is cached 5 s.
func oasaOnly(a *App) *App {
	a.adapters = a.adapters[:1]
	return a
}

func TestV2Lines(t *testing.T) {
	a := oasaOnly(newApp(t, monday1020))
	poll(a, monday1020)
	var ls v2Lines
	if _, ma := v2get(t, a, "/v2/lines", &ls); ma != 3600 || len(ls.Lines) != 1 || stateOf(ls.Sources, "oasa") != model.OK {
		t.Fatalf("max-age %d %s", ma, asJSON(ls))
	}
	want := model.Line{ID: "oasa:L", Mode: "bus", Label: model.Ptr("L"), Name: "WEST - EAST", Color: "#153ce0",
		TextColor: "#ffffff", Source: "oasa"}
	if !reflect.DeepEqual(ls.Lines[0], want) {
		t.Fatalf("line %s", asJSON(ls.Lines[0]))
	}

	res, _ := get(t, a, "/v2/lines")
	etag := res.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on a complete hour-long response")
	}
	if r2, _ := do(a, "GET", "/v2/lines", map[string]string{"If-None-Match": etag}); r2.StatusCode != 304 {
		t.Fatalf("If-None-Match: %d", r2.StatusCode)
	}

	var d v2LineDetail
	if _, ma := v2get(t, a, "/v2/lines/"+esc("oasa:L"), &d); ma != 3600 || d.Line == nil || d.Line.ID != "oasa:L" ||
		len(d.Patterns) != 1 || len(d.Stops) != 5 {
		t.Fatalf("detail %s", asJSON(d))
	}
	p := d.Patterns[0]
	if p.ID != "oasa:SH" || p.Headsign != "EAST" || p.Direction != 0 || len(p.Shape) != 5 ||
		strings.Join(p.Stops, ",") != "oasa:S0,oasa:S1,oasa:S2,oasa:S3,oasa:S4" {
		t.Fatalf("pattern %s", asJSON(p))
	}
	if s := d.Stops["oasa:S3"]; s.Name != "STOP 3" || s.Lat != 37.98 || s.Lon != 23.73 ||
		strings.Join(s.Lines, ",") != "oasa:L" || strings.Join(s.Modes, ",") != "bus" {
		t.Fatalf("stop %s", asJSON(s))
	}

	// Ids as users type them resolve to the current id; unknown ids and namespaces are 404.
	if _, _ = v2get(t, a, "/v2/lines/"+esc("oasa:l"), &d); d.Line == nil || d.Line.ID != "oasa:L" {
		t.Fatalf("alias %s", asJSON(d.Line))
	}
	for _, id := range []string{"oasa:NOPE", "zzz:L", "L"} {
		res, body := get(t, a, "/v2/lines/"+esc(id))
		if res.StatusCode != 404 || !strings.Contains(string(body), `"unknown_line"`) {
			t.Fatalf("%s: %d %s", id, res.StatusCode, body)
		}
	}
}

func TestV2WarmingUp(t *testing.T) {
	a := newApp(t, monday1020)
	a.mu.Lock()
	a.w = nil // no static feed yet
	a.mu.Unlock()
	var ls v2Lines
	res, _ := get(t, a, "/v2/lines")
	_, ma := v2get(t, a, "/v2/lines", &ls)
	for _, l := range ls.Lines {
		if l.Source == "oasa" { // the suburban Lines are embedded: always there
			t.Fatalf("oasa line while warming up: %s", asJSON(l))
		}
	}
	if ma != 5 || ls.Lines == nil ||
		stateOf(ls.Sources, "oasa") != model.WarmingUp || res.Header.Get("ETag") != "" {
		t.Fatalf("max-age %d %s", ma, asJSON(ls))
	}
	var d v2LineDetail
	if code, ma := v2get(t, a, "/v2/lines/"+esc("oasa:L"), &d); code != 200 || ma != 5 || d.Line != nil {
		t.Fatalf("%d %d %s", code, ma, asJSON(d))
	}
	var s v2Stop
	if code, ma := v2get(t, a, "/v2/stops/"+esc("oasa:S3"), &s); code != 200 || ma != 5 || s.Stop != nil ||
		s.Arrivals == nil || len(s.Arrivals) != 0 {
		t.Fatalf("%d %d %s", code, ma, asJSON(s))
	}
}

func TestV2Stops(t *testing.T) {
	a := oasaOnly(newApp(t, monday1020))
	poll(a, monday1020)
	var idx struct {
		Stops   []model.IndexEntry  `json:"stops"`
		Sources []model.SourceState `json:"sources"`
	}
	if _, ma := v2get(t, a, "/v2/stops", &idx); ma != 3600 || len(idx.Stops) != 5 {
		t.Fatalf("%d %s", ma, asJSON(idx))
	}
	if e := idx.Stops[3]; e.ID != "oasa:S3" || e.Name != "STOP 3" || strings.Join(e.Modes, ",") != "bus" ||
		strings.Join(e.Lines, ",") != "oasa:L" {
		t.Fatalf("entry %s", asJSON(e))
	}
	res, body := get(t, a, "/v2/stops/"+esc("oasa:NOPE"))
	if res.StatusCode != 404 || !strings.Contains(string(body), `"unknown_stop"`) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

// /v2/stops/{id} for a bus stop is the /v1 board's data in the new shape, cut by the /v2 rules.
func TestV2StopMatchesV1(t *testing.T) {
	a := newApp(t, monday1020)
	poll(a, monday1020)
	v1, _ := stopBody(t, a, "S3")
	var s v2Stop
	if _, ma := v2get(t, a, "/v2/stops/"+esc("oasa:S3"), &s); ma != stopMaxAge || s.Stop == nil || s.Stop.ID != "oasa:S3" ||
		len(s.Lines) != 1 || s.Lines[0].ID != "oasa:L" || s.UpdatedAt != monday1020.Unix() {
		t.Fatalf("%d %s", ma, asJSON(s))
	}
	// 2 h ahead and 4 per line + headsign: the bus and T02..T04 (v1: 1 h, 3 per variant).
	if len(s.Arrivals) != 4 {
		t.Fatalf("arrivals %s", asJSON(s.Arrivals))
	}
	for i, old := range v1.Arrivals {
		got := s.Arrivals[i]
		day := "2026-10-05"
		want := model.Arrival{Line: "oasa:" + old.Line, Stop: "oasa:S3", Headsign: old.Headsign, At: old.At,
			ScheduledAt: old.Scheduled, Run: model.Ptr("oasa:run:" + *old.TripID + ":" + day)}
		if old.Source == "live" {
			want.Source, want.DelayS, want.Status = model.SourceLive, model.Ptr(old.DelayS), model.Delayed // 440 s late
			want.Vehicle, want.VehicleLabel = model.Ptr("oasa:"+old.Vehicle), model.Ptr(old.Vehicle)
		} else {
			want.Source, want.Status = model.SourceTable, model.Scheduled
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("row %d\n got %s\nwant %s", i, asJSON(got), asJSON(want))
		}
	}
	if last := s.Arrivals[3]; last.At != time.Date(2026, 10, 5, 10, 55, 0, 0, gtfs.Athens).Unix() {
		t.Fatalf("T04 %s", asJSON(last))
	}
}

func TestV2LineVehiclesMatchV1(t *testing.T) {
	a := newApp(t, monday1020)
	poll(a, monday1020)
	_, body := get(t, a, "/v1/lines/L")
	var v1 LineResponse
	json.Unmarshal(body, &v1)
	var v v2Vehicles
	_, ma := v2get(t, a, "/v2/lines/"+esc("oasa:L")+"/vehicles", &v)
	if v.Line != "oasa:L" || v.UpdatedAt != v1.UpdatedAt || v.NextUpdateAt != v1.NextUpdateAt ||
		len(v.Vehicles) != len(v1.Vehicles) || stateOf(v.Sources, "oasa") != model.OK || ma < 5 {
		t.Fatalf("%d %s", ma, asJSON(v))
	}
	old, got := v1.Vehicles[0], v.Vehicles[0]
	want := model.Vehicle{ID: "oasa:44548", Line: "oasa:L", Run: model.Ptr("oasa:run:T00:2026-10-05"), Mode: "bus",
		Headsign: model.Ptr("EAST"), Label: model.Ptr("44548"),
		Position: model.Position{Kind: model.GPS, Lat: old.Lat, Lon: old.Lon, Bearing: old.Bearing, At: old.PositionAt},
		DelayS:   old.DelayS, NextStop: model.Ptr("oasa:S3"), Direction: model.Ptr(0),
		Motion: &model.Motion{Path: old.Path, PathBeyond: old.PathBeyond, PathStops: old.PathStops, Speed: old.Speed,
			PathSpeeds: old.PathSpeeds},
		Quality: &model.VehicleQuality{Level: "unknown"}, // rev 23: no usual service known in the test
		Source:  "oasa"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %s\nwant %s", asJSON(got), asJSON(want))
	}
	if !a.sched.IsWatched("L") {
		t.Error("asking a line's vehicles marks it watched")
	}
	res, b := get(t, a, "/v2/lines/"+esc("oasa:NOPE")+"/vehicles")
	if res.StatusCode != 404 || !strings.Contains(string(b), `"unknown_line"`) {
		t.Fatalf("%d %s", res.StatusCode, b)
	}
}

func TestV2VehiclesAndTiles(t *testing.T) {
	now := monday1020
	a := newAppWith(t, sched.DefaultConfig(), func() time.Time { return now })
	poll(a, now)
	var v v2Vehicles
	res, _ := get(t, a, "/v2/vehicles")
	if _, ma := v2get(t, a, "/v2/vehicles", &v); ma != vehiclesMaxAge || len(v.Vehicles) != 2 ||
		v.Vehicles[0].ID != "oasa:44548" || v.Vehicles[0].Motion == nil || stateOf(v.Sources, "oasa") != model.OK ||
		res.Header.Get("ETag") == "" {
		t.Fatalf("%d %s", ma, asJSON(v))
	}
	if a.sched.IsWatched("L") {
		t.Fatal("/v2/vehicles marked a line watched")
	}
	x, y := tileOf(37.98, 23.725, tileDetailZ)
	_, body := get(t, a, "/v2/vehicles/tiles/13/"+itoa(x)+"/"+itoa(y))
	if !strings.Contains(string(body), `"oasa:44548"`) || !strings.Contains(string(body), `"motion":{`) {
		t.Fatalf("z13 tile %s", body)
	}
	x, y = tileOf(37.98, 23.725, tileOverviewZ)
	_, body = get(t, a, "/v2/vehicles/tiles/9/"+itoa(x)+"/"+itoa(y))
	if !strings.Contains(string(body), `"oasa:44548"`) || strings.Contains(string(body), `"motion"`) ||
		!strings.Contains(string(body), `"sources"`) {
		t.Fatalf("z9 tile %s", body)
	}
	if res, _ := get(t, a, "/v2/vehicles/tiles/13/1/1"); res.StatusCode != 404 {
		t.Fatalf("out of the domain: %d", res.StatusCode)
	}
	if res, _ := get(t, a, "/v2/vehicles/tiles/13/"+itoa(x)+"/"+itoa(y)+"?a=1"); res.StatusCode != 400 {
		t.Fatalf("query: %d", res.StatusCode)
	}
}

func TestV2Runs(t *testing.T) {
	a := newApp(t, monday1020)
	poll(a, monday1020)
	var r struct {
		Run     *model.Run          `json:"run"`
		Sources []model.SourceState `json:"sources"`
	}
	if _, ma := v2get(t, a, "/v2/runs/"+esc("oasa:run:T00:2026-10-05"), &r); ma != 15 || r.Run == nil {
		t.Fatalf("%d %s", ma, asJSON(r))
	}
	run := r.Run
	if run.Line != "oasa:L" || run.Headsign != "EAST" || *run.Pattern != "oasa:SH" || *run.Vehicle != "oasa:44548" ||
		*run.VehicleLabel != "44548" || run.Status != model.Delayed || len(run.Stops) != 5 {
		t.Fatalf("%s", asJSON(run))
	}
	for i, s := range run.Stops {
		sched := time.Date(2026, 10, 5, 10, 5*i, 0, 0, gtfs.Athens).Unix()
		if s.Stop != "oasa:S"+itoa(i) || *s.ScheduledAt != sched || s.Passed != (i < 3) || *s.DelayS != 440 ||
			s.Source != model.SourceLive || s.At != max(sched+440, monday1020.Unix()) && !s.Passed {
			t.Fatalf("stop %d %s", i, asJSON(s))
		}
	}

	// Not started: the timetable, no vehicle.
	v2get(t, a, "/v2/runs/"+esc("oasa:run:T03:2026-10-05"), &r)
	if r.Run == nil || r.Run.Vehicle != nil || r.Run.Status != model.Scheduled || r.Run.Stops[0].Passed ||
		r.Run.Stops[0].Source != model.SourceTable || r.Run.Stops[0].DelayS != nil ||
		r.Run.Stops[0].At != time.Date(2026, 10, 5, 10, 30, 0, 0, gtfs.Athens).Unix() {
		t.Fatalf("%s", asJSON(r.Run))
	}
	for _, id := range []string{"oasa:run:T00:2026-11-30", "oasa:run:T00:2026-10-06", "oasa:run:NOPE:2026-10-05",
		"oasa:run:T00", "oasa:T00", "zzz:run:T00:2026-10-05"} {
		res, body := get(t, a, "/v2/runs/"+esc(id))
		if res.StatusCode != 404 || !strings.Contains(string(body), `"unknown_run"`) {
			t.Fatalf("%s: %d %s", id, res.StatusCode, body)
		}
	}
}

func TestV1UnchangedByV2(t *testing.T) {
	// The /v1 paths keep their exact routes (no /v2 handler answers them).
	a := newApp(t, monday1020)
	poll(a, monday1020)
	if res, _ := get(t, a, "/v2/nope"); res.StatusCode != 404 {
		t.Fatal(res.StatusCode)
	}
	sr, _ := stopBody(t, a, "S3")
	if len(sr.Arrivals) != 3 {
		t.Fatalf("v1 board changed: %s", asJSON(sr))
	}
}

// An incomplete /v2/vehicles snapshot is rebuilt after 5 s, not 30 s: a recovered Source shows.
func TestV2IncompleteSnapshotIsShort(t *testing.T) {
	a := newApp(t, monday1020)
	a.mu.Lock()
	a.w = nil // oasa warming up
	a.mu.Unlock()
	var v v2Vehicles
	if _, ma := v2get(t, a, "/v2/vehicles", &v); ma != 5 || v.NextUpdateAt != monday1020.Unix()+5 {
		t.Fatalf("%d %s", ma, asJSON(v))
	}
	later := monday1020.Add(6 * time.Second)
	a.now = func() time.Time { return later }
	v2get(t, a, "/v2/vehicles", &v)
	if v.UpdatedAt != later.Unix() {
		t.Fatalf("snapshot reused: %s", asJSON(v))
	}
}

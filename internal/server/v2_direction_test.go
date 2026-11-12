package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/gtfs/gtfstest"
	"github.com/angelospk/athens-transit-rt/internal/match"
)

// twoWayFeed is line "40" (ΠΕΙΡΑΙΑΣ - ΣΥΝΤΑΓΜΑ) as OASA publishes it: no direction_id (every trip
// 0), each way on its own side of the road (own stop ids), the way back the busiest shape.
// GO runs Πειραιάς -> Σύνταγμα, NIGHT the same way at night, BACK the other way, SHORT turns back
// halfway, GOSHORT runs the second half of GO. Line "50" is a loop with one shape whose
// headsign names the first place of the line name: a loop is 0 all the same.
func twoWayFeed(t *testing.T) *gtfs.Feed {
	t.Helper()
	var stops, shapes, trips, st strings.Builder
	stops.WriteString("stop_id,stop_name,stop_lat,stop_lon\n")
	shapes.WriteString("shape_id,shape_pt_lat,shape_pt_lon,shape_pt_sequence\n")
	trips.WriteString("route_id,service_id,trip_id,trip_headsign,direction_id,shape_id\n")
	st.WriteString("trip_id,arrival_time,departure_time,stop_id,stop_sequence\n")
	// Σ lies off the Π-Μ axis, so the ends of a half trip do not tie by chance.
	lon := map[string]float64{"P": 23.60, "M": 23.65, "S": 23.70}
	lat := map[string]float64{"P": 37.98, "M": 37.98, "S": 38.03}
	for _, s := range []string{"P", "M", "S"} {
		fmt.Fprintf(&stops, "%s,%s,%.3f,%.2f\n%s2,%s,%.3f,%.2f\n", s, s, lat[s], lon[s], s, s, lat[s]+0.001, lon[s])
	}
	stops.WriteString("L1,L1,38.000,23.60\nL2,L2,38.010,23.62\n")
	shape := func(id string, ids ...string) {
		for i, s := range ids {
			fmt.Fprintf(&shapes, "%s,37.98,%.2f,%d\n", id, lon[strings.TrimSuffix(s, "2")], i+1)
		}
	}
	shape("GO", "P", "M", "S")
	shape("NIGHT", "P", "M", "S")
	shape("BACK", "S2", "M2", "P2")
	shape("SHORT", "M2", "P2")
	shape("GOSHORT", "M", "S")
	shapes.WriteString("LOOP,38.000,23.60,1\nLOOP,38.010,23.62,2\nLOOP,38.000,23.60,3\n")
	n := 0
	trip := func(route, shape, head string, count int, ids ...string) {
		for k := 0; k < count; k++ {
			n++
			fmt.Fprintf(&trips, "%s,day_1,T%d,%s,0,%s\n", route, n, head, shape)
			for i, s := range ids {
				at := 8*3600 + n*600 + i*300
				fmt.Fprintf(&st, "T%d,%s,%s,%s,%d\n", n, hhmmss(at), hhmmss(at), s, i+1)
			}
		}
	}
	// The first BACK trip runs only its second half: the ends of a shape come from its most
	// common stop pattern, else GOSHORT (Μ -> Σ) ties with it and joins the wrong way.
	trip("RX", "BACK", "ΣΥΝΤΑΓΜΑ - ΠΕΙΡΑΙΑΣ", 1, "M2", "P2")
	trip("RX", "BACK", "ΣΥΝΤΑΓΜΑ - ΠΕΙΡΑΙΑΣ", 4, "S2", "M2", "P2")
	trip("RX", "GOSHORT", "ΣΥΝΤΑΓΜΑ", 1, "M", "S")
	trip("RX", "GO", "ΠΕΙΡΑΙΑΣ - ΣΥΝΤΑΓΜΑ", 3, "P", "M", "S")
	trip("RX", "NIGHT", "ΠΕΙΡΑΙΑΣ - ΣΥΝΤΑΓΜΑ (22:00-06:00)", 1, "P", "M", "S")
	trip("RX", "SHORT", "ΠΕΙΡΑΙΑΣ", 1, "M2", "P2")
	trip("RY", "LOOP", "ΛΑ", 2, "L1", "L2", "L1")
	f, err := gtfs.LoadZip(gtfstest.Zip(t, map[string]string{
		"stops.txt":  stops.String(),
		"shapes.txt": shapes.String(),
		"trips.txt":  trips.String(),
		"routes.txt": "route_id,route_short_name,route_long_name,route_type\n" +
			"RX,40,ΠΕΙΡΑΙΑΣ - ΣΥΝΤΑΓΜΑ,3\nRY,50,ΛΑ - ΛΒ (ΚΥΚΛΙΚΗ),3\n",
		"stop_times.txt": st.String(),
		"calendar.txt": "service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date\n" +
			"day_1,1,0,0,0,0,0,0,20260706,20261006\n",
	}), gtfs.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func hhmmss(s int) string { return fmt.Sprintf("%02d:%02d:%02d", s/3600, s/60%60, s%60) }

// The GTFS has no direction_id, so every OASA Pattern was direction 0 (rt#45). The two ways get
// 0 and 1 by where they run, and 0 is the way /v1's daily schedule calls 0: towards the last
// place of the line name (ΣΥΝΤΑΓΜΑ), even when the other way has the most trips.
func TestV2OASAPatternDirections(t *testing.T) {
	a := oasaOnly(newApp(t, monday1020))
	a.SetFeed(twoWayFeed(t), nil)
	for line, want := range map[string]map[string]int{
		"40": {"oasa:GO": 0, "oasa:GOSHORT": 0, "oasa:NIGHT": 0, "oasa:BACK": 1, "oasa:SHORT": 1},
		"50": {"oasa:LOOP": 0},
	} {
		var d v2LineDetail
		if code, _ := v2get(t, a, "/v2/lines/"+esc("oasa:"+line), &d); code != 200 || d.Line == nil {
			t.Fatalf("line %s: %d %s", line, code, asJSON(d))
		}
		got := map[string]int{}
		for _, p := range d.Patterns {
			got[p.ID] = p.Direction
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("line %s: directions %v, want %v", line, got, want)
		}
	}
}

// A bus carries the direction of the Pattern it runs (CONTRACT rev 12), not the GTFS 0.
func TestV2OASAVehicleDirection(t *testing.T) {
	a := oasaOnly(newApp(t, monday1020))
	f := twoWayFeed(t)
	a.SetFeed(f, nil)
	w := a.world()
	for _, ti := range f.TripsForLine("40") {
		tr := f.Trip(ti)
		want := 0
		if sh := f.Shapes[tr.Shape].ID; sh == "BACK" || sh == "SHORT" {
			want = 1
		}
		r := match.Result{Trip: ti}
		v := vehicleV2(f, w.shapeDirs("40"), "bus", "40", &r, &Vehicle{ID: "1"}, boardPos{}, nil)
		if v.Direction == nil || *v.Direction != want {
			t.Errorf("trip %s on %s: direction %v, want %d", tr.ID, f.Shapes[tr.Shape].ID, v.Direction, want)
		}
		b := boardPos{trip: ti, ok: true}
		v = vehicleV2(f, w.shapeDirs("40"), "bus", "40", &match.Result{Trip: -1}, &Vehicle{ID: "1"}, b, nil)
		if v.Direction == nil || *v.Direction != want {
			t.Errorf("placed on %s: direction %v, want %d", tr.ID, v.Direction, want)
		}
	}
}

// A publish keeps the shape directions of its own feed (rt#70), so a bus going the other way
// says 1 on both vehicle endpoints, and still says 1 after a feed swap until its line polls again.
func TestV2OASAVehicleEndpointsDirection(t *testing.T) {
	a := oasaOnly(newApp(t, monday1020))
	poll(a, monday1020)
	if d, w := a.lines["L"], a.world(); d.dirs == nil || fmt.Sprint(d.dirs) != fmt.Sprint(w.shapeDirs("L")) {
		t.Fatalf("published dirs %v, want %v", d.dirs, w.shapeDirs("L"))
	}

	f := twoWayFeed(t)
	a.SetFeed(f, nil)
	back := int32(-1)
	for _, ti := range f.TripsForLine("40") {
		if f.Shapes[f.Trip(ti).Shape].ID == "BACK" {
			back = ti
			break
		}
	}
	at := monday1020.Unix()
	a.mu.Lock()
	a.known["40"] = true
	a.lines["40"] = &lineData{updated: monday1020, feed: f, dirs: a.w.shapeDirs("40"),
		results:  []match.Result{{VehicleID: "b", Line: "40", Trip: back, Time: monday1020}},
		vehicles: []Vehicle{{ID: "b", Lat: 37.98, Lon: 23.65, PositionAt: at}}, board: []boardPos{{}}}
	a.mu.Unlock()
	o := &oasaAdapter{a}
	check := func(when string, all bool) {
		t.Helper()
		lv, err := o.LineVehicles("oasa:40", monday1020)
		if err != nil || len(lv.Vehicles) != 1 || lv.Vehicles[0].Direction == nil || *lv.Vehicles[0].Direction != 1 {
			t.Errorf("%s: line vehicles %v %s", when, err, asJSON(lv))
		}
		if !all {
			return
		}
		vs, err := o.Vehicles(monday1020)
		if err != nil || len(vs) != 1 || vs[0].Direction == nil || *vs[0].Direction != 1 {
			t.Errorf("%s: vehicles %v %s", when, err, asJSON(vs))
		}
	}
	check("same feed", true)
	a.SetFeed(twoWayFeed(t), nil) // same content, a new feed: the line's data is from the old one
	check("after a feed swap", false)
}

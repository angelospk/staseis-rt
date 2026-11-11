package oseth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

func TestNormID(t *testing.T) {
	for in, want := range map[string]string{"0104": "104", "104": "104", " 2629 ": "2629", "0": "0", "000": "0"} {
		if got := NormID(in); got != want {
			t.Errorf("NormID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRouteVehicles(t *testing.T) {
	body, err := os.ReadFile("testdata/route_info.json")
	if err != nil {
		t.Fatal(err)
	}
	var gotPath, gotShape string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotShape = r.URL.Path, r.URL.Query().Get("shapeId")
		w.Write(body)
	}))
	defer srv.Close()
	c := New(srv.URL+"/el/telematics-api", 100)
	vs, err := c.RouteVehicles(context.Background(), "28_3981_1_3", "6031")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/el/telematics-api/route/28_3981_1_3/info" || gotShape != "6031" {
		t.Fatalf("request %s shapeId=%s", gotPath, gotShape)
	}
	if len(vs) != 2 || vs[0].ID != "104" || vs[0].Lat != 40.6401 || vs[0].Lon != 22.9444 || vs[0].Bearing != 90 || vs[0].ShapeID != "6031" {
		t.Fatalf("vehicles %+v", vs)
	}
}

func TestRouteVehiclesErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":"","error":"not found","status_code":404}`))
	}))
	defer srv.Close()
	c := New(srv.URL, 100)
	if c.LastError() != "" {
		t.Fatal("last error before any call")
	}
	if _, err := c.RouteVehicles(context.Background(), "x", "1"); err == nil {
		t.Fatal("want an error for status_code 404 in the body")
	}
	if got := c.LastError(); got != "route x: status 404 not found" {
		t.Fatalf("last error %q", got)
	}
}

// TM100002 means OSETH has no trip for this route and shape right now: no vehicles, not a failure (#123).
func TestRouteVehiclesNoTripIsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":"","error":"Client error: ` + "`GET https://example/route/57_6805_2_3/info`" + ` resulted in a ` + "`400 Bad Request`" + ` response:\n{\"message\":\"Entity not found: There is not trip for the given route id and shape id.\",\"code\":\"TM100002\"}","status_code":500}`))
	}))
	defer srv.Close()
	c := New(srv.URL, 100)
	vs, err := c.RouteVehicles(context.Background(), "57_6805_2_3", "6059")
	if err != nil || len(vs) != 0 {
		t.Fatalf("got %d vehicles, err %v; want 0 and nil", len(vs), err)
	}
	if c.Errors() != 0 || c.LastError() != "" {
		t.Fatalf("errors %d, last error %q; want none counted", c.Errors(), c.LastError())
	}
}

var t0 = time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)

func obs(id string, lat, lon float64, at time.Time) match.Obs {
	return match.Obs{Vehicle: telematics.Vehicle{VehNo: id, RouteCode: "R1", Lat: lat, Lon: lon, Time: at}, LineCode: "L1"}
}

func sight(id string, lat, lon float64, at time.Time) Sighting {
	return Sighting{Vehicle: Vehicle{ID: id, Lat: lat, Lon: lon, Bearing: 45, ShapeID: "S1"}, At: at}
}

func routeFor(shape string) (string, string, bool) {
	if shape == "S1" {
		return "R1", "L1", true
	}
	return "", "", false
}

const minMove = 30.0 // metres
// ~111 m per 0.001 degree of latitude.

func TestMergeNewerMovedOSETHReplacesOASTH(t *testing.T) {
	oa := []match.Obs{obs("104", 40.640, 22.944, t0)}
	prev := map[string]Sighting{"104": sight("104", 40.640, 22.944, t0.Add(-20*time.Second))}
	cur := map[string]Sighting{"104": sight("104", 40.641, 22.944, t0.Add(25*time.Second))}
	got := Merge(oa, cur, prev, routeFor, minMove)
	if len(got) != 1 || got[0].Lat != 40.641 || !got[0].Time.Equal(t0.Add(25*time.Second)) || got[0].RouteCode != "R1" || got[0].VehNo != "104" {
		t.Fatalf("got %+v", got)
	}
}

func TestMergeFrozenOSETHNeverReplacesOASTH(t *testing.T) {
	// OSETH shows the bus where it stood a while ago, and has not moved since: OASTH stays.
	oa := []match.Obs{obs("104", 40.650, 22.944, t0)}
	prev := map[string]Sighting{"104": sight("104", 40.640, 22.944, t0.Add(-20*time.Second))}
	cur := map[string]Sighting{"104": sight("104", 40.640, 22.944, t0.Add(25*time.Second))}
	got := Merge(oa, cur, prev, routeFor, minMove)
	if len(got) != 1 || got[0].Lat != 40.650 || !got[0].Time.Equal(t0) {
		t.Fatalf("got %+v", got)
	}
}

func TestMergeOlderOSETHNeverReplacesOASTH(t *testing.T) {
	oa := []match.Obs{obs("104", 40.650, 22.944, t0)}
	prev := map[string]Sighting{"104": sight("104", 40.630, 22.944, t0.Add(-60*time.Second))}
	cur := map[string]Sighting{"104": sight("104", 40.640, 22.944, t0.Add(-5*time.Second))}
	got := Merge(oa, cur, prev, routeFor, minMove)
	if len(got) != 1 || got[0].Lat != 40.650 {
		t.Fatalf("got %+v", got)
	}
}

func TestMergeOSETHOnlyNeedsMovementAndARoute(t *testing.T) {
	prev := map[string]Sighting{
		"1109": sight("1109", 40.600, 22.950, t0.Add(-30*time.Second)),
		"2629": sight("2629", 40.600, 22.950, t0.Add(-30*time.Second)),
	}
	cur := map[string]Sighting{
		"1109": sight("1109", 40.602, 22.950, t0), // moved ~220 m: real
		"2629": sight("2629", 40.600, 22.950, t0), // frozen: a ghost
		"7777": sight("7777", 40.610, 22.950, t0), // first sighting: not yet trusted
	}
	other := sight("5555", 40.610, 22.950, t0)
	other.ShapeID = "S9" // no route for this shape
	cur["5555"] = other
	prev["5555"] = sight("5555", 40.600, 22.950, t0.Add(-30*time.Second))
	got := Merge(nil, cur, prev, routeFor, minMove)
	if len(got) != 1 || got[0].VehNo != "1109" || got[0].RouteCode != "R1" || got[0].LineCode != "L1" ||
		got[0].Heading != 45 || !got[0].Time.Equal(t0) || got[0].Lat != 40.602 {
		t.Fatalf("got %+v", got)
	}
}

func TestMergeKeepsOASTHWithoutOSETH(t *testing.T) {
	oa := []match.Obs{obs("104", 40.640, 22.944, t0), obs("105", 40.7, 22.9, t0)}
	got := Merge(oa, nil, nil, routeFor, minMove)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if &got[0] == &oa[0] {
		t.Fatal("Merge must return a new slice, not the caller's")
	}
}

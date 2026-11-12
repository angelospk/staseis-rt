package model

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

// Every contract example decodes strictly into its type and encodes back to the same JSON.
func TestFixturesRoundTrip(t *testing.T) {
	cases := map[string]func() any{
		"line.json":             func() any { return &Line{} },
		"stop.json":             func() any { return &Stop{} },
		"vehicle.json":          func() any { return &Vehicle{} },
		"arrival.json":          func() any { return &Arrival{} },
		"pattern.json":          func() any { return &Pattern{} },
		"stop-index-entry.json": func() any { return &IndexEntry{} },
		"run.json":              func() any { return &Run{} },
		"sources.json":          func() any { return &[]SourceState{} },
		"usual.json":            func() any { return &[]Usual{} },
		"vehicle-quality.json":  func() any { return &[]VehicleQuality{} },
		"segment.json":          func() any { return &[]Segment{} },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("../../docs/fixtures/v2/" + name)
			if err != nil {
				t.Fatal(err)
			}
			v := mk()
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(v); err != nil {
				t.Fatal(err)
			}
			out, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			var want, got any
			json.Unmarshal(raw, &want)
			json.Unmarshal(out, &got)
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("round trip:\n got %s\nwant %s", out, raw)
			}
		})
	}
}

func TestNullsStayKeys(t *testing.T) {
	b, _ := json.Marshal(Vehicle{})
	var m map[string]any
	json.Unmarshal(b, &m)
	for _, k := range []string{"run", "headsign", "label", "delay_s", "next_stop", "motion", "quality"} {
		if v, ok := m[k]; !ok || v != nil {
			t.Errorf("%s: %v, %v", k, v, ok)
		}
	}
	b, _ = json.Marshal(Vehicle{}.Lite())
	m = nil
	json.Unmarshal(b, &m)
	if _, ok := m["motion"]; ok {
		t.Error("a lite vehicle has no motion key")
	}
	b, _ = json.Marshal(IndexEntry{ID: "x"})
	if string(b) != `["x","",0,0,[],[]]` {
		t.Errorf("empty index entry %s", b)
	}
}

func TestRunIDs(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Athens")
	day := time.Date(2026, 10, 29, 0, 0, 0, 0, loc)
	id := RunID("oasa", "T:1:x", day, loc)
	if id != "oasa:run:T:1:x:2026-10-29" {
		t.Fatal(id)
	}
	native, noon, ok := ParseRunID("oasa", id, loc)
	if !ok || native != "T:1:x" || !noon.Equal(day.Add(12*time.Hour)) {
		t.Fatal(native, noon, ok)
	}
	for _, bad := range []string{"oasa:run:x", "stasy:run:x:2026-10-29", "oasa:run::2026-10-29", "oasa:run:x:2026-1-29", "oasa:run:x:2026-02-30"} {
		if _, _, ok := ParseRunID("oasa", bad, loc); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if Namespace("stasy:συνταγμα-metro") != "stasy" || Namespace("nons") != "nons" {
		t.Fatal("namespace")
	}
}

func TestStatusAndComplete(t *testing.T) {
	if StatusOf(false, Ptr(900)) != Scheduled || StatusOf(true, nil) != Running ||
		StatusOf(true, Ptr(299)) != Running || StatusOf(true, Ptr(300)) != Delayed {
		t.Fatal("status")
	}
	if !Complete([]SourceState{{State: OK}, {State: Stale}, {State: Indicative}}) ||
		Complete([]SourceState{{State: OK}, {State: WarmingUp}}) || Complete([]SourceState{{State: Down}}) {
		t.Fatal("complete")
	}
}

// On a DST change day the run's noon is still 12:00 local (2026-10-25: clocks go back in Athens).
func TestParseRunIDNoonOnDSTDay(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Athens")
	if err != nil {
		t.Skip(err)
	}
	_, noon, ok := ParseRunID("stasy", "stasy:run:x:2026-10-25", loc)
	if !ok || !noon.Equal(time.Date(2026, 10, 25, 12, 0, 0, 0, loc)) {
		t.Fatal(noon, ok)
	}
}

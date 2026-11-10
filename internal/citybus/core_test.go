package citybus

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// Ported from citybus-bridge test/core.test.ts and test/health.test.ts.

// A straight east-west route of 5 stops, ~0.9 km apart (0.01° lon at 35.3° N is ~908 m).
func lineRaw() *RawCity {
	codes := []string{"A", "B", "C", "D", "E"}
	var stops []RawStop
	var seq []RawSeq
	for i, c := range codes {
		stops = append(stops, RawStop{Code: c, Name: "S" + c, Latitude: 35.3, Longitude: Num(25.1 + float64(i)*0.01),
			LineCodes: []string{"1"}, RouteCodes: []string{"10"}})
		seq = append(seq, RawSeq{Sequence: i + 1, Code: c})
	}
	day := athensClock(time.Now().Unix()).Day
	var trips []RawTripRow
	for i, c := range []string{"A", "B", "C", "D"} {
		trips = append(trips, RawTripRow{ID: 7, Day: day, RouteCode: "10", StopCode: c, TripTime: []string{"10:00", "10:02", "10:04", "10:06"}[i]})
	}
	return &RawCity{
		Lines:     []RawLine{{Code: "1", Name: "ΓΡΑΜΜΗ 1", Color: "#D50000", TextColor: "#ffffff", Routes: []RawRouteRef{{Code: "10", Name: "A - E.", Direction: 1}}}},
		Stops:     stops,
		Sequences: map[string][]RawSeq{"10": seq},
		Points: map[string][]RawPoints{"1": {{RouteCode: "10", RoutePoints: []RawPoint{
			{Sequence: 1, Latitude: 35.3, Longitude: 25.1}, {Sequence: 2, Latitude: 35.3, Longitude: 25.14}}}}},
		Trips: map[string][]RawTripRow{"10": trips},
	}
}

func row(route, lat, lon string, mins int, vehicle string) LiveRow {
	return LiveRow{LineCode: "1", RouteCode: route, Latitude: mustNum(lat), Longitude: mustNum(lon), DepartureMins: Num(mins), VehicleCode: vehicle}
}

func mustNum(s string) Num {
	var n Num
	if err := n.UnmarshalJSON([]byte(`"` + s + `"`)); err != nil {
		panic(err)
	}
	return n
}

func TestNumReadsNumbersAndStrings(t *testing.T) {
	for in, want := range map[string]float64{`35.3`: 35.3, `"25.1389530000001"`: 25.1389530000001, `null`: 0, `""`: 0} {
		var n Num
		if err := n.UnmarshalJSON([]byte(in)); err != nil || float64(n) != want {
			t.Errorf("%s: got %v %v, want %v", in, float64(n), err, want)
		}
	}
	var n Num
	if err := n.UnmarshalJSON([]byte(`"abc"`)); err == nil {
		t.Error("a non-number string must fail")
	}
}

func TestTripTimesFollowStopOrder(t *testing.T) {
	rows := []RawTripRow{{ID: 1, Day: 1, RouteCode: "r", StopCode: "A", TripTime: "07:00"},
		{ID: 1, Day: 1, RouteCode: "r", StopCode: "X", TripTime: "07:01"},
		{ID: 1, Day: 1, RouteCode: "r", StopCode: "C", TripTime: "07:05"}}
	got := tripTimes([]string{"A", "B", "C", "A"}, rows)
	if want := []int{25200, NoTime, 25500, NoTime}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestTripTimesPastMidnightKeepGrowing(t *testing.T) {
	rows := []RawTripRow{{ID: 1, Day: 5, RouteCode: "r", StopCode: "A", TripTime: "23:55"},
		{ID: 1, Day: 5, RouteCode: "r", StopCode: "B", TripTime: "00:10"}}
	if got := tripTimes([]string{"A", "B"}, rows); !reflect.DeepEqual(got, []int{86100, 87000}) {
		t.Fatalf("got %v", got)
	}
}

func TestModelBasics(t *testing.T) {
	m := buildModel(lineRaw())
	r := m.Routes["10"]
	if r.Dir != 0 || r.Name != "A - E" || m.Lines[0].Color != "#d50000" {
		t.Fatalf("dir %d name %q color %q", r.Dir, r.Name, m.Lines[0].Color)
	}
	if got := math.Round(r.StopM[1] / 10); got != 91 {
		t.Fatalf("stop B at %v m", r.StopM[1])
	}
	if want := []int{36000, 36120, 36240, 36360, NoTime}; !reflect.DeepEqual(r.Trips[0].Times, want) {
		t.Fatalf("times %v", r.Trips[0].Times)
	}
	if r.Trips[0].ID != "7" {
		t.Fatalf("trip id %q", r.Trips[0].ID)
	}
}

func TestModelDropsUnusedStopsAndSortsLines(t *testing.T) {
	raw := lineRaw()
	raw.Stops = append(raw.Stops, RawStop{Code: "Z", Name: "nowhere", Latitude: 35, Longitude: 25})
	raw.Lines = append(raw.Lines, RawLine{Code: "02A", Name: "x", Color: "bad", TextColor: "", Routes: []RawRouteRef{{Code: "20", Name: "R", Direction: 2}}})
	raw.Sequences["20"] = []RawSeq{{Sequence: 2, Code: "C"}, {Sequence: 1, Code: "B"}}
	m := buildModel(raw)
	if _, ok := m.Stops["Z"]; ok {
		t.Fatal("a stop no route calls at must be left out")
	}
	if m.Lines[0].Code != "1" || m.Lines[1].Code != "02A" {
		t.Fatalf("line order %v %v", m.Lines[0].Code, m.Lines[1].Code)
	}
	l := m.LineByCode["02A"]
	if l.Color != "#1565c0" || l.TextColor != "#ffffff" {
		t.Fatalf("fallback colours %q %q", l.Color, l.TextColor)
	}
	r := m.Routes["20"]
	if r.Dir != 1 || !reflect.DeepEqual(r.Stops, []string{"B", "C"}) || len(r.Shape) != 2 {
		t.Fatalf("route 20: dir %d stops %v shape %v", r.Dir, r.Stops, r.Shape)
	}
	if !reflect.DeepEqual(m.Stops["B"].Lines, []string{"1", "02A"}) {
		t.Fatalf("stop B lines %v", m.Stops["B"].Lines)
	}
}

func TestProbeStopsCoverMiddleAndEnd(t *testing.T) {
	if got := probeStops(buildModel(lineRaw()), nil); !reflect.DeepEqual(got, []string{"C"}) {
		t.Fatalf("got %v", got) // C is both the middle and 2 from the end
	}
}

// Two lines, one route each, on separate streets.
func mk(code string, lat float64) []RawStop {
	var out []RawStop
	for i, s := range []string{"A", "B", "C", "D", "E"} {
		out = append(out, RawStop{Code: code + s, Name: s, Latitude: Num(lat), Longitude: Num(25.1 + float64(i)*0.01),
			LineCodes: []string{code}, RouteCodes: []string{code + "0"}})
	}
	return out
}

func seqOf(code string) []RawSeq {
	var out []RawSeq
	for i, s := range []string{"A", "B", "C", "D", "E"} {
		out = append(out, RawSeq{Sequence: i + 1, Code: code + s})
	}
	return out
}

func linesRaw(codes ...string) *RawCity {
	raw := &RawCity{Sequences: map[string][]RawSeq{}, Points: map[string][]RawPoints{}, Trips: map[string][]RawTripRow{}}
	for i, c := range codes {
		raw.Lines = append(raw.Lines, RawLine{Code: c, Name: c, Color: "#000000", TextColor: "#ffffff", Routes: []RawRouteRef{{Code: c + "0", Name: "R", Direction: 1}}})
		raw.Stops = append(raw.Stops, mk(c, 35.3+0.1*float64(i))...)
		raw.Sequences[c+"0"] = seqOf(c)
	}
	return raw
}

func TestProbeStopsSkipLines(t *testing.T) {
	m := buildModel(linesRaw("1", "2"))
	got := probeStops(m, nil)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"1C", "2C"}) {
		t.Fatalf("got %v", got)
	}
	if got := probeStops(m, map[string]bool{"2": true}); !reflect.DeepEqual(got, []string{"1C"}) {
		t.Fatalf("skip 2: %v", got)
	}
	if got := probeStops(m, map[string]bool{"1": true, "2": true}); len(got) != 0 {
		t.Fatalf("skip all: %v", got)
	}
}

func TestSightingsPlaceAndMaxAge(t *testing.T) {
	m := buildModel(lineRaw())
	rows := []LiveRow{row("10", "35.3", "25.11", 2, "v1"), row("10", "36.0", "25.11", 1, "far")}
	s := sightings(m, Board{Stop: "D", At: 1000, Rows: rows})
	if len(s) != 1 || s[0].ID != "v1" {
		t.Fatalf("sightings %v", s) // the bus 78 km off the shape is skipped
	}
	if s[0].Speed == nil || *s[0].Speed != 15 {
		t.Fatalf("speed %v", s[0].Speed) // 1.8 km in 120 s is 15 m/s: the cap
	}
	now := place(m, s[0], 1000)
	if now == nil || now.Kind != "gps" || m.Routes["10"].Stops[now.NextIdx] != "C" {
		t.Fatalf("placed now %+v", now)
	}
	later := place(m, s[0], 1120)
	if later.Kind != "estimate" || later.M <= s[0].ToM-30 || later.M > s[0].ToM {
		t.Fatalf("later %v (to %v)", later.M, s[0].ToM)
	}
	if later.At != 1120 || now.At != 1000 {
		t.Fatalf("at: gps keeps the sighting time, estimate is now: %d %d", now.At, later.At)
	}
	if MaxAge != 120 || place(m, s[0], 1000+121) != nil {
		t.Fatal("a sighting older than 120 s must not be placed")
	}
}

func TestStopArrivalsLiveTakesNearestRun(t *testing.T) {
	m := buildModel(lineRaw())
	c := athensClock(time.Now().Unix())
	now := c.Midnight + 36000 - 600 // 09:50, today
	live := &Board{Stop: "B", At: now, Rows: []LiveRow{row("10", "35.3", "25.1", 12, "v1")}}
	a := stopArrivals(m, "B", live, nil, now)
	if len(a) != 1 || !a[0].Live || a[0].At-now != 720 {
		t.Fatalf("live: %+v", a) // the 10:02 run is that bus
	}
	none := stopArrivals(m, "B", nil, nil, now)
	if len(none) != 1 || none[0].Live || none[0].At-now != 720 || none[0].Trip != "7" {
		t.Fatalf("timetable: %+v", none)
	}
	if got := stopArrivals(m, "E", nil, nil, now); len(got) != 0 {
		t.Fatalf("the trip ends at E: %+v", got)
	}
}

// A board row at 0,0 has no GPS: citybus.gr made it from its timetable (vehicleCode is a trip key such
// as 20261107_0302_0000010_22_45). It is no bus; the timetable run stays, once.
func TestStopArrivalsNoGPSRowIsTimetable(t *testing.T) {
	m := buildModel(lineRaw())
	c := athensClock(time.Now().Unix())
	now := c.Midnight + 36000 - 600 // 09:50, today
	ghost := &Board{Stop: "B", At: now, Rows: []LiveRow{row("10", "0", "0", 12, "20261107_10_0000007_10_00")}}
	a := stopArrivals(m, "B", ghost, nil, now)
	if len(a) != 1 || a[0].Live || a[0].Vehicle != "" || a[0].Trip != "7" || a[0].At-now != 720 {
		t.Fatalf("no GPS: %+v", a)
	}
}

func TestStopArrivalsAroundMidnight(t *testing.T) {
	m := buildModel(lineRaw())
	c := athensClock(time.Now().Unix())
	r := m.Routes["10"]
	r.Trips = []Trip{{ID: "t1", Day: (c.Day + 1) % 7, Times: []int{600, 720, 840, 960, NoTime}}} // 00:10 tomorrow at A
	now := c.Midnight + 86400 - 600                                                              // 23:50 today
	a := stopArrivals(m, "B", nil, nil, now)
	if len(a) != 1 || a[0].At-now != 600+720 {
		t.Fatalf("tomorrow: %+v", a)
	}
	r.Trips = []Trip{{ID: "t2", Day: c.Day, Times: []int{86400 + 60, 86400 + 180, NoTime, NoTime, NoTime}}} // 24:01 today
	a = stopArrivals(m, "A", nil, nil, c.Midnight+86400)
	if len(a) != 1 || a[0].At-c.Midnight != 86460 {
		t.Fatalf("yesterday late: %+v", a)
	}
}

func TestStopArrivalsDropPassedLiveRows(t *testing.T) {
	m := buildModel(lineRaw())
	now := int64(10_000)
	for _, at := range []int64{now - 200, now - 61} {
		b := &Board{Stop: "B", At: at, Rows: []LiveRow{row("10", "35.3", "25.1", 1, "v1")}}
		for _, a := range stopArrivals(m, "B", b, nil, now) {
			if a.Live {
				t.Fatalf("board at %d: a passed bus shown as due: %+v", at, a)
			}
		}
	}
}

func TestLiveCityKeepsNewestSighting(t *testing.T) {
	m := buildModel(lineRaw())
	l := newLiveCity(m)
	b := func(lon string, at int64) Board {
		return Board{Stop: "D", At: at, Rows: []LiveRow{row("10", "35.3", lon, 3, "v")}}
	}
	l.add(b("25.12", 2000))
	l.add(b("25.11", 1990))
	if l.Buses["v"].Lon != 25.12 {
		t.Fatalf("lon %v", l.Buses["v"].Lon)
	}
	l.prune(2000 + 121)
	if len(l.Buses) != 0 || len(l.Boards) != 0 {
		t.Fatal("prune must drop sightings and boards older than 120 s")
	}
}

// A loop: out along A..C and back to A on the same street (A B C B A).
func loopRaw() *RawCity {
	var stops []RawStop
	for _, s := range []struct {
		c   string
		lon float64
	}{{"A", 25.1}, {"B", 25.11}, {"C", 25.12}} {
		stops = append(stops, RawStop{Code: s.c, Name: s.c, Latitude: 35.3, Longitude: Num(s.lon), LineCodes: []string{"L"}, RouteCodes: []string{"90"}})
	}
	var seq []RawSeq
	for i, c := range []string{"A", "B", "C", "B", "A"} {
		seq = append(seq, RawSeq{Sequence: i + 1, Code: c})
	}
	return &RawCity{
		Lines:     []RawLine{{Code: "L", Name: "LOOP", Color: "#000000", TextColor: "#ffffff", Routes: []RawRouteRef{{Code: "90", Name: "LOOP", Direction: 1}}}},
		Stops:     stops,
		Sequences: map[string][]RawSeq{"90": seq},
		Points: map[string][]RawPoints{"L": {{RouteCode: "90", RoutePoints: []RawPoint{
			{Sequence: 0, Latitude: 35.3, Longitude: 25.1}, {Sequence: 1, Latitude: 35.3, Longitude: 25.12}, {Sequence: 2, Latitude: 35.3, Longitude: 25.1}}}}},
	}
}

func TestProjectNeverBeforeMinM(t *testing.T) {
	shape := []LL{{35.3, 25.1}, {35.3, 25.12}, {35.3, 25.1}}
	cum := cumulative(shape)
	if p := project(shape, cum, LL{35.3, 25.11}, 3000); p.M < 3000 {
		t.Fatalf("m %v", p.M)
	}
}

func TestLoopStopMetresGrow(t *testing.T) {
	r := buildModel(loopRaw()).Routes["90"]
	if !reflect.DeepEqual(r.Stops, []string{"A", "B", "C", "B", "A"}) {
		t.Fatalf("stops %v", r.Stops)
	}
	for i := 1; i < len(r.StopM); i++ {
		if r.StopM[i] <= r.StopM[i-1] {
			t.Fatalf("stopM %v", r.StopM)
		}
	}
}

func TestLoopBusOnTheWayBack(t *testing.T) {
	m := buildModel(loopRaw())
	r := m.Routes["90"]
	// 1 min to B: the point is on both legs; 1 min fits the ~450 m back to B, not the ~1360 m via C.
	s := sightings(m, Board{Stop: "B", At: 1000, Rows: []LiveRow{{LineCode: "L", RouteCode: "90", Latitude: 35.3, Longitude: 25.115, DepartureMins: 1, VehicleCode: "v"}}})
	if len(s) != 1 || s[0].M <= r.StopM[2] || s[0].ToM != r.StopM[3] {
		t.Fatalf("sighting %+v stopM %v", s, r.StopM)
	}
}

func TestMotionStopsAtDueStop(t *testing.T) {
	m := buildModel(lineRaw())
	s := sightings(m, Board{Stop: "C", At: 1000, Rows: []LiveRow{row("10", "35.3", "25.11", 2, "v")}})[0]
	mo := motion(place(m, s, 1000))
	if len(mo.PathStops) == 0 || mo.PathStops[len(mo.PathStops)-1] > int(math.Round(s.ToM-s.M))+1 {
		t.Fatalf("path stops %v (to %v)", mo.PathStops, s.ToM-s.M)
	}
	if mo.PathBeyond != nil {
		t.Fatalf("path beyond %v", mo.PathBeyond)
	}
	if len(mo.Path) < 2 {
		t.Fatalf("path %v", mo.Path)
	}
}

func TestGeoHelpers(t *testing.T) {
	if d := dist(LL{35.3, 25.1}, LL{35.3, 25.11}); math.Abs(d-908) > 5 {
		t.Fatalf("dist %v", d)
	}
	if b := bearing(LL{35.3, 25.1}, LL{35.3, 25.11}); b != 90 {
		t.Fatalf("bearing %v", b)
	}
	if b := bearing(LL{35.3, 25.1}, LL{35.2, 25.1}); b != 180 {
		t.Fatalf("bearing %v", b)
	}
	if x, y := tileOf(35.3387, 25.1442, 13); x != 4668 || y != 3235 {
		t.Fatalf("tile %d %d", x, y)
	}
	shape := []LL{{35.3, 25.1}, {35.3, 25.11}, {35.3, 25.12}}
	cum := cumulative(shape)
	sl := slice(shape, cum, 100, cum[2]-100)
	if len(sl) != 3 || sl[1] != shape[1] {
		t.Fatalf("slice %v", sl)
	}
	if p := pointAt(shape, cum, -5); p != shape[0] {
		t.Fatalf("pointAt <0 %v", p)
	}
	if p := pointAt(shape, cum, 1e9); p != shape[2] {
		t.Fatalf("pointAt past end %v", p)
	}
	if c := candidates([]LL{{35.3, 25.1}, {35.3, 25.12}, {35.3, 25.1}}, cumulative([]LL{{35.3, 25.1}, {35.3, 25.12}, {35.3, 25.1}}), LL{35.3, 25.11}, 30); len(c) != 2 {
		t.Fatalf("a loop passes twice: %v", c)
	}
}

func TestAthensClock(t *testing.T) {
	// 2026-11-07 01:22:19 EET (UTC+2) is a Saturday.
	c := athensClock(time.Date(2026, 11, 6, 23, 22, 19, 0, time.UTC).Unix())
	if c.Date != "2026-11-07" || c.Day != 6 || c.Sec != 1*3600+22*60+19 {
		t.Fatalf("clock %+v", c)
	}
	if c.Midnight != time.Date(2026, 11, 6, 22, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("midnight %d", c.Midnight)
	}
}

// ---- line health ----

const D = 86400

func TestTimetableOnlyAfterSevenTrackedDays(t *testing.T) {
	if Days != 7 {
		t.Fatal("Days must be 7")
	}
	h := loadHealth(filepath.Join(t.TempDir(), "health.json"), 1000)
	if h.Since != 1000 || h.timetableOnly("91", 1000+6*D) || !h.timetableOnly("91", 1000+7*D) {
		t.Fatal("tracked under 7 days: never; at 7 days: yes")
	}
	h.saw("20", 1000+5*D)
	if h.timetableOnly("20", 1000+7*D) || !h.timetableOnly("20", 1000+12*D+1) {
		t.Fatal("live 2 days ago: no; last live over 7 days ago: yes")
	}
	h.saw("20", 1000+12*D+2)
	if h.timetableOnly("20", 1000+12*D+3) {
		t.Fatal("live again: back at once")
	}
	h.saw("20", 10)
	if h.LastLive["20"] != 1000+12*D+2 {
		t.Fatal("an older sighting never moves it back")
	}
}

func TestHealthSurvivesRestart(t *testing.T) {
	f := filepath.Join(t.TempDir(), "health.json")
	h := loadHealth(f, 500)
	h.saw("7", 600)
	if err := h.save(); err != nil {
		t.Fatal(err)
	}
	again := loadHealth(f, 9999)
	if again.Since != 500 || !reflect.DeepEqual(again.LastLive, map[string]int64{"7": 600}) {
		t.Fatalf("again %+v", again)
	}
	os.WriteFile(f, []byte("{not json"), 0o644)
	fresh := loadHealth(f, 7777)
	if fresh.Since != 7777 || len(fresh.LastLive) != 0 || fresh.FirstSeen == nil {
		t.Fatalf("fresh %+v", fresh)
	}
}

func TestHealthReadsBridgeFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "health.json")
	os.WriteFile(f, []byte(`{"since":1793972740,"lastLive":{"01":1794004952},"firstSeen":{"01":1793972992}}`), 0o644)
	h := loadHealth(f, 1)
	if h.Since != 1793972740 || h.LastLive["01"] != 1794004952 || h.FirstSeen["01"] != 1793972992 {
		t.Fatalf("%+v", h)
	}
}

func TestNewLineGetsOwnSevenDays(t *testing.T) {
	h := loadHealth(filepath.Join(t.TempDir(), "health.json"), 1000)
	h.track([]string{"1"}, 1000)
	h.track([]string{"1", "9"}, 1000+8*D) // line 9 is new on day 8
	if !h.timetableOnly("1", 1000+8*D) || h.timetableOnly("9", 1000+8*D) || !h.timetableOnly("9", 1000+15*D) {
		t.Fatal("line 9 gets its own 7 days")
	}
	h.track([]string{"9"}, 1000+20*D) // tracking start never moves on
	if !h.timetableOnly("9", 1000+20*D) {
		t.Fatal("first seen must not move")
	}
}

func TestGapFor(t *testing.T) {
	for probes, want := range map[int]int{61: 983, 30: 1000, 0: 1000, 200: 300, 1000: 250} {
		if got := gapFor(probes); got != want {
			t.Errorf("gapFor(%d) = %d, want %d", probes, got, want)
		}
	}
}

func TestOnePointShapeFallsBackToStops(t *testing.T) {
	raw := lineRaw()
	raw.Points["1"][0].RoutePoints = raw.Points["1"][0].RoutePoints[:1]
	r := buildModel(raw).Routes["10"]
	if len(r.Shape) != 5 || r.StopM[4] <= 0 {
		t.Fatalf("shape %v stopM %v", r.Shape, r.StopM)
	}
}

// Kavala 04 (2026-11-10): a loop whose shape starts ~27 m from its first stop and ends ~9 m from it.
// The first stop must sit at the start, not at the end with every later stop squeezed after it.
func TestLoopFirstStopNearerTheShapeEnd(t *testing.T) {
	raw := loopRaw()
	raw.Points["L"][0].RoutePoints = []RawPoint{
		{Sequence: 0, Latitude: 35.3, Longitude: 25.1003}, {Sequence: 1, Latitude: 35.3, Longitude: 25.12},
		{Sequence: 2, Latitude: 35.3, Longitude: 25.1001}}
	r := buildModel(raw).Routes["90"]
	if r.StopM[0] > 50 {
		t.Fatalf("first stop at %v m of %v: stopM %v", r.StopM[0], r.Cum[len(r.Cum)-1], r.StopM)
	}
	for i := 1; i < len(r.StopM); i++ {
		if r.StopM[i] <= r.StopM[i-1] {
			t.Fatalf("stopM %v", r.StopM)
		}
	}
	if r.StopM[2] < 1700 || r.StopM[2] > 1850 { // C, the turn, ~1790 m along
		t.Fatalf("C at %v: stopM %v", r.StopM[2], r.StopM)
	}
}

// Out along one street and back along the next (11 m apart). B is 2 m from the way back and 9 m
// from the way out: the nearest point alone puts it on the way back and squeezes C after it.
func TestStopNearerTheWayBackStaysOnTheWayOut(t *testing.T) {
	var stops []RawStop
	for _, s := range []struct {
		c        string
		lat, lon float64
	}{{"A", 35.3, 25.1}, {"B", 35.30008, 25.11}, {"C", 35.3, 25.12}, {"D", 35.3001, 25.105}} {
		stops = append(stops, RawStop{Code: s.c, Name: s.c, Latitude: Num(s.lat), Longitude: Num(s.lon), LineCodes: []string{"L"}, RouteCodes: []string{"91"}})
	}
	var seq []RawSeq
	for i, c := range []string{"A", "B", "C", "D"} {
		seq = append(seq, RawSeq{Sequence: i + 1, Code: c})
	}
	raw := &RawCity{
		Lines:     []RawLine{{Code: "L", Name: "OUT AND BACK", Color: "#000000", TextColor: "#ffffff", Routes: []RawRouteRef{{Code: "91", Name: "X", Direction: 1}}}},
		Stops:     stops,
		Sequences: map[string][]RawSeq{"91": seq},
		Points: map[string][]RawPoints{"L": {{RouteCode: "91", RoutePoints: []RawPoint{
			{Sequence: 0, Latitude: 35.3, Longitude: 25.1}, {Sequence: 1, Latitude: 35.3, Longitude: 25.12},
			{Sequence: 2, Latitude: 35.3001, Longitude: 25.12}, {Sequence: 3, Latitude: 35.3001, Longitude: 25.1}}}}},
	}
	r := buildModel(raw).Routes["91"]
	if r.StopM[1] < 850 || r.StopM[1] > 960 { // B on the way out, ~908 m
		t.Fatalf("B at %v: stopM %v", r.StopM[1], r.StopM)
	}
	if r.StopM[2] < 1750 || r.StopM[2] > 1850 || r.StopM[3] < 3150 || r.StopM[3] > 3230 {
		t.Fatalf("stopM %v", r.StopM)
	}
}

// No order fits (the stops run against the shape): the old nearest-point walk still gives growing metres.
func TestStopsAgainstTheShapeFallBack(t *testing.T) {
	raw := lineRaw()
	raw.Points["1"][0].RoutePoints = []RawPoint{{Sequence: 1, Latitude: 35.3, Longitude: 25.14}, {Sequence: 2, Latitude: 35.3, Longitude: 25.1}}
	r := buildModel(raw).Routes["10"]
	if len(r.StopM) != 5 {
		t.Fatalf("stopM %v", r.StopM)
	}
	for i := 1; i < len(r.StopM); i++ {
		if r.StopM[i] < r.StopM[i-1] {
			t.Fatalf("stopM %v", r.StopM)
		}
	}
}

// Out and back on one street; three stops each ~4 m against the way out (codex review). Clamping a
// stop up to the one before it must not cut the chain: all three stay on one pass, not split.
func TestStopsSlightlyBackStayOnOnePass(t *testing.T) {
	shape := []LL{{35.3, 25.1}, {35.3, 25.12}, {35.3, 25.1}}
	cum := cumulative(shape)
	m := stopMetres(shape, cum, []LL{{35.3, 25.11}, {35.3, 25.10996}, {35.3, 25.10992}})
	if math.Abs(m[2]-m[0]) > 50 || m[1] < m[0] || m[2] < m[1] {
		t.Fatalf("stopM %v", m)
	}
}

// ---- estimated arrivals (rt#136 B): a stop with no board gets the tracked buses due at it ----

func estimated(a []Arrival) []Arrival {
	out := []Arrival{}
	for _, x := range a {
		if x.Estimate {
			out = append(out, x)
		}
	}
	return out
}

// near: within 2 s (metres along the shape are floats).
func near(got, want int64) bool { return got >= want-2 && got <= want+2 }

// A bus due at B in 1 min (the board of B said so): the stops after B get B's time plus the
// timetable's ride from B (2 min per stop), and that bus takes the timetable run of each stop.
func TestEstimatesAfterTheDueStopFollowTheTimetable(t *testing.T) {
	m := buildModel(lineRaw())
	now := athensClock(time.Now().Unix()).Midnight + 36000 - 60 // 09:59, today
	l := newLiveCity(m)
	l.add(Board{Stop: "B", At: now, Rows: []LiveRow{row("10", "35.3", "25.105", 1, "v1")}})
	placed := l.placed(now, "")
	for stop, want := range map[string]int64{"B": 60, "C": 180, "D": 300} {
		a := stopArrivals(m, stop, nil, placed, now)
		if len(a) != 1 || !a[0].Live || !a[0].Estimate || a[0].Vehicle != "v1" || a[0].At-now != want || a[0].Scheduled != nil || a[0].Lat == 0 {
			t.Fatalf("%s: want one estimate in %d s (the timetable run is that bus): %+v", stop, want, a)
		}
	}
	if a := stopArrivals(m, "E", nil, placed, now); len(a) != 0 {
		t.Fatalf("the trip ends at E: %+v", a)
	}
	if a := stopArrivals(m, "A", nil, placed, now); len(estimated(a)) != 0 {
		t.Fatalf("the bus has passed A: %+v", a)
	}
	// 100 s later the bus is past its due time and stands at B: C keeps B's time + 2 min, and B
	// itself (no longer ahead) gets no estimate.
	later := l.placed(now+100, "")
	if a := estimated(stopArrivals(m, "C", nil, later, now+100)); len(a) != 1 || a[0].At != now+180 {
		t.Fatalf("C, 100 s later: %+v", a)
	}
	if a := estimated(stopArrivals(m, "B", nil, later, now+100)); len(a) != 0 {
		t.Fatalf("B, 100 s later: %+v", a)
	}
}

// The stops between the bus and the stop it is due at count back from that time on the timetable.
func TestEstimatesBeforeTheDueStopCountBack(t *testing.T) {
	m := buildModel(lineRaw())
	now := athensClock(time.Now().Unix()).Midnight + 36000 - 60
	l := newLiveCity(m)
	l.add(Board{Stop: "D", At: now, Rows: []LiveRow{row("10", "35.3", "25.105", 6, "v1")}})
	placed := l.placed(now, "")
	for stop, want := range map[string]int64{"B": 120, "C": 240, "D": 360} {
		a := estimated(stopArrivals(m, stop, nil, placed, now))
		if len(a) != 1 || a[0].At-now != want {
			t.Fatalf("%s: want %d s: %+v", stop, want, a)
		}
	}
}

// A board, even an empty or a stale one, wins: no estimates are mixed in.
func TestBoardWinsOverEstimates(t *testing.T) {
	m := buildModel(lineRaw())
	now := athensClock(time.Now().Unix()).Midnight + 36000 - 60
	l := newLiveCity(m)
	l.add(Board{Stop: "B", At: now, Rows: []LiveRow{row("10", "35.3", "25.105", 1, "v1")}})
	placed := l.placed(now, "")
	a := stopArrivals(m, "C", &Board{Stop: "C", At: now - 60}, placed, now)
	if len(a) != 1 || a[0].Live || a[0].Estimate || a[0].Trip != "7" {
		t.Fatalf("empty board: %+v", a)
	}
	a = stopArrivals(m, "C", &Board{Stop: "C", At: now, Rows: []LiveRow{row("10", "35.3", "25.105", 4, "v1")}}, placed, now)
	if len(a) != 1 || !a[0].Live || a[0].Estimate || a[0].At-now != 240 {
		t.Fatalf("board row: %+v", a)
	}
}

// Nothing later than 60 min is estimated.
func TestEstimatesStopAtOneHour(t *testing.T) {
	m := buildModel(lineRaw())
	now := athensClock(time.Now().Unix()).Midnight + 36000 - 60
	l := newLiveCity(m)
	l.add(Board{Stop: "B", At: now, Rows: []LiveRow{row("10", "35.3", "25.105", 59, "v1")}})
	placed := l.placed(now, "")
	if a := estimated(stopArrivals(m, "B", nil, placed, now)); len(a) != 1 || a[0].At-now != 59*60 {
		t.Fatalf("B in 59 min: %+v", a)
	}
	if a := estimated(stopArrivals(m, "C", nil, placed, now)); len(a) != 0 {
		t.Fatalf("C in 61 min: %+v", a)
	}
}

// A loop with no timetable: distance over the bus's speed. The bus is on its way out and due at C in
// 3 min (1362 m, 7.6 m/s): B is 454 m ahead (1 min) and again 908 m after C (2 more min).
func TestEstimatesOnALoopVisitTwiceByDistance(t *testing.T) {
	m := buildModel(loopRaw())
	now := int64(1_000_000)
	l := newLiveCity(m)
	l.add(Board{Stop: "C", At: now, Rows: []LiveRow{{LineCode: "L", RouteCode: "90", Latitude: 35.3, Longitude: 25.105, DepartureMins: 3, VehicleCode: "v"}}})
	placed := l.placed(now, "")
	a := stopArrivals(m, "B", nil, placed, now)
	if len(a) != 2 || !a[0].Estimate || !a[1].Estimate || a[0].Vehicle != "v" || !near(a[0].At-now, 60) || !near(a[1].At-now, 300) {
		t.Fatalf("B twice: %+v", a)
	}
	if a := stopArrivals(m, "C", nil, placed, now); len(a) != 1 || a[0].At-now != 180 {
		t.Fatalf("C: %+v", a)
	}
	if a := stopArrivals(m, "A", nil, placed, now); len(a) != 0 {
		t.Fatalf("A is behind the bus, and the trip ends there: %+v", a)
	}
}

// A bus with no speed (too near its due stop to tell) is taken at 6 m/s.
func TestEstimatesDefaultSpeed(t *testing.T) {
	m := buildModel(loopRaw())
	now := int64(1_000_000)
	l := newLiveCity(m)
	l.add(Board{Stop: "B", At: now, Rows: []LiveRow{{LineCode: "L", RouteCode: "90", Latitude: 35.3, Longitude: 25.1095, DepartureMins: 0, VehicleCode: "v"}}})
	placed := l.placed(now, "")
	if len(placed) != 1 || placed[0].S.Speed != nil {
		t.Fatalf("placed %+v", placed)
	}
	if a := stopArrivals(m, "C", nil, placed, now); len(a) != 1 || !a[0].Estimate || !near(a[0].At-now, 151) {
		t.Fatalf("C, 908 m after B at 6 m/s: %+v", a)
	}
}

// Before the due stop, the trip that fits is the one nearest the bus at the due stop (where its time
// is known), not at the asked stop: rides differ from trip to trip (codex).
func TestEstimatesCountBackOnTheTripNearestAtTheDueStop(t *testing.T) {
	m := buildModel(lineRaw())
	c := athensClock(time.Now().Unix())
	now := c.Midnight + 36000 // 10:00, today
	hm := func(h, m int) int { return h*3600 + m*60 }
	m.Routes["10"].Trips = []Trip{
		{ID: "slow", Day: c.Day, Times: []int{hm(10, 15), hm(10, 17), hm(10, 19), hm(10, 30), NoTime}}, // at C 1 min off the bus's time at D
		{ID: "fits", Day: c.Day, Times: []int{hm(10, 10), hm(10, 12), hm(10, 14), hm(10, 20), NoTime}}, // at D when the bus is
	}
	l := newLiveCity(m)
	l.add(Board{Stop: "D", At: now, Rows: []LiveRow{row("10", "35.3", "25.105", 20, "v1")}})
	a := estimated(stopArrivals(m, "C", nil, l.placed(now, ""), now))
	if len(a) != 1 || a[0].At-now != 20*60-6*60 {
		t.Fatalf("C: want 6 min before D (the 10:20 trip): %+v", a)
	}
}

// With no timetable too, a stop before the due stop is never sooner than the bus can drive there
// (15 m/s): C in 1 min is 1771 m ahead, B is 863 m ahead (codex).
func TestEstimatesByDistanceNeverFasterThanTheCap(t *testing.T) {
	m := buildModel(loopRaw())
	now := int64(1_000_000)
	l := newLiveCity(m)
	l.add(Board{Stop: "C", At: now, Rows: []LiveRow{{LineCode: "L", RouteCode: "90", Latitude: 35.3, Longitude: 25.1005, DepartureMins: 1, VehicleCode: "v"}}})
	a := stopArrivals(m, "B", nil, l.placed(now, ""), now)
	if len(a) == 0 || !a[0].Estimate || !near(a[0].At-now, 58) {
		t.Fatalf("B: want 863 m at 15 m/s, not its share of the minute (29 s): %+v", a)
	}
}

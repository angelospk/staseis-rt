package citybus

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/history"
)

// Runs seen (rt#141) and the sighting history (rt#142). Measured on the public API (2026-11-10):
// 238 of 250 vehicle codes name a run, "YYYYMMDD_<route>_0100000_HH_MM".

func TestRunCode(t *testing.T) {
	for _, c := range []struct {
		code, date string
		dep        int
		ok         bool
	}{
		{"20261110_4_0100000_17_20", "2026-11-10", 17*3600 + 20*60, true},    // Corfu
		{"20261110_0121_0_0100000_07_05", "2026-11-10", 7*3600 + 5*60, true}, // Katerini: route 0121-0
		{"20261110_31018_0100000_00_00", "2026-11-10", 0, true},              // midnight
		{"1607413212998552622", "", 0, false},                                // a device id
		{"20261310_4_0100000_17_20", "", 0, false},                           // month 13
		{"20261110_4_0100000_25_61", "", 0, false},                           // minute 61
		{"20261110_4_0100000_31_00", "", 0, false},                           // hour 31
		{"20261110_17_20", "", 0, false},                                     // no route part
		{"x20261110_4_0100000_17_20", "", 0, false},
		{"", "", 0, false},
	} {
		date, dep, ok := runCode(c.code)
		if ok != c.ok || (ok && (date != c.date || dep != c.dep)) {
			t.Errorf("runCode(%q) = %q %d %v, want %q %d %v", c.code, date, dep, ok, c.date, c.dep, c.ok)
		}
	}
}

// dayBase is the base serviceDays gives for that date (Athens noon minus 12 h, as GTFS counts), also
// on the days the clocks change.
func TestDayBaseIsTheServiceDayBase(t *testing.T) {
	for _, c := range []struct {
		date    string
		weekday int
	}{{"2026-11-10", 2}, {"2026-10-25", 0}, {"2027-03-28", 0}} {
		base, wd, ok := dayBase(c.date)
		if !ok || wd != c.weekday {
			t.Fatalf("dayBase(%s) = %d %d %v", c.date, base, wd, ok)
		}
		day, _ := time.ParseInLocation("2006-01-02 15:04", c.date+" 17:25", athensLoc)
		want := serviceDays(athensClock(day.Unix()))[1]
		if want.date != c.date || base != want.base {
			t.Fatalf("dayBase(%s) = %d, serviceDays gives %d (%s)", c.date, base, want.base, want.date)
		}
	}
	if _, _, ok := dayBase("2026-13-40"); ok {
		t.Fatal("a date that is none")
	}
}

const (
	t1720 = 17*3600 + 20*60
	t1840 = 18*3600 + 40*60
)

// runRaw: lineRaw's route (A..E, ~908 m apart) with two runs on weekday day: 17:20 (id 7) and
// 18:40 (id 8), 2 min a stop, no time at E.
func runRaw(day int) *RawCity {
	raw := lineRaw()
	var trips []RawTripRow
	for _, tr := range []struct {
		id    int64
		h, mi int
	}{{7, 17, 20}, {8, 18, 40}} {
		for i, c := range []string{"A", "B", "C", "D"} {
			trips = append(trips, RawTripRow{ID: tr.id, Day: day, RouteCode: "10", StopCode: c, TripTime: fmt.Sprintf("%02d:%02d", tr.h, tr.mi+2*i)})
		}
	}
	raw.Trips = map[string][]RawTripRow{"10": trips}
	return raw
}

func codeOf(date, route string, dep int) string {
	return fmt.Sprintf("%s_%s_0100000_%02d_%02d", strings.ReplaceAll(date, "-", ""), route, dep/3600, dep/60%60)
}

// seenAt: the sighting of one bus of route 10 at lon, on the board of stop D read at unix at.
func seenAt(t *testing.T, m *Model, code, lon string, at int64) Sighting {
	t.Helper()
	s := sightings(m, Board{Stop: "D", At: at, Rows: []LiveRow{row("10", "35.3", lon, 2, code)}})
	if len(s) != 1 {
		t.Fatalf("sightings %v", s)
	}
	return s[0]
}

func TestTripOf(t *testing.T) {
	raw := runRaw(2)
	// id 9 leaves at 17:20 too; id 5 has no time at its first stop.
	raw.Trips["10"] = append(raw.Trips["10"],
		RawTripRow{ID: 9, Day: 2, RouteCode: "10", StopCode: "A", TripTime: "17:20"},
		RawTripRow{ID: 5, Day: 2, RouteCode: "10", StopCode: "B", TripTime: "12:02"},
		RawTripRow{ID: 5, Day: 2, RouteCode: "10", StopCode: "C", TripTime: "12:04"},
		RawTripRow{ID: 6, Day: 3, RouteCode: "10", StopCode: "A", TripTime: "09:00"})
	r := buildModel(raw).Routes["10"]
	if tr := tripOf(r, 2, t1720); tr == nil || tr.ID != "7" {
		t.Fatalf("17:20 of two trips: the first in the model, got %+v", tr)
	}
	if tr := tripOf(r, 2, 12*3600+2*60); tr == nil || tr.ID != "5" {
		t.Fatalf("a trip counts from its first timed stop, got %+v", tr)
	}
	if tripOf(r, 2, 9*3600) != nil || tripOf(r, 3, 9*3600) == nil {
		t.Fatal("the weekday must match")
	}
	if tripOf(r, 2, t1720+60) != nil {
		t.Fatal("17:21 is no trip")
	}
}

func TestNoteCountsABusThatLeftOnItsRun(t *testing.T) {
	m := buildModel(runRaw(2))
	base, _, _ := dayBase("2026-11-10")
	code := codeOf("2026-11-10", "10", t1720)
	l := loadRuns(filepath.Join(t.TempDir(), "runs.json"), base)
	if l.seen("2026-11-10", "10", t1720) != nil {
		t.Fatal("an empty log")
	}
	at := base + t1720 + 300
	if !l.note(m, seenAt(t, m, code, "25.115", at)) {
		t.Fatal("a bus 1.3 km on, 5 min after the departure, is on its run")
	}
	r := l.seen("2026-11-10", "10", t1720)
	if r == nil || r.First != at || r.Last != at || r.Veh != code {
		t.Fatalf("seen %+v", r)
	}
	if !l.note(m, seenAt(t, m, code, "25.125", at+120)) {
		t.Fatal("the next sighting")
	}
	if r := l.seen("2026-11-10", "10", t1720); r.First != at || r.Last != at+120 {
		t.Fatalf("First stays, Last moves: %+v", r)
	}
	if l.seen("2026-11-10", "10", t1840) != nil || l.seen("2026-11-09", "10", t1720) != nil || l.seen("2026-11-10", "11", t1720) != nil {
		t.Fatal("only that run")
	}
}

func TestNoteLeavesOutWhatIsNoRunYet(t *testing.T) {
	m := buildModel(runRaw(2))
	base, _, _ := dayBase("2026-11-10")
	code := codeOf("2026-11-10", "10", t1720)
	fresh := func() *runLog { return loadRuns(filepath.Join(t.TempDir(), "runs.json"), base) }
	for _, c := range []struct {
		name, code, lon string
		at              int64
		want            bool
	}{
		{"the code of a later run, mid-route (Chania 036 18:40 at 17:52)", codeOf("2026-11-10", "10", t1840), "25.115", base + t1720 + 300, false},
		{"2 min before the departure, already moving", code, "25.115", base + t1720 - 120, true},
		{"3 min before the departure", code, "25.115", base + t1720 - 180, false},
		{"at the first stop after the departure (45 m on)", code, "25.1005", base + t1720 + 60, false},
		{"180 m past the first stop", code, "25.102", base + t1720 + 60, true},
		{"45 min after the end of the run (17:26)", code, "25.125", base + t1720 + 360 + 2700, true},
		{"46 min after the end of the run", code, "25.125", base + t1720 + 360 + 2760, false},
		{"a device id with no trip in time (17:23 there, 18:00 now)", "1607413212998552622", "25.115", base + 18*3600, false},
		{"a code time that is no trip (Heraklion 16:33)", codeOf("2026-11-10", "10", 16*3600+33*60), "25.115", base + 16*3600 + 40*60, false},
		{"the same time on another weekday", codeOf("2026-11-11", "10", t1720), "25.115", base + 86400 + t1720 + 300, false},
	} {
		l := fresh()
		if got := l.note(m, seenAt(t, m, c.code, c.lon, c.at)); got != c.want {
			t.Errorf("%s: note = %v, want %v", c.name, got, c.want)
		}
		if len(l.Runs) != 0 != c.want {
			t.Errorf("%s: log %v", c.name, l.Runs)
		}
	}
}

// On a loop the first stop is also the last: a bus that stands at the terminus can project at the
// route's end. It has not left, wherever its metre is.
func TestNoteLoopTerminusIsNotADeparture(t *testing.T) {
	raw := loopRaw()
	for i, c := range []string{"A", "B", "C", "B", "A"} {
		raw.Trips = map[string][]RawTripRow{"90": append(raw.Trips["90"], RawTripRow{ID: 3, Day: 2, RouteCode: "90", StopCode: c, TripTime: fmt.Sprintf("17:%02d", 20+5*i)})}
	}
	m := buildModel(raw)
	r := m.Routes["90"]
	base, _, _ := dayBase("2026-11-10")
	code := codeOf("2026-11-10", "90", t1720)
	l := loadRuns(filepath.Join(t.TempDir(), "runs.json"), base)
	end := r.Cum[len(r.Cum)-1]
	if l.note(m, Sighting{ID: code, Route: "90", Lat: 35.3, Lon: 25.1002, Seen: base + t1720 + 300, M: end - 20, ToStop: "A", ToM: end}) {
		t.Fatal("a bus 20 m from the terminus has not left, though its metre is the route's end")
	}
	if !l.note(m, Sighting{ID: code, Route: "90", Lat: 35.3, Lon: 25.105, Seen: base + t1720 + 300, M: 450, ToStop: "B", ToM: r.StopM[1]}) {
		t.Fatal("450 m on, it has left")
	}
}

// A 17:20 run counts at 17:25 local also on the days the clocks change.
func TestNoteOnClockChangeDays(t *testing.T) {
	for _, date := range []string{"2026-10-25", "2027-03-28"} {
		m := buildModel(runRaw(0)) // both are Sundays
		at, _ := time.ParseInLocation("2006-01-02 15:04", date+" 17:25", athensLoc)
		l := loadRuns(filepath.Join(t.TempDir(), "runs.json"), at.Unix())
		if !l.note(m, seenAt(t, m, codeOf(date, "10", t1720), "25.115", at.Unix())) {
			t.Errorf("%s: 17:25 local is 5 min into the 17:20 run", date)
		}
		late, _ := time.ParseInLocation("2006-01-02 15:04", date+" 18:13", athensLoc)
		if l2 := loadRuns(filepath.Join(t.TempDir(), "runs.json"), at.Unix()); l2.note(m, seenAt(t, m, codeOf(date, "10", t1720), "25.125", late.Unix())) {
			t.Errorf("%s: 18:13 local is 47 min after the end", date)
		}
	}
}

func TestRunLogSurvivesRestartAndDropsOldDays(t *testing.T) {
	m := buildModel(runRaw(2))
	base, _, _ := dayBase("2026-11-10")
	file := filepath.Join(t.TempDir(), "runs.json")
	l := loadRuns(file, base)
	if err := l.save(base); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("nothing noted: no file is written")
	}
	code := codeOf("2026-11-10", "10", t1720)
	at := base + t1720 + 300
	l.note(m, seenAt(t, m, code, "25.115", at))
	if err := l.save(at); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file + ".tmp"); err == nil {
		t.Fatal("the .tmp file is renamed away")
	}
	again := loadRuns(file, at)
	if r := again.seen("2026-11-10", "10", t1720); r == nil || r.First != at || r.Veh != code {
		t.Fatalf("after a restart: %+v", r)
	}
	// The format on disk, and the days kept: yesterday and on.
	old := `{"runs":{"2026-11-08|10|17:20":{"first":1,"last":2,"veh":"a"},"2026-11-09|10|17:20":{"first":3,"last":4,"veh":"b"},"2026-11-10|10|18:40":{"first":5,"last":6,"veh":"c"}}}`
	if err := os.WriteFile(file, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	kept := loadRuns(file, at)
	if kept.seen("2026-11-08", "10", t1720) != nil {
		t.Fatal("the day before yesterday is dropped on load")
	}
	if r := kept.seen("2026-11-09", "10", t1720); r == nil || r.Last != 4 || r.Veh != "b" {
		t.Fatalf("yesterday is kept: %+v", r)
	}
	if kept.seen("2026-11-10", "10", t1840) == nil {
		t.Fatal("today is kept")
	}
	if err := os.WriteFile(file, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if bad := loadRuns(file, at); bad == nil || len(bad.Runs) != 0 {
		t.Fatal("an unreadable file starts an empty log")
	}
}

// ---- the city: board -> log -> API ----

// runCity: a city on runRaw of today's weekday, its clock at today 17:25. clock moves it.
func runCity(t *testing.T, dir string, hist *history.Writer) (c *City, s *Service, today string, base int64, clock *int64) {
	t.Helper()
	real := athensClock(time.Now().Unix())
	today = real.Date
	base, _, ok := dayBase(today)
	if !ok {
		t.Fatal("dayBase of today")
	}
	now := base + t1720 + 300
	clock = &now
	c = testCity(t, &fake{answers: answersOf(runRaw(real.Day))}, dir)
	c.now = func() time.Time { return time.Unix(*clock, 0) }
	c.hist = hist
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	return c, serviceWith(t, c), today, base, clock
}

// boardWith: stop D's board answers with these buses (code, lon), then the city reads it.
func boardWith(t *testing.T, c *City, buses ...[2]string) {
	t.Helper()
	var rows []LiveRow
	for _, b := range buses {
		rows = append(rows, row("10", "35.3", b[1], 2, b[0]))
	}
	f := c.up.(*fake)
	f.mu.Lock()
	f.answers["/el/1/stops/live/D"] = liveAnswer{Vehicles: rows}
	f.mu.Unlock()
	if _, err := c.board(context.Background(), "D", priUser); err != nil {
		t.Fatal(err)
	}
}

func departures(t *testing.T, s *Service, path string) []map[string]any {
	t.Helper()
	code, sc := getBody(t, s, path)
	if code != 200 {
		t.Fatalf("%s: %d", path, code)
	}
	var out []map[string]any
	for _, d := range sc["directions"].([]any) {
		for _, x := range d.(map[string]any)["departures"].([]any) {
			out = append(out, x.(map[string]any))
		}
	}
	return out
}

func TestScheduleSaysSeenAndLiveNeverFalse(t *testing.T) {
	c, s, today, _, clock := runCity(t, t.TempDir(), nil)
	deps := departures(t, s, "/t/v1/lines/1/schedule")
	if len(deps) != 2 || deps[0]["t"] != "17:20" || deps[1]["t"] != "18:40" {
		t.Fatalf("departures %v", deps)
	}
	for _, d := range deps {
		if _, has := d["seen"]; has {
			t.Fatalf("no bus seen yet: no seen key at all, got %v", d)
		}
		if _, has := d["live"]; has {
			t.Fatalf("no live key, got %v", d)
		}
	}
	boardWith(t, c, [2]string{codeOf(today, "10", t1720), "25.115"})
	deps = departures(t, s, "/t/v1/lines/1/schedule")
	if deps[0]["seen"] != true || deps[0]["live"] != true {
		t.Fatalf("the 17:20 run has a bus on it: %v", deps[0])
	}
	if _, has := deps[1]["seen"]; has {
		t.Fatalf("18:40 is not known: %v", deps[1])
	}
	*clock += MaxAge + 1
	deps = departures(t, s, "/t/v1/lines/1/schedule")
	if deps[0]["seen"] != true {
		t.Fatalf("seen stays: %v", deps[0])
	}
	if _, has := deps[0]["live"]; has {
		t.Fatalf("no bus for over MaxAge: not live, got %v", deps[0])
	}
}

func maxAge(t *testing.T, s *Service, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var n int
	if _, err := fmt.Sscanf(rec.Header().Get("Cache-Control"), "public, max-age=%d", &n); err != nil {
		t.Fatalf("%s: Cache-Control %q", path, rec.Header().Get("Cache-Control"))
	}
	return n
}

// The schedule is cached 60 s, but never past the moment a live run stops being live, or past
// midnight (a new day's schedule).
func TestScheduleCacheNeverOutlivesLiveOrTheDay(t *testing.T) {
	c, s, today, _, clock := runCity(t, t.TempDir(), nil)
	if got := maxAge(t, s, "/t/v1/lines/1/schedule"); got != 60 {
		t.Fatalf("no live run: 60 s, got %d", got)
	}
	boardWith(t, c, [2]string{codeOf(today, "10", t1720), "25.115"})
	if got := maxAge(t, s, "/t/v1/lines/1/schedule"); got != 60 {
		t.Fatalf("a run seen just now is live for 120 s more: 60 s, got %d", got)
	}
	*clock += 100
	if got := maxAge(t, s, "/t/v1/lines/1/schedule"); got != 20 {
		t.Fatalf("live for 20 s more: 20 s, got %d", got)
	}
	*clock += 30
	if got := maxAge(t, s, "/t/v1/lines/1/schedule"); got != 60 {
		t.Fatalf("no longer live: 60 s, got %d", got)
	}
	night, _ := time.ParseInLocation("2006-01-02 15:04:05", today+" 23:59:35", athensLoc)
	*clock = night.Unix()
	if got := maxAge(t, s, "/t/v1/lines/1/schedule"); got != 25 {
		t.Fatalf("25 s to midnight: 25 s, got %d", got)
	}
}

// Two trips of one route leave at 17:20: one bus marks one of them.
func TestScheduleDuplicateTripsOneBusOneSeen(t *testing.T) {
	c, s, today, _, _ := runCity(t, t.TempDir(), nil)
	raw := runRaw(athensClock(c.unix()).Day)
	for i, st := range []string{"A", "B", "C", "D"} {
		raw.Trips["10"] = append(raw.Trips["10"], RawTripRow{ID: 9, Day: athensClock(c.unix()).Day, RouteCode: "10", StopCode: st, TripTime: fmt.Sprintf("17:%02d", 20+2*i)})
	}
	raw.BuiltAt = c.unix()
	c.install(raw)
	boardWith(t, c, [2]string{codeOf(today, "10", t1720), "25.115"})
	seen := 0
	for _, d := range departures(t, s, "/t/v1/lines/1/schedule") {
		if d["seen"] == true {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("one bus, one run seen; got %d", seen)
	}
}

// The key of a run has no trip id: a daily reload that renumbers the trips keeps the day's marks.
func TestReloadWithNewTripIDsKeepsTheMarks(t *testing.T) {
	c, s, today, _, _ := runCity(t, t.TempDir(), nil)
	boardWith(t, c, [2]string{codeOf(today, "10", t1720), "25.115"})
	raw := runRaw(athensClock(c.unix()).Day)
	for i := range raw.Trips["10"] {
		raw.Trips["10"][i].ID += 100
	}
	raw.BuiltAt = c.unix()
	c.install(raw)
	deps := departures(t, s, "/t/v1/lines/1/schedule")
	if deps[0]["trip"] != "107" || deps[0]["seen"] != true {
		t.Fatalf("after the reload: %v", deps[0])
	}
}

func TestVehicleCarriesItsRun(t *testing.T) {
	c, s, today, _, _ := runCity(t, t.TempDir(), nil)
	run, later, extra, dev := codeOf(today, "10", t1720), codeOf(today, "10", t1840), codeOf(today, "10", 16*3600+33*60), "1607413212998552622"
	boardWith(t, c, [2]string{run, "25.115"}, [2]string{later, "25.105"}, [2]string{extra, "25.125"}, [2]string{dev, "25.128"})
	_, v1 := getBody(t, s, "/t/v1/lines/1")
	_, v2 := getBody(t, s, "/t/v2/lines/t:1/vehicles")
	by1, by2 := map[string]map[string]any{}, map[string]map[string]any{}
	for _, v := range v1["vehicles"].([]any) {
		by1[v.(map[string]any)["id"].(string)] = v.(map[string]any)
	}
	for _, v := range v2["vehicles"].([]any) {
		by2[v.(map[string]any)["id"].(string)] = v.(map[string]any)
	}
	if len(by1) != 4 || len(by2) != 4 {
		t.Fatalf("4 buses: %v %v", by1, by2)
	}
	// The contract's label: "HH:MM FIRST STOP → LAST STOP" of the trip (its timed stops).
	if v := by1[run]; v["trip_id"] != "7" || v["trip_label"] != "17:20 SA → SD" {
		t.Fatalf("v1 run: %v %v", v["trip_id"], v["trip_label"])
	}
	if v := by2["t:"+run]; v["run"] != "t:run:7:"+today {
		t.Fatalf("v2 run: %v", v["run"])
	}
	// A bus that carries the code of a later run: that is its run (the operator's word), not yet running.
	if v := by1[later]; v["trip_id"] != "8" || v["trip_label"] != "18:40 SA → SD" {
		t.Fatalf("v1 later: %v %v", v["trip_id"], v["trip_label"])
	}
	if v := by2["t:"+later]; v["run"] != "t:run:8:"+today {
		t.Fatalf("v2 later: %v", v["run"])
	}
	// A code time with no trip in the timetable: no trip id, the label from the code and the route's ends.
	if v := by1[extra]; v["trip_id"] != nil || v["trip_label"] != "16:33 SA → SE" {
		t.Fatalf("v1 extra: %v %v", v["trip_id"], v["trip_label"])
	}
	if v := by2["t:"+extra]; v["run"] != nil {
		t.Fatalf("v2 extra: %v", v["run"])
	}
	// A device id where the 17:20 is due: the coded bus is on the 17:20, so no run (#155).
	if v := by1[dev]; v["trip_id"] != nil {
		t.Fatalf("a device id on a held run: %v", v["trip_id"])
	}
	// Alone, a device id gets its run from the timetable: the 17:20 is due where it is now (#149).
	c, s, today, _, _ = runCity(t, t.TempDir(), nil)
	boardWith(t, c, [2]string{dev, "25.128"})
	_, v1 = getBody(t, s, "/t/v1/lines/1")
	_, v2 = getBody(t, s, "/t/v2/lines/t:1/vehicles")
	if v := v1["vehicles"].([]any)[0].(map[string]any); v["trip_id"] != "7" || v["trip_label"] != "17:20 SA → SD" {
		t.Fatalf("a device id in time: %v %v", v["trip_id"], v["trip_label"])
	}
	if v := v2["vehicles"].([]any)[0].(map[string]any); v["run"] != "t:run:7:"+today {
		t.Fatalf("v2 device: %v", v["run"])
	}
}

func TestRunIsRunningOnlyWhenCountedAndFresh(t *testing.T) {
	c, s, today, _, clock := runCity(t, t.TempDir(), nil)
	runOf := func(id string) map[string]any {
		code, body := getBody(t, s, "/t/v2/runs/t:run:"+id+":"+today)
		if code != 200 {
			t.Fatalf("run %s: %d", id, code)
		}
		return body["run"].(map[string]any)
	}
	if r := runOf("7"); r["status"] != "scheduled" || r["vehicle"] != nil {
		t.Fatalf("no bus yet: %v %v", r["status"], r["vehicle"])
	}
	run, later := codeOf(today, "10", t1720), codeOf(today, "10", t1840)
	boardWith(t, c, [2]string{run, "25.115"}, [2]string{later, "25.105"})
	if r := runOf("7"); r["status"] != "running" || r["vehicle"] != "t:"+run {
		t.Fatalf("a bus counted on it, just now: %v %v", r["status"], r["vehicle"])
	}
	if r := runOf("8"); r["status"] != "scheduled" || r["vehicle"] != nil {
		t.Fatalf("a bus only carries the 18:40 code, 75 min ahead: %v %v", r["status"], r["vehicle"])
	}
	*clock += MaxAge + 1
	if r := runOf("7"); r["status"] != "scheduled" || r["vehicle"] != nil {
		t.Fatalf("no sighting for over MaxAge: %v %v", r["status"], r["vehicle"])
	}
}

func TestRunsFileWrittenAndReadByANewCity(t *testing.T) {
	dir := t.TempDir()
	c, _, today, _, clock := runCity(t, dir, nil)
	boardWith(t, c, [2]string{codeOf(today, "10", t1720), "25.115"})
	*clock += 61 // the log is saved with the health file, at most once a minute
	boardWith(t, c, [2]string{codeOf(today, "10", t1720), "25.118"})
	if _, err := os.Stat(filepath.Join(c.dir, "runs.json")); err != nil {
		t.Fatal("runs.json must be written within a minute of a run seen")
	}
	c2 := testCity(t, &fake{answers: answersOf(runRaw(athensClock(*clock).Day))}, dir)
	c2.now = c.now
	if err := c2.loadStatic(context.Background(), false, priWatched); err != nil {
		t.Fatal(err)
	}
	deps := departures(t, serviceWith(t, c2), "/t/v1/lines/1/schedule")
	if deps[0]["seen"] != true {
		t.Fatalf("a restart keeps the day's runs: %v", deps[0])
	}
}

// ---- history (rt#142) ----

func historyRows(t *testing.T, dir string) [][]string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.csv"))
	var out [][]string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		recs, err := csv.NewReader(strings.NewReader(string(b))).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) == 0 || strings.Join(recs[0], ",")+"\n" != history.Header {
			t.Fatalf("%s: header %v", f, recs)
		}
		out = append(out, recs[1:]...)
	}
	return out
}

func TestHistoryOneRowPerMoveOrMinute(t *testing.T) {
	hdir := filepath.Join(t.TempDir(), "history")
	start := time.Now().Unix()
	// The writer's clock only names the hour file; it stays still, so the writer goroutine shares nothing with the test.
	w, err := history.Open(hdir, 1<<20, time.Hour, func() time.Time { return time.Unix(start, 0) }, quiet)
	if err != nil {
		t.Fatal(err)
	}
	c, _, today, _, clock := runCity(t, t.TempDir(), w)
	run, dev := codeOf(today, "10", t1720), "1607413212998552622"
	t0 := *clock
	boardWith(t, c, [2]string{run, "25.115"}, [2]string{dev, "25.128"}) // row, row
	*clock += 30
	boardWith(t, c, [2]string{run, "25.115"}, [2]string{dev, "25.128"}) // same place, 30 s: nothing
	*clock += 30
	boardWith(t, c, [2]string{run, "25.115"}) // same place, 60 s: a row
	*clock += 10
	boardWith(t, c, [2]string{run, "25.116"}) // moved: a row
	w.Close()
	rows := historyRows(t, hdir)
	var got []string
	for _, r := range rows {
		got = append(got, strings.Join(r, ","))
	}
	ver := c.model.Version
	m116 := history.SM(seenAt(t, c.model, run, "25.116", 1).M)
	m115 := history.SM(seenAt(t, c.model, run, "25.115", 1).M)
	m128 := history.SM(seenAt(t, c.model, dev, "25.128", 1).M)
	// fix_t,line,route_code,veh,lat,lon,gtfs,trip_id,shape_id,s_m,delay_s,run_start,start_delay_s
	want := map[string]bool{
		fmt.Sprintf("%d,t:1,10,t:%s,35300000,25115000,%s,7,,%d,,,", t0, run, ver, m115):    true,
		fmt.Sprintf("%d,t:1,10,t:%s,35300000,25128000,%s,,,%d,,,", t0, dev, ver, m128):     true, // the 17:20 is the coded bus's (#155)
		fmt.Sprintf("%d,t:1,10,t:%s,35300000,25115000,%s,7,,%d,,,", t0+60, run, ver, m115): true,
		fmt.Sprintf("%d,t:1,10,t:%s,35300000,25116000,%s,7,,%d,,,", t0+70, run, ver, m116): true,
	}
	if len(got) != len(want) {
		t.Fatalf("rows:\n%s", strings.Join(got, "\n"))
	}
	for _, g := range got {
		if !want[g] {
			t.Fatalf("row %q not expected; rows:\n%s", g, strings.Join(got, "\n"))
		}
	}
}

// Two boards read in one second show a bus at two places: the writer keeps one row per vehicle per
// second, so the second place is written with the next sighting, not held back for a minute.
func TestHistorySecondPlaceInOneSecondIsWrittenNext(t *testing.T) {
	hdir := filepath.Join(t.TempDir(), "history")
	start := time.Now().Unix()
	w, err := history.Open(hdir, 1<<20, time.Hour, func() time.Time { return time.Unix(start, 0) }, quiet)
	if err != nil {
		t.Fatal(err)
	}
	c, _, today, _, clock := runCity(t, t.TempDir(), w)
	run := codeOf(today, "10", t1720)
	t0 := *clock
	boardWith(t, c, [2]string{run, "25.115"}) // a row
	boardWith(t, c, [2]string{run, "25.116"}) // moved, same second: no row
	*clock++
	boardWith(t, c, [2]string{run, "25.116"}) // the new place, a second later: a row
	w.Close()
	var got []string
	for _, r := range historyRows(t, hdir) {
		got = append(got, r[0]+" "+r[5])
	}
	sort.Strings(got)
	want := []string{fmt.Sprintf("%d 25115000", t0), fmt.Sprintf("%d 25116000", t0+1)}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rows (fix_t lon) = %v, want %v", got, want)
	}
}

// Vehicle codes repeat across cities (route 04 at 17:30 in Kavala and Xanthi): each city's row is kept.
func TestHistoryTwoCitiesSameCodeSameSecond(t *testing.T) {
	hdir := filepath.Join(t.TempDir(), "history")
	start := time.Now().Unix()
	// The writer's clock only names the hour file; it stays still, so the writer goroutine shares nothing with the test.
	w, err := history.Open(hdir, 1<<20, time.Hour, func() time.Time { return time.Unix(start, 0) }, quiet)
	if err != nil {
		t.Fatal(err)
	}
	a, _, today, _, _ := runCity(t, t.TempDir(), w)
	b, _, _, _, _ := runCity(t, t.TempDir(), w)
	b.Conf.ID = "u"
	b.now = a.now
	code := codeOf(today, "10", t1720)
	boardWith(t, a, [2]string{code, "25.115"})
	boardWith(t, b, [2]string{code, "25.115"})
	boardWith(t, a, [2]string{code, "25.116"}) // the same city, the same second: one row a second
	w.Close()
	rows := historyRows(t, hdir)
	if len(rows) != 2 || rows[0][3] == rows[1][3] {
		t.Fatalf("one row per city: %v", rows)
	}
	for _, r := range rows {
		if r[3] != "t:"+code && r[3] != "u:"+code {
			t.Fatalf("veh carries the city: %v", r)
		}
	}
}

func TestHistoryOffWritesNothing(t *testing.T) {
	dir := t.TempDir()
	c, _, today, _, _ := runCity(t, dir, nil)
	boardWith(t, c, [2]string{codeOf(today, "10", t1720), "25.115"})
	if _, err := os.Stat(filepath.Join(dir, "history")); err == nil {
		t.Fatal("no --history: no history directory")
	}
}

func TestConfigTurnsHistoryOn(t *testing.T) {
	dir := t.TempDir()
	off, err := New(Config{StateDir: dir, Cities: []string{"cfu", "kva"}}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if off.hist != nil || off.cities["cfu"].hist != nil {
		t.Fatal("history is off by default")
	}
	on, err := New(Config{StateDir: dir, Cities: []string{"cfu", "kva"}, HistoryMaxBytes: 1 << 20, HistoryKeep: time.Hour}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer on.close()
	if on.hist == nil || on.cities["cfu"].hist != on.hist || on.cities["kva"].hist != on.hist {
		t.Fatal("one writer for every city")
	}
	if st, err := os.Stat(filepath.Join(dir, "history")); err != nil || !st.IsDir() {
		t.Fatal("the writer's directory is <state>/history")
	}
}

// A clean stop (a deploy) keeps what the last minute saw: the run log is saved and the history's
// queue is written out.
func TestServiceStopSavesRunsAndHistory(t *testing.T) {
	dir := t.TempDir()
	hdir := filepath.Join(dir, "history")
	start := time.Now().Unix()
	// The writer's clock only names the hour file; it stays still, so the writer goroutine shares nothing with the test.
	w, err := history.Open(hdir, 1<<20, time.Hour, func() time.Time { return time.Unix(start, 0) }, quiet)
	if err != nil {
		t.Fatal(err)
	}
	c, s, today, _, _ := runCity(t, dir, w)
	s.hist = w
	// The health file was just saved: the log would wait up to a minute.
	c.mu.Lock()
	c.healthSaved = c.unix()
	c.mu.Unlock()
	boardWith(t, c, [2]string{codeOf(today, "10", t1720), "25.115"})
	if _, err := os.Stat(filepath.Join(c.dir, "runs.json")); err == nil {
		t.Fatal("precondition: the log is not on disk yet")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, "127.0.0.1:0") }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
	again := loadRuns(filepath.Join(c.dir, "runs.json"), c.unix())
	if again.seen(today, "10", t1720) == nil {
		t.Fatal("the run seen in the last minute is saved on a stop")
	}
	if rows := historyRows(t, hdir); len(rows) != 1 {
		t.Fatalf("the queued history row is written on a stop: %v", rows)
	}
}

// A bus going against its route is not on its run: it does not move its Last on (#147, review).
func TestNoteSkipsABusAgainstItsRoute(t *testing.T) {
	m := buildModel(runRaw(2))
	base, _, _ := dayBase("2026-11-10")
	code := codeOf("2026-11-10", "10", t1720)
	l := loadRuns(filepath.Join(t.TempDir(), "runs.json"), base)
	at := base + t1720 + 300
	l.note(m, seenAt(t, m, code, "25.125", at))
	s := seenAt(t, m, code, "25.115", at+60)
	s.Against = true
	if l.note(m, s) {
		t.Fatal("a sighting against the route counts on no run")
	}
	if r := l.seen("2026-11-10", "10", t1720); r == nil || r.Last != at {
		t.Fatalf("Last stays: %+v", r)
	}
}

// ---- a run from the timetable, for a bus whose code is a device id (#149) ----

const devID = "1607650707510153454"

// devAt: the sighting of a device-id bus of route 10 at lon, on the board of D at unix at, as a
// city with no other bus reads it (its run from the timetable set).
func devAt(t *testing.T, m *Model, lon string, at int64) Sighting {
	t.Helper()
	return liveAt(t, newLiveCity(m), at, [2]string{devID, lon})[devID]
}

// liveAt: the city reads stop D's board at unix at with these buses (code, lon) of route 10; the
// sightings it found, by bus.
func liveAt(t *testing.T, l *liveCity, at int64, buses ...[2]string) map[string]Sighting {
	t.Helper()
	var rows []LiveRow
	for _, b := range buses {
		rows = append(rows, row("10", "35.3", b[1], 2, b[0]))
	}
	out := map[string]Sighting{}
	for _, s := range l.add(Board{Stop: "D", At: at, Rows: rows}) {
		out[s.ID] = s
	}
	if len(out) != len(buses) {
		t.Fatalf("sightings %v", out)
	}
	return out
}

// The one trip whose time where the bus is lies 3 min early to 10 min late is its run.
func TestTimeRunNamesTheOneTripInTime(t *testing.T) {
	m := buildModel(runRaw(2))
	r := m.Routes["10"]
	base, _, _ := dayBase("2026-11-10")
	s := devAt(t, m, "25.115", base+t1720+300) // ~1361 m, due there 17:23; seen 17:25
	date, dep, ok := runOf(r, s)
	if !ok || date != "2026-11-10" || dep != t1720 {
		t.Fatalf("run %s %d %v, want the 17:20", date, dep, ok)
	}
	l := loadRuns(filepath.Join(t.TempDir(), "runs.json"), base)
	if !l.note(m, s) || l.seen("2026-11-10", "10", t1720) == nil {
		t.Fatal("a device-id bus on its run by time counts on it")
	}
}

// No run by time: near the start, too early or late, two trips in time, against its route.
func TestTimeRunOnlyWhenSure(t *testing.T) {
	raw := runRaw(2)
	base, _, _ := dayBase("2026-11-10")
	for _, c := range []struct {
		name, lon string
		at        int64
		extra     bool
		against   bool
	}{
		{"within 300 m of the start", "25.102", base + t1720 + 60, false, false},
		{"17 min late", "25.115", base + t1720 + 20*60, false, false},
		{"5 min early", "25.115", base + t1720 - 120, false, false},
		{"a second trip 5 min later is in time too", "25.115", base + t1720 + 300, true, false},
		{"against its route", "25.115", base + t1720 + 300, false, true},
	} {
		rw := runRaw(2)
		if c.extra {
			for i, st := range []string{"A", "B", "C", "D"} {
				rw.Trips["10"] = append(rw.Trips["10"], RawTripRow{ID: 9, Day: 2, RouteCode: "10", StopCode: st, TripTime: fmt.Sprintf("17:%02d", 25+2*i)})
			}
		} else {
			rw = raw
		}
		m := buildModel(rw)
		s := devAt(t, m, c.lon, c.at)
		s.Against = c.against
		if _, _, ok := timeRun(m.Routes["10"], s, nil); ok {
			t.Fatalf("%s: no run", c.name)
		}
	}
}

// A code that names a run wins over the time.
func TestTimeRunNotForACodedBus(t *testing.T) {
	m := buildModel(runRaw(2))
	base, _, _ := dayBase("2026-11-10")
	s := seenAt(t, m, codeOf("2026-11-10", "10", t1840), "25.115", base+t1720+300)
	if _, dep, ok := runOf(m.Routes["10"], s); !ok || dep != t1840 {
		t.Fatalf("the code's 18:40, not the time's 17:20: %d %v", dep, ok)
	}
}

// schedAt: the departure before the first timed stop, linear between them, the last time after.
func TestSchedAt(t *testing.T) {
	r := buildModel(runRaw(2)).Routes["10"]
	tr := tripOf(r, 2, t1720)
	for _, c := range []struct {
		m    float64
		want int
	}{{-50, t1720}, {0, t1720}, {r.StopM[1] / 2, t1720 + 60}, {r.StopM[3], t1720 + 360}, {r.StopM[4], t1720 + 360}} {
		if at, dep, ok := schedAt(r, tr, c.m); !ok || at != c.want || dep != t1720 {
			t.Errorf("m %.0f: %d %d %v, want %d", c.m, at, dep, ok, c.want)
		}
	}
	if _, _, ok := schedAt(r, &Trip{Times: []int{NoTime, NoTime}}, 10); ok {
		t.Error("a trip with no time")
	}
}

// A run is a date and a departure. When another trip leaves at the same time first, the run
// would name that one, so no run (review of #149).
func TestTimeRunNotWhenItsDepartureNamesAnotherTrip(t *testing.T) {
	raw := runRaw(2)
	// id 5 leaves at 17:20 too, first in the model, but is 20 min slower: not in time at 1361 m.
	raw.Trips["10"] = append([]RawTripRow{
		{ID: 5, Day: 2, RouteCode: "10", StopCode: "A", TripTime: "17:20"},
		{ID: 5, Day: 2, RouteCode: "10", StopCode: "B", TripTime: "17:40"}}, raw.Trips["10"]...)
	m := buildModel(raw)
	base, _, _ := dayBase("2026-11-10")
	if tr := tripOf(m.Routes["10"], 2, t1720); tr == nil || tr.ID != "5" {
		t.Fatalf("setup: 17:20 names trip 5, got %+v", tr)
	}
	if _, _, ok := runOf(m.Routes["10"], devAt(t, m, "25.115", base+t1720+300)); ok {
		t.Fatal("trip 7 is in time, but its run names trip 5")
	}
}

// A run past midnight belongs to yesterday's service day.
func TestTimeRunPastMidnight(t *testing.T) {
	raw := lineRaw()
	for i, c := range []string{"A", "B", "C", "D"} {
		raw.Trips["10"] = append(raw.Trips["10"], RawTripRow{ID: 3, Day: 2, RouteCode: "10", StopCode: c, TripTime: fmt.Sprintf("24:%02d", 10+2*i)})
	}
	m := buildModel(raw)
	base, _, _ := dayBase("2026-11-10")
	date, dep, ok := runOf(m.Routes["10"], devAt(t, m, "25.115", base+24*3600+15*60))
	if !ok || date != "2026-11-10" || dep != 24*3600+10*60 {
		t.Fatalf("run %s %d %v, want the 24:10 of 2026-11-10", date, dep, ok)
	}
}

// A run another bus of the route holds is not a candidate (#155): of two trips in time, the one
// no bus is on names the device-id bus's run; when the only trip in time is held, no run.
func TestTimeRunSkipsARunAnotherBusHolds(t *testing.T) {
	raw := runRaw(2)
	for i, st := range []string{"A", "B", "C", "D"} { // 17:25, in time at 1361 m too
		raw.Trips["10"] = append(raw.Trips["10"], RawTripRow{ID: 9, Day: 2, RouteCode: "10", StopCode: st, TripTime: fmt.Sprintf("17:%02d", 25+2*i)})
	}
	m := buildModel(raw)
	base, _, _ := dayBase("2026-11-10")
	at, t1725 := base+t1720+300, t1720+300
	if _, _, ok := runOf(m.Routes["10"], devAt(t, m, "25.115", at)); ok {
		t.Fatal("setup: two trips in time, no run")
	}
	got := liveAt(t, newLiveCity(m), at, [2]string{codeOf("2026-11-10", "10", t1725), "25.112"}, [2]string{devID, "25.115"})
	if date, dep, ok := runOf(m.Routes["10"], got[devID]); !ok || date != "2026-11-10" || dep != t1720 {
		t.Fatalf("the 17:25 is held: run %s %d %v, want the 17:20", date, dep, ok)
	}
	m = buildModel(runRaw(2))
	got = liveAt(t, newLiveCity(m), at, [2]string{codeOf("2026-11-10", "10", t1720), "25.112"}, [2]string{devID, "25.115"})
	if _, _, ok := runOf(m.Routes["10"], got[devID]); ok {
		t.Fatal("the only trip in time is held by the coded bus: no run")
	}
	// A bus of another day's run, or one gone (older than MaxAge), holds nothing.
	l := newLiveCity(m)
	liveAt(t, l, at-MaxAge-10, [2]string{codeOf("2026-11-10", "10", t1720), "25.112"})
	if _, _, ok := runOf(m.Routes["10"], liveAt(t, l, at, [2]string{devID, "25.115"})[devID]); !ok {
		t.Fatal("a bus not seen for over MaxAge holds no run")
	}
}

// Once a device-id bus has a run, it keeps it while it goes on along its route (#155): a late
// bus keeps its run. A step back, a gap over MaxAge or a sighting against its route ends it.
func TestDeviceBusKeepsItsRunWhileItGoesOn(t *testing.T) {
	m := buildModel(runRaw(2))
	r := m.Routes["10"]
	base, _, _ := dayBase("2026-11-10")
	at := base + t1720 + 300
	for _, c := range []struct {
		name string
		lon  string
		gap  int64
		keep bool
	}{
		{"on along the route, 15 min late", "25.118", 60, true},
		{"standing", "25.115", 60, true},
		{"back on the route", "25.110", 60, false},
		{"not seen for over MaxAge", "25.118", MaxAge + 10, false},
	} {
		l := newLiveCity(m)
		if _, dep, ok := runOf(r, liveAt(t, l, at, [2]string{devID, "25.115"})[devID]); !ok || dep != t1720 {
			t.Fatalf("%s: setup: the 17:20 by time", c.name)
		}
		// Later sightings fall out of the time window: 17:23 is due at 1361 m, the bus is there
		// 15 min later.
		later := at + 15*60
		for x := at + 60; x <= later-c.gap; x += 60 { // seen every minute, standing
			liveAt(t, l, x, [2]string{devID, "25.115"})
		}
		s := liveAt(t, l, later, [2]string{devID, c.lon})[devID]
		if _, _, ok := timeRun(r, s, nil); ok {
			t.Fatalf("%s: setup: no run by time 15 min late", c.name)
		}
		_, dep, ok := runOf(r, s)
		if c.keep && (!ok || dep != t1720) {
			t.Fatalf("%s: keeps the 17:20, got %d %v", c.name, dep, ok)
		}
		if !c.keep && ok {
			t.Fatalf("%s: no run, got %d", c.name, dep)
		}
	}
}

// Review of #155: the buses of one board hold their runs whatever their order; a bus never keeps
// a run another bus holds; a held run whose departure names another trip does not end the match.
func TestHeldRunsWhateverTheOrder(t *testing.T) {
	m := buildModel(runRaw(2))
	r := m.Routes["10"]
	base, _, _ := dayBase("2026-11-10")
	at, coded := base+t1720+300, codeOf("2026-11-10", "10", t1720)
	got := liveAt(t, newLiveCity(m), at, [2]string{devID, "25.115"}, [2]string{coded, "25.112"})
	if _, _, ok := runOf(r, got[devID]); ok {
		t.Fatal("device bus first on the board: the coded bus later on it holds the 17:20")
	}
	l := newLiveCity(m)
	if _, dep, ok := runOf(r, liveAt(t, l, at, [2]string{devID, "25.115"})[devID]); !ok || dep != t1720 {
		t.Fatal("setup: alone, the 17:20")
	}
	got = liveAt(t, l, at+60, [2]string{coded, "25.112"}, [2]string{devID, "25.116"})
	if _, _, ok := runOf(r, got[devID]); ok {
		t.Fatal("the coded bus now holds the 17:20: the device bus does not keep it")
	}

	raw := runRaw(2)
	raw.Trips["10"] = append([]RawTripRow{ // id 5 leaves at 17:20 too, first in the model, 20 min slower
		{ID: 5, Day: 2, RouteCode: "10", StopCode: "A", TripTime: "17:20"},
		{ID: 5, Day: 2, RouteCode: "10", StopCode: "B", TripTime: "17:40"}}, raw.Trips["10"]...)
	for i, st := range []string{"A", "B", "C", "D"} { // 17:25, in time at 1361 m too
		raw.Trips["10"] = append(raw.Trips["10"], RawTripRow{ID: 9, Day: 2, RouteCode: "10", StopCode: st, TripTime: fmt.Sprintf("17:%02d", 25+2*i)})
	}
	m = buildModel(raw)
	got = liveAt(t, newLiveCity(m), at, [2]string{coded, "25.112"}, [2]string{devID, "25.115"})
	if _, dep, ok := runOf(m.Routes["10"], got[devID]); !ok || dep != t1720+300 {
		t.Fatalf("the 17:20 is held: the 17:25, got %d %v", dep, ok)
	}
}

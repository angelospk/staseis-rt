package usual

import (
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/history"
)

func date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// at is the instant of local wall clock hh:mm on a date.
func at(d time.Time, hh, mm int) time.Time { return wallClock(d, hh*3600+mm*60) }

func TestServiceDateAndDayType(t *testing.T) {
	if got := ServiceDate(at(date(2026, 11, 4), 1, 30)); !got.Equal(date(2026, 11, 3)) {
		t.Errorf("01:30 belongs to the day before, got %v", got)
	}
	if got := ServiceDate(at(date(2026, 11, 4), 4, 0)); !got.Equal(date(2026, 11, 4)) {
		t.Errorf("04:00 starts the day, got %v", got)
	}
	for _, c := range []struct {
		d    time.Time
		want string
	}{
		{date(2026, 11, 4), Weekday},
		{date(2026, 11, 7), Saturday},
		{date(2026, 11, 8), Sunday},
		{date(2026, 10, 28), Sunday}, // OXI day, a Wednesday
		{date(2026, 4, 12), Sunday},  // Orthodox Easter 2026
		{date(2026, 4, 13), Sunday},  // Easter Monday
		{date(2026, 2, 23), Sunday},  // Clean Monday
		{date(2026, 6, 1), Sunday},   // Whit Monday
		{date(2026, 4, 10), Weekday}, // Good Friday: left as it is
		{date(2027, 5, 3), Sunday},   // Easter Monday 2027 (Easter 2027-05-02)
	} {
		if got := DayType(c.d); got != c.want {
			t.Errorf("DayType(%s) = %s, want %s", c.d.Format(time.DateOnly), got, c.want)
		}
	}
}

// ---- BuildDay: pass detection from history archives ----

// fakeRoutes: trip T1 = line 040 to «Πειραιάς», shape S1 with stops A at 3000 m and B at 5000 m.
type fakeRoutes struct{}

func (fakeRoutes) Trip(id string) (string, string, bool) {
	if id == "T1" || id == "T2" {
		return "040", "Πειραιάς", true
	}
	return "", "", false
}

func (fakeRoutes) Shape(id string) *ShapeInfo {
	if id != "S1" {
		return nil
	}
	return &ShapeInfo{Stops: []ShapeStop{{"A", 3000}, {"B", 5000}}, MinGap: 1800}
}

type fix struct {
	t    time.Time
	veh  string
	trip string
	s    int
	feed string
}

const testFeed = "2026-07-08"

// writeFixes writes gzipped hourly archives, one per UTC hour of the fixes.
func writeFixes(t *testing.T, dir string, fixes []fix) {
	t.Helper()
	byHour := map[string]*strings.Builder{}
	var order []string
	for _, x := range fixes {
		h := x.t.UTC().Format("2006-01-02T15")
		b := byHour[h]
		if b == nil {
			b = &strings.Builder{}
			b.WriteString(history.Header)
			byHour[h] = b
			order = append(order, h)
		}
		feed := x.feed
		if feed == "" {
			feed = testFeed
		}
		shape := "S1"
		if x.trip == "" {
			shape = ""
		}
		fmt.Fprintf(b, "%d,040,RC,%s,37980000,23720000,%s,%s,%s,%d,0,,\n", x.t.Unix(), x.veh, feed, x.trip, shape, x.s)
	}
	for _, h := range order {
		f, err := os.Create(filepath.Join(dir, h+".csv.gz"))
		if err != nil {
			t.Fatal(err)
		}
		zw := gzip.NewWriter(f)
		zw.Write([]byte(byHour[h].String()))
		zw.Close()
		f.Close()
	}
}

// keepAlive is one fix per minute of another bus over [from, to), so no minute looks like an
// outage.
func keepAlive(from, to time.Time) []fix {
	var out []fix
	for t := from; t.Before(to); t = t.Add(time.Minute) {
		out = append(out, fix{t: t, veh: "ALIVE", s: -1})
	}
	return out
}

func passesOf(d *Day) []string {
	var out []string
	sort.Slice(d.Passes, func(i, j int) bool { return d.Passes[i].T < d.Passes[j].T })
	for _, p := range d.Passes {
		k := d.Keys[p.Key]
		out = append(out, fmt.Sprintf("%s %s %s %s", d.Stops[p.Stop], k.Line, k.Headsign,
			time.Unix(p.T, 0).In(gtfs.Athens).Format("15:04:05")))
	}
	return out
}

func TestBuildDayPasses(t *testing.T) {
	day := date(2026, 11, 4)
	base := at(day, 8, 0)
	sec := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	fixes := keepAlive(at(day, 4, 0), at(day, 28, 0))
	fixes = append(fixes,
		// v1: from the start of the shape across A (3000 m at 08:09:55) and B (5000 m at 08:19:10).
		fix{sec(0), "v1", "T1", 100, ""},
		fix{sec(590), "v1", "T1", 2900, ""},
		fix{sec(610), "v1", "T1", 3300, ""}, // A: 2900 -> 3300 in 20 s: 590 + 5 = 595 s
		fix{sec(700), "v1", "T1", 2950, ""}, // GPS jitter back 350 m: a new run that starts near A
		fix{sec(1100), "v1", "T1", 4900, ""},
		fix{sec(1200), "v1", "T1", 5100, ""}, // B, but the run started 2050 m before it: counted
		// v2: starts 500 m before A (a matcher re-run mid route): no pass at A.
		fix{sec(60), "v2", "T1", 2500, ""},
		fix{sec(120), "v2", "T1", 3100, ""},
		// v3: crosses A between fixes 11 min apart: no pass.
		fix{sec(0), "v3", "T2", 2000, ""},
		fix{sec(660), "v3", "T2", 3500, ""},
		// v4: crosses A on another feed: ignored.
		fix{sec(0), "v4", "T1", 100, "2026-01-01"},
		fix{sec(30), "v4", "T1", 3500, "2026-01-01"},
	)
	dir := t.TempDir()
	writeFixes(t, dir, fixes)
	d, err := BuildDay(dir, day, testFeed, fakeRoutes{})
	if err != nil || d == nil {
		t.Fatalf("BuildDay: %v %v", d, err)
	}
	got := strings.Join(passesOf(d), "; ")
	want := "A 040 Πειραιάς 08:09:55; B 040 Πειραιάς 08:19:10"
	if got != want {
		t.Errorf("passes:\n got %s\nwant %s", got, want)
	}
	if len(d.Outages) != 0 {
		t.Errorf("outages %v, want none", d.Outages)
	}
}

func TestBuildDaySameBusTwiceNeedsMinGap(t *testing.T) {
	day := date(2026, 11, 4)
	base := at(day, 8, 0)
	sec := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	fixes := keepAlive(at(day, 4, 0), at(day, 28, 0))
	// The same bus crosses A three times: at 0, after 20 min (< MinGap 30 min: jitter) and after
	// 40 min (a real second round).
	for _, off := range []int{0, 1200, 2400} {
		fixes = append(fixes, fix{sec(off), "v1", "T1", 0, ""}, fix{sec(off + 30), "v1", "T1", 2990, ""},
			fix{sec(off + 40), "v1", "T1", 3010, ""})
	}
	dir := t.TempDir()
	writeFixes(t, dir, fixes)
	d, _ := BuildDay(dir, day, testFeed, fakeRoutes{})
	got := strings.Join(passesOf(d), "; ")
	if want := "A 040 Πειραιάς 08:00:35; A 040 Πειραιάς 08:40:35"; got != want {
		t.Errorf("passes:\n got %s\nwant %s", got, want)
	}
}

func TestBuildDayOutagesAndBounds(t *testing.T) {
	day := date(2026, 11, 4)
	var fixes []fix
	// Fixes all day except 10:00-10:07 (an outage) and 12:00-12:03 (too short to count).
	for _, f := range keepAlive(at(day, 4, 0), at(day, 28, 0)) {
		if !(f.t.Before(at(day, 10, 7)) && !f.t.Before(at(day, 10, 0))) &&
			!(f.t.Before(at(day, 12, 3)) && !f.t.Before(at(day, 12, 0))) {
			fixes = append(fixes, f)
		}
	}
	// A pass at 03:50 belongs to this day; one at 04:10 of the next date does not.
	fixes = append(fixes,
		fix{at(day, 27, 40), "v1", "T1", 100, ""}, fix{at(day, 27, 50), "v1", "T1", 3100, ""},
		fix{at(day, 28, 0), "v2", "T1", 100, ""}, fix{at(day, 28, 10), "v2", "T1", 3100, ""})
	dir := t.TempDir()
	writeFixes(t, dir, fixes)
	d, _ := BuildDay(dir, day, testFeed, fakeRoutes{})
	if len(d.Outages) != 1 || d.Outages[0] != [2]int64{at(day, 10, 0).Unix(), at(day, 10, 7).Unix()} {
		t.Errorf("outages %v, want [10:00, 10:07)", d.Outages)
	}
	if got := strings.Join(passesOf(d), "; "); !strings.HasPrefix(got, "A 040 Πειραιάς 03:49") || strings.Contains(got, "04:") {
		t.Errorf("passes %s, want only the one at 03:49", got)
	}
}

func TestBuildDayNoRowsOfTheFeed(t *testing.T) {
	day := date(2026, 11, 4)
	dir := t.TempDir()
	writeFixes(t, dir, []fix{{at(day, 8, 0), "v1", "T1", 100, "other"}})
	if d, err := BuildDay(dir, day, testFeed, fakeRoutes{}); err != nil || d != nil {
		t.Errorf("got %v %v, want nil day (nothing to learn from)", d, err)
	}
}

// ---- Store: day files and the estimate ----

// series adds passes of line 040 «Πειραιάς» at stop A on a date, at local hh:mm:ss offsets.
func series(d *Day, date time.Time, hhmm ...string) {
	for _, s := range hhmm {
		var h, m, sec int
		fmt.Sscanf(s, "%d:%d:%d", &h, &m, &sec)
		d.Add("A", "040", "Πειραιάς", wallClock(date, h*3600+m*60+sec).Unix())
	}
}

func store(t *testing.T, days ...*Day) *Store {
	t.Helper()
	dir := t.TempDir()
	for _, d := range days {
		if err := WriteDay(dir, d); err != nil {
			t.Fatal(err)
		}
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// Wednesday 2026-11-04 08:00; the four Wednesdays/weekdays before it are training days.
var query = at(date(2026, 11, 4), 8, 0)

func weekdays() []time.Time {
	return []time.Time{date(2026, 10, 27), date(2026, 10, 29), date(2026, 10, 30), date(2026, 11, 2)}
}

func only(t *testing.T, rows []Row) Row {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("rows %+v, want 1", rows)
	}
	return rows[0]
}

func TestQueryNextWhenDaysAgree(t *testing.T) {
	var days []*Day
	for i, d := range weekdays() {
		day := NewDay(d)
		series(day, d, fmt.Sprintf("08:1%d:00", i%2), "08:40:00", "09:20:00") // 08:10 or 08:11
		days = append(days, day)
	}
	r := only(t, store(t, days...).Query("A", query))
	if r.Line != "040" || r.Headsign != "Πειραιάς" || r.DayType != Weekday || r.Days != 4 {
		t.Errorf("row %+v", r)
	}
	if r.NextAt == nil || *r.NextAt != at(date(2026, 11, 4), 8, 10).Add(30*time.Second).Unix() {
		t.Errorf("next_at %v, want 08:10:30 (median)", r.NextAt)
	}
	if r.SpreadS == nil || *r.SpreadS > 60 {
		t.Errorf("spread %v, want ≤ 60 s", r.SpreadS)
	}
}

func TestQueryNoNextWhenDaysDisagree(t *testing.T) {
	var days []*Day
	for i, d := range weekdays() {
		day := NewDay(d)
		series(day, d, fmt.Sprintf("08:%02d:00", 5+i*10)) // 08:05, 08:15, 08:25, 08:35
		days = append(days, day)
	}
	r := store(t, days...).Query("A", query)
	if len(r) == 1 && r[0].NextAt != nil {
		t.Errorf("next_at %v with a spread of ~15 min", *r[0].NextAt)
	}
}

func TestQueryNeedsThreeDays(t *testing.T) {
	var days []*Day
	for _, d := range weekdays()[:2] {
		day := NewDay(d)
		series(day, d, "08:10:00")
		days = append(days, day)
	}
	for _, r := range store(t, days...).Query("A", query) {
		if r.NextAt != nil {
			t.Errorf("next_at from 2 days: %+v", r)
		}
	}
}

func TestQueryNeedsMostDaysWithAPass(t *testing.T) {
	// 3 days agree on 08:10, 4 other days have no pass within 3 h: no time.
	var days []*Day
	for i, d := range append(weekdays(), date(2026, 10, 20), date(2026, 10, 21), date(2026, 10, 22)) {
		day := NewDay(d)
		if i < 3 {
			series(day, d, "08:10:00")
		} else {
			series(day, d, "05:00:00") // the day has data, but nothing after 08:00
		}
		days = append(days, day)
	}
	for _, r := range store(t, days...).Query("A", query) {
		if r.NextAt != nil {
			t.Errorf("next_at when 4 of 7 days had no bus: %+v", r)
		}
	}
}

func TestQueryOutageDropsTheDay(t *testing.T) {
	var days []*Day
	for i, d := range weekdays() {
		day := NewDay(d)
		if i == 0 {
			// No fixes 08:00-08:30: its "next pass" 08:45 is not real; the day must not count.
			series(day, d, "08:45:00")
			day.Outages = [][2]int64{{at(d, 8, 0).Unix(), at(d, 8, 30).Unix()}}
		} else {
			series(day, d, "08:10:00")
		}
		days = append(days, day)
	}
	r := only(t, store(t, days...).Query("A", query))
	if r.Days != 3 || r.NextAt == nil || *r.NextAt != at(date(2026, 11, 4), 8, 10).Unix() {
		t.Errorf("row %+v next %v, want 3 days and 08:10", r, r.NextAt)
	}
}

func TestQueryUsesOnlyTheSameDayTypeAndOnlyThePast(t *testing.T) {
	var days []*Day
	for _, d := range []time.Time{date(2026, 10, 31), date(2026, 10, 24), date(2026, 10, 17), date(2026, 10, 10)} {
		day := NewDay(d) // Saturdays
		series(day, d, "08:10:00")
		days = append(days, day)
	}
	future := NewDay(date(2026, 11, 5))
	series(future, date(2026, 11, 5), "08:10:00")
	old := NewDay(date(2026, 10, 1)) // a weekday older than 28 days
	series(old, date(2026, 10, 1), "08:10:00")
	days = append(days, future, old)
	if rows := store(t, days...).Query("A", query); len(rows) != 0 {
		t.Errorf("rows %+v, want none for a weekday", rows)
	}
}

func TestQueryHeadway(t *testing.T) {
	var days []*Day
	for _, d := range weekdays() {
		day := NewDay(d)
		// Every 10 min in hour 8, every 20 min in hour 9.
		series(day, d, "08:00:00", "08:10:00", "08:20:00", "08:30:00", "08:40:00", "08:50:00",
			"09:00:00", "09:20:00", "09:40:00", "10:00:00")
		days = append(days, day)
	}
	r := only(t, store(t, days...).Query("A", query.Add(5*time.Minute)))
	if r.HeadwayMin == nil || *r.HeadwayMin != 10 || r.NextHourHeadwayMin == nil || *r.NextHourHeadwayMin != 20 {
		t.Errorf("headway %v next hour %v, want 10 and 20", r.HeadwayMin, r.NextHourHeadwayMin)
	}
}

func TestQueryHeadwayNeedsSixGaps(t *testing.T) {
	var days []*Day
	for _, d := range weekdays()[:2] {
		day := NewDay(d)
		series(day, d, "08:00:00", "08:30:00", "09:00:00") // 2 gaps in hour 8 per day: 4 in all
		days = append(days, day)
	}
	for _, r := range store(t, days...).Query("A", query) {
		if r.HeadwayMin != nil {
			t.Errorf("headway from 4 gaps: %d", *r.HeadwayMin)
		}
	}
}

func TestQueryAcrossDST(t *testing.T) {
	// 2026-10-25 is the DST change (a Sunday). Sundays before and after it: 08:10 local stays
	// 08:10 local.
	var days []*Day
	for _, d := range []time.Time{date(2026, 10, 18), date(2026, 10, 25), date(2026, 11, 1)} {
		day := NewDay(d)
		series(day, d, "08:10:00")
		days = append(days, day)
	}
	r := only(t, store(t, days...).Query("A", at(date(2026, 11, 8), 8, 0)))
	if r.NextAt == nil || *r.NextAt != at(date(2026, 11, 8), 8, 10).Unix() {
		t.Errorf("next_at %v, want 08:10 local", r.NextAt)
	}
}

func TestQueryAfterMidnightUsesTheEvening(t *testing.T) {
	// 00:30 on Thursday belongs to Wednesday's service day: the training days' passes at 00:40.
	var days []*Day
	for _, d := range weekdays() {
		day := NewDay(d)
		series(day, d, "24:40:00")
		days = append(days, day)
	}
	r := only(t, store(t, days...).Query("A", at(date(2026, 11, 5), 0, 30)))
	if r.NextAt == nil || *r.NextAt != at(date(2026, 11, 5), 0, 40).Unix() {
		t.Errorf("next_at %v, want 00:40", r.NextAt)
	}
}

func TestStoreReloadAndPrune(t *testing.T) {
	dir := t.TempDir()
	d := NewDay(date(2026, 11, 2))
	series(d, date(2026, 11, 2), "08:10:00")
	d.Add("B", "608", "Γαλάτσι", at(date(2026, 11, 2), 9, 0).Unix())
	if err := WriteDay(dir, d); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Has(date(2026, 11, 2)) || s.Has(date(2026, 11, 3)) {
		t.Errorf("Has wrong")
	}
	d2 := NewDay(date(2026, 11, 3))
	series(d2, date(2026, 11, 3), "08:10:00")
	if err := WriteDay(dir, d2); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(date(2026, 11, 3)); err != nil || !s.Has(date(2026, 11, 3)) {
		t.Fatalf("Load: %v", err)
	}
	s.Prune(date(2026, 11, 3))
	if s.Has(date(2026, 11, 2)) {
		t.Errorf("day before the cut kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "20261102.pass")); !os.IsNotExist(err) {
		t.Errorf("pruned file still on disk: %v", err)
	}
	if rows := s.Query("nowhere", query); len(rows) != 0 {
		t.Errorf("unknown stop rows %+v", rows)
	}
}

func TestStoreSkipsBadFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "20261102.pass"), []byte("garbage"), 0o644)
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Has(date(2026, 11, 2)) {
		t.Errorf("a bad file loaded")
	}
}

// A crossing between 03:58 and 04:01 belongs to the day: the fix after 04:00 is read too.
func TestBuildDayCrossingAtTheEnd(t *testing.T) {
	day := date(2026, 11, 4)
	fixes := append(keepAlive(at(day, 4, 0), at(day, 28, 0)),
		fix{at(day, 27, 50), "v1", "T1", 100, ""}, fix{at(day, 27, 58), "v1", "T1", 2900, ""},
		fix{at(day, 28, 1), "v1", "T1", 3200, ""})
	dir := t.TempDir()
	writeFixes(t, dir, fixes)
	d, _ := BuildDay(dir, day, testFeed, fakeRoutes{})
	if got := strings.Join(passesOf(d), "; "); got != "A 040 Πειραιάς 03:59:00" {
		t.Errorf("passes %q, want the one at 03:59:00", got)
	}
}

// At 03:50 the next pass of the training days is at 04:10, in their next day's file.
func TestQueryAcrossTheDayBoundary(t *testing.T) {
	var days []*Day
	for _, d := range weekdays() {
		day := NewDay(d)
		series(day, d, "27:20:00") // 03:20, then nothing on this service day
		days = append(days, day)
		next := NewDay(d.AddDate(0, 0, 1))
		series(next, d.AddDate(0, 0, 1), "04:10:00", "04:20:00")
		days = append(days, next)
	}
	r := only(t, store(t, days...).Query("A", at(date(2026, 11, 5), 3, 50)))
	if r.NextAt == nil || *r.NextAt != at(date(2026, 11, 5), 4, 10).Unix() {
		t.Errorf("next_at %v, want 04:10", r.NextAt)
	}
}

// A training day whose next day has no file cannot say there was no bus after 04:00.
func TestQueryUnknownAfterTheDay(t *testing.T) {
	var days []*Day
	for i, d := range []time.Time{date(2026, 10, 27), date(2026, 10, 29), date(2026, 10, 30),
		date(2026, 11, 2), date(2026, 10, 23), date(2026, 10, 21)} {
		day := NewDay(d)
		if i < 3 {
			series(day, d, "27:20:00", "27:40:00")
		} else {
			series(day, d, "27:20:00") // its next day is not on disk: unknown, not "no bus"
		}
		days = append(days, day)
	}
	// 03:30: three days have 03:40; three have nothing before 04:00 and no file after.
	r := only(t, store(t, days...).Query("A", at(date(2026, 11, 5), 3, 30)))
	if r.NextAt == nil || r.Days != 3 {
		t.Errorf("row %+v, want 3 days and a time", r)
	}
}

// A damaged stop block makes that day unreadable for the stop, not a day without buses.
func TestQueryDamagedBlockIsNotADayWithoutBuses(t *testing.T) {
	dir := t.TempDir()
	for i, d := range append(weekdays(), date(2026, 10, 20), date(2026, 10, 21)) {
		day := NewDay(d)
		series(day, d, "08:10:00")
		if err := WriteDay(dir, day); err != nil {
			t.Fatal(err)
		}
		if i >= 3 { // 3 damaged days: the last byte of A's block (its only block) can never end
			p := filepath.Join(dir, d.Format("20060102")+".pass")
			b, _ := os.ReadFile(p)
			b[len(b)-1] = 0xff
			os.WriteFile(p, b, 0o644)
		}
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := only(t, s.Query("A", query))
	if r.NextAt == nil || r.Days != 3 {
		t.Errorf("row %+v, want a time from the 3 readable days", r)
	}
}

// The history holds both file formats for 30 days after rt#73: hours before the deploy have
// the 11-column header, later ones 13 columns, and the deploy hour has both (hour.csv.gz
// sealed with the old header, hour.1.csv.gz with the new). Each file is read by its own
// header; a crossing whose two fixes sit in different formats still counts.
func TestBuildDayReadsOldAndNewHeaders(t *testing.T) {
	day := date(2026, 11, 4)
	dir := t.TempDir()
	const oldHeader = "fix_t,line,route_code,veh,lat,lon,gtfs,trip_id,shape_id,s_m,delay_s\n"
	write := func(name, header string, rows []fix, extra string) {
		var b strings.Builder
		b.WriteString(header)
		for _, x := range rows {
			shape := "S1"
			if x.trip == "" {
				shape = ""
			}
			fmt.Fprintf(&b, "%d,040,RC,%s,37980000,23720000,%s,%s,%s,%d,0%s\n", x.t.Unix(), x.veh, testFeed, x.trip, shape, x.s, extra)
		}
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		zw := gzip.NewWriter(f)
		zw.Write([]byte(b.String()))
		zw.Close()
		f.Close()
	}
	// Keep-alive rows in every hour, old format before 08:00 local, new after.
	all := keepAlive(at(day, 4, 0), at(day, 28, 0))
	byFile := map[string][]fix{}
	for _, x := range all {
		name := x.t.UTC().Format("2006-01-02T15") + ".csv.gz"
		byFile[name] = append(byFile[name], x)
	}
	// v1 crosses A (3000 m) between 07:59:50 (old file) and 08:00:10 (new file); v2 crosses A
	// and B (5000 m) wholly in an old file, v3 wholly in a new one.
	oldName := at(day, 7, 0).UTC().Format("2006-01-02T15") + ".csv.gz"
	newSame := at(day, 7, 0).UTC().Format("2006-01-02T15") + ".1.csv.gz"
	byFile[oldName] = append(byFile[oldName],
		fix{at(day, 7, 50), "v1", "T1", 100, ""}, fix{at(day, 7, 59).Add(50 * time.Second), "v1", "T1", 2900, ""},
		fix{at(day, 7, 0), "v2", "T1", 100, ""}, fix{at(day, 7, 9), "v2", "T1", 4900, ""}, fix{at(day, 7, 10), "v2", "T1", 5100, ""})
	byFile[newSame] = append(byFile[newSame], fix{at(day, 8, 0).Add(10 * time.Second), "v1", "T1", 3100, ""})
	newName := at(day, 9, 0).UTC().Format("2006-01-02T15") + ".csv.gz"
	byFile[newName] = append(byFile[newName],
		fix{at(day, 9, 0), "v3", "T1", 100, ""}, fix{at(day, 9, 9), "v3", "T1", 4900, ""}, fix{at(day, 9, 10), "v3", "T1", 5100, ""})
	for name, rows := range byFile {
		sort.Slice(rows, func(i, j int) bool { return rows[i].t.Before(rows[j].t) })
		if strings.HasSuffix(name, ".1.csv.gz") || !rows[0].t.Before(at(day, 8, 0)) {
			write(name, history.Header, rows, ",,")
		} else {
			write(name, oldHeader, rows, "")
		}
	}
	d, err := BuildDay(dir, day, testFeed, fakeRoutes{})
	if err != nil || d == nil {
		t.Fatalf("BuildDay: %v %v", d, err)
	}
	got := strings.Join(passesOf(d), "; ")
	want := "A 040 Πειραιάς 07:05:26; B 040 Πειραιάς 07:09:30; A 040 Πειραιάς 08:00:00; " +
		"A 040 Πειραιάς 09:05:26; B 040 Πειραιάς 09:09:30"
	if got != want {
		t.Errorf("passes:\n got %s\nwant %s", got, want)
	}
	if len(d.Outages) != 0 {
		t.Errorf("outages %v: a file format was not read", d.Outages)
	}
}

// TestTrainingDays: the training days of a query are the loaded days of its day type within
// TrainDays before its service date.
func TestTrainingDays(t *testing.T) {
	var days []*Day
	for _, d := range append(weekdays(), date(2026, 10, 31), date(2026, 11, 5), date(2026, 10, 1)) {
		day := NewDay(d) // 4 weekdays, a Saturday, a later weekday, a weekday too old
		series(day, d, "08:10:00")
		days = append(days, day)
	}
	if n := store(t, days...).TrainingDays(query); n != 4 {
		t.Errorf("TrainingDays = %d, want 4", n)
	}
	var nilStore *Store
	if n := nilStore.TrainingDays(query); n != 0 {
		t.Errorf("nil store: %d", n)
	}
}

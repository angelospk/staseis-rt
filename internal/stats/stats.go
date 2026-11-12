// Package stats sums the fix history (internal/history) into one small file per local day:
// vehicles and travel speed per hour and per line (docs/superpowers/specs/2026-10-07-daily-stats.md).
// Day files are kept forever; speeds are stored as sums (metres, seconds), so days add up exactly.
package stats

import (
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Europe/Athens without the system zoneinfo (distroless image)
)

const (
	Version  = 1
	TZ       = "Europe/Athens"   // days are local days of this zone
	MinGap   = 5 * time.Second   // pairs closer than this are GPS noise
	MaxGap   = 120 * time.Second // pairs further apart may hide a detour or a layover
	MaxSpeed = 25.0              // m/s; faster pairs are GPS jumps
	// Late is how long after its fix time a row may still arrive.
	Late = time.Hour
	// Settle is how long after its end a day is summed, so the last files it reads
	// (up to TripsAfter after its end) are gzipped.
	Settle = 4 * time.Hour

	dateLayout = "2006-01-02"
	hourLayout = "2006-01-02T15" // history file names, UTC
)

// FirstDay is the first day listed and served: the seen/missed trip counts of earlier days are
// not reliable (no daily OASA schedule, seen-flag bugs; Harold, 2026-10-29). Keep it equal to
// FIRST_DAY in .github/workflows/stats.yml.
const FirstDay = "2026-10-30"

type Hour struct {
	H        int   `json:"h"` // local hour; the repeated hour of a DST change is merged
	Fixes    int   `json:"fixes"`
	Vehicles int   `json:"vehicles"`
	Lines    int   `json:"lines"`
	DistM    int64 `json:"dist_m"`
	TimeS    int64 `json:"time_s"`
}

type Line struct {
	Line     string `json:"line"`
	Fixes    int    `json:"fixes"`
	Vehicles int    `json:"vehicles"`
	DistM    int64  `json:"dist_m"`
	TimeS    int64  `json:"time_s"`
}

type Day struct {
	Version     int    `json:"version"`
	Date        string `json:"date"`
	TZ          string `json:"tz"`
	GeneratedAt int64  `json:"generated_at"`
	FirstFix    int64  `json:"first_fix"` // 0 when the day has no fix
	LastFix     int64  `json:"last_fix"`
	Files       int    `json:"files"` // history archives read
	Fixes       int    `json:"fixes"`
	Vehicles    int    `json:"vehicles"`
	Lines       int    `json:"lines"`
	Hours       []Hour `json:"hours"`           // 24, index = local hour
	ByLine      []Line `json:"by_line"`         // lines with a fix, by id
	Trips       *Trips `json:"trips,omitempty"` // scheduled trips that ran; absent without a plan of the feed the rows used
	// StartDelay: how late the runs of OASA's daily schedule left; absent when no run was measured.
	StartDelay *StartDelays `json:"start_delay,omitempty"`
	// Headways: regularity of the high-frequency lines of OASA's daily schedule; absent when no
	// direction qualifies.
	Headways *Headways `json:"headways,omitempty"`
	// Frequency: the scheduled frequency band of each line and direction; only a plan that opts
	// in (a citybus.gr city), absent when no direction has enough daytime departures.
	Frequency *Frequency `json:"frequency,omitempty"`
	// OriginOTP: on-time performance at the route origin; only a plan that opts in (a citybus.gr
	// city), absent when no run was seen.
	OriginOTP *OriginOTP `json:"origin_otp,omitempty"`
}

type Index struct {
	Version int      `json:"version"`
	Latest  string   `json:"latest"`
	Days    []string `json:"days"`
}

// Bounds returns the start and end of a local calendar day (23 or 25 hours at a DST change).
func Bounds(date string, loc *time.Location) (time.Time, time.Time, error) {
	d, err := time.ParseInLocation(dateLayout, date, loc)
	if err != nil || d.Format(dateLayout) != date {
		return time.Time{}, time.Time{}, fmt.Errorf("bad date %q", date)
	}
	return d, d.AddDate(0, 0, 1), nil
}

// Inputs lists the gzipped history archives a day needs: those received from TripSlack before its
// start until TripsAfter after its end. ready is false while one of those hours still has a plain or sealed CSV.
func Inputs(dir, date string, loc *time.Location) (files []string, ready bool, err error) {
	start, end, err := Bounds(date, loc)
	if err != nil {
		return nil, false, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false, err
	}
	from, to := start.Add(-TripSlack).UTC().Truncate(time.Hour), end.Add(TripsAfter).UTC()
	ready = true
	for _, e := range entries {
		name := e.Name()
		hour, _, _ := strings.Cut(name, ".")
		t, err := time.Parse(hourLayout, hour)
		if err != nil || t.Before(from) || !t.Before(to) {
			continue
		}
		switch {
		case strings.HasSuffix(name, ".csv.gz"):
			files = append(files, filepath.Join(dir, name))
		case strings.HasSuffix(name, ".csv"), strings.HasSuffix(name, ".csv.part"):
			ready = false
		}
	}
	sort.Strings(files)
	return files, ready, nil
}

type rec struct {
	t        int64
	line     string
	veh      string
	lat, lon float64 // degrees; 0 when unknown
}

// Filter keeps part of the rows of the files: the rows of one city of a shared history.
type Filter struct {
	Prefix    string // keep only rows whose line column starts with Prefix ("" = every row)
	MovedOnly bool   // drop a speed pair whose two fixes are at the same place
}

// Compute sums one day from its input files. A fix counts for the day of its fix time; a speed
// sample is a pair of consecutive fixes of a vehicle inside the day, on the same line, MinGap to
// MaxGap apart and at most MaxSpeed, credited to the later fix's hour and line. With a plan, the
// matched rows of the files (the day, TripSlack before and TripsAfter after) say how many of the
// plan's trips ran.
func Compute(date string, loc *time.Location, files []string, now time.Time, plan *Plan) (*Day, error) {
	return ComputeFiltered(date, loc, files, now, plan, Filter{})
}

// ComputeFiltered is Compute over the rows f keeps.
func ComputeFiltered(date string, loc *time.Location, files []string, now time.Time, plan *Plan, f Filter) (*Day, error) {
	start, end, err := Bounds(date, loc)
	if err != nil {
		return nil, err
	}
	var recs []rec
	var trips *tripCounter
	var otp *otpCounter
	delays := newDelayCounter(start.Unix(), end.Unix())
	if plan != nil {
		trips = newTripCounter(plan)
		if plan.RunDeparture != nil {
			otp = newOTPCounter(plan.RunDeparture, start.Unix(), end.Unix())
		}
	}
	strs := map[string]string{}
	for _, file := range files {
		if recs, err = readFile(file, start.Unix(), end.Unix(), recs, strs, trips, delays, otp, f); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(file), err)
		}
	}
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].veh != recs[j].veh {
			return recs[i].veh < recs[j].veh
		}
		return recs[i].t < recs[j].t
	})

	type acc struct {
		fixes      int
		dist, secs float64
		vehs       map[string]bool
		lines      map[string]bool
	}
	newAcc := func() *acc { return &acc{vehs: map[string]bool{}, lines: map[string]bool{}} }
	var hours [24]*acc
	for i := range hours {
		hours[i] = newAcc()
	}
	lines := map[string]*acc{}
	day := &Day{Version: Version, Date: date, TZ: loc.String(), GeneratedAt: now.Unix(), Files: len(files)}
	if trips != nil {
		day.Trips = trips.result(loc)
		day.Headways = trips.headways(start, loc)
		if plan.Frequency {
			day.Frequency = frequency(plan, start, loc)
		}
	}
	if otp != nil {
		day.OriginOTP = otp.result()
	}
	day.StartDelay = delays.result()
	vehs := map[string]bool{}
	var prev *rec
	for i := range recs {
		r := &recs[i]
		if prev != nil && prev.veh == r.veh && prev.t == r.t {
			continue // the same fix in two archives (a restart)
		}
		h := hours[time.Unix(r.t, 0).In(loc).Hour()]
		l := lines[r.line]
		if l == nil {
			l = newAcc()
			lines[r.line] = l
		}
		h.fixes++
		l.fixes++
		h.vehs[r.veh], h.lines[r.line], l.vehs[r.veh], vehs[r.veh] = true, true, true, true
		day.Fixes++
		if day.FirstFix == 0 || r.t < day.FirstFix {
			day.FirstFix = r.t
		}
		day.LastFix = max(day.LastFix, r.t)
		if prev != nil && prev.veh == r.veh {
			if d, dt, ok := sample(prev, r, f.MovedOnly); ok {
				h.dist += d
				h.secs += dt
				l.dist += d
				l.secs += dt
			}
		}
		prev = r
	}
	day.Vehicles, day.Lines = len(vehs), len(lines)
	day.Hours = make([]Hour, 24)
	for i, a := range hours {
		day.Hours[i] = Hour{H: i, Fixes: a.fixes, Vehicles: len(a.vehs), Lines: len(a.lines),
			DistM: int64(math.Round(a.dist)), TimeS: int64(math.Round(a.secs))}
	}
	day.ByLine = make([]Line, 0, len(lines))
	for id, a := range lines {
		day.ByLine = append(day.ByLine, Line{Line: id, Fixes: a.fixes, Vehicles: len(a.vehs),
			DistM: int64(math.Round(a.dist)), TimeS: int64(math.Round(a.secs))})
	}
	sort.Slice(day.ByLine, func(i, j int) bool { return day.ByLine[i].Line < day.ByLine[j].Line })
	return day, nil
}

// sample returns the straight-line distance and the time between two fixes of one vehicle. With
// movedOnly, a pair at one place is dropped: a citybus row's time is when its board was read, not
// the GPS time, so a standing bus may hold a stale place (staseis-rt#151).
func sample(a, b *rec, movedOnly bool) (dist, secs float64, ok bool) {
	dt := time.Duration(b.t-a.t) * time.Second
	if a.line != b.line || dt < MinGap || dt > MaxGap || a.lat == 0 || b.lat == 0 || a.lon == 0 || b.lon == 0 {
		return 0, 0, false
	}
	if movedOnly && a.lat == b.lat && a.lon == b.lon {
		return 0, 0, false
	}
	const r = 6371000.0
	rad := math.Pi / 180
	x := (b.lon - a.lon) * rad * math.Cos((a.lat+b.lat)/2*rad)
	y := (b.lat - a.lat) * rad
	dist = math.Hypot(x, y) * r
	secs = dt.Seconds()
	if dist/secs > MaxSpeed {
		return 0, 0, false
	}
	return dist, secs, true
}

// readFile appends the rows of one gzipped history file whose fix time is in [from, to), gives
// every matched row to trips (when not nil), every row with a start delay to delays and every
// row to otp (when not nil). Rows f leaves out count nowhere. Strings repeat a lot: strs interns
// them.
func readFile(path string, from, to int64, recs []rec, strs map[string]string, trips *tripCounter, delays *delayCounter, otp *otpCounter, f Filter) ([]rec, error) {
	file, err := os.Open(path)
	if err != nil {
		return recs, err
	}
	defer file.Close()
	zr, err := gzip.NewReader(file)
	if err != nil {
		return recs, err
	}
	cr := csv.NewReader(zr)
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true
	head, err := cr.Read()
	if err != nil {
		return recs, err
	}
	col := map[string]int{}
	for i, h := range head {
		col[h] = i
	}
	for _, c := range []string{"fix_t", "line", "veh", "lat", "lon"} {
		if _, ok := col[c]; !ok {
			return recs, fmt.Errorf("no %s column", c)
		}
	}
	intern := func(s string) string {
		if s == "" {
			return ""
		}
		if v, ok := strs[s]; ok {
			return v
		}
		s = strings.Clone(s)
		strs[s] = s
		return s
	}
	tripCol, gtfsCol, smCol := optCol(col, "trip_id"), optCol(col, "gtfs"), optCol(col, "s_m")
	runCol, startCol := optCol(col, "run_start"), optCol(col, "start_delay_s")
	for {
		row, err := cr.Read()
		if err == io.EOF {
			return recs, nil
		}
		if err != nil {
			return recs, err
		}
		if len(row) != len(head) {
			continue // a row cut by a crash
		}
		if f.Prefix != "" && !strings.HasPrefix(row[col["line"]], f.Prefix) {
			continue // a row of another city of the shared history
		}
		t, err := strconv.ParseInt(row[col["fix_t"]], 10, 64)
		if err != nil {
			continue
		}
		g := ""
		if gtfsCol >= 0 {
			g = row[gtfsCol]
		}
		sm := int64(-1)
		if smCol >= 0 {
			if v, err := strconv.ParseInt(row[smCol], 10, 64); err == nil {
				sm = v
			}
		}
		if trips != nil && tripCol >= 0 && row[tripCol] != "" {
			trips.add(t, intern(row[col["veh"]]), intern(row[tripCol]), g, float64(sm))
		}
		if otp != nil {
			otp.add(intern(row[col["line"]]), intern(row[col["veh"]]), t, float64(sm))
		}
		if runCol >= 0 && startCol >= 0 && row[runCol] != "" {
			run, err1 := strconv.ParseInt(row[runCol], 10, 64)
			sd, err2 := strconv.Atoi(row[startCol])
			if err1 == nil && err2 == nil {
				delays.add(intern(row[col["line"]]), intern(row[col["veh"]]), run, sd)
			}
		}
		if t < from || t >= to {
			continue
		}
		if trips != nil && g != "" {
			trips.stamp(g)
		}
		lat, _ := strconv.ParseInt(row[col["lat"]], 10, 64)
		lon, _ := strconv.ParseInt(row[col["lon"]], 10, 64)
		recs = append(recs, rec{t: t, line: intern(row[col["line"]]), veh: intern(row[col["veh"]]),
			lat: float64(lat) / 1e6, lon: float64(lon) / 1e6})
	}
}

// optCol returns the index of an optional column, -1 when the file has none.
func optCol(col map[string]int, name string) int {
	if i, ok := col[name]; ok {
		return i
	}
	return -1
}

// Run writes <out>/days/<date>.json for every day with history that ended at least Settle ago
// and has no file yet (force: rewrite them too), then rewrites <out>/index.json from the day
// files. It returns the days written. A day that fails, or whose plan is not available, is
// skipped (the next run retries it). A nil plan leaves the days without trips.
func Run(historyDir, out string, loc *time.Location, now time.Time, force bool, plan PlanFunc) ([]string, error) {
	return RunFiltered(historyDir, out, loc, now, force, plan, Filter{})
}

// RunFiltered is Run over the rows f keeps.
func RunFiltered(historyDir, out string, loc *time.Location, now time.Time, force bool, plan PlanFunc, f Filter) ([]string, error) {
	daysDir := filepath.Join(out, "days")
	if err := os.MkdirAll(daysDir, 0o755); err != nil {
		return nil, err
	}
	var written []string
	var errs []error
	for _, date := range candidates(historyDir, loc) {
		_, end, _ := Bounds(date, loc)
		dst := filepath.Join(daysDir, date+".json")
		// none marks a day with no row f keeps: a city of a shared history must not read the
		// day's files again every hour (staseis-rt#151). Only with a prefix: Athens keeps its way.
		none := filepath.Join(daysDir, date+".none")
		if now.Before(end.Add(Settle)) || (!force && (exists(dst) || f.Prefix != "" && exists(none))) {
			continue
		}
		files, ready, err := Inputs(historyDir, date, loc)
		if err != nil || !ready || len(files) == 0 {
			errs = append(errs, err)
			continue
		}
		var p *Plan
		if plan != nil {
			if p, err = plan(date); err != nil {
				errs = append(errs, fmt.Errorf("stats %s: plan: %w", date, err))
				continue
			}
		}
		day, err := ComputeFiltered(date, loc, files, now, p, f)
		if err == nil && day.Fixes == 0 && day.Frequency == nil { // a timetable-only city keeps its frequency
			if f.Prefix != "" {
				if err := os.WriteFile(none, nil, 0o644); err != nil {
					errs = append(errs, fmt.Errorf("stats %s: %w", date, err))
				}
			}
			continue // only late files of the next day, or no row of the city: nothing to say
		}
		if err == nil {
			err = writeJSON(dst, day)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("stats %s: %w", date, err))
			continue
		}
		written = append(written, date)
	}
	return written, errors.Join(append(errs, writeIndex(out))...)
}

// candidates lists the local days the history files can hold fixes of.
func candidates(dir string, loc *time.Location) []string {
	entries, _ := os.ReadDir(dir)
	seen := map[string]bool{}
	for _, e := range entries {
		hour, _, _ := strings.Cut(e.Name(), ".")
		t, err := time.Parse(hourLayout, hour)
		if err != nil {
			continue
		}
		seen[t.Add(-Late).In(loc).Format(dateLayout)] = true
		seen[t.Add(time.Hour-time.Second).In(loc).Format(dateLayout)] = true
	}
	dates := make([]string, 0, len(seen))
	for d := range seen {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	return dates
}

func writeIndex(out string) error {
	entries, err := os.ReadDir(filepath.Join(out, "days"))
	if err != nil {
		return err
	}
	idx := Index{Version: Version, Days: []string{}}
	for _, e := range entries {
		date, ok := strings.CutSuffix(e.Name(), ".json")
		if _, _, err := Bounds(date, time.UTC); ok && err == nil && date >= FirstDay {
			idx.Days = append(idx.Days, date)
		}
	}
	sort.Strings(idx.Days)
	if n := len(idx.Days); n > 0 {
		idx.Latest = idx.Days[n-1]
	}
	return writeJSON(filepath.Join(out, "index.json"), idx)
}

// writeJSON writes v via a temporary file and a rename, so readers never see half a file.
func writeJSON(dst string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if err == nil {
		err = tmp.Chmod(0o644) // CreateTemp makes 0600
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

package server

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/stats"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

// The archive keeps OASA's daily schedule of every line, one gzipped JSON file per service day
// (<state>/daily/YYYY-MM-DD.json.gz), with no pruning: the trips stat plans from it, and it is
// the record of the timetables OASA really published. It is written from the cache the prefetch
// fills, so it asks OASA nothing.
const dailyArchiveVersion = 1

// archivedDaily is one line's daily schedule (telematics.DailySchedule): "HH:MM", 24:00 and
// later for the runs after midnight.
type archivedDaily struct {
	Descr    string   `json:"descr"`
	Circular bool     `json:"circular,omitempty"`
	Go       []string `json:"go"`
	Come     []string `json:"come"`
}

type dailyArchive struct {
	Version int                      `json:"version"`
	Date    string                   `json:"date"`
	Lines   map[string]archivedDaily `json:"lines"` // by line id
}

func (a *App) dailyArchiveDir() string { return filepath.Join(a.cfg.StateDir, "daily") }

func dailyArchivePath(dir, date string) string { return filepath.Join(dir, date+".json.gz") }

// loadDailyArchive reads the archive of a day; an error wrapping fs.ErrNotExist when there is none.
func loadDailyArchive(dir, date string) (*dailyArchive, error) {
	f, err := os.Open(dailyArchivePath(dir, date))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	var arch dailyArchive
	if err := json.NewDecoder(zr).Decode(&arch); err != nil {
		return nil, err
	}
	if arch.Lines == nil {
		arch.Lines = map[string]archivedDaily{}
	}
	return &arch, nil
}

// writeDailyArchive writes an archive via a temporary file and a rename.
func writeDailyArchive(dir string, arch *dailyArchive) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, arch.Date+".*.tmp")
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(tmp)
	err = json.NewEncoder(zw).Encode(arch)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = tmp.Chmod(0o644)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dailyArchivePath(dir, arch.Date))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// saveDailyArchive merges the cached daily schedules of today into today's archive: lines saved
// before (a restart) stay, a line fetched again replaces its entry unless the new one is partial
// (a line code failed). changed is false when the
// file already had them all.
func (a *App) saveDailyArchive(today time.Time) (changed bool, err error) {
	day := today.Format(time.DateOnly)
	lines := map[string]archivedDaily{}
	partial := map[string]bool{}
	a.dailyMu.Lock()
	for line, e := range a.dailyCache {
		if e.day == day && e.ok {
			lines[line] = archivedDaily{Descr: e.d.Descr, Circular: e.d.Circular, Go: e.d.Go, Come: e.d.Come}
			partial[line] = e.partial
		}
	}
	a.dailyMu.Unlock()
	if len(lines) == 0 {
		return false, nil
	}
	dir := a.dailyArchiveDir()
	arch, err := loadDailyArchive(dir, day)
	if errors.Is(err, fs.ErrNotExist) {
		arch, err = &dailyArchive{Version: dailyArchiveVersion, Date: day, Lines: map[string]archivedDaily{}}, nil
	}
	if err != nil {
		return false, err
	}
	for line, d := range lines {
		old, ok := arch.Lines[line]
		if ok && partial[line] {
			continue // a line code failed: keep what was saved, it may be the full schedule
		}
		if !ok || !reflect.DeepEqual(old, d) {
			arch.Lines[line], changed = d, true
		}
	}
	if !changed {
		return false, nil
	}
	return true, writeDailyArchive(dir, arch)
}

// DailyPlan is the stats plan of a local day (stats.NewPlan). When the feed has expired for the
// day and dir has the day's archive, its lines are planned from OASA's daily schedule instead
// (stats.DailyDir): the runs of the day that leave before midnight, and the night runs (24:00
// and later) of the day before. A missing archive keeps the GTFS plan; an unreadable one is an
// error, so the day is tried again.
func DailyPlan(f *gtfs.Feed, version, date string, loc *time.Location, dir string) (*stats.Plan, error) {
	p, err := stats.NewPlan(f, version, date, loc)
	if err != nil || !p.Expired {
		return p, err
	}
	cur, err := loadDailyArchive(dir, date)
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	start, end, _ := stats.Bounds(date, loc)
	prevDate := start.AddDate(0, 0, -1).Format(time.DateOnly)
	prev, err := loadDailyArchive(dir, prevDate)
	if errors.Is(err, fs.ErrNotExist) {
		prev, err = &dailyArchive{}, nil
	}
	if err != nil {
		return nil, err
	}
	p.UseDaily(dailyDirs(f, cur, prev, start, end))
	return p, nil
}

// dailyDirs turns the archives of a day (cur) and of the day before (prev) into the directions
// of their lines with runs that leave in [start, end).
func dailyDirs(f *gtfs.Feed, cur, prev *dailyArchive, start, end time.Time) []stats.DailyDir {
	seen := map[string]bool{}
	var lines []string
	for _, arch := range []*dailyArchive{cur, prev} {
		for line := range arch.Lines {
			if !seen[line] {
				seen[line] = true
				lines = append(lines, line)
			}
		}
	}
	sort.Strings(lines)
	m := match.New(f, nil, match.Greedy)
	prevDay := start.AddDate(0, 0, -1)
	var out []stats.DailyDir
	for _, line := range lines {
		d, ok := cur.Lines[line]
		if !ok {
			d = prev.Lines[line] // only night runs of the day before: its directions and names
		}
		resp := dailyToSchedule(line, "", toDaily(d))
		starts := map[int][]int64{}
		add := func(day time.Time, s ScheduleResponse, night bool) {
			for _, dir := range s.Directions {
				for _, dep := range dir.Departures {
					mins, ok := minutesOf(dep.T)
					if !ok || (mins >= 24*60) != night {
						continue
					}
					t := time.Date(day.Year(), day.Month(), day.Day(), 0, mins, 0, 0, day.Location()) // wall clock: DST days too
					if !t.Before(start) && t.Before(end) {
						starts[dir.Direction] = append(starts[dir.Direction], t.Unix())
					}
				}
			}
		}
		if ok {
			add(start, resp, false)
		}
		dirs := resp.Directions
		if pd, ok := prev.Lines[line]; ok {
			pr := dailyToSchedule(line, "", toDaily(pd))
			add(prevDay, pr, true)
			for _, pdir := range pr.Directions { // a direction with night runs only
				if !slices.ContainsFunc(dirs, func(x ScheduleDirection) bool { return x.Direction == pdir.Direction }) {
					dirs = append(dirs, pdir)
				}
			}
		}
		shapes := lineShapes(f, line)
		loop := d.Circular || len(shapes) == 1 && len(resp.Directions) == 1 // as markDailySeen
		for _, dir := range dirs {
			ts := starts[dir.Direction]
			if len(ts) == 0 {
				continue
			}
			sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
			dd := stats.DailyDir{Line: line, Dir: dir.Direction, Starts: ts, Trips: map[string]stats.Span{}}
			in := map[int32]bool{}
			for _, s := range shapesOf(shapes, dir.Headsign, loop) {
				in[s] = true
			}
			for _, ti := range f.TripsForLine(line) {
				if t := f.Trip(ti); in[t.Shape] {
					dd.Trips[t.ID] = stats.SpanOf(m, t)
				}
			}
			out = append(out, dd)
		}
	}
	return out
}

func toDaily(d archivedDaily) telematics.DailySchedule {
	return telematics.DailySchedule{Descr: d.Descr, Circular: d.Circular, Go: d.Go, Come: d.Come}
}

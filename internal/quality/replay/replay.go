// Package replay runs the live quality (internal/quality) over an hour of the fix history, the
// way the server would have rated it then, and counts the levels. It checks the thresholds on
// real data and measures the cost per fix (docs/QUALITY.md "Live levels").
package replay

import (
	"compress/gzip"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/quality"
	"github.com/angelospk/athens-transit-rt/internal/segspeed"
	"github.com/angelospk/athens-transit-rt/internal/staticsite"
	"github.com/angelospk/athens-transit-rt/internal/usual"
)

const (
	warmup      = 20 * time.Minute // fixes before the hour that fill the tracker
	segmentTick = 5 * time.Minute  // the segments are rated this often within the hour
	typicalDays = 14               // as the server's typical speeds
)

// Config names the history, its feed and the hour to replay (local time, Europe/Athens).
type Config struct {
	History string // directory of hourly .csv.gz archives
	Feed    *gtfs.Feed
	Version string // GTFS version of the history rows to use
	Hour    time.Time
	Usual   string // directory for the usual day files it builds (reused when present)
}

// Result counts the levels: vehicles once per fix within the hour, segments once per
// segmentTick for every segment crossed in the window then.
type Result struct {
	Vehicles     map[string]int // "<basis>/<level>", basis "-" when none
	Bunched      int
	Early        int
	Segments     map[string]int
	Fixes        int           // fixes rated within the hour
	Observed     int           // fixes fed to the tracker (warm-up included)
	Work         time.Duration // time spent in Observe, State, the usual lookup and Vehicle
	SegmentWork  time.Duration // time spent rating the segments
	SegmentCalls int
	HeapBefore   uint64 // live heap before the replay: tables built, rows read
	HeapAfter    uint64 // and after it, rows still held: the difference is the tracker and its caches
	TrackerSize  [3]int // vehicles, passes, traversals at the end
	UsualDays    int
	UsualStops   int
}

type row struct {
	t     int64
	veh   string
	trip  string
	shape string
	s     float64
	delay *int
}

// Run replays cfg.Hour.
func Run(cfg Config) (*Result, error) {
	from := cfg.Hour.Add(-warmup)
	to := cfg.Hour.Add(time.Hour)
	tab, err := segspeed.Build(cfg.History, gtfs.Athens, cfg.Hour.Add(-typicalDays*24*time.Hour), cfg.Hour, cfg.Version)
	if err != nil {
		return nil, err
	}
	st, err := usualStore(cfg)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	rows, err := readRows(cfg.History, from, to, cfg.Version)
	if err != nil {
		return nil, err
	}
	m := match.New(cfg.Feed, nil, match.Greedy)
	stops := map[[2]int32][]quality.Stop{}
	res := &Result{Vehicles: map[string]int{}, Segments: map[string]int{}, UsualDays: st.TrainingDays(cfg.Hour)}
	runtime.GC()
	res.HeapBefore = heap()
	k := quality.NewTracker()
	cache := map[[2]string][]usual.Row{} // (stop, local hour), as the server's usualCache
	lookup := func(stop, line, head string, now time.Time) quality.Usual {
		if res.UsualDays < usual.MinDays {
			return quality.Usual{}
		}
		key := [2]string{stop, now.In(gtfs.Athens).Format("2006-01-02T15")}
		rs, ok := cache[key]
		if !ok {
			rs = st.Query(stop, now)
			cache[key] = rs
		}
		for _, r := range rs {
			if r.Line == line && r.Headsign == head && r.HeadwayMin != nil {
				return quality.Usual{Known: true, HeadwayS: *r.HeadwayMin * 60}
			}
		}
		return quality.Usual{Known: true}
	}
	shapes := map[string][]string{} // shape -> its Pattern stop ids, as /v2/lines/{id}
	lineFiles := map[string]staticsite.LineFile{}
	nextSeg := cfg.Hour.Add(segmentTick).Unix()
	rate := func(now int64) {
		start := time.Now()
		for id, pat := range shapes {
			for _, o := range k.Segments(id, pat, now) {
				level := quality.Unknown
				u, ok := tab.Between(id, o.FromAlong, o.ToAlong, segspeed.Slot(time.Unix(now, 0), gtfs.Athens))
				switch {
				case !ok || u <= 0:
					res.Segments["unknown: no usual speed"]++
				case o.N < quality.MinTraversals:
					res.Segments["unknown: 1 traversal"]++
				default:
					level = quality.SegmentLevel(o.MedianMS / u)
					res.Segments[string(level)]++
				}
			}
		}
		res.SegmentWork += time.Since(start)
		res.SegmentCalls++
	}
	for _, r := range rows {
		for r.t >= nextSeg && nextSeg <= to.Unix() {
			rate(nextSeg)
			nextSeg += int64(segmentTick.Seconds())
		}
		ti := cfg.Feed.TripIndex(r.trip)
		if ti < 0 {
			continue
		}
		t := cfg.Feed.Trip(ti)
		if t.Shape < 0 || cfg.Feed.Shapes[t.Shape].ID != r.shape {
			continue
		}
		g := m.Geometry(t)
		if g == nil || !g.FromShape {
			continue
		}
		line, head := cfg.Feed.Line(t), cfg.Feed.Headsign(t)
		key := [2]int32{t.Shape, t.Pattern}
		ss, ok := stops[key]
		if !ok {
			pat := cfg.Feed.Patterns[t.Pattern].Stops
			for i, s := range pat {
				if i < len(g.StopAlong) {
					ss = append(ss, quality.Stop{ID: cfg.Feed.Stops[s].ID, Along: g.StopAlong[i]})
				}
			}
			stops[key] = ss
		}
		if _, ok := shapes[r.shape]; !ok {
			if _, ok := lineFiles[line]; !ok {
				lineFiles[line] = staticsite.LineFileOf(cfg.Feed, line)
			}
			shapes[r.shape] = lineFiles[line].Variants[r.shape].Stops
		}
		start := time.Now()
		k.Observe(quality.Fix{Vehicle: r.veh, Trip: t.ID, Line: line, Headsign: head, Shape: r.shape, Stops: ss, S: r.s, T: r.t})
		res.Observed++
		var lv quality.VehicleLevel
		state, ok := k.State(r.veh, t.ID)
		if ok && state.RefStop != "" {
			lv = quality.Vehicle(lookup(state.RefStop, line, head, time.Unix(r.t, 0)), state.Gap, r.delay)
		} else {
			lv = quality.VehicleLevel{Level: quality.Unknown}
		}
		res.Work += time.Since(start)
		if r.t < cfg.Hour.Unix() {
			continue
		}
		res.Fixes++
		basis := lv.Basis
		if basis == "" {
			basis = "-"
		}
		res.Vehicles[basis+"/"+string(lv.Level)]++
		if lv.Bunched {
			res.Bunched++
		}
		if lv.Early {
			res.Early++
		}
	}
	for nextSeg <= to.Unix() {
		rate(nextSeg)
		nextSeg += int64(segmentTick.Seconds())
	}
	runtime.GC()
	res.HeapAfter = heap()
	v, p, tr := k.Size()
	res.TrackerSize = [3]int{v, p, tr}
	res.UsualStops = len(cache)
	runtime.KeepAlive(k)
	runtime.KeepAlive(rows)
	runtime.KeepAlive(stops)
	runtime.KeepAlive(tab)
	runtime.KeepAlive(m)
	return res, nil
}

func heap() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// usualStore builds (or reuses) the usual day files of the training days of cfg.Hour.
func usualStore(cfg Config) (*usual.Store, error) {
	if err := os.MkdirAll(cfg.Usual, 0o755); err != nil {
		return nil, err
	}
	st, err := usual.OpenStore(cfg.Usual)
	if err != nil {
		return nil, err
	}
	sd := usual.ServiceDate(cfg.Hour)
	routes := usual.FeedRoutes(cfg.Feed)
	for d := sd.AddDate(0, 0, -usual.TrainDays); d.Before(sd); d = d.AddDate(0, 0, 1) {
		if usual.DayType(d) != usual.DayType(sd) || st.Has(d) {
			continue
		}
		day, err := usual.BuildDay(cfg.History, d, cfg.Version, routes)
		if err != nil {
			return nil, err
		}
		if day == nil {
			continue
		}
		if err := usual.WriteDay(cfg.Usual, day); err != nil {
			return nil, err
		}
		if err := st.Load(d); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// readRows reads the matched rows of version with fix time in [from, to), in fix-time order, from
// the archives hour.csv.gz and hour.N.csv.gz (after a restart within the hour).
func readRows(dir string, from, to time.Time, version string) ([]row, error) {
	var out []row
	for h := from.Truncate(time.Hour).Add(-time.Hour); h.Before(to); h = h.Add(time.Hour) {
		name := h.UTC().Format("2006-01-02T15")
		paths, err := filepath.Glob(filepath.Join(dir, name+".*.csv.gz"))
		if err != nil {
			return nil, err
		}
		for _, path := range append([]string{filepath.Join(dir, name+".csv.gz")}, paths...) {
			if err := readArchive(path, from.Unix(), to.Unix(), version, &out); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].t < out[j].t })
	return out, nil
}

func readArchive(path string, from, to int64, version string, out *[]row) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	cr := csv.NewReader(zr)
	cr.FieldsPerRecord = -1
	head, err := cr.Read()
	if err != nil {
		return err
	}
	col := map[string]int{}
	for i, h := range head {
		col[h] = i
	}
	for _, c := range []string{"fix_t", "veh", "gtfs", "trip_id", "shape_id", "s_m", "delay_s"} {
		if _, ok := col[c]; !ok {
			return fmt.Errorf("no %s column", c)
		}
	}
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			return nil
		}
		var pe *csv.ParseError
		if errors.As(err, &pe) || (err == nil && len(rec) != len(head)) {
			continue // a bad row, or one cut by a crash
		}
		if err != nil {
			return err
		}
		if rec[col["gtfs"]] != version || rec[col["trip_id"]] == "" || rec[col["shape_id"]] == "" {
			continue
		}
		t, err := strconv.ParseInt(rec[col["fix_t"]], 10, 64)
		if err != nil || t < from || t >= to {
			continue
		}
		s, err := strconv.ParseFloat(rec[col["s_m"]], 64)
		if err != nil || s < 0 {
			continue
		}
		r := row{t: t, veh: rec[col["veh"]], trip: rec[col["trip_id"]], shape: rec[col["shape_id"]], s: s}
		if d, err := strconv.Atoi(rec[col["delay_s"]]); err == nil {
			r.delay = &d
		}
		*out = append(*out, r)
	}
}

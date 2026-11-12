// Daily statistics per city (staseis-rt#151). The citybus.gr cities share one fix history, so
// every city sums only its own rows (line "pat:601") into its own <state>/<slug>/stats, against
// the plan of its own timetable (stats.SourceTimetable). A row's time is when a board was read,
// not the GPS time, so a pair that did not move is dropped (stats.Filter.MovedOnly).

package citybus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/stats"
)

// statsIndexMaxAge and statsDayMaxAge: a day file never changes once written.
const (
	statsIndexMaxAge = 600
	statsDayMaxAge   = 86400
)

// statsPlan is the plan of a local day from the city's timetable: every trip of a route, of the
// service day (Date-1, Date, Date+1), that leaves in [start of the day, start of the next). The
// trip times are seconds after midnight of the service day (dayBase), so a trip past 24:00 counts
// for the day before. An error means no model, or a date that is none.
func (c *City) statsPlan(date string) (*stats.Plan, error) {
	start, end, err := stats.Bounds(date, athensLoc)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.model
	if m == nil {
		return nil, errors.New("no static model yet")
	}
	p := &stats.Plan{GTFS: m.Version, Source: stats.SourceTimetable, Trips: []stats.PlanTrip{},
		Frequency: true, RunDeparture: c.runDeparture}
	for i := -1; i <= 1; i++ {
		d := start.AddDate(0, 0, i).Format(time.DateOnly)
		base, weekday, ok := dayBase(d)
		if !ok {
			return nil, fmt.Errorf("bad date %q", d)
		}
		for _, code := range sortedRouteCodes(m) {
			r := m.Routes[code]
			for j := range r.Trips {
				t := &r.Trips[j]
				if t.Day != weekday {
					continue
				}
				first, last := -1, -1
				for k, x := range t.Times {
					if x == NoTime {
						continue
					}
					if first < 0 {
						first = k
					}
					last = k
				}
				if first < 0 {
					continue // a trip with no time
				}
				s := base + int64(t.Times[first])
				if s < start.Unix() || s >= end.Unix() {
					continue
				}
				pt := stats.PlanTrip{ID: t.ID, Line: c.lid(r.Line), Dir: r.Dir, Start: s,
					End: base + int64(t.Times[last])}
				if first < len(r.StopM) {
					pt.FromM = r.StopM[first]
				}
				if last < len(r.StopM) {
					pt.ToM = r.StopM[last]
				}
				p.Trips = append(p.Trips, pt)
			}
		}
	}
	sort.Slice(p.Trips, func(i, j int) bool {
		a, b := p.Trips[i], p.Trips[j]
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		return a.ID < b.ID
	})
	return p, nil
}

// runDeparture reads a vehicle of the city's history rows ("pat:20261110_4_0100000_17_20") as a
// run: its scheduled departure (unix s). A device id, or a row of another city, is no run.
func (c *City) runDeparture(veh string) (int64, bool) {
	code, ok := strings.CutPrefix(veh, c.Conf.ID+":")
	if !ok {
		return 0, false
	}
	date, dep, ok := runCode(code)
	if !ok {
		return 0, false
	}
	base, _, ok := dayBase(date)
	return base + int64(dep), ok
}

// statsRoute serves the city's stats files (index and days) and reports whether it handled the
// path. It runs before the ready check: the files are written by the service, not by the model.
func (c *City) statsRoute(w http.ResponseWriter, path string) bool {
	if path == "/v1/stats" {
		c.serveStatsFile(w, filepath.Join(c.dir, "stats", "index.json"), statsIndexMaxAge)
		return true
	}
	m := reStatsDay.FindStringSubmatch(path)
	if m == nil {
		return false
	}
	date := dec(m[1])
	if _, _, err := stats.Bounds(date, time.UTC); err != nil {
		writeErr(w, "bad_date", http.StatusBadRequest)
		return true
	}
	c.serveStatsFile(w, filepath.Join(c.dir, "stats", "days", date+".json"), statsDayMaxAge)
	return true
}

// serveStatsFile writes a stats file as it is; a missing file is not_found.
func (c *City) serveStatsFile(w http.ResponseWriter, p string, maxAge int) {
	b, err := os.ReadFile(p)
	if err != nil {
		writeErr(w, "not_found", http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(maxAge))
	w.Write(b)
}

// runStats sums the finished days of the city's rows of the shared history into <dir>/stats.
func (c *City) runStats(historyDir string, now time.Time) ([]string, error) {
	return stats.RunFiltered(historyDir, filepath.Join(c.dir, "stats"), athensLoc, now, false,
		c.statsPlan, stats.Filter{Prefix: c.Conf.ID + ":", MovedOnly: true})
}

// dailyStats sums the days of every city's rows, at start and then hourly, until ctx ends.
func (s *Service) dailyStats(ctx context.Context, historyDir string) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		for _, c := range s.order {
			days, err := c.runStats(historyDir, time.Now())
			if len(days) > 0 {
				s.log.Info("stats: wrote days", "city", c.Conf.ID, "days", days)
			}
			if err != nil {
				s.log.Warn("stats", "city", c.Conf.ID, "err", err)
			}
			if ctx.Err() != nil {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

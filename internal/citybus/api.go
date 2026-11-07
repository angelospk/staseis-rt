package citybus

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The answers, in the atrt contract shapes (docs/CONTRACT.md: /v1 tiles, lines, stops; /v2), as
// the bridge gave them. /v2 ids are namespaced per city: her:1235, her:run:<trip>:<date>.

type obj = map[string]any

func (c *City) lid(code string) string { return c.Conf.ID + ":" + code }

func (c *City) unNs(id string) string { return strings.TrimPrefix(id, c.Conf.ID+":") }

func (c *City) lids(codes []string) []string {
	out := make([]string, len(codes))
	for i, s := range codes {
		out[i] = c.lid(s)
	}
	return out
}

func hhmm(s int) string { return fmt.Sprintf("%02d:%02d", s/3600, s/60%60) }

func shape5(sh []LL) [][2]float64 {
	out := make([][2]float64, len(sh))
	for i, p := range sh {
		out[i] = [2]float64{round5(p[0]), round5(p[1])}
	}
	return out
}

// llOrNil keeps JSON null for a nil path.
func llOrNil(p []LL) any {
	if p == nil {
		return nil
	}
	return p
}

func intsOrNil(p []int) any {
	if p == nil {
		return nil
	}
	return p
}

func (c *City) sourcesLocked() []obj {
	var newest any
	var n int64
	if c.live != nil {
		for _, b := range c.live.Boards {
			n = max(n, b.At)
		}
	}
	if n > 0 {
		newest = n
	}
	state := "warming_up"
	if c.model != nil {
		state = "ok"
	}
	return []obj{{"id": c.Conf.ID, "state": state, "updated_at": newest}}
}

func (c *City) v2LineLocked(code string) obj {
	l := c.model.LineByCode[code]
	return obj{"id": c.lid(l.Code), "mode": "bus", "label": l.Code, "name": l.Name, "name_en": nil, "color": l.Color,
		"text_color": l.TextColor, "source": c.Conf.ID}
}

func (c *City) v2StopLocked(code string) obj {
	s := c.model.Stops[code]
	return obj{"id": c.lid(s.Code), "name": s.Name, "name_en": nil, "lat": s.Lat, "lon": s.Lon, "modes": []string{"bus"},
		"lines": c.lids(s.Lines), "source": c.Conf.ID}
}

func motionObj(m Motion) obj {
	return obj{"path": llOrNil(m.Path), "path_beyond": llOrNil(m.PathBeyond), "path_stops": intsOrNil(m.PathStops), "speed": m.Speed}
}

func (c *City) v2Vehicle(p Placed) obj {
	mo := motionObj(motion(&p))
	mo["path_speeds"], mo["knots"] = nil, nil
	return obj{"id": c.lid(p.S.ID), "line": c.lid(p.Route.Line), "run": nil, "mode": "bus", "headsign": p.Route.Name, "label": nil,
		"position": obj{"kind": p.Kind, "lat": p.Lat, "lon": p.Lon, "bearing": p.Bearing, "at": p.At},
		"delay_s":  nil, "next_stop": c.lid(p.Route.Stops[p.NextIdx]), "at_terminus": false, "direction": p.Route.Dir,
		"motion": mo, "source": c.Conf.ID}
}

func v1Vehicle(p Placed, full bool) obj {
	o := obj{"line": p.Route.Line, "id": p.S.ID, "lat": p.Lat, "lon": p.Lon, "bearing": p.Bearing, "position_at": p.At,
		"variant": p.Route.Code, "delay_s": nil, "speed": p.S.Speed}
	if full {
		for k, v := range motionObj(motion(&p)) {
			o[k] = v
		}
		o["path_speeds"] = nil
	}
	return o
}

func (c *City) status() obj {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.unix()
	lines := map[string]bool{}
	if c.live != nil {
		for _, p := range c.live.placed(now, "") {
			lines[p.Route.Line] = true
		}
	}
	version := ""
	if c.model != nil {
		version = c.model.Version
	}
	gap := c.up.pacer().gapNow()
	return obj{"ok": c.model != nil, "gtfs_version": version, "gtfs_expires": "", "updated_at": now, "lines_active": len(lines),
		"budget_rps": math.Round(float64(time.Second)/float64(gap)*100) / 100}
}

// healthReport: which lines sent a live bus in the last Days; it survives a restart in health.json.
func (c *City) healthReport() obj {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.unix()
	lines := []obj{}
	if c.model != nil {
		for _, l := range c.model.Lines {
			var last any
			if t, ok := c.health.LastLive[l.Code]; ok {
				last = t
			}
			lines = append(lines, obj{"line": l.Code, "last_live": last, "timetable_only": c.health.timetableOnly(l.Code, now)})
		}
	}
	return obj{"since": c.health.Since, "days": Days, "lines": lines}
}

func (c *City) staticLines() obj {
	c.mu.Lock()
	defer c.mu.Unlock()
	lines := []obj{}
	for _, l := range c.model.Lines {
		lines = append(lines, obj{"id": l.Code, "name": l.Name, "name_en": "", "color": l.Color, "text_color": l.TextColor, "kind": "bus"})
	}
	return obj{"gtfs_version": c.model.Version, "lines": lines}
}

func (c *City) staticLine(code string) obj {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.model.LineByCode[code]
	if l == nil {
		return nil
	}
	variants, stops := obj{}, obj{}
	for _, rc := range l.Routes {
		r := c.model.Routes[rc]
		variants[rc] = obj{"headsign": r.Name, "direction": r.Dir, "shape": shape5(r.Shape), "stops": r.Stops}
		for _, s := range r.Stops {
			st := c.model.Stops[s]
			stops[s] = obj{"name": st.Name, "lat": st.Lat, "lon": st.Lon}
		}
	}
	return obj{"id": l.Code, "variants": variants, "stops": stops}
}

func (c *City) v2Lines() obj {
	c.mu.Lock()
	defer c.mu.Unlock()
	lines := []obj{}
	for _, l := range c.model.Lines {
		lines = append(lines, c.v2LineLocked(l.Code))
	}
	return obj{"lines": lines, "sources": c.sourcesLocked()}
}

func (c *City) v2Stops() obj {
	c.mu.Lock()
	defer c.mu.Unlock()
	codes := make([]string, 0, len(c.model.Stops))
	for k := range c.model.Stops {
		codes = append(codes, k)
	}
	sort.Strings(codes)
	stops := make([]any, 0, len(codes))
	for _, k := range codes {
		s := c.model.Stops[k]
		stops = append(stops, []any{c.lid(s.Code), s.Name, s.Lat, s.Lon, []string{"bus"}, c.lids(s.Lines)})
	}
	return obj{"sources": c.sourcesLocked(), "stops": stops}
}

func (c *City) v2LineDetail(id string) obj {
	c.mu.Lock()
	defer c.mu.Unlock()
	code := c.unNs(id)
	l := c.model.LineByCode[code]
	if l == nil {
		return nil
	}
	stops := obj{}
	patterns := []obj{}
	for _, rc := range l.Routes {
		r := c.model.Routes[rc]
		for _, s := range r.Stops {
			stops[c.lid(s)] = c.v2StopLocked(s)
		}
		patterns = append(patterns, obj{"id": c.lid(r.Code), "headsign": r.Name, "direction": r.Dir, "stops": c.lids(r.Stops),
			"shape": shape5(r.Shape)})
	}
	return obj{"line": c.v2LineLocked(code), "patterns": patterns, "stops": stops, "sources": c.sourcesLocked()}
}

func (c *City) v2LineVehicles(id string) obj {
	c.mu.Lock()
	defer c.mu.Unlock()
	code := c.unNs(id)
	if c.model.LineByCode[code] == nil {
		return nil
	}
	now := c.unix()
	c.watchLocked(code)
	vs := []obj{}
	for _, p := range c.live.placed(now, code) {
		vs = append(vs, c.v2Vehicle(p))
	}
	return obj{"line": c.lid(code), "updated_at": now, "next_update_at": now + 10, "vehicles": vs, "sources": c.sourcesLocked()}
}

func (c *City) v1Line(code string) obj {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.model.LineByCode[code] == nil {
		return nil
	}
	now := c.unix()
	c.watchLocked(code)
	vs := []obj{}
	for _, p := range c.live.placed(now, code) {
		o := v1Vehicle(p, true)
		o["route_code"], o["trip_id"], o["trip_label"], o["next_stop_id"] = p.Route.Code, nil, nil, p.Route.Stops[p.NextIdx]
		vs = append(vs, o)
	}
	return obj{"line": code, "updated_at": now, "next_update_at": now + 10, "vehicles": vs}
}

func (c *City) tile(z, x, y int) obj {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.unix()
	vs := []obj{}
	if c.live != nil {
		for _, p := range c.live.placed(now, "") {
			if tx, ty := tileOf(p.Lat, p.Lon, z); tx == x && ty == y {
				vs = append(vs, v1Vehicle(p, z >= 13))
			}
		}
	}
	return obj{"updated_at": now, "next_update_at": now + 10, "vehicles": vs}
}

func (c *City) hasStop(code string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.model.Stops[code] != nil
}

func (c *City) v1StopBoard(code string) obj {
	if !c.hasStop(code) {
		return nil
	}
	b := c.freshBoard(code)
	c.mu.Lock()
	defer c.mu.Unlock()
	s, now := c.model.Stops[code], c.unix()
	if s == nil { // a daily reload took the stop out while the board was read
		return nil
	}
	arr := []obj{}
	for _, a := range stopArrivals(c.model, code, b, now) {
		src := "schedule"
		if a.Live {
			src = "live"
		}
		var trip any
		if a.Trip != "" {
			trip = a.Trip
		}
		arr = append(arr, obj{"line": a.Line, "headsign": a.Headsign, "variant": a.Route, "vehicle": a.Vehicle, "eta_s": a.At - now,
			"at": a.At, "delay_s": 0, "position_at": a.Seen, "source": src, "trip_id": trip, "scheduled": a.Scheduled,
			"stops_away": nil, "distance_m": nil})
	}
	return obj{"stop": obj{"id": code, "name": s.Name, "lat": s.Lat, "lon": s.Lon}, "lines": s.Lines, "usual": []any{},
		"updated_at": now, "arrivals": arr}
}

func (c *City) v2StopBoard(id string) obj {
	code := c.unNs(id)
	if !c.hasStop(code) {
		return nil
	}
	b := c.freshBoard(code)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.model.Stops[code] == nil { // a daily reload took the stop out while the board was read
		return nil
	}
	now := c.unix()
	date := athensClock(now).Date
	lines := []obj{}
	for _, l := range c.model.Stops[code].Lines {
		lines = append(lines, c.v2LineLocked(l))
	}
	arr := []obj{}
	for _, a := range stopArrivals(c.model, code, b, now) {
		src, status := "timetable", "scheduled"
		if a.Live {
			src, status = "live", "running"
		}
		var run, vehicle any
		if a.Trip != "" {
			d := a.Date
			if d == "" {
				d = date
			}
			run = c.Conf.ID + ":run:" + a.Trip + ":" + d
		}
		if a.Vehicle != "" {
			vehicle = c.lid(a.Vehicle)
		}
		arr = append(arr, obj{"line": c.lid(a.Line), "stop": c.lid(code), "headsign": a.Headsign, "at": a.At, "scheduled_at": a.Scheduled,
			"delay_s": nil, "source": src, "run": run, "vehicle": vehicle, "vehicle_label": nil, "platform": nil, "status": status})
	}
	return obj{"stop": c.v2StopLocked(code), "lines": lines, "usual": []any{}, "updated_at": now, "sources": c.sourcesLocked(),
		"arrivals": arr}
}

var runRe = regexp.MustCompile(`^([a-z]+):run:([^:]+):(\d{4})-(\d{2})-(\d{2})$`)

func (c *City) v2Run(id string) obj {
	m := runRe.FindStringSubmatch(id)
	if m == nil || m[1] != c.Conf.ID {
		return nil
	}
	day, err := time.ParseInLocation("2006-01-02", m[3]+"-"+m[4]+"-"+m[5], athensLoc)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.unix()
	// The service day of the run: yesterday, today or tomorrow (its times may pass 24:00).
	clock := athensClock(day.Add(12 * time.Hour).Unix())
	if d := clock.Midnight - athensClock(now).Midnight; d > 90000 || d < -90000 {
		return nil
	}
	codes := make([]string, 0, len(c.model.Routes))
	for k := range c.model.Routes {
		codes = append(codes, k)
	}
	sort.Strings(codes)
	for _, rc := range codes {
		r := c.model.Routes[rc]
		for _, t := range r.Trips {
			if t.ID != m[2] || t.Day != clock.Day {
				continue
			}
			stops := []obj{}
			for i, s := range r.Stops {
				if t.Times[i] == NoTime {
					continue
				}
				at := clock.Midnight + int64(t.Times[i])
				stops = append(stops, obj{"stop": c.lid(s), "scheduled_at": at, "at": at, "delay_s": nil, "source": "timetable",
					"platform": nil, "passed": at < now-60})
			}
			return obj{"run": obj{"id": id, "line": c.lid(r.Line), "headsign": r.Name, "pattern": c.lid(r.Code), "vehicle": nil,
				"vehicle_label": nil, "status": "scheduled", "stops": stops}, "sources": c.sourcesLocked()}
		}
	}
	return nil
}

func (c *City) schedule(code string) obj {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.model.LineByCode[code]
	if l == nil {
		return nil
	}
	clock := athensClock(c.unix())
	dirs := []obj{}
	for _, d := range []int{0, 1} {
		var routes []*Route
		for _, rc := range l.Routes {
			if r := c.model.Routes[rc]; r.Dir == d {
				routes = append(routes, r)
			}
		}
		if len(routes) == 0 {
			continue
		}
		type dep struct {
			s    int
			trip string
		}
		var deps []dep
		count := map[string]int{}
		for _, r := range routes {
			count[r.Name]++
			for _, t := range r.Trips {
				if t.Day != clock.Day {
					continue
				}
				for _, x := range t.Times {
					if x != NoTime {
						deps = append(deps, dep{x, t.ID})
						break
					}
				}
			}
		}
		sort.SliceStable(deps, func(i, j int) bool { return deps[i].s < deps[j].s })
		out := []obj{}
		for _, x := range deps {
			out = append(out, obj{"t": hhmm(x.s), "trip": x.trip, "seen": false})
		}
		// The headsign of the most routes (the first one on a tie).
		headsign, best := "", 0
		for _, r := range routes {
			if count[r.Name] > best {
				headsign, best = r.Name, count[r.Name]
			}
		}
		dirs = append(dirs, obj{"direction": d, "headsign": headsign, "departures": out})
	}
	return obj{"line": code, "date": clock.Date, "expired": false, "source": "citybus", "directions": dirs}
}

func (c *City) debug() obj {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.unix()
	o := obj{"city": c.Conf, "built_at": nil, "lines": 0, "stops": 0, "routes": 0, "probes": len(c.probes),
		"gap_ms": c.up.pacer().gapNow().Milliseconds(), "last_cycle": c.lastCycle, "queued": c.up.pacer().queued()}
	checks := 0
	for _, p := range c.checkProbes {
		checks += len(p)
	}
	o["check_probes"] = checks
	skipped := []string{}
	for l := range c.skipped {
		skipped = append(skipped, l)
	}
	sort.Strings(skipped)
	o["skipped_lines"] = skipped
	watched := []string{}
	for l := range c.watched {
		watched = append(watched, l)
	}
	sort.Strings(watched)
	o["watched"] = watched
	if c.model != nil {
		o["built_at"], o["lines"], o["stops"], o["routes"] = c.model.BuiltAt, len(c.model.Lines), len(c.model.Stops), len(c.model.Routes)
	}
	var placed []Placed
	if c.live != nil {
		placed = c.live.placed(now, "")
	}
	gps, est := 0, 0
	var oldest any
	var old int64 = -1
	for _, p := range placed {
		if p.Kind == "gps" {
			gps++
		} else {
			est++
		}
		old = max(old, now-p.S.Seen)
	}
	if old >= 0 {
		oldest = old
	}
	o["buses"], o["gps"], o["estimate"], o["oldest_s"] = len(placed), gps, est, oldest
	if cl, ok := c.up.(*client); ok {
		o["upstream"] = cl.statsNow()
	}
	return o
}

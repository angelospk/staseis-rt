// Live state of one city: the buses seen on stop boards, where they are now, and stop arrivals.
// citybus.gr has no fleet call; each stop board (stops/live/{code}) lists the buses on their way to
// that stop with their position and minutes to the stop. We keep the newest sighting per bus.
// Port of citybus-bridge src/live.ts.

package citybus

import (
	"math"
	"sort"
	"time"
)

// MaxAge is how long a sighting is used: a bus not seen for 2 rotations (~60 s each) is gone,
// so no position is older than 2 min.
const MaxAge = 120

// sightings turns one board into sightings (pure). A bus off its route's shape (> 300 m) is skipped.
func sightings(m *Model, b Board) []Sighting {
	out := []Sighting{}
	for _, r := range b.Rows {
		route := m.Routes[r.RouteCode]
		lat, lon := float64(r.Latitude), float64(r.Longitude)
		if route == nil || !finite(lat) || !finite(lon) {
			continue
		}
		// A route out and back on one street passes the point twice. Take the pass whose distance to
		// the board's stop fits its minutes best (at ~6 m/s); a pass with no visit of that stop ahead
		// is out.
		t := float64(r.DepartureMins)*60 + float64(r.DepartureSeconds)
		p := proj{M: 0, Off: math.Inf(1)}
		idx, fit := -1, math.Inf(1)
		for _, c := range candidates(route.Shape, route.Cum, LL{lat, lon}, 30) {
			if c.Off > 300 {
				continue
			}
			i := -1
			for k, s := range route.Stops {
				if s == b.Stop && route.StopM[k] >= c.M-30 {
					i = k
					break
				}
			}
			if i < 0 {
				continue
			}
			f := math.Abs(route.StopM[i] - c.M - t*6)
			if f < fit {
				fit, p, idx = f, c, i
			}
		}
		if idx < 0 {
			continue
		}
		toM := route.StopM[idx]
		eta := b.At + int64(t)
		left := math.Max(0, toM-p.M)
		var speed *float64
		if t > 30 && left > 100 {
			v := math.Min(15, math.Max(2, left/t))
			speed = &v
		}
		out = append(out, Sighting{ID: r.VehicleCode, Route: r.RouteCode, Lat: lat, Lon: lon, Seen: b.At,
			M: math.Min(p.M, toM), ToStop: b.Stop, ToM: toM, ETA: eta, Speed: speed})
	}
	return out
}

func finite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// place returns where a bus is at time now: its sighting, moved on along the shape at its speed
// (never past the stop it was due at, never on before it was due to leave a terminus).
func place(mo *Model, s Sighting, now int64) *Placed {
	route := mo.Routes[s.Route]
	if route == nil {
		return nil
	}
	age := now - s.Seen
	if age > MaxAge {
		return nil
	}
	moved := s.M
	if s.Speed != nil && age > 0 {
		moved = math.Min(s.ToM, s.M+*s.Speed*float64(age))
	}
	kind := "gps"
	if moved-s.M > 30 {
		kind = "estimate"
	}
	m := moved
	lat, lon := s.Lat, s.Lon
	if kind != "gps" {
		q := pointAt(route.Shape, route.Cum, m)
		lat, lon = q[0], q[1]
	}
	nextIdx := -1
	for i, x := range route.StopM {
		if x > m+15 {
			nextIdx = i
			break
		}
	}
	if nextIdx < 0 {
		nextIdx = len(route.Stops) - 1
	}
	ahead := pointAt(route.Shape, route.Cum, math.Min(route.Cum[len(route.Cum)-1], m+40))
	var brg *float64
	if dist(LL{lat, lon}, ahead) > 5 {
		v := bearing(LL{lat, lon}, ahead)
		brg = &v
	}
	at := now
	if kind == "gps" {
		at = s.Seen
	}
	return &Placed{S: s, Route: route, Lat: lat, Lon: lon, M: m, At: at, Kind: kind, Bearing: brg, NextIdx: nextIdx}
}

// motion is the path ahead for the client's mover: up to 1.5 km, then on to the third stop.
func motion(p *Placed) Motion {
	r := p.Route
	end := math.Min(r.Cum[len(r.Cum)-1], p.S.ToM) // never past the stop the bus was due at
	if end-p.M < 5 {
		return Motion{}
	}
	pathEnd := math.Min(end, p.M+1500)
	third := end
	if i := min(len(r.Stops)-1, p.NextIdx+2); i >= 0 && i < len(r.StopM) {
		third = r.StopM[i]
	}
	beyondEnd := math.Max(pathEnd, math.Min(end, third))
	path := slice(r.Shape, r.Cum, p.M, pathEnd)
	var beyond []LL
	if beyondEnd > pathEnd {
		beyond = slice(r.Shape, r.Cum, pathEnd, beyondEnd)
	}
	var stopsM []int
	for _, x := range r.StopM {
		if x > p.M && x <= beyondEnd {
			stopsM = append(stopsM, int(math.Round(x-p.M)))
		}
	}
	out := Motion{PathBeyond: beyond, Speed: p.S.Speed}
	if len(path) >= 2 {
		out.Path = path
	}
	if len(stopsM) > 0 {
		out.PathStops = stopsM
	}
	return out
}

type liveCity struct {
	model  *Model
	Buses  map[string]Sighting
	Boards map[string]Board
}

func newLiveCity(m *Model) *liveCity {
	return &liveCity{model: m, Buses: map[string]Sighting{}, Boards: map[string]Board{}}
}

func (l *liveCity) add(b Board) []Sighting {
	l.Boards[b.Stop] = b
	found := sightings(l.model, b)
	for _, s := range found {
		old, ok := l.Buses[s.ID]
		// Newer wins; at the same time, the board of the nearer stop (its minutes are surer).
		if !ok || s.Seen > old.Seen || (s.Seen == old.Seen && s.ToM-s.M < old.ToM-old.M) {
			l.Buses[s.ID] = s
		}
	}
	return found
}

func (l *liveCity) prune(now int64) {
	for id, s := range l.Buses {
		if now-s.Seen > MaxAge {
			delete(l.Buses, id)
		}
	}
	for k, b := range l.Boards {
		if now-b.At > MaxAge {
			delete(l.Boards, k)
		}
	}
}

func (l *liveCity) placed(now int64, line string) []Placed {
	out := []Placed{}
	for _, s := range l.Buses {
		p := place(l.model, s, now)
		if p != nil && (line == "" || p.Route.Line == line) {
			out = append(out, *p)
		}
	}
	return out
}

var athensLoc = loadAthens()

func loadAthens() *time.Location {
	loc, err := time.LoadLocation("Europe/Athens")
	if err != nil {
		panic(err)
	}
	return loc
}

// athensClock is the seconds after midnight, the weekday (0 = Sunday, as citybus.gr) and the date,
// in Athens.
func athensClock(unix int64) Clock {
	t := time.Unix(unix, 0).In(athensLoc)
	sec := int64(t.Hour()*3600 + t.Minute()*60 + t.Second())
	return Clock{Date: t.Format("2006-01-02"), Day: int(t.Weekday()), Sec: sec, Midnight: unix - sec}
}

// serviceDay is one Athens service day: its weekday (0 = Sunday), its date and the unix s of its midnight.
type serviceDay struct {
	day  int
	base int64
	date string
}

// serviceDays: yesterday (its runs past midnight have times over 24:00), today and tomorrow.
func serviceDays(clock Clock) [3]serviceDay {
	var out [3]serviceDay
	for i, off := range []int{-1, 0, 1} {
		// DST days are 23 or 25 h, so take the midnight of that day from noon, not from ±86400.
		base := athensClock(clock.Midnight + int64(off)*86400 + 43200).Midnight
		out[i] = serviceDay{day: ((clock.Day+off)%7 + 7) % 7, base: base, date: athensClock(base + 43200).Date}
	}
	return out
}

const (
	// estimateHorizon: no arrival is estimated later than this (s).
	estimateHorizon = 3600
	// urbanSpeed is the speed (m/s) of a bus that has none of its own; sightings fits with the same.
	urbanSpeed = 6.0
	// maxSpeed is the cap of a sighting's speed (m/s): no bus reaches a stop sooner than at this.
	maxSpeed = 15.0
)

// rideSecs is the timetable's seconds from stop index a to stop index b (a <= b) of the route, on
// the trip whose time at a is nearest to at (the trip that fits the bus best). ok is false when no
// trip of these days has both times, or when the times cannot be a ride over dist metres (they
// run backwards, or are faster than 20 m/s with a minute to spare: the timetable is in minutes).
func rideSecs(r *Route, a, b int, at int64, days [3]serviceDay, dist float64) (int64, bool) {
	best, ride := int64(math.MaxInt64), int64(-1)
	for _, d := range days {
		for _, t := range r.Trips {
			if t.Day != d.day || t.Times[a] == NoTime || t.Times[b] == NoTime {
				continue
			}
			off := d.base + int64(t.Times[a]) - at
			if off < 0 {
				off = -off
			}
			if off < best {
				best, ride = off, int64(t.Times[b]-t.Times[a])
			}
		}
	}
	if ride < 0 || dist > float64(ride+60)*20 {
		return 0, false
	}
	return ride, true
}

// estimateAt is when a placed bus is due at stop index k of its route (ahead of it). The anchor is
// the stop the bus was last seen due at (S.ToStop at S.ETA, citybus.gr's own minutes). A stop after
// the anchor is due at S.ETA plus the timetable's ride from the anchor, else plus distance over
// speed. A stop before the anchor counts back from S.ETA on the timetable, never sooner than the
// bus can drive there and never after the anchor; else it shares the time left by distance.
func estimateAt(p *Placed, k int, now int64, days [3]serviceDay) int64 {
	r, s := p.Route, p.S
	x := r.StopM[k]
	a := -1
	for i, c := range r.Stops {
		if c == s.ToStop && math.Abs(r.StopM[i]-s.ToM) < 1 {
			a = i
			break
		}
	}
	if x >= s.ToM {
		v := urbanSpeed
		if s.Speed != nil {
			v = *s.Speed
		}
		ride := int64(math.Round((x - s.ToM) / v))
		if a >= 0 && k >= a {
			if t, ok := rideSecs(r, a, k, s.ETA, days, x-s.ToM); ok {
				ride = t
			}
		}
		return max(now, s.ETA+ride)
	}
	due := max(now, s.ETA) // the bus is not at the anchor yet
	soonest := now + int64(math.Round((x-p.M)/maxSpeed))
	if a >= 0 && k <= a {
		if t, ok := rideSecs(r, k, a, s.ETA, days, s.ToM-x); ok {
			return min(due, max(soonest, s.ETA-t))
		}
	}
	return now + int64(math.Round(float64(due-now)*(x-p.M)/(s.ToM-p.M)))
}

// estimates lists the placed buses due at a stop: every visit of the stop that is ahead of a bus
// on its route (a loop may call twice), never where the trip ends, up to estimateHorizon ahead.
func estimates(stop string, placed []Placed, now int64) []Arrival {
	days := serviceDays(athensClock(now))
	out := []Arrival{}
	for i := range placed {
		p := &placed[i]
		r := p.Route
		for k, c := range r.Stops {
			if c != stop || k >= len(r.Stops)-1 || r.StopM[k] <= p.M+15 {
				continue
			}
			at := estimateAt(p, k, now, days)
			if at-now > estimateHorizon {
				continue
			}
			out = append(out, Arrival{Line: r.Line, Route: r.Code, Headsign: r.Name, At: at, Live: true, Estimate: true,
				Vehicle: p.S.ID, Lat: p.Lat, Lon: p.Lon, Seen: p.At})
		}
	}
	// placed comes from a map: a fixed order for equal times.
	sort.Slice(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At < out[j].At
		}
		return out[i].Vehicle < out[j].Vehicle
	})
	return out
}

// stopArrivals lists a stop's arrivals: the board's buses with a position (live), then the timetable runs not yet
// covered by a live bus (each live bus takes the scheduled run nearest its time, within 15 min).
// With no board, the live rows are estimates from the placed buses (see estimates); a board, even
// an empty one, wins over them. Up to 2 h ahead, at most 4 per line|headsign and 30 in all.
func stopArrivals(m *Model, stop string, board *Board, placed []Placed, now int64) []Arrival {
	days := serviceDays(athensClock(now))
	out := []Arrival{}
	live := map[string][]int64{}
	if board == nil {
		for _, a := range estimates(stop, placed, now) {
			out = append(out, a)
			live[a.Route] = append(live[a.Route], a.At)
		}
	}
	if board != nil {
		for _, r := range board.Rows {
			route := m.Routes[r.RouteCode]
			if route == nil || (r.Latitude == 0 && r.Longitude == 0) {
				continue // 0,0: no GPS, a row citybus.gr made from its timetable; the timetable run below stands
			}
			at := board.At + int64(float64(r.DepartureMins)*60+float64(r.DepartureSeconds))
			if at < now {
				continue // an old board: that bus has passed
			}
			out = append(out, Arrival{Line: route.Line, Route: route.Code, Headsign: route.Name,
				At: max(at, now), Live: true, Vehicle: r.VehicleCode,
				Lat: float64(r.Latitude), Lon: float64(r.Longitude), Seen: board.At})
			live[route.Code] = append(live[route.Code], at)
		}
	}
	type sched struct {
		at   int64
		trip string
		date string
	}
	for _, code := range sortedRouteCodes(m) {
		route := m.Routes[code]
		// Every visit of the stop (a loop may call twice), never where a trip ends.
		visits := []int{}
		for i, c := range route.Stops {
			if c == stop && i < len(route.Stops)-1 {
				visits = append(visits, i)
			}
		}
		if len(visits) == 0 {
			continue
		}
		list := []sched{}
		// Yesterday's runs past midnight (times over 24:00), today's, and tomorrow's first ones.
		for _, d := range days {
			for _, t := range route.Trips {
				if t.Day != d.day {
					continue
				}
				for _, idx := range visits {
					if t.Times[idx] == NoTime {
						continue
					}
					at := d.base + int64(t.Times[idx])
					if at >= now-60 && at <= now+7200 {
						list = append(list, sched{at: at, trip: t.ID, date: d.date})
					}
				}
			}
		}
		for _, lat := range live[route.Code] {
			bi, bd := -1, int64(901)
			for i, s := range list {
				d := s.at - lat
				if d < 0 {
					d = -d
				}
				if d < bd {
					bd, bi = d, i
				}
			}
			if bi >= 0 {
				list = append(list[:bi], list[bi+1:]...)
			}
		}
		for _, s := range list {
			if s.at < now {
				continue
			}
			at := s.at
			out = append(out, Arrival{Line: route.Line, Route: route.Code, Headsign: route.Name,
				At: at, Scheduled: &at, Trip: s.trip, Date: s.date})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	const perKey = 4
	per := map[string]int{}
	kept := []Arrival{}
	for _, a := range out {
		k := a.Line + "|" + a.Headsign
		per[k]++
		if per[k] <= perKey {
			kept = append(kept, a)
		}
	}
	if len(kept) > 30 {
		kept = kept[:30]
	}
	return kept
}

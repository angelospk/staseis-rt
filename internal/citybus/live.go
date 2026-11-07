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

// stopArrivals lists a stop's arrivals: the board's buses with a position (live), then the timetable runs not yet
// covered by a live bus (each live bus takes the scheduled run nearest its time, within 15 min).
// Up to 2 h ahead, at most 4 per line|headsign and 30 in all.
func stopArrivals(m *Model, stop string, board *Board, now int64) []Arrival {
	clock := athensClock(now)
	out := []Arrival{}
	live := map[string][]int64{}
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
		for _, off := range []int{-1, 0, 1} {
			day := ((clock.Day+off)%7 + 7) % 7
			// DST days are 23 or 25 h, so take the midnight of that day from noon, not from ±86400.
			base := athensClock(clock.Midnight + int64(off)*86400 + 43200).Midnight
			date := athensClock(base + 43200).Date
			for _, t := range route.Trips {
				if t.Day != day {
					continue
				}
				for _, idx := range visits {
					if t.Times[idx] == NoTime {
						continue
					}
					at := base + int64(t.Times[idx])
					if at >= now-60 && at <= now+7200 {
						list = append(list, sched{at: at, trip: t.ID, date: date})
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

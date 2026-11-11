// Timetable runs seen (rt#141). A citybus.gr vehicle code names a trip slot:
// "20261110_4_0100000_17_20" is the run of 2026-11-10 that leaves at 17:20 (the route is in the
// board row). A plain device id ("1607413212998552622") names no run. A run is seen when a bus
// with its code is on it: a bus short after its first stop, well after it, not before it.
// The log survives a restart in runs.json; old days are dropped.

package citybus

import (
	"encoding/json"
	"os"
	"regexp"
	"time"
)

// codeRe: a run code: a service date, the route part, then the departure.
var codeRe = regexp.MustCompile(`^(\d{4})(\d{2})(\d{2})_.+_(\d{2})_(\d{2})$`)

// runCode reads a vehicle code as a run: its service date ("YYYY-MM-DD") and its departure (s
// after midnight; hours up to 29 count the runs past midnight). ok is false for a device id.
func runCode(code string) (date string, dep int, ok bool) {
	m := codeRe.FindStringSubmatch(code)
	if m == nil {
		return "", 0, false
	}
	h, mi := atoi2(m[4]), atoi2(m[5])
	if h > 29 || mi > 59 {
		return "", 0, false
	}
	date = m[1] + "-" + m[2] + "-" + m[3]
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return "", 0, false // a date that is none: month 13, day 40
	}
	return date, h*3600 + mi*60, true
}

// runOf: the run of a sighting. The code names it; a device id carries the run the live city
// gave it from the timetable (liveCity.devRun).
func runOf(r *Route, s Sighting) (date string, dep int, ok bool) {
	if date, dep, ok = runCode(s.ID); ok {
		return date, dep, true
	}
	return s.RunDate, s.RunDep, s.RunDate != ""
}

// Time match of a device-id bus (#149): of the trips of yesterday's and today's service day, the
// one whose time where the bus is lies from timeEarly before to timeLate after now. Only a bus
// timeFromM along its route counts (one at the start can wait there for any run), and only one
// trip may match. A run another bus of the route is on (held, by runKey) is no candidate, and a
// bus keeps its run while it goes on (#155, liveCity.devRun).
//
// Measured on coded buses with their own code hidden (2026-11-11, 28 cities, 60 min, 6247
// sightings of runs in the timetable): time match alone 67.4% matched, 93.3% right; with held
// runs left out and the run kept 67.7% matched, 96.9% right. Most wrong ones are codes that
// disagree with the timetable (Kavala's runs 9-21 min before their code); without Kavala 67.4% /
// 96.4% before, 69.2% / 98.2% now. Tried and left out: the departure past timeFromM naming the
// trip (+0.3 points matched), a late window from the headway (fewer right).
const (
	timeEarly = 180
	timeLate  = 600
	timeFromM = 300
)

func timeRun(r *Route, s Sighting, held map[string]bool) (date string, dep int, ok bool) {
	if r == nil || s.Against || s.M < timeFromM {
		return "", 0, false
	}
	n, days := 0, serviceDays(athensClock(s.Seen))
	for _, d := range days[:2] { // yesterday's runs past midnight, and today's
		now := int(s.Seen - d.base)
		for i := range r.Trips {
			t := &r.Trips[i]
			if t.Day != d.day {
				continue
			}
			at, first, ok := schedAt(r, t, s.M)
			if !ok || now-at < -timeEarly || now-at > timeLate {
				continue
			}
			if held[runKey(d.date, s.Route, first)] {
				continue // another bus is on it
			}
			if tripOf(r, d.day, first) != t {
				return "", 0, false // its departure names another trip: the run would show that one
			}
			if n++; n > 1 {
				return "", 0, false
			}
			date, dep = d.date, first
		}
	}
	return date, dep, n == 1
}

// schedAt: the trip's time at m along the route (linear between its timed stops; its departure
// before the first one, the last time after the last one) and its departure. false: no timed stop.
func schedAt(r *Route, t *Trip, m float64) (at, dep int, ok bool) {
	prev := -1
	for i, x := range t.Times {
		if x == NoTime || i >= len(r.StopM) {
			continue
		}
		if prev < 0 {
			dep = x
			if m <= r.StopM[i] {
				return x, dep, true
			}
		} else if m <= r.StopM[i] {
			pm, px := r.StopM[prev], t.Times[prev]
			if r.StopM[i] <= pm {
				return x, dep, true
			}
			return px + int(float64(x-px)*(m-pm)/(r.StopM[i]-pm)), dep, true
		}
		prev = i
	}
	if prev < 0 {
		return 0, 0, false
	}
	return t.Times[prev], dep, true // past its last timed stop: that time
}

func atoi2(s string) int { return int(s[0]-'0')*10 + int(s[1]-'0') }

// dayBase: the base serviceDays gives for that date (Athens noon minus 12 h, as GTFS counts, also
// on the days the clocks change) and its weekday (0 = Sunday).
func dayBase(date string) (base int64, weekday int, ok bool) {
	t, err := time.ParseInLocation("2006-01-02 15:04", date+" 12:00", athensLoc)
	if err != nil {
		return 0, 0, false
	}
	return t.Unix() - 43200, int(t.Weekday()), true
}

// tripOf: the first trip of the route, in its order, that runs on day and leaves at dep (the
// time of its first timed stop). nil when none.
func tripOf(r *Route, day, dep int) *Trip {
	if r == nil {
		return nil
	}
	for i := range r.Trips {
		t := &r.Trips[i]
		if t.Day != day {
			continue
		}
		for _, x := range t.Times {
			if x == NoTime {
				continue
			}
			if x == dep {
				return t
			}
			break // the first timed stop is its departure
		}
	}
	return nil
}

// runKey names a run in the log: no trip id, so a daily reload that renumbers the trips keeps the
// day's marks.
func runKey(date, route string, dep int) string {
	return date + "|" + route + "|" + hhmm(dep)
}

// runSeen is one run a bus was counted on: its first and newest sighting (unix s) and the bus.
type runSeen struct {
	First int64  `json:"first"`
	Last  int64  `json:"last"`
	Veh   string `json:"veh"`
}

// runLog is a city's day of seen runs, in runs.json.
type runLog struct {
	Runs  map[string]*runSeen `json:"runs"`
	file  string
	dirty bool
}

// loadRuns reads a runs.json; a missing or unreadable file gives an empty log. Runs older than
// yesterday's service day are dropped.
func loadRuns(file string, now int64) *runLog {
	l := &runLog{Runs: map[string]*runSeen{}, file: file}
	if b, err := os.ReadFile(file); err == nil {
		var doc runLog
		if err := json.Unmarshal(b, &doc); err == nil && doc.Runs != nil {
			l.Runs = doc.Runs
		}
	}
	l.dropBefore(serviceDays(athensClock(now))[0].date)
	return l
}

// dropBefore drops every run of a service day before date (plain string compare of the dates).
func (l *runLog) dropBefore(date string) {
	for k := range l.Runs {
		if len(k) >= 10 && k[:10] < date {
			delete(l.Runs, k)
		}
	}
}

// note counts a sighting on its run when the bus can be on it: the code names a run of this
// route's timetable, the time is from just before its departure to 45 min after its end, the bus
// is well past the first stop, and it stands away from that stop (so a bus waiting at a terminus
// counts on no run). It returns true when the sighting counted.
func (l *runLog) note(m *Model, s Sighting) bool {
	route := m.Routes[s.Route]
	if route == nil || s.Against { // a bus against its route is not on its run (#147)
		return false
	}
	date, dep, ok := runOf(route, s)
	if !ok {
		return false
	}
	base, weekday, ok := dayBase(date)
	if !ok {
		return false
	}
	t := tripOf(route, weekday, dep)
	if t == nil {
		return false
	}
	first, last := -1, -1
	for i, x := range t.Times {
		if x == NoTime {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	if first < 0 || last < first || last >= len(route.StopM) || first >= len(route.Stops) {
		return false
	}
	start, end := base+int64(t.Times[first]), base+int64(t.Times[last])
	if s.Seen < start-120 || s.Seen > end+2700 {
		return false
	}
	if s.M < route.StopM[first]+100 {
		return false
	}
	st := m.Stops[route.Stops[first]]
	if st == nil || dist(LL{s.Lat, s.Lon}, LL{st.Lat, st.Lon}) < 100 {
		return false
	}
	if l.Runs == nil {
		l.Runs = map[string]*runSeen{}
	}
	k := runKey(date, s.Route, dep)
	if r, ok := l.Runs[k]; ok {
		r.Last = max(r.Last, s.Seen)
		r.Veh = s.ID
	} else {
		l.Runs[k] = &runSeen{First: s.Seen, Last: s.Seen, Veh: s.ID}
	}
	l.dirty = true
	return true
}

// seen: the run of that service date, route and departure, nil when no bus was counted on it.
func (l *runLog) seen(date, route string, dep int) *runSeen {
	return l.Runs[runKey(date, route, dep)]
}

// save writes the log to its file through a .tmp and a rename, so a half-written file never
// appears. Nothing noted since the last save writes nothing.
func (l *runLog) save(now int64) error {
	if !l.dirty || l.file == "" {
		return nil
	}
	l.dropBefore(serviceDays(athensClock(now))[0].date)
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if err := os.WriteFile(l.file+".tmp", b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(l.file+".tmp", l.file); err != nil {
		return err
	}
	l.dirty = false
	return nil
}

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
	if route == nil {
		return false
	}
	date, dep, ok := runCode(s.ID)
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

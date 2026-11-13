// The static model of one city, built from the citybus.gr REST answers (pure: no I/O here).
// Port of citybus-bridge src/model.ts.

package citybus

import (
	"sort"
	"strconv"
	"strings"
)

// secs reads "HH:MM" as seconds after midnight.
func secs(hhmm string) (int, bool) {
	i := strings.IndexByte(hhmm, ':')
	if i < 1 || i > 2 || len(hhmm)-i-1 != 2 {
		return 0, false
	}
	h := 0
	for _, c := range hhmm[:i] {
		if c < '0' || c > '9' {
			return 0, false
		}
		h = h*10 + int(c-'0')
	}
	m := 0
	for _, c := range hhmm[i+1:] {
		if c < '0' || c > '9' {
			return 0, false
		}
		m = m*10 + int(c-'0')
	}
	return h*3600 + m*60, true
}

func colorOf(c, fallback string) string {
	if len(c) == 7 && c[0] == '#' {
		ok := true
		for i := 1; i < 7; i++ {
			ch := c[i]
			if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F') {
				ok = false
				break
			}
		}
		if ok {
			return strings.ToLower(c)
		}
	}
	return fallback
}

// tripTimes turns the rows of one trip into a time per stop of the route, walking the stop order
// (a stop may repeat on a loop).
func tripTimes(stops []string, rows []RawTripRow) []int {
	out := make([]int, len(stops))
	for i := range out {
		out[i] = NoTime
	}
	i, last := 0, NoTime
	for _, r := range rows {
		j := i
		for j < len(stops) && stops[j] != r.StopCode {
			j++
		}
		if j == len(stops) {
			continue // a stop not on the route: skip it
		}
		t, ok := secs(r.TripTime)
		if !ok {
			out[j] = NoTime
			i = j + 1
			continue
		}
		if t < last-43200 {
			t += 86400 // past midnight: 00:10 after 23:55 is 24:10
		}
		out[j] = t
		last = t
		i = j + 1
	}
	return out
}

func firstTime(t Trip) int {
	for _, x := range t.Times {
		if x != NoTime {
			return x
		}
	}
	return 0
}

// padCode is the sort key of a line code: left-padded with "0" to 8 chars.
func padCode(s string) string {
	if len(s) >= 8 {
		return s
	}
	return strings.Repeat("0", 8-len(s)) + s
}

// sortedRouteCodes lists the route codes of m in sorted order, so output never depends on map order.
func sortedRouteCodes(m *Model) []string {
	out := make([]string, 0, len(m.Routes))
	for c := range m.Routes {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func buildModel(raw *RawCity) *Model {
	stops := make(map[string]*Stop, len(raw.Stops))
	for _, s := range raw.Stops {
		stops[s.Code] = &Stop{Code: s.Code, Name: s.Name, Lat: float64(s.Latitude), Lon: float64(s.Longitude),
			Lines: uniqSorted(s.LineCodes)}
	}
	shapes := make(map[string][]LL)
	for _, list := range raw.Points {
		for _, p := range list {
			pts := append([]RawPoint(nil), p.RoutePoints...)
			sort.SliceStable(pts, func(i, j int) bool { return pts[i].Sequence < pts[j].Sequence })
			shape := make([]LL, 0, len(pts))
			for _, q := range pts {
				shape = append(shape, LL{float64(q.Latitude), float64(q.Longitude)})
			}
			shapes[p.RouteCode] = shape
		}
	}
	routes := make(map[string]*Route)
	lines := []*Line{}
	for _, l := range raw.Lines {
		codes := []string{}
		for _, r := range l.Routes {
			seqRaw := append([]RawSeq(nil), raw.Sequences[r.Code]...)
			sort.SliceStable(seqRaw, func(i, j int) bool { return seqRaw[i].Sequence < seqRaw[j].Sequence })
			seq := make([]string, 0, len(seqRaw))
			for _, s := range seqRaw {
				if _, ok := stops[s.Code]; ok {
					seq = append(seq, s.Code)
				}
			}
			if len(seq) < 2 {
				continue
			}
			// No shape (or one point): the straight line through the stops.
			shape := shapes[r.Code]
			if len(shape) < 2 {
				shape = make([]LL, 0, len(seq))
				for _, c := range seq {
					s := stops[c]
					shape = append(shape, LL{s.Lat, s.Lon})
				}
			}
			cum := cumulative(shape)
			pts := make([]LL, len(seq))
			for i, c := range seq {
				pts[i] = LL{stops[c].Lat, stops[c].Lon}
			}
			stopM := stopMetres(shape, cum, pts)
			byTrip := make(map[int64][]RawTripRow)
			order := []int64{}
			for _, row := range raw.Trips[r.Code] {
				if _, ok := byTrip[row.ID]; !ok {
					order = append(order, row.ID)
				}
				byTrip[row.ID] = append(byTrip[row.ID], row)
			}
			trips := []Trip{}
			for _, id := range order {
				rows := byTrip[id]
				times := tripTimes(seq, rows)
				any := false
				for _, t := range times {
					if t != NoTime {
						any = true
						break
					}
				}
				if any {
					trips = append(trips, Trip{ID: strconv.FormatInt(id, 10), Day: rows[0].Day, Times: times})
				}
			}
			sort.SliceStable(trips, func(a, b int) bool {
				if trips[a].Day != trips[b].Day {
					return trips[a].Day < trips[b].Day
				}
				return firstTime(trips[a]) < firstTime(trips[b])
			})
			dir := 0
			if r.Direction == 2 {
				dir = 1
			}
			routes[r.Code] = &Route{Code: r.Code, Line: l.Code, Name: strings.TrimSpace(strings.TrimSuffix(r.Name, ".")),
				Dir: dir, Stops: seq, Shape: shape, Cum: cum, StopM: stopM, Trips: trips}
			codes = append(codes, r.Code)
		}
		if len(codes) == 0 {
			continue
		}
		lines = append(lines, &Line{Code: l.Code, Name: l.Name, Color: colorOf(l.Color, "#1565c0"),
			TextColor: colorOf(l.TextColor, "#ffffff"), Routes: codes})
	}
	sort.SliceStable(lines, func(i, j int) bool { return padCode(lines[i].Code) < padCode(lines[j].Code) })
	// Stops no route calls at are left out.
	used := map[string]bool{}
	for _, r := range routes {
		for _, c := range r.Stops {
			used[c] = true
		}
	}
	for c := range stops {
		if !used[c] {
			delete(stops, c)
		}
	}
	for _, s := range stops {
		set := map[string]bool{}
		for _, r := range routes {
			for _, c := range r.Stops {
				if c == s.Code {
					set[r.Line] = true
					break
				}
			}
		}
		ls := make([]string, 0, len(set))
		for l := range set {
			ls = append(ls, l)
		}
		sort.Slice(ls, func(i, j int) bool { return padCode(ls[i]) < padCode(ls[j]) })
		s.Lines = ls
	}
	lineByCode := make(map[string]*Line, len(lines))
	for _, l := range lines {
		lineByCode[l.Code] = l
	}
	return &Model{Lines: lines, LineByCode: lineByCode, Stops: stops, Routes: routes, BuiltAt: raw.BuiltAt, Version: raw.version(), StopsVersion: raw.stopsVersion()}
}

func uniqSorted(in []string) []string {
	set := map[string]bool{}
	for _, s := range in {
		set[s] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// probeStops returns the stops whose live board shows every bus of every route. A board lists the
// buses on their way to that stop, so a stop near the end of a route sees the whole route; a second
// stop near the middle covers a board that only looks a limited time ahead. Greedy: stops that serve
// several routes at the wanted places count once.
func probeStops(m *Model, skip map[string]bool) []string {
	need := map[string]map[string]bool{} // stop -> the (route, slot) needs it meets
	all := map[string]bool{}
	for _, code := range sortedRouteCodes(m) {
		r := m.Routes[code]
		if skip[r.Line] {
			continue // a line we no longer rotate over
		}
		n := len(r.Stops)
		for i, c := range r.Stops {
			f := float64(i) / float64(n-1)
			slots := []string{}
			if i >= 1 && f >= 0.35 && f <= 0.6 {
				slots = append(slots, "mid")
			}
			if i >= n-3 && i <= n-2 {
				slots = append(slots, "end")
			}
			for _, s := range slots {
				k := r.Code + "|" + s
				all[k] = true
				if need[c] == nil {
					need[c] = map[string]bool{}
				}
				need[c][k] = true
			}
		}
	}
	chosen := []string{}
	left := make(map[string]bool, len(all))
	for k := range all {
		left[k] = true
	}
	for len(left) > 0 {
		best, gain := "", 0
		for c, ks := range need {
			g := 0
			for k := range ks {
				if left[k] {
					g++
				}
			}
			if g > gain || (g == gain && g > 0 && (best == "" || c < best)) {
				best, gain = c, g
			}
		}
		if gain == 0 {
			break
		}
		chosen = append(chosen, best)
		for k := range need[best] {
			delete(left, k)
		}
	}
	return chosen
}

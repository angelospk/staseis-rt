package server

import (
	"math"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/geo"
	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/model"
	"github.com/angelospk/athens-transit-rt/internal/segspeed"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

// Stop boards: the buses on their way to one stop and about when they get there, from what the
// line polls already hold (no OASA request), and while the timetable is valid the trips that
// have not left yet.
const (
	stopMaxAge      = 15
	staleFix        = 5 * time.Minute // an older fix is not promised at a stop: the bus may be gone
	boardHorizon    = time.Hour       // arrivals up to this far ahead
	boardAtStopM    = 30              // GPS noise: a vehicle this far past a stop is still at it
	boardPerVariant = 3               // rows per line and variant, soonest first
	// boardDefaultMS is the speed where neither typical speeds nor a timetable say anything
	// (an unmatched vehicle off the history): about the lines through the centre, stops included.
	boardDefaultMS = 3.5
	// boardMinBinMS: a slower typical bin counts this fast. The history counts layovers at
	// terminals as driving time there.
	boardMinBinMS = 0.5
)

type StopInfo struct {
	ID   string  `json:"id"`
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
	Code string  `json:"code,omitempty"` // rev 21: the code on the stop sign (GTFS stop_code)
}

// StopArrival is a bus on its way to the stop (source "live") or a trip that has not left its
// first stop yet and has no bus (source "schedule": vehicle "", delay_s and position_at 0).
type StopArrival struct {
	Line       string  `json:"line"`
	Headsign   string  `json:"headsign"`
	Variant    string  `json:"variant"`
	Vehicle    string  `json:"vehicle"`
	EtaS       int     `json:"eta_s"`
	At         int64   `json:"at"`
	DelayS     int     `json:"delay_s"`
	PositionAt int64   `json:"position_at"`
	Source     string  `json:"source"`
	TripID     *string `json:"trip_id"`    // null: an unmatched bus
	Scheduled  *int64  `json:"scheduled"`  // the trip's timetable time at the stop
	StopsAway  *int    `json:"stops_away"` // 1: the stop is the bus's next one
	DistanceM  *int    `json:"distance_m"` // along the route

	day time.Time // service day of TripID (/v2 run ids); zero without a trip
}

type StopResponse struct {
	Stop     StopInfo      `json:"stop"`
	Lines    []string      `json:"lines"`
	Arrivals []StopArrival `json:"arrivals"`
	Usual    []model.Usual `json:"usual"` // rev 19
	// Warming (rev 22): a line of the stop is polled now but has no poll yet (after a restart),
	// so its buses are missing from arrivals.
	Warming   bool  `json:"warming"`
	UpdatedAt int64 `json:"updated_at"`
}

// stopLines maps each stop to the lines whose trips call there, sorted; built once per feed.
func stopLines(f *gtfs.Feed) map[int32][]string {
	byPattern := map[int32]map[string]bool{}
	for i := range f.Trips {
		t := &f.Trips[i]
		if byPattern[t.Pattern] == nil {
			byPattern[t.Pattern] = map[string]bool{}
		}
		byPattern[t.Pattern][f.Line(t)] = true
	}
	out := map[int32][]string{}
	for p, lines := range byPattern {
		for _, s := range f.Patterns[p].Stops {
			for l := range lines {
				if !slices.Contains(out[s], l) {
					out[s] = append(out[s], l)
				}
			}
		}
	}
	for _, l := range out {
		sort.Strings(l)
	}
	return out
}

// boardPos is where a vehicle is on a stop pattern, recorded with each poll. trip is the
// matched trip, or for an unmatched vehicle the most common trip of its shape (pattern and
// headsign only). ok is false when the vehicle could not be placed.
type boardPos struct {
	g       *geo.Geometry
	trip    int32
	along   float64
	next    int  // index in the pattern of the first stop ahead
	waiting bool // at the first stop, not left yet
	ok      bool
}

// boardPosOf places a matched vehicle by its match, an unmatched one by its position on the
// shape (s from motionOf, -1 when ambiguous) when its shape's stops fit the shape.
func boardPosOf(w *world, r *match.Result, shape int32, s float64, speed *float64) boardPos {
	f := w.feed
	if r.Matched() {
		g := w.matcher.Geometry(f.Trip(r.Trip))
		if g == nil {
			return boardPos{}
		}
		next := r.NextIndex
		if r.Waiting {
			next = 0
		}
		return boardPos{g: g, trip: r.Trip, along: r.Along, next: next, waiting: r.Waiting, ok: true}
	}
	if shape < 0 || s < 0 {
		return boardPos{}
	}
	trip := shapeTrip(f, r.Line, shape)
	if trip < 0 {
		return boardPos{}
	}
	g := w.matcher.Geometry(f.Trip(trip))
	if g == nil || !g.FromShape { // s is along the shape; a line through the stops is another scale
		return boardPos{}
	}
	next := sort.SearchFloat64s(g.StopAlong, s-boardAtStopM)
	if next == len(g.StopAlong) {
		return boardPos{}
	}
	waiting := next == 0 && math.Abs(s-g.StopAlong[0]) <= match.TerminalRadiusM && (speed == nil || *speed == 0)
	return boardPos{g: g, trip: trip, along: s, next: next, waiting: waiting, ok: true}
}

// shapeTrip is the first trip of the line's most common stop pattern on a shape (the static
// variant's stops, staticsite), -1 when the line has no trip on it.
func shapeTrip(f *gtfs.Feed, line string, shape int32) int32 {
	count, first := map[int32]int{}, map[int32]int32{}
	best := int32(-1)
	for _, ti := range f.TripsForLine(line) {
		t := &f.Trips[ti]
		if t.Shape != shape {
			continue
		}
		if count[t.Pattern] == 0 {
			first[t.Pattern] = ti
		}
		count[t.Pattern]++
		if best < 0 || count[t.Pattern] > count[f.Trips[best].Pattern] {
			best = first[t.Pattern]
		}
	}
	return best
}

func (a *App) handleStop(w http.ResponseWriter, r *http.Request) {
	now := a.now()
	wd := a.world()
	if wd == nil {
		writeJSON(w, http.StatusServiceUnavailable, minMaxAge, map[string]string{"error": "warming_up"})
		return
	}
	f := wd.feed
	s := f.StopIndex(r.PathValue("id"))
	if s < 0 {
		writeJSON(w, http.StatusNotFound, unknownMaxAge, map[string]string{"error": "unknown_stop"})
		return
	}
	lines := wd.stopLines[s]
	if lines == nil {
		lines = []string{}
	}
	arr := a.stopBoard(wd, s, lines, now)
	// Only the lines with a bus on the way are asked of OASA more often: a big stop has 16 lines.
	seen := map[string]bool{}
	for _, x := range arr {
		if x.Source == "live" && !seen[x.Line] {
			seen[x.Line] = true
			a.sched.Watch(x.Line)
		}
	}
	warming := a.boardWarming(wd, lines)
	ma := stopMaxAge
	if warming {
		ma = minMaxAge
	}
	st := f.Stops[s]
	writeJSON(w, http.StatusOK, ma, StopResponse{Stop: StopInfo{st.ID, st.Name, st.Lat, st.Lon, st.Code},
		Lines: lines, Arrivals: arr, Usual: a.usualV1(wd, s, lines, now), Warming: warming, UpdatedAt: now.Unix()})
}

// boardWarming: a line of the stop has no poll of this feed yet although it is polled now, or
// its route codes are not known yet (as /v1/lines/{id} answers warming_up).
func (a *App) boardWarming(w *world, lines []string) bool {
	for _, l := range lines {
		_, polled := a.sched.NextUpdate(l)
		a.mu.RLock()
		warming, d := a.warming[l], a.lines[l]
		a.mu.RUnlock()
		if warming || polled && (d == nil || d.feed != w.feed) {
			return true
		}
	}
	return false
}

// boardVehicle is one vehicle of a line's last poll, with its place.
type boardVehicle struct {
	line  string
	r     *match.Result
	pos   boardPos
	shift int64 // its run's shift to OASA's daily schedule (dailyShifts)
}

// stopBoard lists the buses on their way to the stop (each once, by its newest fix) and, while
// the timetable is valid, the trips that leave later and have no bus yet; soonest first, at
// most boardPerVariant per line and variant.
func (a *App) stopBoard(w *world, stop int32, lines []string, now time.Time) []StopArrival {
	out := []StopArrival{}
	per := map[[2]string]int{}
	for _, row := range a.stopRows(w, stop, lines, now, boardHorizon, nil) {
		k := [2]string{row.Line, row.Variant}
		if per[k] < boardPerVariant {
			per[k]++
			row.EtaS = int(max(0, row.At-now.Unix()))
			out = append(out, row)
		}
	}
	return out
}

// stopRows is every row of the stop board up to horizon ahead, soonest first, uncut. daily is
// the line's timetable for an expired GTFS, nil when there is none (/v1).
func (a *App) stopRows(w *world, stop int32, lines []string, now time.Time, horizon time.Duration,
	daily func(line string) (telematics.DailySchedule, bool)) []StopArrival {
	f := w.feed
	a.mu.RLock()
	// A bus that changed lines keeps its old line's result for a while: only its newest fix counts.
	type fix struct {
		at   time.Time
		line string
	}
	newest := map[string]fix{}
	for id, d := range a.lines {
		if d.feed != f {
			continue
		}
		for i := range d.results {
			r := &d.results[i]
			if n, ok := newest[r.VehicleID]; !ok || r.Time.After(n.at) || r.Time.Equal(n.at) && id < n.line {
				newest[r.VehicleID] = fix{r.Time, id}
			}
		}
	}
	var buses []boardVehicle
	running := map[[2]int64]bool{} // trip, service day of every matched bus of these lines
	for _, l := range lines {
		d := a.lines[l]
		if d == nil || d.feed != f {
			continue
		}
		for i := range d.results {
			r := &d.results[i]
			if newest[r.VehicleID].line != l || now.Sub(r.Time) >= staleFix {
				continue
			}
			if r.Matched() {
				running[[2]int64{int64(r.Trip), r.Day.Unix()}] = true
			}
			if i >= len(d.board) {
				continue
			}
			buses = append(buses, boardVehicle{l, r, d.board[i], d.shiftOf(i)})
		}
	}
	a.mu.RUnlock()

	tab := a.segs.Load()
	if tab.Feed() != worldVersion(w) {
		tab = nil
	}
	// An expired timetable still matches vehicles (PlanningTime), but its times are not OASA's:
	// the runs of a line come from OASA's daily schedule when the cache has it.
	valid := f.FeedEnd().IsZero() || !gtfs.Midnight(now).After(f.FeedEnd())
	var pending map[string][]dailyRun
	if !valid && daily != nil {
		pending = map[string][]dailyRun{}
		for _, l := range lines {
			if d, ok := daily(l); ok {
				pending[l] = dailyPending(f, l, d, now)
			}
		}
	}
	until := now.Add(horizon).Unix()
	var rows []StopArrival
	for _, b := range buses {
		var leave time.Time // a bus waiting at its start makes the next daily run of its shape
		if b.pos.waiting && b.r.Matched() {
			leave = takeRun(f, pending, b.line, f.Trip(b.r.Trip).Shape, now)
		}
		if row, ok := liveArrival(f, tab, stop, b, now, leave); ok && row.At <= until {
			rows = append(rows, row)
		}
	}
	if valid {
		for _, l := range lines {
			rows = append(rows, scheduleArrivals(f, l, stop, now, horizon, running)...)
		}
	}
	for _, l := range lines {
		rows = append(rows, dailyArrivals(f, l, pending[l], stop, now, horizon)...)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].At != rows[j].At {
			return rows[i].At < rows[j].At
		}
		if rows[i].Line != rows[j].Line {
			return rows[i].Line < rows[j].Line
		}
		return rows[i].Vehicle < rows[j].Vehicle
	})
	return rows
}

// liveArrival is the bus's next pass of the stop (not as the last stop of its pattern): from
// its fix, the time to drive there (boardTravel); a matched bus waiting at its first stop
// leaves at its scheduled departure, or now when that is past. An unmatched bus standing at its
// first stop has no time and is left out. Not past the stop by its last fix, it is never
// overdue: late, it is due now. leave, when not zero, is the departure of a waiting bus instead
// of its trip's (OASA's daily run on an expired GTFS).
func liveArrival(f *gtfs.Feed, tab *segspeed.Table, stop int32, b boardVehicle, now, leave time.Time) (StopArrival, bool) {
	p, r := b.pos, b.r
	if !p.ok {
		return StopArrival{}, false
	}
	t := f.Trip(p.trip)
	stops := f.Patterns[t.Pattern].Stops
	first := p.next // a stop just reached still counts: the bus is at it
	if first > 0 && p.along-p.g.StopAlong[first-1] <= boardAtStopM {
		first--
	}
	j := slices.Index(stops[first:], stop)
	if j < 0 || first+j == len(stops)-1 {
		return StopArrival{}, false
	}
	j += first
	from := p.along
	if p.waiting {
		from = p.g.StopAlong[0]
	}
	dist := max(0, p.g.StopAlong[j]-from)
	row := StopArrival{Line: b.line, Headsign: f.Headsign(t), Vehicle: r.VehicleID, PositionAt: r.Time.Unix(),
		Source: "live", StopsAway: ptr(j - first + 1), DistanceM: ptr(int(math.Round(dist)))}
	if t.Shape >= 0 {
		row.Variant = f.Shapes[t.Shape].ID
	}
	start, timetable := r.Time, (*gtfs.Trip)(nil)
	if r.Matched() {
		row.TripID, row.day = ptr(t.ID), r.Day
		row.Scheduled = ptr(r.Day.Unix() + int64(f.StopTime(t, j).Arr) + b.shift)
		row.DelayS = r.Delay - int(b.shift)
		timetable = t
		if p.waiting {
			dep := r.Day.Add(time.Duration(f.Start(t)) * time.Second)
			if !leave.IsZero() {
				dep = leave
				row.Scheduled, row.DelayS = ptr(leave.Unix()+int64(f.StopTime(t, j).Arr-f.Start(t))), 0
			}
			start = maxTime(dep, now)
		}
	} else if p.waiting {
		return StopArrival{}, false // departure unknown
	}
	secs := boardTravel(f, p.g, f.ShapeID(t), tab, segspeed.Slot(start, gtfs.Athens), timetable, from, p.g.StopAlong[j])
	row.At = max(start.Unix()+int64(math.Round(secs)), now.Unix())
	return row, true
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// boardTravel is the seconds to drive from `from` to `to` along g: per 200 m bin of the shape
// the typical speed where the history has one (a FromShape geometry; at least boardMinBinMS),
// elsewhere the timetable's running time over that stretch (trip, nil for none), or
// boardDefaultMS. Waits at stops and lights are in both.
func boardTravel(f *gtfs.Feed, g *geo.Geometry, shape string, tab *segspeed.Table, slot int, trip *gtfs.Trip, from, to float64) float64 {
	if !g.FromShape {
		tab = nil
	}
	if trip != nil && f.NumStops(trip) < 2 {
		trip = nil
	}
	secs := 0.0
	for x := from; x < to; {
		bin := int(math.Floor(x / segspeed.BinM))
		end := min(to, float64(bin+1)*segspeed.BinM)
		if v, ok := tab.Speed(shape, bin, slot); ok {
			secs += (end - x) / max(v, boardMinBinMS)
		} else if trip != nil {
			secs += schedAt(f, g, trip, end, end == to) - schedAt(f, g, trip, x, false)
		} else {
			secs += (end - x) / boardDefaultMS
		}
		x = end
	}
	return secs
}

// schedAt is the trip's scheduled time (seconds of its service day) at a point along g, between
// the stops' times as the matcher measures delay. At a stop it is the departure, or with arrive
// the arrival: the wait at the stop asked for is not driving time to it.
func schedAt(f *gtfs.Feed, g *geo.Geometry, t *gtfs.Trip, along float64, arrive bool) float64 {
	p := g.Progress(along)
	if k := int(p); arrive && float64(k) == p && k > 0 {
		return float64(f.StopTime(t, k).Arr)
	}
	seg := min(int(p), f.NumStops(t)-2)
	a, b := f.StopTime(t, seg), f.StopTime(t, seg+1)
	return float64(a.Dep) + (p-float64(seg))*float64(b.Arr-a.Dep)
}

// scheduleArrivals is the line's trips that have not left their first stop yet, pass the stop
// within horizon (the first pass, not as the last stop) and no matched bus runs.
func scheduleArrivals(f *gtfs.Feed, line string, stop int32, now time.Time, horizon time.Duration, running map[[2]int64]bool) []StopArrival {
	var out []StopArrival
	// Asked at the horizon, the candidates are the trips of its service day and the day before
	// that started by then: at 23:50 tomorrow's trips from 00:00 too. Those already gone are
	// skipped below.
	for _, c := range f.CandidateTrips(line, now.Add(horizon), 0, 48*3600) {
		t := f.Trip(c.Trip)
		if running[[2]int64{int64(c.Trip), c.Day.Unix()}] || c.Day.Add(time.Duration(f.Start(t))*time.Second).Before(now) {
			continue
		}
		stops := f.Patterns[t.Pattern].Stops
		j := slices.Index(stops[:len(stops)-1], stop)
		if j < 0 {
			continue
		}
		at := c.Day.Unix() + int64(f.StopTime(t, j).Arr)
		if at > now.Add(horizon).Unix() {
			continue
		}
		row := StopArrival{Line: line, Headsign: f.Headsign(t), TripID: ptr(t.ID), At: at, Scheduled: ptr(at), Source: "schedule", day: c.Day}
		if t.Shape >= 0 {
			row.Variant = f.Shapes[t.Shape].ID
		}
		out = append(out, row)
	}
	return out
}

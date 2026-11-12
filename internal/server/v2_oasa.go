package server

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/model"
	"github.com/angelospk/athens-transit-rt/internal/quality"
	"github.com/angelospk/athens-transit-rt/internal/staticsite"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

// The oasa adapter: buses and trolleys from the OASA GTFS, the matcher's line polls and the
// stop boards. Its data is the /v1 data in the /v2 shape; ids are "oasa:" + the /v1 id.

const (
	oasaNS         = "oasa"
	v2Horizon      = 2 * time.Hour // Arrivals up to this far ahead
	v2PerHeadsign  = 4             // Arrivals per Line and headsign
	v2MaxArrivals  = 30
	oasaPrefix     = oasaNS + ":"
	oasaRunDayLeew = 36 * time.Hour // a run id's day: yesterday, today or tomorrow
)

type oasaAdapter struct{ a *App }

func (o *oasaAdapter) Namespace() string { return oasaNS }

func (o *oasaAdapter) Resolve(kind Kind, id string) string {
	if kind == KindLine {
		return oasaPrefix + o.a.canonLine(strings.TrimPrefix(id, oasaPrefix))
	}
	return id
}

// State: warming up until the feed loads and the first polls answer, stale when no poll has
// succeeded for 5 minutes (as /v1/status ok:false), indicative on an expired GTFS. Never down:
// the static feed always stands in.
func (o *oasaAdapter) State(now time.Time) model.SourceState {
	return o.state(now, nil)
}

// StateOf is State for the lines of one request: on an expired GTFS only a line without a daily
// schedule of today cached is indicative; the others still have OASA's timetable.
func (o *oasaAdapter) StateOf(now time.Time, lines []string) model.SourceState {
	return o.state(now, lines)
}

func (o *oasaAdapter) state(now time.Time, lines []string) model.SourceState {
	a := o.a
	s := model.SourceState{ID: oasaNS, State: model.OK}
	a.mu.RLock()
	w, lastOK, lastPublish := a.w, a.lastOK, a.lastPublish
	a.mu.RUnlock()
	if !lastPublish.IsZero() {
		s.UpdatedAt = model.Ptr(lastPublish.Unix())
	}
	ok := w != nil && a.Status().OK
	switch {
	case !ok && lastOK.IsZero():
		s.State = model.WarmingUp
	case !ok:
		s.State = model.Stale
	case expired(w.feed, now) && !a.dailyToday(now, lines):
		s.State = model.Indicative
	}
	return s
}

// dailyToday tells whether OASA's daily schedule of today is cached for every line id of the
// request, or for some line of the feed when the request names none.
func (a *App) dailyToday(now time.Time, lines []string) bool {
	today := gtfs.Midnight(now)
	if len(lines) == 0 {
		return a.anyDailyCached(today)
	}
	for _, id := range lines {
		if _, ok := a.dailyCached(strings.TrimPrefix(id, oasaPrefix), today); !ok {
			return false
		}
	}
	return true
}

// expired: the feed's calendar ended before today (its times then are not OASA's).
func expired(f *gtfs.Feed, now time.Time) bool {
	end := f.FeedEnd()
	return !end.IsZero() && gtfs.Midnight(now).After(end)
}

// modes is the mode of every line of the world's feed, built once per feed.
func (w *world) lineModes() map[string]string {
	w.modesOnce.Do(func() {
		w.modes = map[string]string{}
		for _, l := range w.feed.Lines() {
			w.modes[l] = staticsite.LineInfo(w.feed, nil, l).Kind
		}
	})
	return w.modes
}

func (o *oasaAdapter) lineModel(w *world, line string) model.Line {
	li := staticsite.LineInfo(w.feed, o.a.meta.Snapshot().Lines, line)
	out := model.Line{ID: oasaPrefix + line, Mode: li.Kind, Label: model.Ptr(line), Name: li.Name,
		Color: li.Color, TextColor: li.TextColor, Source: oasaNS}
	if li.NameEn != "" {
		out.NameEn = model.Ptr(li.NameEn)
	}
	return out
}

func (o *oasaAdapter) Lines() ([]model.Line, error) {
	w := o.a.world()
	if w == nil {
		return nil, errWarming
	}
	out := []model.Line{}
	for _, l := range w.feed.Lines() {
		out = append(out, o.lineModel(w, l))
	}
	return out, nil
}

// line is the feed and native id of a line id the feed has.
func (o *oasaAdapter) line(id string) (*world, string, error) {
	w := o.a.world()
	if w == nil {
		return nil, "", errWarming
	}
	native, ok := strings.CutPrefix(id, oasaPrefix)
	if !ok || len(w.feed.TripsForLine(native)) == 0 {
		return nil, "", errUnknown
	}
	return w, native, nil
}

func (o *oasaAdapter) LineInfo(id string) (*model.Line, error) {
	w, native, err := o.line(id)
	if err != nil {
		return nil, err
	}
	l := o.lineModel(w, native)
	return &l, nil
}

func (o *oasaAdapter) Line(id string) (*LineDetail, error) {
	w, native, err := o.line(id)
	if err != nil {
		return nil, err
	}
	lf := staticsite.LineFileOf(w.feed, native)
	dirs := w.shapeDirs(native)
	d := &LineDetail{Line: o.lineModel(w, native), Patterns: []model.Pattern{}, Stops: map[string]model.Stop{}}
	for shape, v := range lf.Variants {
		// The GTFS has no direction_id: the shape's way round gives the direction (v.Direction is
		// the GTFS 0, kept only for a shape the direction map does not know).
		dir := v.Direction
		if got, ok := dirs[w.feed.ShapeIndex(shape)]; ok {
			dir = got
		}
		p := model.Pattern{ID: oasaPrefix + shape, Headsign: v.Headsign, Direction: dir, Shape: v.Shape,
			Stops: make([]string, len(v.Stops))}
		for i, s := range v.Stops {
			p.Stops[i] = oasaPrefix + s
			if _, ok := d.Stops[p.Stops[i]]; !ok {
				d.Stops[p.Stops[i]] = o.stopModel(w, w.feed.StopIndex(s))
			}
		}
		d.Patterns = append(d.Patterns, p)
	}
	sort.Slice(d.Patterns, func(i, j int) bool { return d.Patterns[i].ID < d.Patterns[j].ID })
	return d, nil
}

func (o *oasaAdapter) stopModel(w *world, i int32) model.Stop {
	st := &w.feed.Stops[i]
	modes := w.lineModes()
	lines := append([]string(nil), w.stopLines[i]...)
	sortLines(lines, func(l string) (string, string) { return modes[l], l })
	out := model.Stop{ID: oasaPrefix + st.ID, Name: st.Name, Code: st.Code, Lat: round5(st.Lat), Lon: round5(st.Lon),
		Modes: []string{}, Lines: make([]string, len(lines)), Source: oasaNS}
	seen := map[string]bool{}
	for k, l := range lines {
		out.Lines[k] = oasaPrefix + l
		if m := modes[l]; !seen[m] {
			seen[m] = true
			out.Modes = append(out.Modes, m)
		}
	}
	sort.Strings(out.Modes)
	return out
}

// Stops are the stops some line calls at.
func (o *oasaAdapter) Stops() ([]model.Stop, error) {
	w := o.a.world()
	if w == nil {
		return nil, errWarming
	}
	out := []model.Stop{}
	for i := range w.feed.Stops {
		if len(w.stopLines[int32(i)]) > 0 {
			out = append(out, o.stopModel(w, int32(i)))
		}
	}
	return out, nil
}

func (o *oasaAdapter) stop(id string) (*world, int32, error) {
	w := o.a.world()
	if w == nil {
		return nil, -1, errWarming
	}
	native, ok := strings.CutPrefix(id, oasaPrefix)
	i := w.feed.StopIndex(native)
	if !ok || i < 0 {
		return nil, -1, errUnknown
	}
	return w, i, nil
}

func (o *oasaAdapter) Stop(id string) (*model.Stop, error) {
	w, i, err := o.stop(id)
	if err != nil {
		return nil, err
	}
	s := o.stopModel(w, i)
	return &s, nil
}

// Arrivals are the stop board's rows (/v1/stops/{id}: buses on the way, then trips that have
// not left) and OASA's own predictions for the buses the board does not have (#22), 2 h ahead,
// at most 4 per line and headsign and 30 in all. The lines with a bus of ours on the way are
// polled more often, as for /v1.
func (o *oasaAdapter) Arrivals(_ context.Context, stopID string, now time.Time) ([]model.Arrival, error) {
	w, i, err := o.stop(stopID)
	if err != nil {
		return nil, err
	}
	var rows []model.Arrival
	ours := map[[2]string]bool{} // line, vehicle
	for _, row := range o.a.stopRows(w, i, w.stopLines[i], now, v2Horizon,
		func(l string) (telematics.DailySchedule, bool) { return o.a.dailyCached(l, gtfs.Midnight(now)) }) {
		ar := model.Arrival{Line: oasaPrefix + row.Line, Stop: stopID, Headsign: row.Headsign, At: row.At,
			ScheduledAt: row.Scheduled, Source: model.SourceTable}
		if row.TripID != nil {
			ar.Run = model.Ptr(model.RunID(oasaNS, *row.TripID, row.day, gtfs.Athens))
		}
		live := row.Source == "live"
		if live {
			ar.Source, ar.Vehicle, ar.VehicleLabel = model.SourceLive, model.Ptr(oasaPrefix+row.Vehicle), model.Ptr(row.Vehicle)
			if row.TripID != nil {
				ar.DelayS = model.Ptr(row.DelayS)
			}
			ours[[2]string{row.Line, row.Vehicle}] = true
		}
		ar.Status = model.StatusOf(live, ar.DelayS)
		rows = append(rows, ar)
	}
	rows = append(rows, o.predicted(w, i, stopID, now, ours)...)
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].At < rows[b].At })
	out := []model.Arrival{}
	per := map[[2]string]int{}
	watched := map[string]bool{}
	for _, ar := range rows {
		k := [2]string{ar.Line, ar.Headsign}
		if per[k] >= v2PerHeadsign {
			continue
		}
		per[k]++
		if line := strings.TrimPrefix(ar.Line, oasaPrefix); ar.VehicleLabel != nil && ours[[2]string{line, *ar.VehicleLabel}] && !watched[line] {
			watched[line] = true // a bus of ours on the way
			o.a.sched.Watch(line)
		}
		if out = append(out, ar); len(out) == v2MaxArrivals {
			break
		}
	}
	return out, nil
}

// predicted is OASA's own predictions for stop i (getStopArrivals) as live Arrivals, without the
// buses already in ours (line, vehicle) and route codes of no known line. It never waits for
// OASA: a stale cache starts one background call (shared with /v1/stops/{id}/arrivals, one per
// stop at a time, in the scheduler's queue) and the last good answer, up to staleArrivalsMax
// old, is used meanwhile.
func (o *oasaAdapter) predicted(w *world, i int32, stopID string, now time.Time, ours map[[2]string]bool) []model.Arrival {
	native := w.feed.Stops[i].ID
	o.a.refreshArrivals(native)
	e, ok := o.a.cachedArrivals(native, staleArrivalsMax)
	if !ok {
		return nil
	}
	lineOf := o.a.routeLines()
	var out []model.Arrival
	for _, p := range e.list {
		line := lineOf[p.RouteCode]
		if line == "" || ours[[2]string{line, p.VehCode}] {
			continue
		}
		ar := model.Arrival{Line: oasaPrefix + line, Stop: stopID, Headsign: routeHeadsign(w, line, p.RouteCode),
			At: max(e.at.Unix()+int64(p.Minutes)*60, now.Unix()), Source: model.SourceLive,
			Vehicle: model.Ptr(oasaPrefix + p.VehCode), VehicleLabel: model.Ptr(p.VehCode)}
		ar.Status = model.StatusOf(true, nil)
		if ar.At <= now.Add(v2Horizon).Unix() {
			out = append(out, ar)
		}
	}
	return out
}

// routeHeadsign is the headsign of the line's busiest shape of an OASA route code: from the
// RouteMapper's cached answer (refreshLines asks for every route code of the active lines), else
// the shape whose id is the route code; "" for none. It never makes the mapper call OASA.
func routeHeadsign(w *world, line, routeCode string) string {
	heads := lineShapes(w.feed, line)
	if shapes, ok := w.mapper.Cached(line, routeCode); ok {
		if s := bestShape(w.feed, shapes, line); s >= 0 {
			return heads[s]
		}
	}
	for s, head := range heads {
		if w.feed.Shapes[s].ID == routeCode {
			return head
		}
	}
	return ""
}

// vehicle is a /v1 vehicle of a line poll in the /v2 shape. The direction of the run is the
// direction of its shape (dirs), the GTFS direction_id being 0 on every OASA trip (rt#45).
func vehicleV2(f *gtfs.Feed, dirs map[int32]int, mode, line string, r *match.Result, v *Vehicle, b boardPos, q *quality.VehicleLevel) model.Vehicle {
	out := model.Vehicle{ID: oasaPrefix + v.ID, Line: oasaPrefix + line, Mode: mode, Label: model.Ptr(v.ID),
		Position: model.Position{Kind: model.GPS, Lat: v.Lat, Lon: v.Lon, Bearing: v.Bearing, At: v.PositionAt},
		DelayS:   v.DelayS, AtTerminus: v.AtTerminus, Source: oasaNS}
	if q != nil {
		out.Quality = qualityV2(*q)
	}
	dir := func(t *gtfs.Trip) int {
		if d, ok := dirs[t.Shape]; ok {
			return d
		}
		return int(t.Dir)
	}
	if r.Matched() {
		t := f.Trip(r.Trip)
		out.Run = model.Ptr(model.RunID(oasaNS, t.ID, r.Day, gtfs.Athens))
		out.Headsign = model.Ptr(f.Headsign(t))
		out.Direction = model.Ptr(dir(t))
	} else if b.ok {
		t := f.Trip(b.trip)
		out.Headsign = model.Ptr(f.Headsign(t))
		out.Direction = model.Ptr(dir(t))
	}
	if v.NextStopID != nil {
		out.NextStop = model.Ptr(oasaPrefix + *v.NextStopID)
	}
	if v.Path != nil || v.Speed != nil {
		out.Motion = &model.Motion{Path: v.Path, PathBeyond: v.PathBeyond, PathStops: v.PathStops, Speed: v.Speed,
			PathSpeeds: v.PathSpeeds}
	}
	return out
}

// LineVehicles is /v1/lines/{id} in the /v2 shape: the line is watched, the cache lasts until
// its next poll.
func (o *oasaAdapter) LineVehicles(id string, now time.Time) (*LineVehicles, error) {
	a := o.a
	w := a.world()
	if w == nil {
		return nil, errWarming
	}
	line := strings.TrimPrefix(id, oasaPrefix)
	a.mu.RLock()
	known := a.known[line]
	a.mu.RUnlock()
	if !known {
		return nil, errUnknown
	}
	a.sched.Watch(line)
	next, polled := a.sched.NextUpdate(line)
	a.mu.RLock()
	defer a.mu.RUnlock()
	warming, d := a.warming[line], a.lines[line]
	mode := w.lineModes()[line]
	conv := func() []model.Vehicle {
		out := make([]model.Vehicle, len(d.vehicles))
		for i := range d.vehicles {
			out[i] = vehicleV2(d.feed, d.dirs, mode, line, &d.results[i], &d.vehicles[i], d.board[i], d.level(i))
		}
		return out
	}
	switch {
	case warming:
		return nil, errWarming
	case !polled:
		lv := &LineVehicles{UpdatedAt: now.Unix(), NextUpdateAt: now.Unix() + inactiveMaxAge, MaxAge: inactiveMaxAge}
		if d != nil && now.Sub(d.updated) <= lineKeep {
			lv.UpdatedAt, lv.Vehicles = d.updated.Unix(), conv()
		}
		return lv, nil
	case d == nil:
		return nil, errWarming
	}
	return &LineVehicles{Vehicles: conv(), Segments: a.lineSegments(w, line, now), UpdatedAt: d.updated.Unix(), NextUpdateAt: max(next.Unix(), now.Unix()+minMaxAge),
		MaxAge: int(clamp(int64(next.Sub(now)/time.Second), minMaxAge, maxMaxAge))}, nil
}

// Vehicles is /v1/vehicles in the /v2 shape: every vehicle with a fix of the last 5 minutes on
// the current feed, once (newest fix wins), sorted by line and id.
func (o *oasaAdapter) Vehicles(now time.Time) ([]model.Vehicle, error) {
	a := o.a
	w := a.world()
	if w == nil {
		return nil, errWarming
	}
	modes := w.lineModes()
	a.mu.RLock()
	ids := make([]string, 0, len(a.lines))
	for id, d := range a.lines {
		if d.feed == w.feed {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := []model.Vehicle{}
	at := map[string]int{}
	for _, id := range ids {
		d := a.lines[id]
		for i := range d.vehicles {
			v := &d.vehicles[i]
			if now.Unix()-v.PositionAt > int64(vehicleMaxAge/time.Second) {
				continue
			}
			mv := vehicleV2(d.feed, d.dirs, modes[id], id, &d.results[i], v, d.board[i], d.level(i))
			mv.Position.Lat, mv.Position.Lon = round5(v.Lat), round5(v.Lon)
			if j, ok := at[v.ID]; !ok {
				at[v.ID] = len(out)
				out = append(out, mv)
			} else if v.PositionAt > out[j].Position.At {
				out[j] = mv
			}
		}
	}
	a.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Run is a trip of yesterday, today or tomorrow that runs that day: its stops at their
// timetable times, plus the delay of the bus matched to it (a fix of the last 5 minutes). The
// stops the bus has passed are marked; without a bus none is (no signal says it left). A run of
// OASA's daily schedule ("<trip>@<HHMM>") is the timetable of the trip that stands for it, no bus.
func (o *oasaAdapter) Run(id string, now time.Time) (*model.Run, error) {
	a := o.a
	w := a.world()
	if w == nil {
		return nil, errWarming
	}
	f := w.feed
	native, noon, ok := model.ParseRunID(oasaNS, id, gtfs.Athens)
	if !ok {
		return nil, errUnknown
	}
	day := gtfs.ServiceDay(noon)
	if d := noon.Sub(gtfs.ServiceDay(now).Add(12 * time.Hour)); d > oasaRunDayLeew || d < -oasaRunDayLeew {
		return nil, errUnknown
	}
	if trip, hhmm, daily := strings.Cut(native, "@"); daily {
		return o.dailyRun(f, id, trip, hhmm, day, now)
	}
	ti := f.TripIndex(native)
	if ti < 0 {
		return nil, errUnknown
	}
	t := f.Trip(ti)
	if !f.ServiceActive(t.Service, day) {
		return nil, errUnknown
	}
	line := f.Line(t)
	var bus *match.Result
	busShift := int64(0)
	a.mu.RLock()
	if d := a.lines[line]; d != nil && d.feed == f {
		for i := range d.results {
			r := d.results[i]
			if r.Matched() && r.Trip == ti && r.Day.Equal(day) && now.Sub(r.Time) < staleFix && (bus == nil || r.Time.After(bus.Time)) {
				bus, busShift = &r, d.shiftOf(i)
			}
		}
	}
	a.mu.RUnlock()
	run := &model.Run{ID: id, Line: oasaPrefix + line, Headsign: f.Headsign(t), Status: model.Scheduled,
		Stops: make([]model.RunStop, f.NumStops(t))}
	if t.Shape >= 0 {
		run.Pattern = model.Ptr(oasaPrefix + f.Shapes[t.Shape].ID)
	}
	if bus != nil {
		run.Vehicle, run.VehicleLabel = model.Ptr(oasaPrefix+bus.VehicleID), model.Ptr(bus.VehicleID)
	}
	// On an expired GTFS a bus waiting at its start leaves at the daily run it makes, as on the
	// stop board: the times shift to it, and the delay against the old trip does not apply.
	shift, delay := int64(0), int64(0)
	if bus != nil {
		// A bus that left on a daily run: the times move to it (dailyShifts).
		shift, delay = busShift, int64(bus.Delay)-busShift
		if bus.Waiting && expired(f, now) {
			if d, ok := a.dailyCached(line, gtfs.Midnight(now)); ok {
				pending := map[string][]dailyRun{line: dailyPending(f, line, d, now)}
				if leave := takeRun(f, pending, line, t.Shape, now); !leave.IsZero() {
					shift, delay = leave.Unix()-day.Unix()-int64(f.Start(t)), 0
				}
			}
		}
		run.Status = model.StatusOf(true, model.Ptr(int(delay)))
	}
	for k := range run.Stops {
		st := f.StopTime(t, k)
		sched := day.Unix() + int64(st.Arr) + shift
		rs := model.RunStop{Stop: oasaPrefix + f.Stops[st.Stop].ID, ScheduledAt: model.Ptr(sched), At: sched,
			Source: model.SourceTable}
		if bus != nil {
			rs.Source, rs.DelayS = model.SourceLive, model.Ptr(int(delay))
			rs.Passed = !bus.Waiting && k < bus.NextIndex
			rs.At = sched + delay
			if !rs.Passed {
				rs.At = max(rs.At, now.Unix())
			}
		}
		run.Stops[k] = rs
	}
	return run, nil
}

// dailyRun is a run of OASA's daily schedule id "<trip>@<HHMM>": the timetable of that trip,
// from today's departure plus the stops' offsets. The bus that makes it is not matched to the
// trip, so no vehicle is shown.
func (o *oasaAdapter) dailyRun(f *gtfs.Feed, id, trip, hhmm string, day, now time.Time) (*model.Run, error) {
	m, ok := dailyMinutes(hhmm)
	ti := f.TripIndex(trip)
	if ok && ti >= 0 && !day.Equal(gtfs.ServiceDay(now)) {
		// After midnight the daily cache of the day may already hold the next day's runs: the
		// planner's timetable keeps the service day's until 03:00 (contract §9).
		if run, found := o.a.plannedRun(id, now); found {
			return run, nil
		}
	}
	if !ok || ti < 0 || !day.Equal(gtfs.ServiceDay(now)) {
		return nil, errUnknown
	}
	t := f.Trip(ti)
	line := f.Line(t)
	d, cached := o.a.dailyCached(line, gtfs.Midnight(now))
	if !cached {
		return nil, errUnknown
	}
	dep, found := int64(0), false
	for _, r := range dailyRuns(f, line, gtfs.Midnight(now).Format(time.DateOnly), d, day) {
		if r.m == m && r.trip == ti {
			dep, found = dailyAt(gtfs.Midnight(now), m).Unix(), true
			break
		}
	}
	if !found {
		return nil, errUnknown
	}
	run := &model.Run{ID: id, Line: oasaPrefix + line, Headsign: f.Headsign(t), Status: model.Scheduled,
		Stops: make([]model.RunStop, f.NumStops(t))}
	if t.Shape >= 0 {
		run.Pattern = model.Ptr(oasaPrefix + f.Shapes[t.Shape].ID)
	}
	for k := range run.Stops {
		at := dep + int64(f.StopTime(t, k).Arr) - int64(f.Start(t))
		run.Stops[k] = model.RunStop{Stop: oasaPrefix + f.Stops[f.StopTime(t, k).Stop].ID, ScheduledAt: model.Ptr(at),
			At: at, Source: model.SourceTable}
	}
	return run, nil
}

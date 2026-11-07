package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/model"
	"github.com/angelospk/athens-transit-rt/internal/plan"
	"github.com/angelospk/athens-transit-rt/internal/rail"
)

// /v2/plan (docs/CONTRACT.md §9; design docs/superpowers/specs/2026-11-03-trip-planner.md): the
// adapters hand internal/plan one service day of every Source, read only from what the backend
// already holds (a build never calls an upstream), and the handler encodes its answer.

const (
	planSlots     = 2
	planWait      = 2 * time.Second
	planMaxAge    = 60
	planRebuild   = 10 * time.Minute // at most one rebuild for changed inputs this often
	planCheck     = time.Minute
	planDayStart  = 3 * time.Hour                // a service day runs 03:00 to 03:00
	planLateBuild = 4*time.Hour + 30*time.Minute // a new day is built by 04:30 whatever is cached
	planPastAfter = time.Hour                    // after may lie this far before now
)

// planner holds the Timetable of one service day and the slots of the running plans.
type planner struct {
	cur  atomic.Pointer[planSnap]
	sem  chan struct{}
	wait time.Duration

	buildMu sync.Mutex
}

type planSnap struct {
	tt    *plan.Timetable
	noon  time.Time // the service day's noon
	key   planKey
	built time.Time
}

// planKey tells whether the inputs of a build changed.
type planKey struct {
	feed   *gtfs.Feed
	daily  int // lines with a daily schedule of the service day cached
	full   int // of them, those not partial (a retry completed one)
	stasy  *rail.STASY
	boards [2]bool // the suburban boards of D and D+1 are cached (a refresh of one is no new input)
}

func newPlanner() *planner { return &planner{sem: make(chan struct{}, planSlots), wait: planWait} }

func (p *planner) snap() *planSnap { return p.cur.Load() }

// acquire takes a plan slot, waiting up to p.wait; false when none came free.
func (p *planner) acquire(ctx context.Context) bool {
	select {
	case p.sem <- struct{}{}:
		return true
	default:
	}
	t := time.NewTimer(p.wait)
	defer t.Stop()
	select {
	case p.sem <- struct{}{}:
		return true
	case <-t.C:
	case <-ctx.Done():
	}
	return false
}

func (p *planner) release() { <-p.sem }

// planNoon is the noon of the service day (03:00 to 03:00 Athens time) that holds now, by the
// same 03:00 instant as planBounds (03:00 happens twice when DST ends).
func planNoon(now time.Time) time.Time {
	l := now.In(gtfs.Athens)
	noon := time.Date(l.Year(), l.Month(), l.Day(), 12, 0, 0, 0, gtfs.Athens)
	if start, _ := planBounds(noon); now.Before(start) {
		noon = noon.AddDate(0, 0, -1)
	}
	return noon
}

// planBounds are the first and the last instants (exclusive) of the service day of noon.
func planBounds(noon time.Time) (time.Time, time.Time) {
	start := time.Date(noon.Year(), noon.Month(), noon.Day(), 3, 0, 0, 0, gtfs.Athens)
	next := noon.AddDate(0, 0, 1)
	return start, time.Date(next.Year(), next.Month(), next.Day(), 3, 0, 0, 0, gtfs.Athens)
}

// buildPlan builds the Timetable of the service day of now and installs it. A build that fails
// keeps the old one.
func (a *App) buildPlan(now time.Time) (err error) {
	a.planner.buildMu.Lock()
	defer a.planner.buildMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("plan build: %v\n%s", r, debug.Stack())
		}
	}()
	noon := planNoon(now)
	start := time.Now()
	in, key := a.planInput(noon)
	tt := plan.Build(in)
	a.planner.cur.Store(&planSnap{tt: tt, noon: noon, key: key, built: now})
	st := tt.Stats()
	a.log.Info("plan: timetable built", "day", in.Day, "stops", st.Stops, "trips", st.Trips, "routes", st.Routes,
		"transfers", st.Transfers, "skipped", st.Skipped, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// planLoop rebuilds the Timetable (design, Build: "When"): once a feed is loaded; for a new
// service day after 03:00 once its daily schedules are prefetched (or the GTFS covers it), and by
// 04:30 in any case; and at most once per 10 min when an input changed.
func (a *App) planLoop(ctx context.Context) {
	tick := time.NewTicker(planCheck)
	defer tick.Stop()
	for {
		a.planTick(a.now())
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (a *App) planTick(now time.Time) {
	w := a.world()
	if w == nil {
		return
	}
	s := a.planner.snap()
	noon := planNoon(now)
	var due bool
	switch {
	case s == nil:
		due = true
	case !s.noon.Equal(noon):
		start, _ := planBounds(noon)
		due = !expired(w.feed, noon) || a.prefetchedDay() == noon.Format(time.DateOnly) ||
			!now.Before(start.Add(planLateBuild-planDayStart))
	case !gtfs.Midnight(now).Equal(gtfs.Midnight(noon)):
		// Past midnight the caches move on to the next calendar day (a line's daily schedule, the
		// suburban board): a rebuild would lose the service day's runs, so none before 03:00,
		// except for the STASY timetable when the build lacks it (a start after midnight).
		due = s.key.stasy == nil && a.planKeyOf(noon).stasy != nil
	default:
		k := a.planKeyOf(noon)
		// A daily schedule lost (a failed retry) is no new input: the timetable keeps its runs.
		changed := k.feed != s.key.feed || k.stasy != s.key.stasy || k.boards != s.key.boards ||
			k.daily > s.key.daily || k.full > s.key.full
		due = changed && (now.Sub(s.built) >= planRebuild || k.stasy != nil && s.key.stasy == nil ||
			k.feed != s.key.feed)
	}
	if due {
		if err := a.buildPlan(now); err != nil {
			a.log.Error("plan: build failed, keeping the old timetable", "err", err)
		}
	}
}

func (a *App) prefetchedDay() string {
	if v, ok := a.prefetched.Load().(string); ok {
		return v
	}
	return ""
}

func (a *App) planKeyOf(noon time.Time) planKey {
	var k planKey
	if w := a.world(); w != nil {
		k.feed = w.feed
	}
	day := noon.Format(time.DateOnly)
	a.dailyMu.Lock()
	for _, m := range []map[string]dailyEntry{a.dailyCache, a.dailyPrev} {
		for _, e := range m {
			if e.day == day && e.ok {
				k.daily++
				if !e.partial {
					k.full++
				}
			}
		}
	}
	a.dailyMu.Unlock()
	if f, ok := a.stasy.Feed(); ok {
		k.stasy = f
	}
	if sub := a.subAdapter(); sub != nil {
		for i := range 2 {
			_, _, k.boards[i] = sub.board.CachedBoard(noon.AddDate(0, 0, i))
		}
	}
	return k
}

func (a *App) subAdapter() *proastiakosAdapter {
	for _, ad := range a.adapters {
		if s, ok := ad.(*proastiakosAdapter); ok {
			return s
		}
	}
	return nil
}

func (a *App) stasyAdapterOf() *stasyAdapter {
	for _, ad := range a.adapters {
		if s, ok := ad.(*stasyAdapter); ok {
			return s
		}
	}
	return nil
}

// planBuild collects one service day's Input.
type planBuild struct {
	in       *plan.Input
	ref      int64 // Unix seconds of the reference of D
	lo, hi   int32 // the window of first departures, seconds from ref
	stopIdx  map[string]int32
	shapeIdx map[*[2]float64]int32 // by the first point of a shared shape
}

func (b *planBuild) stop(id string, lat, lon float64, station bool) int32 {
	if i, ok := b.stopIdx[id]; ok {
		return i
	}
	i := int32(len(b.in.Stops))
	b.in.Stops = append(b.in.Stops, plan.Stop{ID: id, Lat: lat, Lon: lon, Station: station})
	b.stopIdx[id] = i
	return i
}

// shape is the index of a shape in the Input, added once; -1 for none.
func (b *planBuild) shape(pts [][2]float64) int32 {
	if len(pts) < 2 {
		return -1
	}
	if i, ok := b.shapeIdx[&pts[0]]; ok {
		return i
	}
	i := int32(len(b.in.Shapes))
	b.in.Shapes = append(b.in.Shapes, pts)
	b.shapeIdx[&pts[0]] = i
	return i
}

// rel is an instant as seconds from D's reference.
func (b *planBuild) rel(unix int64) int32 { return int32(unix - b.ref) }

// inWindow: a trip whose first departure is in [D 03:00, D+1 03:00).
func (b *planBuild) inWindow(t *plan.Trip) bool {
	d := t.Times[0].Dep
	return b.lo <= d && d < b.hi
}

// planInput is the Input of the service day of noon from every Source, and the key of its
// inputs.
func (a *App) planInput(noon time.Time) (*plan.Input, planKey) {
	key := a.planKeyOf(noon)
	ref := gtfs.ServiceDay(noon)
	start, end := planBounds(noon)
	b := &planBuild{in: &plan.Input{Day: noon.Format(time.DateOnly), Ref: ref}, ref: ref.Unix(),
		lo: int32(start.Unix() - ref.Unix()), hi: int32(end.Unix() - ref.Unix()),
		stopIdx: map[string]int32{}, shapeIdx: map[*[2]float64]int32{}}
	if w := a.world(); w != nil {
		a.planOASA(b, w, noon)
	}
	if s := a.stasyAdapterOf(); s != nil {
		if x, err := s.index(); err == nil {
			planSTASY(b, x, noon)
		}
	}
	if s := a.subAdapter(); s != nil {
		s.planTrips(b, noon)
	}
	return b.in, key
}

// planOASA: the GTFS trips of D and those of D+1 that start before 03:00 while the GTFS covers
// D; on an expired GTFS, the runs of OASA's daily schedule of D of every line with one cached.
func (a *App) planOASA(b *planBuild, w *world, noon time.Time) {
	f := w.feed
	stopOf := make([]int32, len(f.Stops))
	for i := range f.Stops {
		stopOf[i] = -1
		if len(w.stopLines[int32(i)]) > 0 {
			st := &f.Stops[i]
			stopOf[i] = b.stop(oasaPrefix+st.ID, round5(st.Lat), round5(st.Lon), false)
		}
	}
	modes := w.lineModes()
	trip := func(t *gtfs.Trip, run func() string, dep int64) {
		if d := b.rel(dep); d < b.lo || d >= b.hi { // dep is the first departure
			return
		}
		line := f.Line(t)
		pt := plan.Trip{Line: oasaPrefix + line, Mode: modes[line], Headsign: f.Headsign(t), Run: run(), Shape: -1,
			Times: make([]plan.StopTime, f.NumStops(t))}
		if t.Shape >= 0 {
			pt.Pattern = oasaPrefix + f.Shapes[t.Shape].ID
			pt.Shape = b.shape(f.Shapes[t.Shape].Points)
		}
		start := int64(f.Start(t))
		for k := range pt.Times {
			st := f.StopTime(t, k)
			pt.Times[k] = plan.StopTime{Stop: stopOf[st.Stop], Arr: b.rel(dep + int64(st.Arr) - start),
				Dep: b.rel(dep + int64(st.Dep) - start)}
		}
		b.in.Trips = append(b.in.Trips, pt)
	}
	if !expired(f, noon) {
		for _, day := range []time.Time{noon, noon.AddDate(0, 0, 1)} {
			ref := gtfs.ServiceDay(day)
			active := map[int32]bool{}
			for ti := range f.Trips {
				t := &f.Trips[ti]
				on, seen := active[t.Service]
				if !seen {
					on = f.ServiceActive(t.Service, ref)
					active[t.Service] = on
				}
				if on {
					trip(t, func() string { return model.RunID(oasaNS, t.ID, ref, gtfs.Athens) }, ref.Unix()+int64(f.Start(t)))
				}
			}
		}
		return
	}
	today, ref := gtfs.Midnight(noon), gtfs.ServiceDay(noon)
	date := today.Format(time.DateOnly)
	for _, line := range f.Lines() {
		d, ok := a.dailyCached(line, today)
		if !ok {
			continue
		}
		for _, r := range dailyRuns(f, line, date, d, ref) {
			t := f.Trip(r.trip)
			trip(t, func() string { return model.RunID(oasaNS, dailyNative(t, r.m), ref, gtfs.Athens) }, dailyAt(today, r.m).Unix())
		}
	}
}

// planSTASY: the trips of the timetable dates of D and D+1 (the same weekday stands in on an
// expired timetable), on the Stops and Patterns of the stasy adapter.
func planSTASY(b *planBuild, x *stasyIndex, noon time.Time) {
	f := x.f
	stopOf := make([]int32, len(x.stops))
	for k := range x.stops {
		m := &x.stops[k].m
		stopOf[k] = b.stop(m.ID, m.Lat, m.Lon, x.stops[k].m.Modes[0] == model.Metro)
	}
	for _, day := range []time.Time{noon, noon.AddDate(0, 0, 1)} {
		ref := gtfs.ServiceDay(day).Unix()
		for ti := range int32(f.NumTrips()) {
			if x.tripPat[ti] < 0 {
				continue
			}
			if runs, _ := f.ServiceOn(ti, day); !runs {
				continue
			}
			rows := x.tripStops(ti)
			if t0 := ref + int64(rows[0].T) - b.ref; t0 < int64(b.lo) || t0 >= int64(b.hi) {
				continue
			}
			id, route, head := f.TripInfo(ti)
			p := &x.patterns[route][x.tripPat[ti]]
			pt := plan.Trip{Line: x.lines[route].ID, Mode: x.modes[route], Headsign: head, Pattern: p.m.ID,
				Run: model.RunID(stasyNS, id, gtfs.ServiceDay(day), gtfs.Athens), Shape: b.shape(p.m.Shape),
				Times: make([]plan.StopTime, len(rows))}
			for k, r := range rows {
				pt.Times[k] = plan.StopTime{Stop: stopOf[x.stopOf[r.Stop]], Arr: b.rel(ref + int64(r.A)), Dep: b.rel(ref + int64(r.T))}
			}
			b.in.Trips = append(b.in.Trips, pt)
		}
	}
}

// planTrips: the trains of the suburban boards of D and D+1 that are already cached (never a
// fetch), each train once.
func (s *proastiakosAdapter) planTrips(b *planBuild, noon time.Time) {
	x := s.index()
	stopOf := make([]int32, len(x.stops))
	for k := range x.stops {
		m := &x.stops[k]
		stopOf[k] = b.stop(m.ID, m.Lat, m.Lon, true)
	}
	seen := map[string]bool{}
	for _, day := range []time.Time{noon, noon.AddDate(0, 0, 1)} {
		list, _, ok := s.board.CachedBoard(day)
		if !ok {
			continue
		}
		for i := range list {
			sc := &list[i]
			if sc.Status == "cancelled" {
				continue
			}
			c := s.lineOfSchedule(x, sc)
			if c < 0 {
				continue
			}
			pt := plan.Trip{Line: x.lines[c].ID, Mode: model.Suburban, Headsign: sc.Destination,
				Run: subRunID(sc.Train, day), Shape: -1}
			if dir := s.schedDir(x, c, sc); dir >= 0 {
				pt.Pattern = x.patterns[c][dir].ID
				pt.Shape = b.shape(x.patterns[c][dir].Shape)
			}
			for j := range sc.Stops {
				st := &sc.Stops[j]
				k, ok := s.stopOf(x, st.StationID)
				arr, dep := st.Arr, st.Dep
				if arr.IsZero() {
					arr = dep
				}
				if dep.IsZero() {
					dep = arr
				}
				if !ok || arr.IsZero() || st.Status == "cancelled" {
					continue
				}
				call := plan.StopTime{Stop: stopOf[k], Arr: b.rel(arr.Unix()), Dep: b.rel(dep.Unix())}
				if n := len(pt.Times); n > 0 && pt.Times[n-1].Stop == call.Stop { // one call per Stop
					pt.Times[n-1].Dep = call.Dep
					continue
				}
				pt.Times = append(pt.Times, call)
			}
			if len(pt.Times) < 2 || !b.inWindow(&pt) {
				continue
			}
			first := fmt.Sprint(sc.Train, "@", pt.Times[0].Dep)
			if seen[first] {
				continue
			}
			seen[first] = true
			b.in.Trips = append(b.in.Trips, pt)
		}
	}
}

// plannedRun is a Run of the planner's Timetable, while its service day lasts: the runs of a plan
// still open after midnight, when the Source answers only for the calendar day (contract §9).
func (a *App) plannedRun(id string, now time.Time) (*model.Run, bool) {
	s := a.planner.snap()
	if s == nil {
		return nil, false
	}
	if _, end := planBounds(s.noon); !now.Before(end) {
		return nil, false
	}
	info, ok := s.tt.Run(id)
	if !ok {
		return nil, false
	}
	run := &model.Run{ID: id, Line: info.Line, Headsign: info.Headsign, Status: model.Scheduled,
		Stops: make([]model.RunStop, len(info.Stops))}
	if info.Pattern != "" {
		run.Pattern = model.Ptr(info.Pattern)
	}
	last := len(info.Stops) - 1
	for k, st := range info.Stops {
		at := st.Dep
		if k == last || strings.HasPrefix(id, oasaPrefix) {
			at = st.Arr // OASA's runs show the arrival at every stop
		}
		run.Stops[k] = model.RunStop{Stop: st.Stop, ScheduledAt: model.Ptr(at), At: at, Source: model.SourceTable,
			Passed: at <= now.Unix() && !strings.HasPrefix(id, oasaPrefix)}
	}
	return run, true
}

// ---- Handler ----

type planPoint struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type planBody struct {
	ServiceDay  string                `json:"service_day"`
	From        planPoint             `json:"from"`
	To          planPoint             `json:"to"`
	After       int64                 `json:"after"`
	Last        bool                  `json:"last"`
	Itineraries []plan.Itinerary      `json:"itineraries"`
	WalkOnly    *plan.WalkOnly        `json:"walk_only"`
	Lines       map[string]model.Line `json:"lines"`
	Stops       map[string]model.Stop `json:"stops"`
	Sources     []model.SourceState   `json:"sources"`
}

// parsePoint reads "lat,lon".
func parsePoint(s string) (planPoint, bool) {
	la, lo, ok := strings.Cut(s, ",")
	if !ok {
		return planPoint{}, false
	}
	lat, err1 := strconv.ParseFloat(la, 64)
	lon, err2 := strconv.ParseFloat(lo, 64)
	if err1 != nil || err2 != nil || math.IsNaN(lat) || math.IsNaN(lon) || math.Abs(lat) > 90 || math.Abs(lon) > 180 {
		return planPoint{}, false
	}
	return planPoint{lat, lon}, true
}

func inAttica(p planPoint) bool {
	return p.Lat >= 37.6 && p.Lat <= 38.4 && p.Lon >= 23.2 && p.Lon <= 24.2
}

func planError(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusBadRequest, unknownMaxAge, map[string]string{"error": code})
}

func (a *App) handleV2Plan(w http.ResponseWriter, r *http.Request) {
	now := a.now()
	q := r.URL.Query()
	from, ok := parsePoint(q.Get("from"))
	if !ok {
		planError(w, "bad_from")
		return
	}
	to, ok := parsePoint(q.Get("to"))
	if !ok {
		planError(w, "bad_to")
		return
	}
	if !inAttica(from) || !inAttica(to) {
		planError(w, "outside_area")
		return
	}
	after := now.Unix()
	if s := q.Get("after"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		start, end := planBounds(planNoon(now))
		if err != nil || v >= end.Unix() || v < start.Unix() && v < now.Add(-planPastAfter).Unix() {
			planError(w, "bad_after")
			return
		}
		after = v
	}
	n := 3
	if s := q.Get("n"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 1 || v > plan.MaxN {
			planError(w, "bad_n")
			return
		}
		n = v
	}
	last := q.Get("last") == "true" || q.Get("last") == "1"
	if !a.planner.acquire(r.Context()) {
		w.Header().Set("Retry-After", "2")
		writeJSON(w, http.StatusTooManyRequests, 0, map[string]string{"error": "busy"})
		return
	}
	defer a.planner.release()
	s := a.planner.snap()
	noon := planNoon(now)
	if s != nil {
		noon = s.noon
	}
	src := a.planSources(now, noon)
	body := planBody{ServiceDay: noon.Format(time.DateOnly), From: from, To: to, After: after, Last: last,
		Itineraries: []plan.Itinerary{}, Lines: map[string]model.Line{}, Stops: map[string]model.Stop{}, Sources: src}
	maxAge := planMaxAge
	if s == nil || !model.Complete(src) {
		maxAge = v2IncompleteAge
	}
	pf, pt := plan.LatLon{Lat: from.Lat, Lon: from.Lon}, plan.LatLon{Lat: to.Lat, Lon: to.Lon}
	body.WalkOnly = plan.Walk(pf, pt)
	if s != nil {
		res := s.tt.Plan(plan.Query{From: pf, To: pt, After: after, Last: last, N: n})
		body.Itineraries = res.Itineraries
		for _, it := range res.Itineraries {
			for _, l := range it.Legs {
				if l.Kind == plan.KindRide {
					a.planLine(body.Lines, l.Line)
				}
				for _, p := range []plan.Place{l.From, l.To} {
					if p.Stop != nil {
						a.planStop(body.Stops, *p.Stop)
					}
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, maxAge, body)
}

func (a *App) planLine(m map[string]model.Line, id string) {
	if _, ok := m[id]; ok {
		return
	}
	if ad := a.adapter(id); ad != nil {
		if l, err := ad.LineInfo(id); err == nil {
			m[id] = *l
		}
	}
}

func (a *App) planStop(m map[string]model.Stop, id string) {
	if _, ok := m[id]; ok {
		return
	}
	if ad := a.adapter(id); ad != nil {
		if st, err := ad.Stop(id); err == nil {
			m[id] = *st
		}
	}
}

// planSources are the §4 states of every Source; oasa is warming_up while its GTFS is expired
// and no daily schedule of the service day is cached.
func (a *App) planSources(now, noon time.Time) []model.SourceState {
	src := a.states(now, a.adapters...)
	for i := range src {
		if src[i].ID != oasaNS {
			continue
		}
		if w := a.world(); w != nil && expired(w.feed, noon) && !a.anyDailyCached(gtfs.Midnight(noon)) {
			src[i].State = model.WarmingUp
		}
	}
	return src
}

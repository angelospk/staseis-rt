// Package match assigns live OASA vehicles to scheduled GTFS trips.
//
// The telematics API reports vehicles per route code with no trip identifier, so each
// vehicle is assigned to a scheduled trip by comparing where it is along the route with
// where each candidate trip should be at that moment. Positions along a trip are measured on
// the GTFS shape; trips whose shape is missing or does not fit their stops fall back to
// straight lines between consecutive stops.
//
// Port of upstream oasa_rt/matcher.py (Matcher, HungarianMatcher, MemoryMatcher).
package match

import (
	"math"
	"slices"
	"sort"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/geo"
	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/lsa"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

const (
	MaxOffRouteM        = 250 // farther than this from the trip geometry -> not on this trip
	TerminalRadiusM     = 100 // "waiting at the first stop"
	MinDelay, MaxDelay  = -10 * 60, 60 * 60
	StickyBonus         = 180 // prefer keeping last cycle's assignment (seconds of cost)
	WrongHeadingCost    = 300 // vehicle heading opposes the direction of the route there
	Infeasible          = 1e9 // Hungarian assignment: impossible pair
	Unmatched           = 1e6 // Hungarian assignment: vehicle left unmatched
	HistoryS            = 15 * 60
	InconsistentFixCost = 3600 // a past position that does not fit the trip at all
	RunGapS             = 10 * 60
	AnchorBonus         = 1800 // departure-anchored trip; outweighs any position-based cost
	DepartedM           = 150  // this far from the first stop counts as departed
	AnchorEarlyS        = 5 * 60
	AnchorLateS         = 15 * 60
	CandidateBefore     = 15 * 60 // candidate trips: scheduled run padded before the start
	CandidateAfter      = 60 * 60 // ... and after the end
	CommitAfter         = 3       // distinct GPS fixes on one trip before the memory matcher keeps it
	forgetAfter         = time.Hour
)

// Kind selects the assignment method.
type Kind int

const (
	Greedy    Kind = iota // cheapest pair first (upstream's live default)
	Hungarian             // minimum total cost per line
	Memory                // Hungarian + departure anchoring + position history
)

func ParseKind(s string) (Kind, bool) {
	switch s {
	case "greedy":
		return Greedy, true
	case "hungarian":
		return Hungarian, true
	case "memory":
		return Memory, true
	}
	return 0, false
}

// Obs is one getBusLocation row tagged with the line code it was polled under.
type Obs struct {
	telematics.Vehicle
	LineCode string
}

// Result is a vehicle with its matched trip, if any.
type Result struct {
	VehicleID string
	Line      string // line number, e.g. "040"
	RouteID   string // GTFS route_id (of the trip once matched)
	RouteCode string
	LineCode  string
	Lat, Lon  float64
	Bearing   float64
	Time      time.Time // GPS fix
	Trip      int32     // -1 when unmatched
	Day       time.Time // service day of Trip
	Delay     int       // seconds, positive = late
	NextIndex int       // index in the trip's stop times of the next stop
	Waiting   bool      // waiting at the first stop
	Along     float64   // metres along the trip's Geometry line (matched only)
	Why       string    // why it is unmatched, one of the Why* values ("" when matched)
}

// Why a vehicle is unmatched (rt#116). The first check that fails names it.
const (
	WhyNoShape   = "no_shape"  // no GTFS shape for the line and route code
	WhyNoTrip    = "no_trip"   // no trip of those shapes runs around this time
	WhyOffRoute  = "off_route" // farther than MaxOffRouteM from every candidate trip
	WhyTooEarly  = "too_early" // on the line, but earlier than MinDelay for every trip
	WhyTooLate   = "too_late"  // on the line, but later than MaxDelay for every trip
	WhyBackwards = "backwards" // only fits its last trip by driving backwards along it
	WhyTaken     = "taken"     // fits a trip, but the assignment gave it to another vehicle
)

func (r *Result) Matched() bool { return r.Trip >= 0 }

type candidate struct {
	cost, rawCost float64
	vid           string
	trip          int32
	day           time.Time
	delay         int
	nextIndex     int
	waiting       bool
	progress      float64
	along         float64
	sticky        bool
}

type tripDay struct {
	trip int32
	day  int64 // unix seconds of the service day's midnight
}

type prevMatch struct {
	trip     string // upstream compares trip ids only, whatever the day
	progress float64
	seen     time.Time
}

type geoKey struct{ shape, pattern int32 }

type fix struct {
	ts      time.Time
	pos     [2]float64
	heading float64
	hasHead bool
}

type track struct {
	routeCode      string
	fixes          []fix
	lastAtTerminal time.Time // zero while no departure is pending
	anchor         *tripDay
	seen           time.Time
	positions      map[posKey][]geo.Cand
}

type posKey struct {
	ts   int64
	line *geo.Polyline
}

// Matcher is not safe for concurrent use.
type Matcher struct {
	Feed      *gtfs.Feed
	Mapper    *RouteMapper
	Kind      Kind
	UseShapes bool

	previous   map[string]prevMatch
	geometry   map[geoKey]*geo.Geometry
	shapeLines map[int32]*geo.Polyline
	tracks     map[string]*track
	commits    map[string]*commit
	finished   map[string]map[tripDay]time.Time // vehicle -> trips it reached the end of -> when the mark expires
}

// commit is the trip a vehicle has held over its last fixes (memory matcher only). Once
// committed, the vehicle keeps that trip while it stays feasible, and no other vehicle gets it.
type commit struct {
	td        tripDay
	fixes     int
	lastFix   time.Time
	committed bool
}

func New(feed *gtfs.Feed, mapper *RouteMapper, kind Kind) *Matcher {
	return &Matcher{Feed: feed, Mapper: mapper, Kind: kind, UseShapes: true,
		previous: map[string]prevMatch{}, geometry: map[geoKey]*geo.Geometry{},
		shapeLines: map[int32]*geo.Polyline{}, tracks: map[string]*track{},
		commits: map[string]*commit{}, finished: map[string]map[tripDay]time.Time{}}
}

// Geometry returns the (cached) geometry of a trip's stop pattern, or nil when a stop is unknown.
func (m *Matcher) Geometry(t *gtfs.Trip) *geo.Geometry {
	k := geoKey{t.Shape, t.Pattern}
	if g, ok := m.geometry[k]; ok {
		return g
	}
	g := m.buildGeometry(t)
	m.geometry[k] = g
	return g
}

func (m *Matcher) stopXY(stop int32) [2]float64 {
	s := &m.Feed.Stops[stop]
	return geo.XY(s.Lat, s.Lon)
}

func (m *Matcher) buildGeometry(t *gtfs.Trip) *geo.Geometry {
	f := m.Feed
	pat := f.Patterns[t.Pattern].Stops
	stops := make([][2]float64, len(pat))
	for i, s := range pat {
		stops[i] = m.stopXY(s)
	}
	if m.UseShapes && t.Shape >= 0 {
		line := m.ShapeLine(t.Shape)
		if along := geo.SnapStops(line, stops); along != nil {
			return &geo.Geometry{Line: line, StopAlong: along, FromShape: true}
		}
	}
	line := geo.NewPolyline(stops)
	return &geo.Geometry{Line: line, StopAlong: slices.Clone(line.Cum), FromShape: false}
}

// ShapeLine is the (cached) projected line of a shape.
func (m *Matcher) ShapeLine(shape int32) *geo.Polyline {
	line := m.shapeLines[shape]
	if line == nil {
		pts := m.Feed.Shapes[shape].Points
		xy := make([][2]float64, len(pts))
		for i, p := range pts {
			xy[i] = geo.XY(p[0], p[1])
		}
		line = geo.NewPolyline(xy)
		m.shapeLines[shape] = line
	}
	return line
}

// routeIDFor is the GTFS route_id to report for a vehicle whose trip is unknown.
func (m *Matcher) routeIDFor(lineCode, line string) string {
	for _, ti := range m.Feed.TripsForLine(line) {
		r := &m.Feed.Routes[m.Feed.Trips[ti].Route]
		if r.ID == lineCode {
			return lineCode
		}
	}
	if trips := m.Feed.TripsForLine(line); len(trips) > 0 {
		return m.Feed.Routes[m.Feed.Trips[trips[0]].Route].ID
	}
	return lineCode
}

// MatchLine matches the vehicles of every route of one line number.
func (m *Matcher) MatchLine(line string, vehicles []Obs) []Result {
	order := []string{}
	byID := map[string]*Result{}
	for _, v := range vehicles {
		r := &Result{VehicleID: v.VehNo, Line: line, RouteID: m.routeIDFor(v.LineCode, line),
			RouteCode: v.RouteCode, LineCode: v.LineCode, Lat: v.Lat, Lon: v.Lon, Bearing: v.Heading,
			Time: v.Time, Trip: -1}
		if _, dup := byID[r.VehicleID]; !dup {
			order = append(order, r.VehicleID)
		}
		byID[r.VehicleID] = r
	}
	var cands []candidate
	for _, id := range order {
		cands = append(cands, m.candidates(line, byID[id])...)
	}
	if m.Kind == Memory {
		var now time.Time
		for _, r := range byID { // only the fixes kept, not dropped duplicates
			if r.Time.After(now) {
				now = r.Time
			}
		}
		cands = m.applyCommits(cands, byID, now)
	}
	progress := map[string]float64{}
	for _, c := range m.assign(cands) {
		r := byID[c.vid]
		r.Trip, r.Day, r.Delay, r.NextIndex, r.Waiting, r.Along = c.trip, c.day, c.delay, c.nextIndex, c.waiting, c.along
		r.RouteID = m.Feed.Routes[m.Feed.Trips[c.trip].Route].ID
		progress[c.vid] = c.progress
	}
	out := make([]Result, 0, len(order))
	for _, id := range order {
		r := byID[id]
		if !r.Matched() {
			r.Why = m.why(line, r)
		}
		if r.Matched() {
			m.previous[id] = prevMatch{m.Feed.Trips[r.Trip].ID, progress[id], r.Time}
		} else {
			delete(m.previous, id)
		}
		out = append(out, *r)
	}
	if m.Kind == Memory {
		m.updateCommits(out)
	}
	return out
}

// applyCommits drops the trips each vehicle already finished, keeps a committed vehicle on its
// trip when that trip is still feasible, and takes that trip away from the other vehicles.
// A committed trip without a feasible candidate (off route, delay out of range, moving
// backwards) releases the commit. A committed vehicle missing from this poll keeps its trip
// reserved for up to RunGapS after its last fix.
func (m *Matcher) applyCommits(cands []candidate, present map[string]*Result, now time.Time) []candidate {
	kept := cands[:0]
	for _, c := range cands {
		if _, done := m.finished[c.vid][tripDay{c.trip, c.day.Unix()}]; !done {
			kept = append(kept, c)
		}
	}
	reserved := map[tripDay]string{}
	for _, c := range kept {
		if cm := m.commits[c.vid]; cm != nil && cm.committed && cm.td == (tripDay{c.trip, c.day.Unix()}) {
			reserved[cm.td] = c.vid
		}
	}
	for vid, cm := range m.commits {
		if _, here := present[vid]; !here && cm.committed && now.Sub(cm.lastFix) <= RunGapS*time.Second {
			reserved[cm.td] = vid
		}
	}
	out := kept[:0]
	for _, c := range kept {
		td := tripDay{c.trip, c.day.Unix()}
		if owner, ok := reserved[td]; ok && owner != c.vid {
			continue // another vehicle runs this trip
		}
		if cm := m.commits[c.vid]; cm != nil && reserved[cm.td] == c.vid && td != cm.td {
			continue // this vehicle keeps its committed trip
		}
		out = append(out, c)
	}
	return out
}

// updateCommits counts how many distinct fixes each vehicle has spent on its trip, and marks a
// trip finished once the vehicle is within TerminalRadiusM of its last stop.
func (m *Matcher) updateCommits(results []Result) {
	for i := range results {
		r := &results[i]
		if !r.Matched() {
			delete(m.commits, r.VehicleID)
			continue
		}
		td := tripDay{r.Trip, r.Day.Unix()}
		cm := m.commits[r.VehicleID]
		switch {
		case cm == nil || cm.td != td:
			cm = &commit{td: td, fixes: 1, lastFix: r.Time}
			m.commits[r.VehicleID] = cm
		case r.Time.After(cm.lastFix):
			cm.fixes++
			cm.lastFix = r.Time
		}
		if cm.fixes >= CommitAfter {
			cm.committed = true
		}
		for id, other := range m.commits {
			if id != r.VehicleID && other.td == td {
				delete(m.commits, id) // the trip changed hands while that vehicle was away
			}
		}
		t := m.Feed.Trip(r.Trip)
		g := m.Geometry(t)
		last := m.stopXY(m.Feed.StopTime(t, m.Feed.NumStops(t)-1).Stop)
		if g.StopAlong[len(g.StopAlong)-1]-r.Along <= TerminalRadiusM &&
			geo.Dist(geo.XY(r.Lat, r.Lon), last) <= TerminalRadiusM {
			if m.finished[r.VehicleID] == nil {
				m.finished[r.VehicleID] = map[tripDay]time.Time{}
			}
			// Keep the mark while the trip can still be a candidate: a bus can arrive early.
			end := r.Day.Add(time.Duration(m.Feed.StopTime(t, m.Feed.NumStops(t)-1).Arr+CandidateAfter) * time.Second)
			if until := r.Time.Add(forgetAfter); until.After(end) {
				end = until
			}
			m.finished[r.VehicleID][td] = end
			delete(m.commits, r.VehicleID)
		}
	}
}

// Forget drops state of vehicles not seen for an hour (upstream keeps it forever).
func (m *Matcher) Forget(now time.Time) {
	for id, p := range m.previous {
		if now.Sub(p.seen) > forgetAfter {
			delete(m.previous, id)
		}
	}
	for id, t := range m.tracks {
		if now.Sub(t.seen) > forgetAfter {
			delete(m.tracks, id)
		}
	}
	for id, cm := range m.commits {
		if now.Sub(cm.lastFix) > forgetAfter {
			delete(m.commits, id)
		}
	}
	for id, done := range m.finished {
		for td, until := range done {
			if now.After(until) {
				delete(done, td)
			}
		}
		if len(done) == 0 {
			delete(m.finished, id)
		}
	}
}

// baseCandidates scores every trip the vehicle could be running from its current position.
func (m *Matcher) baseCandidates(line string, r *Result) []candidate {
	f := m.Feed
	pos := geo.XY(r.Lat, r.Lon)
	shapes := m.Mapper.ShapesFor(line, r.RouteCode)
	prev, hasPrev := m.previous[r.VehicleID]
	onLine := map[*geo.Polyline][]geo.Cand{}
	var out []candidate
	for _, ct := range m.shapeCandidates(line, r.Time, shapes) {
		t := f.Trip(ct.Trip)
		g := m.Geometry(t)
		if g == nil {
			continue
		}
		cands, ok := onLine[g.Line]
		if !ok {
			cands = g.Line.Candidates(pos[0], pos[1], MaxOffRouteM, r.Bearing, r.Bearing != 0)
			onLine[g.Line] = cands
		}
		sticky := hasPrev && t.ID == prev.trip
		var prevProgress *float64
		if sticky {
			prevProgress = &prev.progress
		}
		s, ok := m.score(t, ct.Day, g, cands, r.Time, pos, prevProgress, nil)
		if !ok {
			continue
		}
		cost := s.cost
		if sticky {
			cost -= StickyBonus
		}
		out = append(out, candidate{cost: cost, rawCost: s.cost, vid: r.VehicleID, trip: ct.Trip, day: ct.Day,
			delay: s.delay, nextIndex: s.nextIndex, waiting: s.waiting, progress: s.progress, along: s.along, sticky: sticky})
	}
	return out
}

// weekdayShifts is the order in which an expired feed tries other weekdays: nearest first.
var weekdayShifts = []int{1, -1, 2, -2, 3, -3}

// shapeCandidates returns the candidate trips around ts whose shape is in shapes. When the
// feed has expired and none runs on today's weekday, it takes them from the nearest weekday
// that has some: OASA changes which weekdays run a variant, and an expired feed cannot follow
// (rt#51: 309Β runs shape 5863 only Wed-Fri in the 2026-07-08 feed, but also on Tuesdays).
func (m *Matcher) shapeCandidates(line string, ts time.Time, shapes map[int32]bool) []gtfs.Candidate {
	f := m.Feed
	keep := func(cts []gtfs.Candidate) []gtfs.Candidate {
		out := cts[:0]
		for _, ct := range cts {
			if shapes[f.Trip(ct.Trip).Shape] {
				out = append(out, ct)
			}
		}
		return out
	}
	cts := keep(f.CandidateTrips(line, ts, CandidateBefore, CandidateAfter))
	if len(cts) > 0 || len(shapes) == 0 || !f.Expired(ts) {
		return cts
	}
	for _, shift := range weekdayShifts {
		if cts = keep(f.CandidateTripsShifted(line, ts, CandidateBefore, CandidateAfter, shift)); len(cts) > 0 {
			return cts
		}
	}
	return nil
}

type scored struct {
	cost      float64
	delay     int
	nextIndex int
	waiting   bool
	progress  float64
	along     float64
}

// missed collects why score found no fit: the delays nearest to the allowed range and the
// positions skipped as backwards.
type missed struct {
	early, late float64 // seconds outside [MinDelay, MaxDelay], smallest seen (0 = none)
	backwards   int
}

// score is the cheapest way the vehicle at pos at ts can be running trip t. A non-nil miss
// records the positions it rejected.
func (m *Matcher) score(t *gtfs.Trip, midnight time.Time, g *geo.Geometry, positions []geo.Cand,
	ts time.Time, pos [2]float64, prevProgress *float64, miss *missed) (scored, bool) {
	f := m.Feed
	secs := ts.Sub(midnight).Seconds()
	n := f.NumStops(t)
	start := float64(f.Start(t))
	var best scored
	found := false
	for _, c := range positions {
		progress := g.Progress(c.Along)
		if prevProgress != nil && progress < *prevProgress-1 {
			if miss != nil {
				miss.backwards++
			}
			continue // buses do not drive backwards along their route
		}
		seg := int(progress)
		if seg >= n-1 {
			seg = n - 2
		}
		frac := progress - float64(seg)
		stop, next := f.StopTime(t, seg), f.StopTime(t, seg+1)
		waiting := seg == 0 && secs < start && geo.Dist(pos, m.stopXY(stop.Stop)) <= TerminalRadiusM
		var delay, cost float64
		if waiting {
			delay, cost = 0, (start-secs)*0.5
		} else {
			delay = secs - (float64(stop.Dep) + frac*float64(next.Arr-stop.Dep))
			if delay < MinDelay || delay > MaxDelay {
				if miss != nil {
					miss.note(delay)
				}
				continue
			}
			// Buses run late far more often than early.
			if delay >= 0 {
				cost = delay
			} else {
				cost = -3 * delay
			}
		}
		if c.Wrong {
			cost += WrongHeadingCost
		}
		if !found || cost < best.cost {
			best = scored{cost, int(math.RoundToEven(delay)), seg + 1, waiting, progress, c.Along}
			found = true
		}
	}
	return best, found
}

func (ms *missed) note(delay float64) {
	if delay < MinDelay {
		if d := MinDelay - delay; ms.early == 0 || d < ms.early {
			ms.early = d
		}
	} else if d := delay - MaxDelay; ms.late == 0 || d < ms.late {
		ms.late = d
	}
}

// why names the first check that keeps an unmatched vehicle off every trip. It repeats the
// checks of baseCandidates, only for unmatched vehicles, so the matching path stays as it was.
func (m *Matcher) why(line string, r *Result) string {
	shapes := m.Mapper.ShapesFor(line, r.RouteCode)
	if len(shapes) == 0 {
		return WhyNoShape
	}
	cts := m.shapeCandidates(line, r.Time, shapes)
	if len(cts) == 0 {
		return WhyNoTrip
	}
	pos := geo.XY(r.Lat, r.Lon)
	prev, hasPrev := m.previous[r.VehicleID]
	onRoute := false
	var miss missed
	for _, ct := range cts {
		t := m.Feed.Trip(ct.Trip)
		g := m.Geometry(t)
		if g == nil {
			continue
		}
		cands := g.Line.Candidates(pos[0], pos[1], MaxOffRouteM, r.Bearing, r.Bearing != 0)
		if len(cands) == 0 {
			continue
		}
		onRoute = true
		var prevProgress *float64
		if hasPrev && t.ID == prev.trip {
			prevProgress = &prev.progress
		}
		if _, ok := m.score(t, ct.Day, g, cands, r.Time, pos, prevProgress, &miss); ok {
			return WhyTaken
		}
	}
	switch {
	case !onRoute:
		return WhyOffRoute
	case miss.early > 0 && (miss.late == 0 || miss.early <= miss.late):
		return WhyTooEarly
	case miss.late > 0:
		return WhyTooLate
	case miss.backwards > 0:
		return WhyBackwards
	}
	return WhyTaken
}

func (m *Matcher) candidates(line string, r *Result) []candidate {
	base := m.baseCandidates(line, r)
	if m.Kind != Memory {
		return base
	}
	tr := m.updateTrack(r, base)
	if len(base) == 0 {
		return base
	}
	out := make([]candidate, 0, len(base))
	for _, c := range base {
		t := m.Feed.Trip(c.trip)
		g := m.Geometry(t)
		costs := []float64{c.rawCost}
		for _, fx := range tr.fixes[:len(tr.fixes)-1] {
			k := posKey{fx.ts.Unix(), g.Line}
			cands, ok := tr.positions[k]
			if !ok {
				cands = g.Line.Candidates(fx.pos[0], fx.pos[1], MaxOffRouteM, fx.heading, fx.hasHead)
				tr.positions[k] = cands
			}
			if s, ok := m.score(t, c.day, g, cands, fx.ts, fx.pos, nil, nil); ok {
				costs = append(costs, s.cost)
			} else {
				costs = append(costs, InconsistentFixCost)
			}
		}
		cost := median(costs)
		if c.sticky {
			cost -= StickyBonus
		}
		if tr.anchor != nil && *tr.anchor == (tripDay{c.trip, c.day.Unix()}) {
			cost -= AnchorBonus
		}
		c.cost = cost
		out = append(out, c)
	}
	return out
}

func median(v []float64) float64 {
	s := slices.Clone(v)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// updateTrack keeps the vehicle's recent positions for this run and detects its departure.
func (m *Matcher) updateTrack(r *Result, base []candidate) *track {
	tr := m.tracks[r.VehicleID]
	pos := geo.XY(r.Lat, r.Lon)
	if tr == nil || tr.routeCode != r.RouteCode ||
		(len(tr.fixes) > 0 && r.Time.Sub(tr.fixes[len(tr.fixes)-1].ts).Seconds() > RunGapS) {
		tr = &track{routeCode: r.RouteCode, positions: map[posKey][]geo.Cand{}}
		m.tracks[r.VehicleID] = tr
		delete(m.commits, r.VehicleID) // a new run: another route code, or a long gap
	}
	tr.seen = r.Time
	if len(tr.fixes) == 0 || !tr.fixes[len(tr.fixes)-1].ts.Equal(r.Time) {
		tr.fixes = append(tr.fixes, fix{r.Time, pos, r.Bearing, r.Bearing != 0})
	}
	kept := tr.fixes[:0]
	for _, fx := range tr.fixes {
		if r.Time.Sub(fx.ts).Seconds() <= HistoryS {
			kept = append(kept, fx)
		}
	}
	tr.fixes = kept

	nearest, haveNear := math.Inf(1), false
	seen := map[int32]bool{}
	for _, c := range base {
		first := m.Feed.StopTime(m.Feed.Trip(c.trip), 0).Stop
		if seen[first] {
			continue
		}
		seen[first] = true
		nearest, haveNear = min(nearest, geo.Dist(pos, m.stopXY(first))), true
	}
	switch {
	case haveNear && nearest <= TerminalRadiusM:
		// At the first stop: a new run is about to start, so forget the previous one.
		tr.lastAtTerminal, tr.anchor = r.Time, nil
		tr.fixes = tr.fixes[len(tr.fixes)-1:]
	case !tr.lastAtTerminal.IsZero() && haveNear && nearest > DepartedM:
		departure := tr.lastAtTerminal.Add(r.Time.Sub(tr.lastAtTerminal) / 2)
		tr.anchor = m.anchorTrip(base, departure)
		tr.lastAtTerminal = time.Time{}
	}
	m.prunePositions(tr)
	return tr
}

// prunePositions drops cached projections of fixes that left the history window
// (upstream's cache grows without bound).
func (m *Matcher) prunePositions(tr *track) {
	if len(tr.fixes) == 0 {
		clear(tr.positions)
		return
	}
	oldest := tr.fixes[0].ts.Unix()
	for k := range tr.positions {
		if k.ts < oldest {
			delete(tr.positions, k)
		}
	}
}

// anchorTrip is the trip scheduled to leave closest to the observed departure (late is likelier).
func (m *Matcher) anchorTrip(base []candidate, departure time.Time) *tripDay {
	var best *tripDay
	bestScore := 0.0
	for _, c := range base {
		late := departure.Sub(c.day).Seconds() - float64(m.Feed.Start(m.Feed.Trip(c.trip)))
		if late < -AnchorEarlyS || late > AnchorLateS {
			continue
		}
		score := late
		if late < 0 {
			score = -3 * late
		}
		if best == nil || score < bestScore {
			best, bestScore = &tripDay{c.trip, c.day.Unix()}, score
		}
	}
	return best
}

// Anchored reports whether the vehicle's current match comes from an observed departure.
func (m *Matcher) Anchored(r *Result) bool {
	tr := m.tracks[r.VehicleID]
	return r.Matched() && tr != nil && tr.anchor != nil && *tr.anchor == (tripDay{r.Trip, r.Day.Unix()})
}

func (m *Matcher) assign(cands []candidate) []candidate {
	if m.Kind == Greedy {
		return m.assignGreedy(cands)
	}
	return m.assignHungarian(cands)
}

// assignGreedy: one-to-one, cheapest pair first.
func (m *Matcher) assignGreedy(cands []candidate) []candidate {
	sorted := slices.Clone(cands)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].cost < sorted[j].cost })
	var out []candidate
	usedTrips, usedVeh := map[tripDay]bool{}, map[string]bool{}
	for _, c := range sorted {
		k := tripDay{c.trip, c.day.Unix()}
		if usedVeh[c.vid] || usedTrips[k] {
			continue
		}
		usedTrips[k], usedVeh[c.vid] = true, true
		out = append(out, c)
	}
	return out
}

// assignHungarian minimises the total cost of a line at once, so one vehicle cannot take the
// only trip another vehicle could be running.
func (m *Matcher) assignHungarian(cands []candidate) []candidate {
	if len(cands) == 0 {
		return nil
	}
	vehSet, tripSet := map[string]bool{}, map[tripDay]bool{}
	for _, c := range cands {
		vehSet[c.vid] = true
		tripSet[tripDay{c.trip, c.day.Unix()}] = true
	}
	vehicles := make([]string, 0, len(vehSet))
	for v := range vehSet {
		vehicles = append(vehicles, v)
	}
	sort.Strings(vehicles)
	trips := make([]tripDay, 0, len(tripSet))
	for t := range tripSet {
		trips = append(trips, t)
	}
	// Upstream sorts (trip_id, service_date) tuples.
	sort.Slice(trips, func(i, j int) bool {
		a, b := m.Feed.Trips[trips[i].trip].ID, m.Feed.Trips[trips[j].trip].ID
		if a != b {
			return a < b
		}
		return trips[i].day < trips[j].day
	})
	vi, ti := map[string]int{}, map[tripDay]int{}
	for i, v := range vehicles {
		vi[v] = i
	}
	for j, t := range trips {
		ti[t] = j
	}
	n, k := len(vehicles), len(trips)
	// One column per trip plus one "unmatched" column per vehicle. Unmatched dwarfs every real
	// cost, so as many vehicles as possible are matched, then the total cost is minimised.
	cost := make([][]float64, n)
	for i := range cost {
		cost[i] = make([]float64, k+n)
		for j := range cost[i] {
			cost[i][j] = Infeasible
		}
		cost[i][k+i] = Unmatched
	}
	byCell := map[[2]int]candidate{}
	for _, c := range cands {
		i, j := vi[c.vid], ti[tripDay{c.trip, c.day.Unix()}]
		if c.cost < cost[i][j] {
			cost[i][j] = c.cost
			byCell[[2]int{i, j}] = c
		}
	}
	rows, cols, err := lsa.Solve(cost)
	if err != nil {
		return m.assignGreedy(cands) // cannot happen with finite costs; stay useful anyway
	}
	var out []candidate
	for x := range rows {
		if c, ok := byCell[[2]int{rows[x], cols[x]}]; ok {
			out = append(out, c)
		}
	}
	return out
}

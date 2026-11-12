package stats

import (
	"math"
	"sort"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/match"
)

const (
	// MinCoverage is the share of a trip's shape its fixes must span for the trip to count as run.
	MinCoverage = 0.5
	// TripSlack is how long before a trip's start or after its end a fix still counts for it.
	TripSlack = 30 * time.Minute
	// TripsAfter is how long past a day's end its last trips run: Inputs reach this far.
	TripsAfter = 3 * time.Hour
	// RunEarly and RunLate: a run of OASA's daily schedule takes a bus that left this early or
	// this late (DailyDir).
	RunEarly = 5 * time.Minute
	RunLate  = TripSlack
	// RunNearM: a fix this close to its trip's first stop (or before it) is a bus still at the stand.
	RunNearM = 300.0
	// RunBack: a bus this share of its route behind its previous fix started a new run (DailyDir).
	RunBack = 0.5

	// The plan a line's trips come from (schedule_source); a day with both is SourceMixed.
	SourceDaily     = "oasa_daily"
	SourceGTFS      = "gtfs"
	SourceMixed     = "mixed"
	SourceTimetable = "citybus_timetable" // the timetable of a citybus.gr city (staseis-rt#151)
)

// PlanTrip is one scheduled trip of a day. FromM and ToM are the metres along the shape of its
// first and last stop; they are equal when the trip has no usable shape.
type PlanTrip struct {
	ID    string  `json:"id"`
	Line  string  `json:"line"`
	Dir   int     `json:"dir"`   // GTFS direction_id
	Start int64   `json:"start"` // unix s
	End   int64   `json:"end"`
	FromM float64 `json:"from_m"`
	ToM   float64 `json:"to_m"`
}

// Plan is the schedule of one local day, from the GTFS feed version GTFS: the GTFS trips, and
// for the lines of Daily the runs of OASA's daily schedule instead (UseDaily).
type Plan struct {
	GTFS    string     `json:"gtfs"`
	Expired bool       `json:"expired"`          // the day lies outside the feed's validity
	Source  string     `json:"source,omitempty"` // where the GTFS trips come from; "" = SourceGTFS
	Trips   []PlanTrip `json:"trips"`
	Daily   []DailyDir `json:"daily,omitempty"`
	// GTFSHeadways: the headways also measure the GTFS trips, by line and direction_id (a city
	// without OASA's daily schedule: Thessaloniki, #164).
	GTFSHeadways bool `json:"gtfs_headways,omitempty"`
}

// Span is the metres along a trip's shape of its first and last stop; equal when the trip has
// no usable shape.
type Span struct {
	FromM float64 `json:"from_m"`
	ToM   float64 `json:"to_m"`
}

// DailyDir is one direction of a line in OASA's daily schedule: its runs that leave in the day
// (unix s, sorted), and the GTFS trips whose matched rows are buses on it, with their spans.
// Dir is 0 for the Go runs, 1 for the Come runs.
//
// Match rule: the matched rows of one vehicle on any of the Trips are one bus run, whatever trip
// id each row has: the matcher moves a bus between the trips of a direction at the stand and on
// the way (#131). A new run starts when the bus is more than RunBack of the route behind its
// previous fix (back at the first stop) or after TripSlack without a fix. A run left at its last
// fix within RunNearM of the first stop, or at its first fix when it has none (first seen
// mid-route). A fix before the run's first fix is dropped: a copy of an earlier run's. In time
// order, each run takes the first unused bus that left from RunEarly before to RunLate after
// it; a run without one is missed. The bus's coverage decides run or partial, as for a GTFS
// trip: each fix counts as its share (0 to 1) of its own trip's span, so trips on different
// shapes add up. A trip in two directions (a loop) pairs with neither. A direction without a
// trip of its own with a usable span (no GTFS shape ends where it does) has its runs without a
// bus unmeasured.
type DailyDir struct {
	Line   string          `json:"line"`
	Dir    int             `json:"dir"`
	Starts []int64         `json:"starts"`
	Trips  map[string]Span `json:"trips"`
}

// UseDaily plans the lines of dirs from OASA's daily schedule: their GTFS trips leave the plan.
func (p *Plan) UseDaily(dirs []DailyDir) {
	daily := map[string]bool{}
	for _, d := range dirs {
		daily[d.Line] = true
	}
	trips := p.Trips[:0:0]
	for _, t := range p.Trips {
		if !daily[t.Line] {
			trips = append(trips, t)
		}
	}
	p.Trips, p.Daily = trips, dirs
}

// SpanOf is the span of trip t of m's feed (Span).
func SpanOf(m *match.Matcher, t *gtfs.Trip) Span {
	if g := m.Geometry(t); g != nil && g.FromShape && len(g.StopAlong) > 0 {
		return Span{g.StopAlong[0], g.StopAlong[len(g.StopAlong)-1]}
	}
	return Span{}
}

// PlanFunc returns the plan of a local day (YYYY-MM-DD).
type PlanFunc func(date string) (*Plan, error)

// TripCount counts scheduled trips by what the history shows of them.
type TripCount struct {
	Planned    int `json:"planned"`
	Run        int `json:"run"`        // fixes cover at least MinCoverage of the shape
	Partial    int `json:"partial"`    // seen, but covering less
	Missed     int `json:"missed"`     // no fix at all
	Unmeasured int `json:"unmeasured"` // no usable shape to measure against
}

type TripLine struct {
	Line           string `json:"line"`
	ScheduleSource string `json:"schedule_source"` // SourceDaily or SourceGTFS
	TripCount
	MissedTrips []string `json:"missed_trips"`
}

type Trips struct {
	GTFS        string `json:"gtfs"`
	GTFSExpired bool   `json:"gtfs_expired"`
	// ScheduleSource is where the planned trips come from: SourceDaily, SourceGTFS or SourceMixed.
	ScheduleSource string  `json:"schedule_source"`
	MinCoverage    float64 `json:"min_coverage"`
	TripCount
	ByLine []TripLine `json:"by_line"`
}

// NewPlan lists the trips of feed f (of the given version) that start in a local day. Times past
// 24:00 belong to the previous service day, and on a spring-forward day the service day counts
// from 23:00 the evening before, so D-1, D and D+1 are read.
func NewPlan(f *gtfs.Feed, version, date string, loc *time.Location) (*Plan, error) {
	start, end, err := Bounds(date, loc)
	if err != nil {
		return nil, err
	}
	p := &Plan{GTFS: version, Trips: []PlanTrip{}}
	if fs, fe := f.FeedStart(), f.FeedEnd(); (!fs.IsZero() && date < fs.Format(dateLayout)) ||
		(!fe.IsZero() && date > fe.Format(dateLayout)) {
		p.Expired = true
	}
	noon := time.Date(start.Year(), start.Month(), start.Day(), 12, 0, 0, 0, loc)
	m := match.New(f, nil, match.Greedy)
	for _, sd := range []time.Time{gtfs.ServiceDay(noon.AddDate(0, 0, -1)), gtfs.ServiceDay(noon), gtfs.ServiceDay(noon.AddDate(0, 0, 1))} {
		for i := range f.Trips {
			t := &f.Trips[i]
			if !f.ServiceActive(t.Service, sd) {
				continue
			}
			s := sd.Add(time.Duration(f.Start(t)) * time.Second)
			if s.Before(start) || !s.Before(end) {
				continue
			}
			pt := PlanTrip{ID: t.ID, Line: f.Line(t), Dir: int(t.Dir), Start: s.Unix(),
				End: sd.Add(time.Duration(f.End(t)) * time.Second).Unix()}
			sp := SpanOf(m, t)
			pt.FromM, pt.ToM = sp.FromM, sp.ToM
			p.Trips = append(p.Trips, pt)
		}
	}
	sort.Slice(p.Trips, func(i, j int) bool {
		a, b := p.Trips[i], p.Trips[j]
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		return a.ID < b.ID
	})
	return p, nil
}

// tripCounter sums how the plan's trips ran from the matched history rows, one row at a time,
// so the rows need not be kept.
type tripCounter struct {
	plan          *Plan
	byID          map[string][]int
	state         []tripSeen
	dailyOf       map[string]int     // GTFS trip id -> index in plan.Daily (the first direction with it)
	cur           map[busKey]*busRun // the latest bus run of each vehicle on a daily direction
	buses         []*busRun          // all bus runs on the daily directions
	tripAt        map[dirVeh]tripAt  // GTFSHeadways: the last measured fix of a vehicle on a direction
	tripPass      []int64            // GTFSHeadways: the earliest pass of each plan trip (0: none)
	stamped, ours int                // rows of the day with a feed version, and those of the plan's
}

// dirVeh is a vehicle on the GTFS trips of one line and direction: the matcher can move a bus
// between them on the way (#131), so a pass is found across the switch.
type dirVeh struct {
	line string
	dir  int
	veh  string
}

// tripAt is the share of its trip's span at a vehicle's last measured fix, at time t.
type tripAt struct {
	at float64
	t  int64
}

type tripSeen struct {
	any, measured bool
	lo, hi        float64
}

// busKey is a vehicle on a daily direction.
type busKey struct {
	dir int
	veh string
}

// busRun is one run of a bus on a daily direction (DailyDir). seen is in shares of the trip
// spans, on Span{0, 1} (span) when one of its trips has a usable span.
type busRun struct {
	key               busKey
	seen              tripSeen
	span              Span
	first, last, near int64   // first and last fix; last fix near the start (0: none)
	at                float64 // share of the route at the last measured fix (-1: none)
	atT               int64   // time of that fix
	pass              int64   // when the bus passed HeadwayAt of the route (0: not seen)
}

// left is when the bus left its start (DailyDir).
func (b *busRun) left() int64 {
	if b.near != 0 {
		return b.near
	}
	return b.first
}

func newTripCounter(plan *Plan) *tripCounter {
	c := &tripCounter{plan: plan, byID: map[string][]int{}, state: make([]tripSeen, len(plan.Trips)),
		dailyOf: map[string]int{}, cur: map[busKey]*busRun{}}
	if plan.GTFSHeadways {
		c.tripAt, c.tripPass = map[dirVeh]tripAt{}, make([]int64, len(plan.Trips))
	}
	for i, pt := range plan.Trips {
		c.byID[pt.ID] = append(c.byID[pt.ID], i)
	}
	for i, d := range plan.Daily {
		for id := range d.Trips {
			if j, ok := c.dailyOf[id]; !ok {
				c.dailyOf[id] = i
			} else if j != i {
				c.dailyOf[id] = -1 // in two directions: its bus could be either run
			}
		}
	}
	return c
}

func (s *tripSeen) note(sm float64, sp Span) {
	s.any = true
	if sm < 0 {
		return
	}
	sm = min(max(sm, sp.FromM), sp.ToM)
	if !s.measured {
		s.lo, s.hi, s.measured = sm, sm, true
	} else {
		s.lo, s.hi = min(s.lo, sm), max(s.hi, sm)
	}
}

// add counts one matched row: fix time t, vehicle, trip id, feed version and metres along the
// shape (< 0 when unknown).
func (c *tripCounter) add(t int64, veh, trip, gtfsVer string, sm float64) {
	if gtfsVer != c.plan.GTFS {
		return
	}
	slack := int64(TripSlack / time.Second)
	for _, k := range c.byID[trip] {
		pt := &c.plan.Trips[k]
		if t < pt.Start-slack || t > pt.End+slack {
			continue
		}
		c.state[k].note(sm, Span{pt.FromM, pt.ToM})
		if c.tripPass != nil && sm >= 0 && pt.ToM > pt.FromM {
			share := min(max((sm-pt.FromM)/(pt.ToM-pt.FromM), 0), 1)
			kv := dirVeh{pt.Line, pt.Dir, veh}
			prev, ok := c.tripAt[kv]
			if ok && t <= prev.t {
				continue // a copy of an older row
			}
			if p, crossed := crossing(prev.at, prev.t, share, t); ok && crossed && (c.tripPass[k] == 0 || p < c.tripPass[k]) {
				c.tripPass[k] = p
			}
			c.tripAt[kv] = tripAt{share, t}
		}
	}
	if d, ok := c.dailyOf[trip]; ok && d >= 0 {
		sp := c.plan.Daily[d].Trips[trip]
		share := -1.0
		if sm >= 0 && sp.ToM > sp.FromM {
			share = min(max((sm-sp.FromM)/(sp.ToM-sp.FromM), 0), 1)
		}
		k := busKey{d, veh}
		b := c.cur[k]
		if b != nil && t < b.first {
			// The history writes a vehicle's fixes in time order (history.Writer drops one not
			// newer than its last), so an older row is a copy written again after a restart.
			return
		}
		if b == nil || t > b.last && (share >= 0 && share < b.at-RunBack || t-b.last > slack) {
			b = &busRun{key: k, first: t, last: t, at: -1}
			c.cur[k] = b
			c.buses = append(c.buses, b)
		}
		if t >= b.last && share >= 0 {
			if p, ok := crossing(b.at, b.atT, share, t); b.pass == 0 && ok {
				b.pass = p
			}
			b.at, b.atT = share, t
		}
		b.first, b.last = min(b.first, t), max(b.last, t)
		if sm >= 0 && sm-sp.FromM <= RunNearM {
			b.near = max(b.near, t)
		}
		if sp.ToM > sp.FromM {
			b.span = Span{0, 1}
		}
		b.seen.note(share, b.span)
	}
}

// crossing is when a bus at share at0 at time t0 (at0 < 0: no fix) and at share at1 at t1 passed
// HeadwayAt of the route, interpolated; false when it did not cross it between the two fixes or
// they are more than HeadwayMaxGap apart.
func crossing(at0 float64, t0 int64, at1 float64, t1 int64) (int64, bool) {
	if at0 < 0 || at0 >= HeadwayAt || at1 < HeadwayAt || t1 <= t0 || t1-t0 > int64(HeadwayMaxGap/time.Second) {
		return 0, false
	}
	return t0 + int64(math.Round(float64(t1-t0)*(HeadwayAt-at0)/(at1-at0))), true
}

// stamp counts one row of the day (matched or not) by the feed version it was written with.
func (c *tripCounter) stamp(gtfsVer string) {
	c.stamped++
	if gtfsVer == c.plan.GTFS {
		c.ours++
	}
}

// measurable tells whether daily direction i has a trip of its own (in no other direction) with
// a usable span: only then can a run without a bus be called missed.
func (c *tripCounter) measurable(i int) bool {
	for id, sp := range c.plan.Daily[i].Trips {
		if c.dailyOf[id] == i && sp.ToM > sp.FromM {
			return true
		}
	}
	return false
}

// count adds one planned trip, seen as s on span sp, to l; missed names it.
func (l *TripLine) count(s tripSeen, sp Span, missed string) {
	l.Planned++
	switch {
	case sp.ToM <= sp.FromM:
		l.Unmeasured++
	case !s.any:
		l.Missed++
		l.MissedTrips = append(l.MissedTrips, missed)
	case s.measured && (s.hi-s.lo)/(sp.ToM-sp.FromM) >= MinCoverage:
		l.Run++
	default:
		l.Partial++
	}
}

// result is the trips section; nil when the day's rows are mostly of another feed version.
func (c *tripCounter) result(loc *time.Location) *Trips {
	if c.ours == 0 || 2*c.ours < c.stamped {
		return nil
	}
	plan, state := c.plan, c.state
	out := &Trips{GTFS: plan.GTFS, GTFSExpired: plan.Expired, MinCoverage: MinCoverage}
	lines := map[string]*TripLine{}
	lineOf := func(id, source string) *TripLine {
		l := lines[id]
		if l == nil {
			l = &TripLine{Line: id, ScheduleSource: source}
			lines[id] = l
		}
		return l
	}
	source := plan.Source
	if source == "" {
		source = SourceGTFS
	}
	for k, pt := range plan.Trips {
		lineOf(pt.Line, source).count(state[k], Span{pt.FromM, pt.ToM}, pt.ID)
	}
	byDir := make([][]*busRun, len(plan.Daily))
	for _, b := range c.buses {
		byDir[b.key.dir] = append(byDir[b.key.dir], b)
	}
	early, late := int64(RunEarly/time.Second), int64(RunLate/time.Second)
	for i, d := range plan.Daily {
		l := lineOf(d.Line, SourceDaily)
		buses := byDir[i]
		sort.Slice(buses, func(i, j int) bool { // tie-break, so reruns agree
			a, b := buses[i], buses[j]
			if a.left() != b.left() {
				return a.left() < b.left()
			}
			if a.key.veh != b.key.veh {
				return a.key.veh < b.key.veh
			}
			return a.first < b.first
		})
		used := make([]bool, len(buses))
		name := [...]string{"go ", "come "}[min(max(d.Dir, 0), 1)]
		for _, start := range d.Starts {
			var s tripSeen
			var sp Span
			for k, b := range buses {
				if used[k] || b.left() < start-early {
					continue
				}
				if b.left() <= start+late {
					used[k], s, sp = true, b.seen, b.span
				}
				break
			}
			if !s.any && c.measurable(i) {
				sp = Span{0, 1} // no bus: missed, unless no trip could have measured it
			}
			l.count(s, sp, name+time.Unix(start, 0).In(loc).Format("15:04"))
		}
	}
	out.ByLine = make([]TripLine, 0, len(lines))
	sources := map[string]bool{}
	for _, l := range lines {
		out.Planned += l.Planned
		out.Run += l.Run
		out.Partial += l.Partial
		out.Missed += l.Missed
		out.Unmeasured += l.Unmeasured
		out.ByLine = append(out.ByLine, *l)
		sources[l.ScheduleSource] = true
	}
	switch {
	case sources[SourceDaily] && sources[source]:
		out.ScheduleSource = SourceMixed
	case sources[SourceDaily]:
		out.ScheduleSource = SourceDaily
	default:
		out.ScheduleSource = source
	}
	sort.Slice(out.ByLine, func(i, j int) bool { return out.ByLine[i].Line < out.ByLine[j].Line })
	return out
}

package stats

import (
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

	// The plan a line's trips come from (schedule_source); a day with both is SourceMixed.
	SourceDaily = "oasa_daily"
	SourceGTFS  = "gtfs"
	SourceMixed = "mixed"
)

// PlanTrip is one scheduled trip of a day. FromM and ToM are the metres along the shape of its
// first and last stop; they are equal when the trip has no usable shape.
type PlanTrip struct {
	ID    string  `json:"id"`
	Line  string  `json:"line"`
	Start int64   `json:"start"` // unix s
	End   int64   `json:"end"`
	FromM float64 `json:"from_m"`
	ToM   float64 `json:"to_m"`
}

// Plan is the schedule of one local day, from the GTFS feed version GTFS: the GTFS trips, and
// for the lines of Daily the runs of OASA's daily schedule instead (UseDaily).
type Plan struct {
	GTFS    string     `json:"gtfs"`
	Expired bool       `json:"expired"` // the day lies outside the feed's validity
	Trips   []PlanTrip `json:"trips"`
	Daily   []DailyDir `json:"daily,omitempty"`
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
// Match rule: the matched rows of one vehicle on one of the Trips are one bus run. It left at
// its last fix within RunNearM of the trip's first stop, or at its first fix when it
// has none (first seen mid-route). In time order, each run takes the first unused bus that left
// from RunEarly before to RunLate after it; a run without one is missed. The bus's coverage of
// its own trip's span decides run or partial, as for a GTFS trip. A trip in two directions (a
// loop) pairs with neither. A direction without a trip of its own with a usable span (no GTFS
// shape ends where it does) has its runs without a bus unmeasured.
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
			pt := PlanTrip{ID: t.ID, Line: f.Line(t), Start: s.Unix(),
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
	buses         map[busKey]*busRun // the bus runs on the daily directions
	stamped, ours int                // rows of the day with a feed version, and those of the plan's
}

type tripSeen struct {
	any, measured bool
	lo, hi        float64
}

// busKey is one bus run on a daily direction: the rows of a vehicle on a GTFS trip.
type busKey struct {
	dir       int
	veh, trip string
}

type busRun struct {
	key         busKey
	seen        tripSeen
	span        Span
	first, near int64 // first fix; last fix near the start (0: none)
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
		dailyOf: map[string]int{}, buses: map[busKey]*busRun{}}
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
	}
	if d, ok := c.dailyOf[trip]; ok && d >= 0 {
		k := busKey{d, veh, trip}
		b := c.buses[k]
		if b == nil {
			b = &busRun{key: k, span: c.plan.Daily[d].Trips[trip], first: t}
			c.buses[k] = b
		}
		b.first = min(b.first, t)
		if sm >= 0 && sm-b.span.FromM <= RunNearM {
			b.near = max(b.near, t)
		}
		b.seen.note(sm, b.span)
	}
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
	for k, pt := range plan.Trips {
		lineOf(pt.Line, SourceGTFS).count(state[k], Span{pt.FromM, pt.ToM}, pt.ID)
	}
	byDir := make([][]*busRun, len(plan.Daily))
	for k, b := range c.buses {
		byDir[k.dir] = append(byDir[k.dir], b)
	}
	early, late := int64(RunEarly/time.Second), int64(RunLate/time.Second)
	for i, d := range plan.Daily {
		l := lineOf(d.Line, SourceDaily)
		buses := byDir[i]
		sort.Slice(buses, func(i, j int) bool { // the map's order is random: tie-break, so reruns agree
			a, b := buses[i], buses[j]
			if a.left() != b.left() {
				return a.left() < b.left()
			}
			if a.key.veh != b.key.veh {
				return a.key.veh < b.key.veh
			}
			return a.key.trip < b.key.trip
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
	case sources[SourceDaily] && sources[SourceGTFS]:
		out.ScheduleSource = SourceMixed
	case sources[SourceDaily]:
		out.ScheduleSource = SourceDaily
	default:
		out.ScheduleSource = SourceGTFS
	}
	sort.Slice(out.ByLine, func(i, j int) bool { return out.ByLine[i].Line < out.ByLine[j].Line })
	return out
}

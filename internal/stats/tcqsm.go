package stats

// Two TCQSM 4th-edition measures of the citybus.gr cities (staseis-rt#167, feasibility in
// docs/research/tcqsm-other-cities.md): the frequency band of each line from the timetable
// (Exhibit 5-6), and the on-time performance at the route origin (Exhibit 5-8). A plan opts in
// to each (Plan.Frequency, Plan.RunDeparture); Athens and Thessaloniki do not.

import (
	"math"
	"sort"
	"time"
)

const (
	// FreqFromH and FreqToH: only departures in [from, to) local time count (daytime).
	FreqFromH, FreqToH = 7, 21
	// FreqMinTrips: a direction with fewer daytime departures has no frequency (school runs,
	// duplicate entries).
	FreqMinTrips = 4

	// OTPAtM: a run departs when it crosses this shape metre ("left the stand").
	OTPAtM = 100
	// OTPMaxGap: the two sightings around the crossing must be at most this far apart.
	OTPMaxGap = 60
	// OTPLateS: a departure 0 to this many seconds after the schedule is on time.
	OTPLateS = 300
	// OTPEarlyWindow and OTPLateWindow: a crossing further from the schedule is not the run's
	// departure (a bus that drove past the stand before its run, or a code kept too long).
	OTPEarlyWindow = 10 * 60
	OTPLateWindow  = 60 * 60
)

// Frequency is the scheduled frequency of each line and direction (TCQSM Exhibit 5-6).
type Frequency struct {
	FromH    int             `json:"from_h"`
	ToH      int             `json:"to_h"`
	MinTrips int             `json:"min_trips"`
	ByLine   []LineFrequency `json:"by_line"`
}

// LineFrequency: Trips departures in the window, their mean headway (last - first) / (Trips - 1)
// in minutes (one decimal), and its band.
type LineFrequency struct {
	Line       string  `json:"line"`
	Dir        int     `json:"dir"`
	Trips      int     `json:"trips"`
	HeadwayMin float64 `json:"headway_min"`
	Band       string  `json:"band"`
}

// FrequencyBand is the TCQSM band of a mean headway in minutes, rounded to whole minutes.
func FrequencyBand(min float64) string {
	m := math.Round(min)
	switch {
	case m <= 5:
		return "<=5"
	case m <= 10:
		return "6-10"
	case m <= 15:
		return "11-15"
	case m <= 30:
		return "16-30"
	case m <= 59:
		return "31-59"
	case m == 60:
		return "60"
	}
	return ">60"
}

// frequency is the plan's frequency on the local day starting at start; nil when no direction
// has FreqMinTrips departures in the window.
func frequency(p *Plan, start time.Time, loc *time.Location) *Frequency {
	from := time.Date(start.Year(), start.Month(), start.Day(), FreqFromH, 0, 0, 0, loc).Unix()
	to := time.Date(start.Year(), start.Month(), start.Day(), FreqToH, 0, 0, 0, loc).Unix()
	type dirKey struct {
		line string
		dir  int
	}
	deps := map[dirKey]map[int64]bool{}
	for _, t := range p.Trips {
		if t.Start < from || t.Start >= to {
			continue
		}
		k := dirKey{t.Line, t.Dir}
		if deps[k] == nil {
			deps[k] = map[int64]bool{}
		}
		deps[k][t.Start] = true
	}
	out := &Frequency{FromH: FreqFromH, ToH: FreqToH, MinTrips: FreqMinTrips, ByLine: []LineFrequency{}}
	for k, set := range deps {
		if len(set) < FreqMinTrips {
			continue
		}
		first, last := int64(math.MaxInt64), int64(math.MinInt64)
		for s := range set {
			first, last = min(first, s), max(last, s)
		}
		h := float64(last-first) / float64(len(set)-1) / 60
		out.ByLine = append(out.ByLine, LineFrequency{Line: k.line, Dir: k.dir, Trips: len(set),
			HeadwayMin: math.Round(h*10) / 10, Band: FrequencyBand(h)})
	}
	if len(out.ByLine) == 0 {
		return nil
	}
	sort.Slice(out.ByLine, func(i, j int) bool {
		a, b := out.ByLine[i], out.ByLine[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Dir < b.Dir
	})
	return out
}

// OTPCount counts runs: all of them, those whose departure was measured, and how those left.
// Share is OnTime / Measured (three decimals), absent when nothing was measured.
type OTPCount struct {
	Runs     int      `json:"runs"`
	Measured int      `json:"measured"`
	OnTime   int      `json:"on_time"`
	Early    int      `json:"early"`
	Late     int      `json:"late"`
	Share    *float64 `json:"share,omitempty"`
}

// LineOTP is the on-time performance of one line.
type LineOTP struct {
	Line string `json:"line"`
	OTPCount
}

// OriginOTP is the on-time performance at the route origin (TCQSM Exhibit 5-8): a departure 0 to
// LateS seconds after the schedule is on time; an early one is not. Band is the TCQSM band of
// the share, absent when nothing was measured.
type OriginOTP struct {
	AtM     int `json:"at_m"`
	MaxGapS int `json:"max_gap_s"`
	LateS   int `json:"late_s"`
	OTPCount
	Band   string    `json:"band,omitempty"`
	ByLine []LineOTP `json:"by_line"`
}

// OTPBand is the TCQSM band of onTime of n runs, in whole percent rounded down.
func OTPBand(onTime, n int) string {
	switch p := onTime * 100; {
	case p >= 95*n:
		return "95-100"
	case p >= 90*n:
		return "90-94"
	case p >= 80*n:
		return "80-89"
	case p >= 70*n:
		return "70-79"
	}
	return "<70"
}

type otpSample struct {
	t int64
	m float64
}

// otpRun keeps what one run needs to place its departure: its sightings before OTPAtM near
// the schedule (few: a standing bus writes a row a minute), and its first sighting past it.
type otpRun struct {
	line  string
	sched int64
	low   []otpSample
	hi    otpSample
	hasHi bool
}

// otpCounter collects the runs whose scheduled departure (dep of the vehicle code) is in
// [from, to). The rows may come in any order.
type otpCounter struct {
	dep      func(veh string) (int64, bool)
	from, to int64
	runs     map[string]*otpRun
	none     map[string]bool // vehicles that are no run of the day
}

func newOTPCounter(dep func(string) (int64, bool), from, to int64) *otpCounter {
	return &otpCounter{dep: dep, from: from, to: to, runs: map[string]*otpRun{}, none: map[string]bool{}}
}

// add takes one row: vehicle veh of line at time t, sm metres along its shape (< 0: unknown).
func (c *otpCounter) add(line, veh string, t int64, sm float64) {
	if c.none[veh] {
		return
	}
	r := c.runs[veh]
	if r == nil {
		s, ok := c.dep(veh)
		if !ok || s < c.from || s >= c.to {
			c.none[veh] = true
			return
		}
		r = &otpRun{line: line, sched: s}
		c.runs[veh] = r
	}
	if sm < 0 || t < r.sched-OTPEarlyWindow-OTPMaxGap || t > r.sched+OTPLateWindow {
		return
	}
	if sm < OTPAtM {
		r.low = append(r.low, otpSample{t, sm})
	} else if t >= r.sched-OTPEarlyWindow && (!r.hasHi || t < r.hi.t) {
		r.hi, r.hasHi = otpSample{t, sm}, true
	}
}

// delay is the run's departure minus its schedule (s); false when it was not measured.
func (r *otpRun) delay() (float64, bool) {
	if !r.hasHi {
		return 0, false
	}
	var lo otpSample
	found := false
	for _, s := range r.low {
		if s.t < r.hi.t && (!found || s.t > lo.t) {
			lo, found = s, true
		}
	}
	if !found || r.hi.t-lo.t > OTPMaxGap || r.hi.m <= lo.m {
		return 0, false
	}
	at := float64(lo.t) + (OTPAtM-lo.m)/(r.hi.m-lo.m)*float64(r.hi.t-lo.t)
	return at - float64(r.sched), true
}

func (n *OTPCount) count(d float64, ok bool) {
	n.Runs++
	if !ok {
		return
	}
	n.Measured++
	switch {
	case d < 0:
		n.Early++
	case d <= OTPLateS:
		n.OnTime++
	default:
		n.Late++
	}
}

func (n *OTPCount) share() {
	if n.Measured > 0 {
		s := round3(float64(n.OnTime) / float64(n.Measured))
		n.Share = &s
	}
}

// result is nil when no run was seen.
func (c *otpCounter) result() *OriginOTP {
	if len(c.runs) == 0 {
		return nil
	}
	out := &OriginOTP{AtM: OTPAtM, MaxGapS: OTPMaxGap, LateS: OTPLateS, ByLine: []LineOTP{}}
	lines := map[string]*LineOTP{}
	for _, r := range c.runs {
		l := lines[r.line]
		if l == nil {
			l = &LineOTP{Line: r.line}
			lines[r.line] = l
		}
		d, ok := r.delay()
		out.count(d, ok)
		l.count(d, ok)
	}
	out.share()
	if out.Measured > 0 {
		out.Band = OTPBand(out.OnTime, out.Measured)
	}
	for _, l := range lines {
		l.share()
		out.ByLine = append(out.ByLine, *l)
	}
	sort.Slice(out.ByLine, func(i, j int) bool { return out.ByLine[i].Line < out.ByLine[j].Line })
	return out
}

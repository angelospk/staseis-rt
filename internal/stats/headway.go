package stats

import (
	"math"
	"sort"
	"time"
)

const (
	// HeadwayAt is the reference point of a daily direction: the bus passes it when its share of
	// the route crosses this value (mid-route), interpolated between two fixes.
	HeadwayAt = 0.5
	// HeadwayMaxGap: two fixes further apart than this do not place a pass.
	HeadwayMaxGap = 5 * time.Minute
	// HeadwayFromH and HeadwayToH: only passes in [from, to) local time count (daytime).
	HeadwayFromH, HeadwayToH = 7, 21
	// HeadwayMaxRef: only directions whose mean observed headway is at most this count
	// (high-frequency lines; TCQSM 4th ed. Exhibit 5-9 applies to headways of 10 min or less).
	HeadwayMaxRef = 10 * time.Minute
	// HeadwayMaxRefGTFS: the limit of a plan that measures GTFS trips (Thessaloniki, #181). No
	// direction there has a mean headway of 10 min or less, so the block would never appear.
	// Above 10 min riders partly follow the timetable, so the block says which limit it used.
	HeadwayMaxRefGTFS = 15 * time.Minute
	// HeadwayOutageMin: a run of this many minutes in a row with sparse fixes city-wide is an
	// outage of the backend, not of the buses: no headway is taken across it. It is HeadwayMaxGap
	// in minutes: no bus can be placed over a hole that long.
	HeadwayOutageMin = int(HeadwayMaxGap / time.Minute)
	// HeadwayOutageShare: a minute is sparse when its fixes (all lines of the city) are under this
	// share of the median minute of the window. On 2026-11-13 Thessaloniki, where the fixes of
	// the degraded 13:20-13:50 outage were 12-30 % of the median minute, the cut is 30 %.
	HeadwayOutageShare = 0.3
	// HeadwayMinN: a direction with fewer headways is left out.
	HeadwayMinN = 8
)

// Headways is the regularity of the day's high-frequency lines (headway adherence, TCQSM).
type Headways struct {
	FromH int     `json:"from_h"`
	ToH   int     `json:"to_h"`
	At    float64 `json:"at"` // share of the route where passes are taken
	// MaxS is the limit on the mean headway of the listed directions (HeadwayMaxRef, or
	// HeadwayMaxRefGTFS); the TCQSM bands are meant for 600 s or less.
	MaxS   int64         `json:"max_headway_s"`
	ByLine []LineHeadway `json:"by_line"`
}

// LineHeadway is one direction of one line, by TCQSM 4th edition (TCRP Research Report 262,
// ch. 5): cv_h = stddev(h) / mean(h), with the mean observed headway in place of the scheduled one
// (not known at the reference point).
type LineHeadway struct {
	Line     string  `json:"line"`
	Dir      int     `json:"dir"` // 0 Go, 1 Come (OASA daily schedule); GTFS direction_id (GTFSHeadways)
	N        int     `json:"n"`   // headways measured
	MeanS    int64   `json:"mean_headway_s"`
	MedianS  int64   `json:"median_headway_s"`
	CVh      float64 `json:"cv_h"`
	WaitMin  float64 `json:"wait_min"`
	EWTMin   float64 `json:"ewt_min"`
	OffShare float64 `json:"off_headway_share"`
	LOS      string  `json:"los"`
}

// HeadwayStat sums a direction's headways (seconds; Mean and Median in seconds).
type HeadwayStat struct {
	Mean, Median float64
	CV           float64 // population stddev / mean
	WaitMin      float64 // average wait of a passenger arriving at random: 0.5 mean (1 + cv^2) (TCQSM Eq. 5-1)
	EWTMin       float64 // excess wait over perfectly even service: WaitMin - mean/2
	OffShare     float64 // share of headways with |h - mean| > mean/2 (bunched or a gap)
}

// HeadwayStats sums the headways hs (seconds); false when there are fewer than HeadwayMinN.
func HeadwayStats(hs []float64) (HeadwayStat, bool) {
	if len(hs) < HeadwayMinN {
		return HeadwayStat{}, false
	}
	s := append([]float64(nil), hs...)
	sort.Float64s(s)
	n := len(s)
	var st HeadwayStat
	if n%2 == 1 {
		st.Median = s[n/2]
	} else {
		st.Median = (s[n/2-1] + s[n/2]) / 2
	}
	var sum float64
	for _, h := range s {
		sum += h
	}
	st.Mean = sum / float64(n)
	if st.Mean <= 0 {
		return HeadwayStat{}, false
	}
	var v float64
	off := 0
	for _, h := range s {
		v += (h - st.Mean) * (h - st.Mean)
		if math.Abs(h-st.Mean) > st.Mean/2 {
			off++
		}
	}
	v /= float64(n)
	st.CV = math.Sqrt(v) / st.Mean
	st.EWTMin = v / (2 * st.Mean) / 60
	st.WaitMin = st.Mean/2/60 + st.EWTMin
	st.OffShare = float64(off) / float64(n)
	return st, true
}

// LOS is the TCQSM headway-adherence level of service of a cv_h.
func LOS(cv float64) string {
	for i, b := range []float64{0.21, 0.30, 0.39, 0.52, 0.74} {
		if cv <= b {
			return string(rune('A' + i))
		}
	}
	return "F"
}

// headways sums the passes of the daily directions, and with GTFSHeadways of the GTFS trips by
// line and direction_id: those in [HeadwayFromH, HeadwayToH) of the local day starting at start.
// nil when no direction qualifies (mean headway over HeadwayMaxRef; HeadwayMaxRefGTFS with GTFSHeadways).
func (c *tripCounter) headways(start time.Time, loc *time.Location) *Headways {
	from := time.Date(start.Year(), start.Month(), start.Day(), HeadwayFromH, 0, 0, 0, loc).Unix()
	to := time.Date(start.Year(), start.Month(), start.Day(), HeadwayToH, 0, 0, 0, loc).Unix()
	type dirKey struct {
		line string
		dir  int
	}
	passes := map[dirKey][]int64{}
	keep := func(k dirKey, p int64) {
		if p >= from && p < to {
			passes[k] = append(passes[k], p)
		}
	}
	for _, b := range c.buses {
		d := c.plan.Daily[b.key.dir]
		keep(dirKey{d.Line, d.Dir}, b.pass)
	}
	for k, p := range c.tripPass {
		pt := c.plan.Trips[k]
		keep(dirKey{pt.Line, pt.Dir}, p)
	}
	maxRef := HeadwayMaxRef
	if c.plan.GTFSHeadways {
		maxRef = HeadwayMaxRefGTFS
	}
	holes := c.outages(from, to)
	out := &Headways{FromH: HeadwayFromH, ToH: HeadwayToH, At: HeadwayAt, MaxS: int64(maxRef.Seconds()), ByLine: []LineHeadway{}}
	for k, ps := range passes {
		sort.Slice(ps, func(a, b int) bool { return ps[a] < ps[b] })
		hs := make([]float64, 0, len(ps))
		for i := 1; i < len(ps); i++ {
			if !spans(holes, ps[i-1], ps[i]) {
				hs = append(hs, float64(ps[i]-ps[i-1]))
			}
		}
		st, ok := HeadwayStats(hs)
		if !ok || st.Mean > maxRef.Seconds() {
			continue
		}
		out.ByLine = append(out.ByLine, LineHeadway{Line: k.line, Dir: k.dir, N: len(hs),
			MeanS: int64(math.Round(st.Mean)), MedianS: int64(math.Round(st.Median)), CVh: round3(st.CV),
			WaitMin: math.Round(st.WaitMin*100) / 100, EWTMin: math.Round(st.EWTMin*100) / 100,
			OffShare: round3(st.OffShare), LOS: LOS(st.CV)})
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

// outages are the runs of at least HeadwayOutageMin minutes in [from, to) (unix seconds, on a
// minute) in which the city-wide fixes of each minute are under HeadwayOutageShare of the
// median minute, as half-open [start, end) seconds. None when no fix count was kept (or the
// median is 0: the share is then 0 and no minute is under it).
func (c *tripCounter) outages(from, to int64) [][2]int64 {
	if len(c.fixMin) == 0 || to <= from {
		return nil
	}
	n := int((to - from) / 60)
	counts := make([]int, n)
	for i := range counts {
		counts[i] = c.fixMin[from/60+int64(i)]
	}
	sorted := append([]int(nil), counts...)
	sort.Ints(sorted)
	limit := HeadwayOutageShare * float64(sorted[n/2])
	var out [][2]int64
	run := 0
	for i := 0; i <= n; i++ {
		if i < n && float64(counts[i]) < limit {
			run++
			continue
		}
		if run >= HeadwayOutageMin {
			out = append(out, [2]int64{from + int64(i-run)*60, from + int64(i)*60})
		}
		run = 0
	}
	return out
}

// spans: whether the time between two passes a < b overlaps one of the outages.
func spans(holes [][2]int64, a, b int64) bool {
	for _, h := range holes {
		if h[0] < b && h[1] > a {
			return true
		}
	}
	return false
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

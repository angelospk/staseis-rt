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
	// HeadwayMinN: a direction with fewer headways is left out.
	HeadwayMinN = 8
)

// Headways is the regularity of the day's high-frequency lines (headway adherence, TCQSM).
type Headways struct {
	FromH  int           `json:"from_h"`
	ToH    int           `json:"to_h"`
	At     float64       `json:"at"` // share of the route where passes are taken
	ByLine []LineHeadway `json:"by_line"`
}

// LineHeadway is one direction of one line, by TCQSM 4th edition (TCRP Research Report 262,
// ch. 5): cv_h = stddev(h) / mean(h), with the mean observed headway in place of the scheduled one
// (not known at the reference point).
type LineHeadway struct {
	Line     string  `json:"line"`
	Dir      int     `json:"dir"` // 0 Go, 1 Come (OASA daily schedule)
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

// headways sums the passes of the daily directions: those in [HeadwayFromH, HeadwayToH) of the
// local day starting at start. nil when no direction qualifies.
func (c *tripCounter) headways(start time.Time, loc *time.Location) *Headways {
	from := time.Date(start.Year(), start.Month(), start.Day(), HeadwayFromH, 0, 0, 0, loc).Unix()
	to := time.Date(start.Year(), start.Month(), start.Day(), HeadwayToH, 0, 0, 0, loc).Unix()
	byDir := make([][]int64, len(c.plan.Daily))
	for _, b := range c.buses {
		if b.pass >= from && b.pass < to {
			byDir[b.key.dir] = append(byDir[b.key.dir], b.pass)
		}
	}
	out := &Headways{FromH: HeadwayFromH, ToH: HeadwayToH, At: HeadwayAt, ByLine: []LineHeadway{}}
	for i, ps := range byDir {
		sort.Slice(ps, func(a, b int) bool { return ps[a] < ps[b] })
		hs := make([]float64, 0, len(ps))
		for k := 1; k < len(ps); k++ {
			hs = append(hs, float64(ps[k]-ps[k-1]))
		}
		st, ok := HeadwayStats(hs)
		if !ok || st.Mean > HeadwayMaxRef.Seconds() {
			continue
		}
		d := c.plan.Daily[i]
		out.ByLine = append(out.ByLine, LineHeadway{Line: d.Line, Dir: d.Dir, N: len(hs),
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

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

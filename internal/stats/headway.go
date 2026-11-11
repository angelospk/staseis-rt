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
	// HeadwayMaxRef: only directions whose median observed headway is at most this count
	// (high-frequency lines).
	HeadwayMaxRef = 12 * time.Minute
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

// LineHeadway is one direction of one line. The reference headway is the median observed one
// (the scheduled headway at the reference point is not known), so cv_h = stddev(h) / ref.
type LineHeadway struct {
	Line    string  `json:"line"`
	Dir     int     `json:"dir"` // 0 Go, 1 Come (OASA daily schedule)
	N       int     `json:"n"`   // headways measured
	RefMedS int64   `json:"ref_headway_median_s"`
	MeanS   int64   `json:"mean_headway_s"`
	CVh     float64 `json:"cv_h"`
	EWTMin  float64 `json:"ewt_min"`
	LOS     string  `json:"los"`
}

// HeadwayStats returns the median headway (s), cv_h and excess wait time (minutes) of the
// headways hs (seconds), and false when there are fewer than HeadwayMinN of them.
// cv_h = stddev(h - ref) / ref with ref the median; EWT = sum(h^2)/(2 sum(h)) - ref/2.
func HeadwayStats(hs []float64) (ref, cv, ewtMin float64, ok bool) {
	if len(hs) < HeadwayMinN {
		return 0, 0, 0, false
	}
	s := append([]float64(nil), hs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		ref = s[n/2]
	} else {
		ref = (s[n/2-1] + s[n/2]) / 2
	}
	if ref <= 0 {
		return 0, 0, 0, false
	}
	var sum, sq float64
	for _, h := range s {
		sum += h
		sq += h * h
	}
	mean := sum / float64(n)
	var v float64
	for _, h := range s {
		v += (h - mean) * (h - mean)
	}
	cv = math.Sqrt(v/float64(n)) / ref
	ewtMin = (sq/(2*sum) - ref/2) / 60
	return ref, cv, ewtMin, true
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
		ref, cv, ewt, ok := HeadwayStats(hs)
		if !ok || ref > HeadwayMaxRef.Seconds() {
			continue
		}
		var sum float64
		for _, h := range hs {
			sum += h
		}
		d := c.plan.Daily[i]
		out.ByLine = append(out.ByLine, LineHeadway{Line: d.Line, Dir: d.Dir, N: len(hs),
			RefMedS: int64(math.Round(ref)), MeanS: int64(math.Round(sum / float64(len(hs)))),
			CVh: round3(cv), EWTMin: math.Round(ewt*100) / 100, LOS: LOS(cv)})
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

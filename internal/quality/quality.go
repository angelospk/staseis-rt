// Package quality gives the live level of service on the scale of docs/QUALITY.md: one of
// normal, worse, much_worse or unknown, always against the usual service for the same hour and
// day type. It rates a stop-to-stop segment by the speed of the buses that crossed it lately and
// a vehicle by its gap to the bus ahead (frequent directions) or its delay (sparse ones). The
// thresholds are the "Live levels" table of docs/QUALITY.md.
package quality

// Level is the agreed scale: green, yellow, red, grey.
type Level string

const (
	Normal    Level = "normal"
	Worse     Level = "worse"
	MuchWorse Level = "much_worse"
	Unknown   Level = "unknown"
)

const (
	SegmentNormal = 0.8 // a segment at this share of its usual speed or more is normal,
	SegmentWorse  = 0.5 // at this share or more a little worse, under it much worse
	MinTraversals = 2   // traversals a segment needs in Window before it has a level

	FrequentMaxS = 600  // a usual headway this long or shorter makes a frequent direction
	HeadwayOK    = 0.25 // |gap − usual| / usual up to this is normal (TCQSM off-headway: 0.5)
	HeadwayWorse = 0.5  // up to this a little worse, over it much worse
	BunchedShare = 0.5  // a gap shorter than this share of the usual headway is bunched

	EarlyS = -60 // more than 60 s early is a little worse (early)
	LateS  = 300 // up to 5 min late is normal,
	LateS2 = 600 // up to 10 min a little worse, more much worse
)

// SegmentLevel rates a segment by its speed now over its usual speed.
func SegmentLevel(ratio float64) Level {
	switch {
	case ratio >= SegmentNormal:
		return Normal
	case ratio >= SegmentWorse:
		return Worse
	}
	return MuchWorse
}

// HeadwayLevel rates a gap to the bus ahead against the usual headway (both seconds, usual > 0)
// and says whether the bus runs bunched with the one ahead.
func HeadwayLevel(gapS, usualS int) (Level, bool) {
	d := float64(gapS-usualS) / float64(usualS)
	if d < 0 {
		d = -d
	}
	bunched := float64(gapS) < BunchedShare*float64(usualS)
	switch {
	case d <= HeadwayOK:
		return Normal, bunched
	case d <= HeadwayWorse:
		return Worse, bunched
	}
	return MuchWorse, bunched
}

// DelayLevel rates a delay against the timetable (seconds, early negative) and says whether
// the bus runs early.
func DelayLevel(delayS int) (Level, bool) {
	switch {
	case delayS < EarlyS:
		return Worse, true
	case delayS <= LateS:
		return Normal, false
	case delayS <= LateS2:
		return Worse, false
	}
	return MuchWorse, false
}

// Basis names the measure a vehicle level comes from.
const (
	BasisHeadway  = "headway"
	BasisSchedule = "schedule"
)

// Usual is the usual service of a vehicle's line and headsign at its stop now. Known is false
// when the past days of this day type are not known at all; HeadwayS is 0 when the stop has no
// usual headway (a sparse direction).
type Usual struct {
	Known    bool
	HeadwayS int
}

// VehicleLevel is a vehicle's level and the numbers behind it.
type VehicleLevel struct {
	Level         Level
	Basis         string // BasisHeadway, BasisSchedule or "" (unknown, no measure)
	GapS          int    // headway basis with a gap: seconds since the bus ahead passed Stop
	UsualHeadwayS int    // headway basis: the usual headway at the stop
	Stop          string // headway basis with a gap: the stop the gap was measured at
	Bunched       bool
	Early         bool
}

// Vehicle rates a vehicle: by its gap on a frequent direction, by its delay on a sparse one.
func Vehicle(u Usual, gap *Gap, delayS *int) VehicleLevel {
	if !u.Known {
		return VehicleLevel{Level: Unknown}
	}
	if u.HeadwayS > 0 && u.HeadwayS <= FrequentMaxS {
		v := VehicleLevel{Level: Unknown, Basis: BasisHeadway, UsualHeadwayS: u.HeadwayS}
		if gap != nil {
			v.GapS, v.Stop = int(gap.GapS), gap.Stop
			v.Level, v.Bunched = HeadwayLevel(v.GapS, u.HeadwayS)
		}
		return v
	}
	if delayS == nil {
		return VehicleLevel{Level: Unknown}
	}
	l, early := DelayLevel(*delayS)
	return VehicleLevel{Level: l, Basis: BasisSchedule, Early: early}
}

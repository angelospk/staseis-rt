package quality

import (
	"testing"
)

// Every threshold of docs/QUALITY.md "Live levels", on both sides of its boundary.

func TestSegmentLevel(t *testing.T) {
	for _, c := range []struct {
		ratio float64
		want  Level
	}{
		{1.3, Normal}, {1, Normal}, {0.8, Normal}, {0.7999, Worse}, {0.5, Worse}, {0.4999, MuchWorse}, {0, MuchWorse},
	} {
		if got := SegmentLevel(c.ratio); got != c.want {
			t.Errorf("SegmentLevel(%v) = %s, want %s", c.ratio, got, c.want)
		}
	}
}

func TestHeadwayLevel(t *testing.T) {
	const usual = 400 // s
	for _, c := range []struct {
		gap     int
		want    Level
		bunched bool
	}{
		{400, Normal, false},
		{500, Normal, false},    // d = 0.25
		{501, Worse, false},     // d > 0.25
		{300, Normal, false},    // d = 0.25 short
		{299, Worse, false},     // d > 0.25 short
		{600, Worse, false},     // d = 0.5
		{601, MuchWorse, false}, // d > 0.5
		{200, Worse, false},     // d = 0.5, gap = 0.5 × usual: not bunched
		{199, MuchWorse, true},  // gap < 0.5 × usual
		{0, MuchWorse, true},
	} {
		got, bunched := HeadwayLevel(c.gap, usual)
		if got != c.want || bunched != c.bunched {
			t.Errorf("HeadwayLevel(%d, %d) = %s, %v; want %s, %v", c.gap, usual, got, bunched, c.want, c.bunched)
		}
	}
}

func TestDelayLevel(t *testing.T) {
	for _, c := range []struct {
		delay int
		want  Level
		early bool
	}{
		{0, Normal, false},
		{-60, Normal, false},
		{-61, Worse, true},
		{-900, Worse, true},
		{300, Normal, false},
		{301, Worse, false},
		{600, Worse, false},
		{601, MuchWorse, false},
	} {
		got, early := DelayLevel(c.delay)
		if got != c.want || early != c.early {
			t.Errorf("DelayLevel(%d) = %s, %v; want %s, %v", c.delay, got, early, c.want, c.early)
		}
	}
}

// TestVehicle: the usual headway at the stop picks the measure; without a usual service known
// for the day type, or without the number the measure needs, the level is unknown.
func TestVehicle(t *testing.T) {
	gap := &Gap{Stop: "S5", At: 1000, GapS: 420}
	late := 700
	for _, c := range []struct {
		name  string
		usual Usual
		gap   *Gap
		delay *int
		want  VehicleLevel
	}{
		{"frequent, normal gap", Usual{Known: true, HeadwayS: 420}, gap, &late,
			VehicleLevel{Level: Normal, Basis: BasisHeadway, GapS: 420, UsualHeadwayS: 420, Stop: "S5"}},
		{"frequent at 10 min", Usual{Known: true, HeadwayS: 600}, &Gap{Stop: "S5", GapS: 1000}, nil,
			VehicleLevel{Level: MuchWorse, Basis: BasisHeadway, GapS: 1000, UsualHeadwayS: 600, Stop: "S5"}},
		{"frequent, bunched", Usual{Known: true, HeadwayS: 420}, &Gap{Stop: "S5", GapS: 60}, nil,
			VehicleLevel{Level: MuchWorse, Basis: BasisHeadway, GapS: 60, UsualHeadwayS: 420, Stop: "S5", Bunched: true}},
		{"frequent, no gap yet", Usual{Known: true, HeadwayS: 420}, nil, &late,
			VehicleLevel{Level: Unknown, Basis: BasisHeadway, UsualHeadwayS: 420}},
		{"sparse: 11 min", Usual{Known: true, HeadwayS: 660}, gap, &late,
			VehicleLevel{Level: MuchWorse, Basis: BasisSchedule}},
		{"sparse: no usual headway", Usual{Known: true}, gap, model(-120),
			VehicleLevel{Level: Worse, Basis: BasisSchedule, Early: true}},
		{"sparse, no delay", Usual{Known: true}, gap, nil, VehicleLevel{Level: Unknown}},
		{"no usual service known", Usual{}, gap, &late, VehicleLevel{Level: Unknown}},
	} {
		if got := Vehicle(c.usual, c.gap, c.delay); got != c.want {
			t.Errorf("%s: Vehicle = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func model(v int) *int { return &v }

package quality

import (
	"math"
	"testing"
)

// Five stops 500 m apart on shape SH.
var stops = []Stop{{"S0", 0}, {"S1", 500}, {"S2", 1000}, {"S3", 1500}, {"S4", 2000}}

// ids are the stop ids of a stop list, the Pattern a client was given.
func ids(st []Stop) []string {
	out := make([]string, len(st))
	for i, x := range st {
		out[i] = x.ID
	}
	return out
}

func fix(veh string, s float64, t int64) Fix {
	return Fix{Vehicle: veh, Trip: "T-" + veh, Line: "040", Headsign: "Σύνταγμα", Shape: "SH", Stops: stops, S: s, T: t}
}

// drive feeds a vehicle moving at speed m/s from s0 at t0, one fix every 10 s, to s1.
func drive(k *Tracker, veh string, s0, s1 float64, t0 int64, speed float64) {
	for t, s := t0, s0; s <= s1; t, s = t+10, s+10*speed {
		k.Observe(fix(veh, s, t))
	}
}

// TestGapToTheBusAhead: a pass is interpolated between two fixes; the gap is the time since
// the previous pass of another bus of the same line and headsign at the same stop. The first
// two stops of a shape give no gap (buses wait there, as in internal/usual).
func TestGapToTheBusAhead(t *testing.T) {
	k := NewTracker()
	k.Observe(fix("A", 900, 0))
	k.Observe(fix("A", 1200, 30)) // passes S2 (1000) at t = 10
	k.Observe(fix("B", 600, 300))
	k.Observe(fix("B", 1100, 400)) // passes S2 at t = 380
	if g, ok := k.State("A", "T-A"); ok && g.Gap != nil {
		t.Errorf("A is first: no gap, got %+v", g.Gap)
	}
	st, ok := k.State("B", "T-B")
	if !ok || st.Gap == nil {
		t.Fatalf("B: no gap")
	}
	if *st.Gap != (Gap{Stop: "S2", At: 380, GapS: 370}) {
		t.Errorf("B gap = %+v", *st.Gap)
	}
	if st.RefStop != "S2" {
		t.Errorf("RefStop = %q, want S2", st.RefStop)
	}
	// A pass at S1 (index 1) gives no gap.
	k2 := NewTracker()
	k2.Observe(fix("A", 400, 0))
	k2.Observe(fix("A", 600, 30))
	k2.Observe(fix("B", 400, 200))
	k2.Observe(fix("B", 600, 230))
	if st, _ := k2.State("B", "T-B"); st.Gap != nil {
		t.Errorf("gap at the second stop: %+v", st.Gap)
	}
	if st, _ := k2.State("B", "T-B"); st.RefStop != "S2" {
		t.Errorf("RefStop before the third stop = %q, want S2", st.RefStop)
	}
}

// TestGapKeys: another headsign, line or the same bus again is not the bus ahead; a pass too
// long ago (over MaxGapS) gives no gap.
func TestGapKeys(t *testing.T) {
	k := NewTracker()
	other := fix("X", 900, 0)
	other.Headsign = "Πειραιάς"
	k.Observe(other)
	other.S, other.T = 1200, 30
	k.Observe(other)
	k.Observe(fix("B", 900, 100))
	k.Observe(fix("B", 1200, 130))
	if st, _ := k.State("B", "T-B"); st.Gap != nil {
		t.Errorf("other headsign counted: %+v", st.Gap)
	}
	// C passes S2 more than MaxGapS after B.
	k.Observe(fix("C", 900, 130+MaxGapS))
	k.Observe(fix("C", 1200, 160+MaxGapS))
	if st, _ := k.State("C", "T-C"); st.Gap != nil {
		t.Errorf("pass too old counted: %+v", st.Gap)
	}
}

// TestOvertaking: passes reach the tracker out of time order (a later poll of a bus that passed
// earlier). The gap is to the latest earlier pass by another bus, and the bus behind the late
// pass gets its gap measured again.
func TestOvertaking(t *testing.T) {
	k := NewTracker()
	k.Observe(fix("A", 900, 0))
	k.Observe(fix("A", 1200, 30)) // S2 at 10
	k.Observe(fix("C", 900, 500))
	k.Observe(fix("C", 1200, 530)) // S2 at 510
	k.Observe(fix("B", 900, 250))
	k.Observe(fix("B", 1200, 280)) // S2 at 260, between A and C, seen after C
	st, _ := k.State("B", "T-B")
	if st.Gap == nil || st.Gap.GapS != 250 {
		t.Errorf("B gap = %+v, want 250 s after A", st.Gap)
	}
	st, _ = k.State("C", "T-C")
	if st.Gap == nil || st.Gap.GapS != 250 {
		t.Errorf("C's gap after B's late pass: %+v, want 250 (B is the bus ahead)", st.Gap)
	}
}

// TestRunRules: a new trip starts over, a silence over MaxFixGapS interpolates nothing, and a
// step back does not pass a stop twice.
func TestRunRules(t *testing.T) {
	k := NewTracker()
	k.Observe(fix("A", 900, 0))
	k.Observe(fix("A", 1200, 30))
	k.Observe(fix("B", 900, 100))
	f := fix("B", 1200, 100+MaxFixGapS+1)
	k.Observe(f)
	if st, _ := k.State("B", "T-B"); st.Gap != nil {
		t.Errorf("pass across a silence: %+v", st.Gap)
	}
	// Back and forth around S3: one pass only.
	k.Observe(fix("B", 1490, f.T+30))
	k.Observe(fix("B", 1510, f.T+60))
	k.Observe(fix("B", 1480, f.T+90))
	k.Observe(fix("B", 1520, f.T+120))
	if n := k.passCount("040", "Σύνταγμα", "S3"); n != 1 {
		t.Errorf("passes at S3 = %d, want 1", n)
	}
	// A jump back over 300 m starts a new run; passing S3 again within 10 min is jitter.
	k.Observe(fix("B", 1100, f.T+130))
	k.Observe(fix("B", 1600, f.T+160))
	if n := k.passCount("040", "Σύνταγμα", "S3"); n != 1 {
		t.Errorf("passes at S3 after a jump back = %d, want 1", n)
	}
	// A new trip of the same vehicle: the old state is gone.
	g := fix("B", 1600, f.T+190)
	g.Trip = "T-B2"
	k.Observe(g)
	if _, ok := k.State("B", "T-B"); ok {
		t.Error("state of the old trip kept")
	}
	if st, ok := k.State("B", "T-B2"); !ok || st.Gap != nil {
		t.Errorf("new trip state = %+v, %v", st, ok)
	}
}

// TestSegments: a traversal runs from one stop pass to the next stop's pass; its speed is the
// stop distance over the time between the two passes. Segments lists, per segment of a shape,
// the traversals that ended within Window before now, with their median speed.
func TestSegments(t *testing.T) {
	k := NewTracker()
	drive(k, "A", 0, 2000, 0, 10) // 10 m/s: S1 at 50, S2 100, S3 150, S4 200
	drive(k, "B", 0, 2000, 60, 5) // 5 m/s: S1 at 160 … S4 at 460
	drive(k, "C", 0, 1000, 0, 2)  // 2 m/s, only to S2: S1 at 250, S2 at 500
	segs := k.Segments("SH", ids(stops), 600)
	// No S0-S1: a run that starts at a stop has not passed it.
	want := map[string]struct {
		n   int
		med float64
	}{
		"S1-S2": {3, 5}, "S2-S3": {2, 7.5}, "S3-S4": {2, 7.5},
	}
	if len(segs) != len(want) {
		t.Fatalf("segments = %+v", segs)
	}
	for i, s := range segs {
		w, ok := want[s.From+"-"+s.To]
		if !ok || s.N != w.n || math.Abs(s.MedianMS-w.med) > 0.01 {
			t.Errorf("segment %s-%s: n %d median %.2f, want %+v", s.From, s.To, s.N, s.MedianMS, w)
		}
		if i > 0 && segs[i-1].FromAlong >= s.FromAlong {
			t.Error("segments not in order along the shape")
		}
	}
	if s := segs[0]; s.FromAlong != 500 || s.ToAlong != 1000 {
		t.Errorf("S1-S2 alongs = %v..%v", s.FromAlong, s.ToAlong)
	}
	// Later than Window after every traversal: none left.
	if segs := k.Segments("SH", ids(stops), 600+int64(Window.Seconds())); len(segs) != 0 {
		t.Errorf("old traversals listed: %+v", segs)
	}
	if segs := k.Segments("OTHER", ids(stops), 600); len(segs) != 0 {
		t.Errorf("unknown shape: %+v", segs)
	}
}

// TestImplausibleTraversal: a traversal faster than MaxSpeed is a GPS jump, not traffic.
func TestImplausibleTraversal(t *testing.T) {
	k := NewTracker()
	k.Observe(fix("A", 400, 0))
	k.Observe(fix("A", 600, 10))
	k.Observe(fix("A", 1100, 20)) // S1→S2 in 500/50 m/s
	if segs := k.Segments("SH", ids(stops), 30); len(segs) != 0 {
		t.Errorf("jump counted: %+v", segs)
	}
}

// TestPrune: vehicles silent for VehicleKeep, passes older than MaxGapS and traversals older than
// Window are dropped.
func TestPrune(t *testing.T) {
	k := NewTracker()
	drive(k, "A", 0, 2000, 0, 10)
	if v, p, tr := k.Size(); v != 1 || p == 0 || tr == 0 {
		t.Fatalf("Size = %d %d %d", v, p, tr)
	}
	k.Prune(200 + MaxGapS + int64(VehicleKeep.Seconds()))
	if v, p, tr := k.Size(); v != 0 || p != 0 || tr != 0 {
		t.Errorf("after Prune: %d %d %d", v, p, tr)
	}
}

// TestSegmentsPoolLines: a segment is the road between two stops; a bus of another line and
// shape that stops at both, one after the other, crossed it too. A shape lists the segments of
// its stops crossed by any line, also when none of its own buses did.
func TestSegmentsPoolLines(t *testing.T) {
	k := NewTracker()
	drive(k, "A", 0, 1000, 0, 10) // line 040 on SH: S1→S2 at 10 m/s
	k.Observe(fix("A", 1000, 100))
	other := []Stop{{"X0", 0}, {"S1", 300}, {"S2", 800}, {"X3", 1500}}
	for t, s := int64(0), 0.0; s <= 1500; t, s = t+10, s+50 { // line 999 on SH2: 5 m/s
		k.Observe(Fix{Vehicle: "Z", Trip: "TZ", Line: "999", Headsign: "Χ", Shape: "SH2", Stops: other, S: s, T: t})
	}
	segs := k.Segments("SH", ids(stops), 400)
	var s12 *SegmentObs
	for i := range segs {
		if segs[i].From == "S1" && segs[i].To == "S2" {
			s12 = &segs[i]
		}
	}
	if s12 == nil || s12.N != 2 || math.Abs(s12.MedianMS-7.5) > 0.01 || s12.FromAlong != 500 || s12.ToAlong != 1000 {
		t.Fatalf("SH S1-S2 = %+v (all %+v)", s12, segs)
	}
	// SH2 lists S1-S2 with its own alongs, and S2-X3 only it crossed.
	segs = k.Segments("SH2", ids(other), 400)
	if len(segs) != 2 || segs[0].From != "S1" || segs[0].FromAlong != 300 || segs[0].N != 2 || segs[1].From != "S2" {
		t.Errorf("SH2 = %+v", segs)
	}
}

// TestRepeatMakesNoTraversal: a run started over by a jump back passes stops again; those
// repeats within minRepeatS make no gap and no second traversal.
func TestRepeatMakesNoTraversal(t *testing.T) {
	k := NewTracker()
	drive(k, "A", 400, 1100, 0, 10)  // S1 at 10, S2 at 60
	drive(k, "A", 400, 1100, 80, 10) // 700 m back: a new run, S1 and S2 again
	segs := k.Segments("SH", ids(stops), 200)
	if len(segs) != 1 || segs[0].N != 1 {
		t.Errorf("segments = %+v, want S1-S2 once", segs)
	}
}

// TestJumpPassesNothing: a fix pair faster than MaxSpeed (plus GPS slack) is a jump; the run
// starts over at the new fix and the stops in between are not passed.
func TestJumpPassesNothing(t *testing.T) {
	k := NewTracker()
	k.Observe(fix("A", 900, 0))
	k.Observe(fix("A", 1900, 10)) // 100 m/s over S2 and S3
	if n := k.passCount("040", "Σύνταγμα", "S2") + k.passCount("040", "Σύνταγμα", "S3"); n != 0 {
		t.Errorf("passes across a jump: %d", n)
	}
	k.Observe(fix("A", 2010, 20)) // on from the new place: S4 passed
	if n := k.passCount("040", "Σύνταγμα", "S4"); n != 1 {
		t.Errorf("S4 passes = %d, want 1", n)
	}
}

// TestSegmentsFollowThePattern: a shape lists the segments of the stops it is asked for (the
// Pattern clients were given), whatever stop lists its trips had; a pair it does not have
// consecutive is not listed.
func TestSegmentsFollowThePattern(t *testing.T) {
	k := NewTracker()
	drive(k, "A", 400, 1100, 0, 10) // local: S1→S2
	express := []Stop{stops[0], stops[1], stops[3]}
	for t, s := int64(0), 400.0; s <= 1600; t, s = t+10, s+100 {
		k.Observe(Fix{Vehicle: "E", Trip: "TE", Line: "040", Headsign: "Σύνταγμα", Shape: "SH", Stops: express, S: s, T: t})
	}
	got := map[string]int{}
	for _, s := range k.Segments("SH", ids(stops), 300) {
		got[s.From+"-"+s.To] = s.N
	}
	if got["S1-S2"] != 1 || len(got) != 1 {
		t.Errorf("segments %v, want only S1-S2 of the pattern", got)
	}
	if segs := k.Segments("SH", ids(express), 300); len(segs) != 1 || segs[0].To != "S3" || segs[0].ToAlong != 1500 {
		t.Errorf("express pattern: %+v", segs)
	}
	if segs := k.Segments("SH", []string{"S1", "NOWHERE"}, 300); len(segs) != 0 {
		t.Errorf("a stop with no known place: %+v", segs)
	}
}

// TestLateFirstGap: a bus that passed with no bus ahead known gets its gap when the pass of the
// bus ahead arrives later.
func TestLateFirstGap(t *testing.T) {
	k := NewTracker()
	k.Observe(fix("C", 900, 500))
	k.Observe(fix("C", 1200, 530)) // S2 at 510, alone
	k.Observe(fix("B", 900, 250))
	k.Observe(fix("B", 1200, 280)) // S2 at 260, seen later
	if st, _ := k.State("C", "T-C"); st.Gap == nil || st.Gap.GapS != 250 || st.Gap.Stop != "S2" || st.Gap.At != 510 {
		t.Errorf("C gap = %+v, want 250 at S2", st.Gap)
	}
}

// TestSegmentsSameRoadOnly: two shapes may join the same two stops by different roads. A
// traversal counts for a shape only when its stop distance is within SameRoad of the shape's.
func TestSegmentsSameRoadOnly(t *testing.T) {
	k := NewTracker()
	drive(k, "A", 400, 1100, 0, 10)                                      // SH: S1→S2 is 500 m
	detour := []Stop{{"X0", 0}, {"S1", 300}, {"S2", 1100}, {"X3", 1500}} // 800 m between S1 and S2
	for t, s := int64(0), 0.0; s <= 1500; t, s = t+10, s+50 {
		k.Observe(Fix{Vehicle: "Z", Trip: "TZ", Line: "999", Headsign: "Χ", Shape: "SH2", Stops: detour, S: s, T: t})
	}
	for _, s := range k.Segments("SH", ids(stops), 400) {
		if s.From == "S1" && s.N != 1 {
			t.Errorf("SH S1-S2 n = %d, want 1 (the detour is another road)", s.N)
		}
	}
}

// TestReset: a new feed starts the tracker over.
func TestReset(t *testing.T) {
	k := NewTracker()
	drive(k, "A", 0, 2000, 0, 10)
	k.Reset()
	if v, p, tr := k.Size(); v+p+tr != 0 || len(k.Segments("SH", ids(stops), 300)) != 0 {
		t.Errorf("after Reset: %d %d %d", v, p, tr)
	}
}

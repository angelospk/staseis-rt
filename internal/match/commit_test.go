package match

import "testing"

// A bus blocked on its route (a parked car, a breakdown) falls further and further behind its
// trip. Once its own history has scrolled out of the 15-minute window, the next trip fits its
// position better, and the memory matcher used to hand it that trip mid-run. A vehicle that has
// held a trip for several cycles keeps it while the trip stays feasible.
func TestBlockedBusKeepsItsTrip(t *testing.T) {
	m := newMatcher(t, Memory)
	steps := []struct {
		p    float64
		h, n int
	}{{0.3, 10, 12}, {1.2, 10, 17}, {2.0, 10, 22}, {2.1, 10, 30}, {2.2, 10, 36}, {2.4, 10, 42}}
	for _, s := range steps {
		r := m.MatchLine("L", []Obs{obs("v", s.p, at(s.h, s.n, 0))})[0]
		if tripID(m, r) != "T01" {
			t.Fatalf("at %02d:%02d (stop %.1f): trip %s, want T01", s.h, s.n, s.p, tripID(m, r))
		}
	}
}

// The trip a committed vehicle holds is not given to another vehicle on the same line.
func TestCommittedTripNotTakenByAnother(t *testing.T) {
	m := newMatcher(t, Memory)
	for _, s := range []struct {
		p    float64
		h, n int
	}{{0.3, 10, 12}, {1.2, 10, 17}, {2.0, 10, 22}, {2.1, 10, 30}, {2.2, 10, 36}} {
		m.MatchLine("L", []Obs{obs("v", s.p, at(s.h, s.n, 0))})
	}
	// w shows up right behind v at 10:42; both fit T01 and T02.
	res := m.MatchLine("L", []Obs{obs("v", 2.4, at(10, 42, 0)), obs("w", 2.35, at(10, 42, 0))})
	if tripID(m, res[0]) != "T01" || tripID(m, res[1]) == "T01" {
		t.Fatalf("v %s, w %s", tripID(m, res[0]), tripID(m, res[1]))
	}
}

// A commit is released when its trip stops fitting: here the bus is suddenly back near the first
// stop, behind its committed position, so it must match another trip (or none).
func TestCommitReleasedWhenTripInfeasible(t *testing.T) {
	m := newMatcher(t, Memory)
	for _, s := range []struct {
		p    float64
		h, n int
	}{{0.3, 10, 12}, {1.2, 10, 17}, {2.0, 10, 22}} {
		m.MatchLine("L", []Obs{obs("v", s.p, at(s.h, s.n, 0))})
	}
	r := m.MatchLine("L", []Obs{obs("v", 0.6, at(10, 30, 0))})[0] // within RunGapS: no new track
	if tripID(m, r) == "T01" {
		t.Fatal("still on T01 after moving backwards")
	}
}

// A trip the vehicle finished is never given back to it: at a loop's terminus the end of the old
// trip and the start of the next one are the same place.
func TestFinishedTripNotReassigned(t *testing.T) {
	m := newMatcher(t, Memory)
	for _, s := range []struct {
		p    float64
		h, n int
	}{{0.3, 10, 12}, {1.2, 10, 17}, {2.0, 10, 22}, {3.0, 10, 27}, {3.9, 10, 31}} {
		if r := m.MatchLine("L", []Obs{obs("v", s.p, at(s.h, s.n, 0))})[0]; tripID(m, r) != "T01" {
			t.Fatalf("setup at %02d:%02d: %s", s.h, s.n, tripID(m, r))
		}
	}
	// The vehicle runs T03 from its start (10:30 is long gone; it left late at 10:33).
	m.MatchLine("L", []Obs{obs("v", 0.3, at(10, 33, 0))})
	// A stray fix near the end at 10:34: T01 (+4 min) is the cheapest by position, but v finished it.
	r := m.MatchLine("L", []Obs{obs("v", 3.95, at(10, 34, 0))})[0]
	if tripID(m, r) == "T01" {
		t.Fatal("finished trip T01 given back")
	}
}

// A poll can miss a committed vehicle. While it is away (less than RunGapS), its trip stays
// reserved, so a nearby bus does not take it and the owner gets it back on its next fix.
func TestAbsentOwnerKeepsTrip(t *testing.T) {
	m := newMatcher(t, Memory)
	for _, s := range []struct {
		p    float64
		h, n int
	}{{0.3, 10, 12}, {1.2, 10, 17}, {2.0, 10, 22}} {
		m.MatchLine("L", []Obs{obs("v", s.p, at(s.h, s.n, 0))})
	}
	if r := m.MatchLine("L", []Obs{obs("w", 2.1, at(10, 24, 0))})[0]; tripID(m, r) == "T01" {
		t.Fatal("w took T01 while its owner was missing from one poll")
	}
	if r := m.MatchLine("L", []Obs{obs("v", 2.4, at(10, 26, 0)), obs("w", 2.15, at(10, 26, 0))})[0]; tripID(m, r) != "T01" {
		t.Fatalf("v came back on %q, want T01", tripID(m, r))
	}
}

// A fix far beside the last stop is not an arrival, even when it projects near the end of the line.
func TestFinishNeedsToBeAtTheLastStop(t *testing.T) {
	m := newMatcher(t, Memory)
	for _, s := range []struct {
		p    float64
		h, n int
	}{{0.3, 10, 12}, {1.2, 10, 17}, {2.0, 10, 22}, {3.0, 10, 27}} {
		m.MatchLine("L", []Obs{obs("v", s.p, at(s.h, s.n, 0))})
	}
	beside := obs("v", 3.95, at(10, 30, 0))
	beside.Lat += 0.0018 // about 200 m north of the line
	if r := m.MatchLine("L", []Obs{beside})[0]; tripID(m, r) != "T01" {
		t.Fatalf("beside the terminus: %q, want T01", tripID(m, r))
	}
	if r := m.MatchLine("L", []Obs{obs("v", 3.97, at(10, 31, 0))})[0]; tripID(m, r) != "T01" {
		t.Fatalf("at the terminus: %q, want T01 (the fix beside it was not an arrival)", tripID(m, r))
	}
}

// A bus can finish its trip up to 10 minutes early (MinDelay), but the trip stays a candidate
// until CandidateAfter past its scheduled end. The finished mark must last that long, not just an
// hour after the arrival.
func TestEarlyFinishOutlivesCandidateWindow(t *testing.T) {
	m := newMatcher(t, Memory)
	for _, s := range []struct {
		p    float64
		h, n int
	}{{1.0, 10, 0}, {2.0, 10, 5}, {3.0, 10, 10}, {3.97, 10, 12}} { // T00 ends at 10:20
		if r := m.MatchLine("L", []Obs{obs("v", s.p, at(s.h, s.n, 0))})[0]; tripID(m, r) != "T00" {
			t.Fatalf("setup at %02d:%02d: %q", s.h, s.n, tripID(m, r))
		}
	}
	if len(m.finished["v"]) != 1 {
		t.Fatalf("finished: %v", m.finished["v"])
	}
	m.Forget(at(11, 15, 0)) // 63 min after the arrival, 55 min after the scheduled end
	if len(m.finished["v"]) != 1 {
		t.Fatal("finished mark dropped while T00 is still a candidate")
	}
	m.Forget(at(11, 21, 0))
	if len(m.finished["v"]) != 0 {
		t.Fatal("finished mark kept after the candidate window")
	}
}

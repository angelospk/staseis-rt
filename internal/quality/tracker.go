package quality

import (
	"math"
	"slices"
	"sort"
	"sync"
	"time"
)

const (
	Window      = 15 * time.Minute // a segment's level uses the traversals that ended this recently
	MaxGapS     = 3 * 3600         // a pass of the bus ahead longer ago is no bus ahead (usual.MaxWaitS)
	MaxFixGapS  = 600              // a pass is interpolated between two fixes at most this far apart
	VehicleKeep = 15 * time.Minute // a vehicle silent this long is forgotten
	MaxSpeed    = 20.0             // m/s; a faster traversal is a GPS jump
	runBackM    = 300.0            // a jump back of more than this starts a new run (usual.runBackM)
	minRepeatS  = 600              // the same bus at the same stop again sooner is jitter (usual.minRepeatS)
	jumpSlackM  = 100.0            // GPS slack on MaxSpeed for a fix pair; faster is a jump
	SameRoad    = 0.2              // a traversal counts for a shape when its stop distance is this close (share)
	firstGap    = 2                // the first stops of a shape give no gap: buses wait there
	keepPasses  = 4                // recent passes kept per line, headsign and stop
)

// Stop is a stop of a shape and its position along it, metres.
type Stop struct {
	ID    string
	Along float64
}

// Fix is one matched position of a vehicle on its trip: S metres along the shape at unix time T.
// Stops are every stop of the trip's pattern, by Along.
type Fix struct {
	Vehicle, Trip  string
	Line, Headsign string
	Shape          string
	Stops          []Stop
	S              float64
	T              int64
}

// Gap is the gap to the bus ahead measured at the vehicle's last stop pass.
type Gap struct {
	Stop string
	At   int64 // when the vehicle passed Stop
	GapS int64 // seconds since the bus ahead passed Stop
}

// VehicleState is what the tracker knows of a vehicle's run: its last gap (nil before its
// first measured pass) and RefStop, the stop whose usual service rates it (the gap's stop, else
// the next stop that can give a gap).
type VehicleState struct {
	Gap     *Gap
	RefStop string
}

// SegmentObs is one segment of a shape, between two of its stops one after the other, with the
// traversals of the last Window by any line that stops at both in that order.
type SegmentObs struct {
	From, To           string
	FromAlong, ToAlong float64
	N                  int
	MedianMS           float64
}

type pass struct {
	veh string
	t   int64
}

type passKey struct{ line, headsign, stop string }

type traversal struct {
	end   int64
	speed float64
	m     float64 // stop distance along the shape it was measured on
}

type vehicle struct {
	trip, line, headsign, shape string
	stops                       []Stop
	s, maxS                     float64
	t                           int64
	lastIdx                     int // index of the last stop passed, -1 none
	lastT                       int64
	gap                         *Gap
	passed                      map[int]int64 // stop index -> pass time, this trip (repeat rule)
}

// Tracker follows the vehicles' stop passes; it is safe for concurrent use.
type Tracker struct {
	mu       sync.Mutex
	vehicles map[string]*vehicle
	passes   map[passKey][]pass              // newest last, at most keepPasses
	segments map[[2]string][]traversal       // (from, to) stop ids -> traversals, by arrival
	shapes   map[string]map[string][]float64 // shape -> stop id -> its places along the shape (a loop: two), ascending
}

func NewTracker() *Tracker {
	return &Tracker{vehicles: map[string]*vehicle{}, passes: map[passKey][]pass{},
		segments: map[[2]string][]traversal{}, shapes: map[string]map[string][]float64{}}
}

// Reset forgets everything: a new GTFS feed may reuse trip and shape ids for other geometry.
func (k *Tracker) Reset() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.vehicles, k.passes = map[string]*vehicle{}, map[passKey][]pass{}
	k.segments, k.shapes = map[[2]string][]traversal{}, map[string]map[string][]float64{}
}

// Observe adds a fix. A fix that is not newer passes nothing. A fix of a new trip or shape,
// after a silence over MaxFixGapS, more than runBackM back or faster than MaxSpeed from the last
// one (a jump) starts the vehicle's run over; a fix not past the furthest point of the run
// passes nothing.
func (k *Tracker) Observe(f Fix) {
	k.mu.Lock()
	defer k.mu.Unlock()
	at := k.shapes[f.Shape]
	if at == nil {
		at = map[string][]float64{}
		k.shapes[f.Shape] = at
	}
	for _, st := range f.Stops {
		if xs := at[st.ID]; !slices.Contains(xs, st.Along) {
			xs = append(xs, st.Along)
			sort.Float64s(xs)
			at[st.ID] = xs
		}
	}
	v := k.vehicles[f.Vehicle]
	if v != nil && f.T <= v.t {
		return
	}
	if v == nil || v.trip != f.Trip || v.shape != f.Shape || f.T-v.t > MaxFixGapS || f.S < v.s-runBackM ||
		f.S-v.s > MaxSpeed*float64(f.T-v.t)+jumpSlackM {
		passed := map[int]int64{}
		if v != nil && v.trip == f.Trip && v.shape == f.Shape {
			passed = v.passed // the same trip again: its stops were passed already
		}
		k.vehicles[f.Vehicle] = &vehicle{trip: f.Trip, line: f.Line, headsign: f.Headsign, shape: f.Shape,
			stops: f.Stops, s: f.S, maxS: f.S, t: f.T, lastIdx: -1, passed: passed}
		return
	}
	prevS, prevT, maxS := v.s, v.t, v.maxS
	v.s, v.t = f.S, f.T
	if f.S <= maxS {
		return
	}
	v.maxS = f.S
	ds, dt := f.S-prevS, float64(f.T-prevT)
	if ds <= 0 {
		return // still behind a point it reached before (noise back, then forward)
	}
	i := sort.Search(len(v.stops), func(i int) bool { return v.stops[i].Along > maxS })
	for ; i < len(v.stops) && v.stops[i].Along <= f.S; i++ {
		st := v.stops[i]
		p := prevT + int64(math.Round((st.Along-prevS)/ds*dt))
		if st.Along <= prevS {
			p = prevT
		}
		k.pass(f.Vehicle, v, i, p)
	}
}

// pass records that v passed its stop i at p. A repeat (the same stop within minRepeatS, after
// a jump back) records nothing.
func (k *Tracker) pass(id string, v *vehicle, i int, p int64) {
	st := v.stops[i]
	if t0, ok := v.passed[i]; ok && p-t0 < minRepeatS && t0-p < minRepeatS {
		v.lastIdx, v.lastT = i, p
		return
	}
	v.passed[i] = p
	if v.lastIdx == i-1 && v.lastIdx >= 0 && p > v.lastT {
		prev := v.stops[v.lastIdx]
		m := st.Along - prev.Along
		if speed := m / float64(p-v.lastT); speed <= MaxSpeed {
			key := [2]string{prev.ID, st.ID}
			k.segments[key] = append(k.segments[key], traversal{p, speed, m})
		}
	}
	v.lastIdx, v.lastT = i, p
	if i < firstGap {
		return
	}
	key := passKey{v.line, v.headsign, st.ID}
	ps := k.passes[key]
	v.gap = nil
	for j := len(ps) - 1; j >= 0; j-- {
		if ps[j].veh != id && ps[j].t <= p {
			if p-ps[j].t <= MaxGapS {
				v.gap = &Gap{Stop: st.ID, At: p, GapS: p - ps[j].t}
			}
			break
		}
	}
	// Insert in time order, keep the newest keepPasses.
	j := sort.Search(len(ps), func(j int) bool { return ps[j].t > p })
	ps = append(ps, pass{})
	copy(ps[j+1:], ps[j:])
	ps[j] = pass{id, p}
	// A pass that arrived late may sit before a bus whose gap at this stop, its last one, was
	// measured without it (or not at all).
	if j+1 < len(ps) && ps[j+1].veh != id {
		next := ps[j+1]
		if w := k.vehicles[next.veh]; w != nil && w.lastIdx >= 0 && w.stops[w.lastIdx].ID == st.ID &&
			w.lastT == next.t && w.line == v.line && w.headsign == v.headsign && next.t-p <= MaxGapS {
			w.gap = &Gap{Stop: st.ID, At: next.t, GapS: next.t - p}
		}
	}
	if len(ps) > keepPasses {
		ps = append(ps[:0:0], ps[len(ps)-keepPasses:]...)
	}
	k.passes[key] = ps
}

// State is the vehicle's state on trip; ok is false when the tracker follows it on no or
// another trip.
func (k *Tracker) State(vehicle, trip string) (VehicleState, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v := k.vehicles[vehicle]
	if v == nil || v.trip != trip {
		return VehicleState{}, false
	}
	st := VehicleState{}
	if v.gap != nil {
		g := *v.gap
		st.Gap, st.RefStop = &g, g.Stop
		return st, true
	}
	i := sort.Search(len(v.stops), func(i int) bool { return v.stops[i].Along > v.maxS })
	i = max(i, firstGap)
	if i < len(v.stops) {
		st.RefStop = v.stops[i].ID
	}
	return st, true
}

// Segments lists the segments between the stops given (a Pattern of shape, in order) that buses
// of any line crossed, stopping at both, in (now − Window, now], over a stop distance within
// SameRoad of the shape's (the same road), with their count and median speed. A stop the tracker
// has no place for on shape (no fix of a trip with it yet) ends no segment.
func (k *Tracker) Segments(shape string, stops []string, now int64) []SegmentObs {
	if len(stops) < 2 {
		return nil
	}
	from := now - int64(Window.Seconds())
	k.mu.Lock()
	defer k.mu.Unlock()
	at := k.shapes[shape]
	var out []SegmentObs
	var speeds []float64
	// Each stop takes its first place past the previous stop's (a loop's end stop is its start).
	place := func(stop string, after float64) (float64, bool) {
		for _, x := range at[stop] {
			if x > after {
				return x, true
			}
		}
		return 0, false
	}
	a, okA := place(stops[0], math.Inf(-1))
	for i := 1; i < len(stops); i++ {
		b, okB := place(stops[i], a)
		if !okB {
			b, okB = place(stops[i], math.Inf(-1)) // unknown order: start over from this stop
			okA = false
		}
		if !okA || !okB {
			a, okA = b, okB
			continue
		}
		m := b - a
		speeds = speeds[:0]
		for _, x := range k.segments[[2]string{stops[i-1], stops[i]}] {
			if x.end > from && x.end <= now && math.Abs(x.m-m) <= SameRoad*m {
				speeds = append(speeds, x.speed)
			}
		}
		if len(speeds) > 0 {
			out = append(out, SegmentObs{From: stops[i-1], To: stops[i], FromAlong: a, ToAlong: b,
				N: len(speeds), MedianMS: median(speeds)})
		}
		a = b
	}
	return out
}

func median(v []float64) float64 {
	sort.Float64s(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// Prune forgets vehicles silent for VehicleKeep, passes older than MaxGapS and traversals that
// ended Window or more before now.
func (k *Tracker) Prune(now int64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for id, v := range k.vehicles {
		if now-v.t > int64(VehicleKeep.Seconds()) {
			delete(k.vehicles, id)
		}
	}
	for key, ps := range k.passes {
		if ps[len(ps)-1].t < now-MaxGapS {
			delete(k.passes, key)
		}
	}
	old := now - int64(Window.Seconds())
	for key, trav := range k.segments {
		kept := trav[:0]
		for _, x := range trav {
			if x.end > old {
				kept = append(kept, x)
			}
		}
		if len(kept) == 0 {
			delete(k.segments, key)
		} else {
			k.segments[key] = kept
		}
	}
}

// Size is the number of vehicles, passes and traversals held.
func (k *Tracker) Size() (vehicles, passes, traversals int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, ps := range k.passes {
		passes += len(ps)
	}
	for _, trav := range k.segments {
		traversals += len(trav)
	}
	return len(k.vehicles), passes, traversals
}

// passCount is the number of passes held for a line, headsign and stop (tests).
func (k *Tracker) passCount(line, headsign, stop string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.passes[passKey{line, headsign, stop}])
}

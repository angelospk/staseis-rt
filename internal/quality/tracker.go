package quality

import (
	"math"
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
	firstGap    = 2                // the first stops of a shape give no gap: buses wait there
	keepPasses  = 4                // recent passes kept per line, headsign and stop
)

// Stop is a stop of a shape and its position along it, metres.
type Stop struct {
	ID    string
	Along float64
}

// Fix is one matched position of a vehicle on its trip: S metres along the shape at unix time T.
// Stops are every stop of the shape, by Along.
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
}

type vehicle struct {
	trip, line, headsign, shape string
	stops                       []Stop
	s, maxS                     float64
	t                           int64
	lastIdx                     int // index of the last stop passed, -1 none
	lastT                       int64
	gap                         *Gap
}

// Tracker follows the vehicles' stop passes; it is safe for concurrent use.
type Tracker struct {
	mu       sync.Mutex
	vehicles map[string]*vehicle
	passes   map[passKey][]pass        // newest last, at most keepPasses
	segments map[[2]string][]traversal // (from, to) stop ids -> traversals, by arrival
	shapes   map[string][]Stop         // shape -> its stops, as the last fix on it gave them
}

func NewTracker() *Tracker {
	return &Tracker{vehicles: map[string]*vehicle{}, passes: map[passKey][]pass{},
		segments: map[[2]string][]traversal{}, shapes: map[string][]Stop{}}
}

// Observe adds a fix. A fix of a new trip or shape, after a silence over MaxFixGapS or more than
// runBackM back, starts the vehicle's run over; a fix that is not newer, or not past the
// furthest point of the run, passes nothing.
func (k *Tracker) Observe(f Fix) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.shapes[f.Shape] = f.Stops
	v := k.vehicles[f.Vehicle]
	if v == nil || v.trip != f.Trip || v.shape != f.Shape || f.T-v.t > MaxFixGapS || f.S < v.s-runBackM {
		if v != nil && f.T <= v.t {
			return
		}
		k.vehicles[f.Vehicle] = &vehicle{trip: f.Trip, line: f.Line, headsign: f.Headsign, shape: f.Shape,
			stops: f.Stops, s: f.S, maxS: f.S, t: f.T, lastIdx: -1}
		return
	}
	if f.T <= v.t {
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

// pass records that v passed its stop i at p.
func (k *Tracker) pass(id string, v *vehicle, i int, p int64) {
	st := v.stops[i]
	if v.lastIdx == i-1 && v.lastIdx >= 0 && p > v.lastT {
		prev := v.stops[v.lastIdx]
		if speed := (st.Along - prev.Along) / float64(p-v.lastT); speed <= MaxSpeed {
			key := [2]string{prev.ID, st.ID}
			k.segments[key] = append(k.segments[key], traversal{p, speed})
		}
	}
	v.lastIdx, v.lastT = i, p
	if i < firstGap {
		return
	}
	key := passKey{v.line, v.headsign, st.ID}
	ps := k.passes[key]
	for _, x := range ps {
		if x.veh == id && p-x.t < minRepeatS && x.t-p < minRepeatS {
			return
		}
	}
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

// Segments lists the segments of shape (its stops as the tracker last saw them) with traversals
// by any line that ended in (now − Window, now], in order along the shape, with their count and
// median speed.
func (k *Tracker) Segments(shape string, now int64) []SegmentObs {
	from := now - int64(Window.Seconds())
	k.mu.Lock()
	defer k.mu.Unlock()
	stops := k.shapes[shape]
	var out []SegmentObs
	var speeds []float64
	for i := 1; i < len(stops); i++ {
		a, b := stops[i-1], stops[i]
		speeds = speeds[:0]
		for _, x := range k.segments[[2]string{a.ID, b.ID}] {
			if x.end > from && x.end <= now {
				speeds = append(speeds, x.speed)
			}
		}
		if len(speeds) > 0 {
			out = append(out, SegmentObs{From: a.ID, To: b.ID, FromAlong: a.Along, ToAlong: b.Along,
				N: len(speeds), MedianMS: median(speeds)})
		}
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

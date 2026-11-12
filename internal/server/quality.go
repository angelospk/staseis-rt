package server

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/geo"
	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/model"
	"github.com/angelospk/athens-transit-rt/internal/quality"
	"github.com/angelospk/athens-transit-rt/internal/segspeed"
	"github.com/angelospk/athens-transit-rt/internal/usual"
)

// Live quality (contract rev 23, docs/QUALITY.md "Live levels"): every matched fix of a poll
// feeds a.live, which finds the stop passes; a vehicle gets its level at the poll, a segment
// when a client reads the line's vehicles. Nothing here asks OASA for more.

// usualCache holds the usual rows of the stops looked up in one local hour: Query reads a block
// of every training day from disk, and a stop's usual headway does not change within the hour.
type usualCache struct {
	mu    sync.Mutex
	store *usual.Store
	hour  string
	days  int
	rows  map[string][]usual.Row
}

// usualHeadway is the usual service of a line and headsign at a stop now. Unknown (Known false)
// without a store or with fewer than usual.MinDays training days of today's day type.
func (a *App) usualHeadway(stop, line, headsign string, now time.Time) quality.Usual {
	st := a.usual.Load()
	if st == nil {
		return quality.Usual{}
	}
	c := &a.usualRows
	hour := now.In(gtfs.Athens).Format("2006-01-02T15")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store != st || c.hour != hour {
		c.store, c.hour, c.days, c.rows = st, hour, st.TrainingDays(now), map[string][]usual.Row{}
	}
	if c.days < usual.MinDays {
		return quality.Usual{}
	}
	rows, ok := c.rows[stop]
	if !ok {
		rows = st.Query(stop, now)
		c.rows[stop] = rows
	}
	for _, r := range rows {
		if r.Line == line && r.Headsign == headsign && r.HeadwayMin != nil {
			return quality.Usual{Known: true, HeadwayS: *r.HeadwayMin * 60}
		}
	}
	return quality.Usual{Known: true}
}

// liveStops is every stop of a trip's pattern with its place along the trip's shape (cached per
// shape and pattern; call with a.matchMu held).
func (w *world) liveStops(t *gtfs.Trip, g *geo.Geometry) []quality.Stop {
	k := [2]int32{t.Shape, t.Pattern}
	if s, ok := w.qstops[k]; ok {
		return s
	}
	pat := w.feed.Patterns[t.Pattern].Stops
	out := make([]quality.Stop, 0, len(pat))
	for i, s := range pat {
		if i < len(g.StopAlong) {
			out = append(out, quality.Stop{ID: w.feed.Stops[s].ID, Along: g.StopAlong[i]})
		}
	}
	if w.qstops == nil {
		w.qstops = map[[2]int32][]quality.Stop{}
	}
	w.qstops[k] = out
	return out
}

// liveQuality feeds the matched fixes of a poll to the tracker and rates each vehicle. A vehicle
// not matched, waiting at its first stop or not placed on its trip's shape is unknown. Call with
// a.matchMu held, after the daily shifts (delay_s is final).
func (a *App) liveQuality(w *world, results []match.Result, vehicles []Vehicle) []quality.VehicleLevel {
	out := make([]quality.VehicleLevel, len(results))
	now := a.now()
	for i := range results {
		r := &results[i]
		out[i] = quality.VehicleLevel{Level: quality.Unknown}
		if !r.Matched() || r.Waiting || vehicles[i].AlongM == nil {
			continue
		}
		t := w.feed.Trip(r.Trip)
		g := w.matcher.Geometry(t)
		if t.Shape < 0 || g == nil || !g.FromShape {
			continue
		}
		line, head := w.feed.Line(t), w.feed.Headsign(t)
		a.live.Observe(quality.Fix{Vehicle: r.VehicleID, Trip: t.ID, Line: line, Headsign: head,
			Shape: w.feed.Shapes[t.Shape].ID, Stops: w.liveStops(t, g), S: r.Along, T: r.Time.Unix()})
		st, ok := a.live.State(r.VehicleID, t.ID)
		if !ok || st.RefStop == "" {
			continue
		}
		out[i] = quality.Vehicle(a.usualHeadway(st.RefStop, line, head, now), st.Gap, vehicles[i].DelayS)
	}
	return out
}

// level is the live level of result i, nil when the line data has none.
func (d *lineData) level(i int) *quality.VehicleLevel {
	if i < len(d.quality) {
		return &d.quality[i]
	}
	return nil
}

// qualityV2 is a vehicle level in the /v2 shape, stop ids namespaced.
func qualityV2(q quality.VehicleLevel) *model.VehicleQuality {
	out := &model.VehicleQuality{Level: string(q.Level), Bunched: q.Bunched, Early: q.Early}
	if q.Basis != "" {
		out.Basis = model.Ptr(q.Basis)
	}
	if q.Basis == quality.BasisHeadway {
		out.UsualHeadwayS = model.Ptr(q.UsualHeadwayS)
		if q.Stop != "" {
			out.GapS, out.Stop = model.Ptr(q.GapS), model.Ptr(oasaPrefix+q.Stop)
		}
	}
	return out
}

// lineSegments is the live level of every segment of the line's shapes crossed within
// quality.Window before now, by pattern and place along it.
func (a *App) lineSegments(w *world, line string, now time.Time) []model.Segment {
	f := w.feed
	var ids []string
	for s := range w.shapeDirs(line) {
		if s >= 0 && int(s) < len(f.Shapes) {
			ids = append(ids, f.Shapes[s].ID)
		}
	}
	sort.Strings(ids)
	tab := a.segs.Load()
	if tab.Feed() != worldVersion(w) {
		tab = nil
	}
	slot := segspeed.Slot(now, gtfs.Athens)
	out := []model.Segment{}
	for _, id := range ids {
		for _, o := range a.live.Segments(id, now.Unix()) {
			seg := model.Segment{Pattern: oasaPrefix + id, From: oasaPrefix + o.From, To: oasaPrefix + o.To,
				Level: string(quality.Unknown), Speed: round1(o.MedianMS), N: o.N}
			if u, ok := tab.Between(id, o.FromAlong, o.ToAlong, slot); ok && u > 0 {
				ratio := o.MedianMS / u
				seg.Ratio, seg.UsualSpeed = model.Ptr(math.Round(ratio*100)/100), model.Ptr(round1(u))
				if o.N >= quality.MinTraversals {
					seg.Level = string(quality.SegmentLevel(ratio))
				}
			}
			out = append(out, seg)
		}
	}
	return out
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

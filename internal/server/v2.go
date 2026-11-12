package server

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/model"
)

// The /v2 API (docs/CONTRACT-rev8-draft.md): one model for every mode. Each Source has one
// Adapter; the handlers route an id to the adapter of its namespace and only encode.

const (
	v2ListMaxAge    = 3600 // lines, stops, line detail
	v2RunMaxAge     = 15
	v2IncompleteAge = 5  // a response with a Source warming up or down
	v2ETagFrom      = 60 // ETag + 304 for complete responses cached at least this long
)

var (
	errUnknown = errors.New("unknown id")
	errWarming = errors.New("source warming up")
)

// Kind is what an id names, for Resolve.
type Kind int

const (
	KindLine Kind = iota
	KindStop
	KindRun
)

// LineDetail is the body of /v2/lines/{id} without its sources.
type LineDetail struct {
	Line     model.Line
	Patterns []model.Pattern
	Stops    map[string]model.Stop
}

// LineVehicles is a line's vehicles now and when they are next updated.
type LineVehicles struct {
	Vehicles     []model.Vehicle
	Segments     []model.Segment // rev 23: live segment levels; nil for Sources without them
	UpdatedAt    int64
	NextUpdateAt int64
	MaxAge       int
}

// Adapter turns one Source's data into the model. Lookups return errUnknown for an id the
// Source does not know and errWarming while it has nothing loaded yet; ids are current ids (the
// handler resolves aliases first).
type Adapter interface {
	Namespace() string
	State(now time.Time) model.SourceState
	Resolve(kind Kind, id string) string
	Lines() ([]model.Line, error)
	LineInfo(id string) (*model.Line, error)
	Line(id string) (*LineDetail, error)
	Stops() ([]model.Stop, error)
	Stop(id string) (*model.Stop, error)
	// Arrivals may ask the Source for more (a line watched, a stream opened).
	Arrivals(ctx context.Context, stopID string, now time.Time) ([]model.Arrival, error)
	LineVehicles(id string, now time.Time) (*LineVehicles, error)
	Vehicles(now time.Time) ([]model.Vehicle, error)
	Run(id string, now time.Time) (*model.Run, error)
}

// adapter is the Adapter of an id's namespace, nil when none.
func (a *App) adapter(id string) Adapter {
	ns := model.Namespace(id)
	for _, ad := range a.adapters {
		if ad.Namespace() == ns && len(id) > len(ns) {
			return ad
		}
	}
	return nil
}

func (a *App) states(now time.Time, ads ...Adapter) []model.SourceState {
	out := make([]model.SourceState, len(ads))
	for i, ad := range ads {
		out[i] = ad.State(now)
	}
	return out
}

// lineStater is an Adapter whose state depends on the lines of the request: an expired timetable
// of one line is not that of another.
type lineStater interface {
	StateOf(now time.Time, lines []string) model.SourceState
}

// statesOf is states for one adapter, with the lines of the request.
func (a *App) statesOf(now time.Time, ad Adapter, lines []string) []model.SourceState {
	if ls, ok := ad.(lineStater); ok {
		return []model.SourceState{ls.StateOf(now, lines)}
	}
	return a.states(now, ad)
}

func (a *App) routeV2(mux *http.ServeMux) {
	mux.HandleFunc("GET /v2/lines", a.handleV2Lines)
	mux.HandleFunc("GET /v2/lines/{id}", a.handleV2Line)
	mux.HandleFunc("GET /v2/lines/{id}/vehicles", a.handleV2LineVehicles)
	mux.HandleFunc("GET /v2/stops", a.handleV2Stops)
	mux.HandleFunc("GET /v2/stops/{id}", a.handleV2Stop)
	mux.HandleFunc("GET /v2/vehicles", a.handleV2Vehicles)
	mux.HandleFunc("GET /v2/vehicles/tiles/{z}/{x}/{y}", a.handleV2Tile)
	mux.HandleFunc("GET /v2/vehicles/rail", a.handleV2RailVehicles)
	mux.HandleFunc("GET /v2/vehicles/suburban", a.handleV2SuburbanVehicles)
	mux.HandleFunc("GET /v2/runs/{id}", a.handleV2Run)
	mux.HandleFunc("GET /v2/alerts", a.handleV2Alerts)
	mux.HandleFunc("GET /v2/plan", a.handleV2Plan)
	mux.HandleFunc("GET /v2/municipal", a.handleV2Municipal)
}

// writeV2 writes a /v2 body: a response with a Source warming up or down is cached at most 5 s
// with no ETag; a complete one cached a minute or more gets an ETag and answers 304.
func writeV2(w http.ResponseWriter, r *http.Request, maxAge int, sources []model.SourceState, v any) {
	if !model.Complete(sources) {
		writeJSON(w, http.StatusOK, min(maxAge, v2IncompleteAge), v)
		return
	}
	if maxAge < v2ETagFrom {
		writeJSON(w, http.StatusOK, maxAge, v)
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(b)
	// Weak: the same body gzipped by the Handler or not.
	tag := `"` + hex.EncodeToString(sum[:8]) + `"`
	h := w.Header()
	h.Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
	h.Set("ETag", "W/"+tag)
	if etagMatch(r.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}

func notFound(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusNotFound, unknownMaxAge, map[string]string{"error": code})
}

// lookup resolves a path id to its adapter and current id; ok is false when no adapter has it.
func (a *App) lookup(r *http.Request, kind Kind) (Adapter, string, bool) {
	id := r.PathValue("id")
	ad := a.adapter(id)
	if ad == nil {
		return nil, "", false
	}
	return ad, ad.Resolve(kind, id), true
}

func (a *App) handleV2Lines(w http.ResponseWriter, r *http.Request) {
	now := a.now()
	out := []model.Line{}
	for _, ad := range a.adapters {
		if ls, err := ad.Lines(); err == nil {
			out = append(out, ls...)
		}
	}
	src := a.states(now, a.adapters...)
	writeV2(w, r, v2ListMaxAge, src, map[string]any{"lines": out, "sources": src})
}

func (a *App) handleV2Line(w http.ResponseWriter, r *http.Request) {
	ad, id, ok := a.lookup(r, KindLine)
	if !ok {
		notFound(w, "unknown_line")
		return
	}
	d, err := ad.Line(id)
	if errors.Is(err, errUnknown) {
		notFound(w, "unknown_line")
		return
	}
	src := a.statesOf(a.now(), ad, []string{id})
	body := map[string]any{"line": nil, "patterns": []model.Pattern{}, "stops": map[string]model.Stop{}, "sources": src}
	if err == nil {
		body["line"], body["patterns"], body["stops"] = d.Line, d.Patterns, d.Stops
	}
	writeV2(w, r, v2ListMaxAge, src, body)
}

func (a *App) handleV2LineVehicles(w http.ResponseWriter, r *http.Request) {
	ad, id, ok := a.lookup(r, KindLine)
	if !ok {
		notFound(w, "unknown_line")
		return
	}
	now := a.now()
	lv, err := ad.LineVehicles(id, now)
	if errors.Is(err, errUnknown) {
		notFound(w, "unknown_line")
		return
	}
	if err != nil {
		lv = &LineVehicles{UpdatedAt: now.Unix(), NextUpdateAt: now.Unix() + minMaxAge, MaxAge: minMaxAge}
	}
	if lv.Vehicles == nil {
		lv.Vehicles = []model.Vehicle{}
	}
	if lv.Segments == nil {
		lv.Segments = []model.Segment{}
	}
	src := a.statesOf(now, ad, []string{id})
	writeV2(w, r, lv.MaxAge, src, map[string]any{"line": id, "updated_at": lv.UpdatedAt,
		"next_update_at": lv.NextUpdateAt, "vehicles": lv.Vehicles, "segments": lv.Segments, "sources": src})
}

// v2StopsBody is the /v2/stops body, marshalled and gzipped once (758 KB raw for ~9000 stops)
// and reused while key holds.
type v2StopsBody struct {
	key string
	enc *encoded
}

// handleV2Stops serves the stop index from bytes built once and reused until the stops or a
// Source's state change. The updated_at of an ok or indicative Source moves with every poll
// and changes no stop, so it is left out of that key: the strong ETag (and 304) holds while
// the index stays the same, and the body carries the updated_at of its build. A stale
// Source's updated_at no longer moves, so it is in the key.
func (a *App) handleV2Stops(w http.ResponseWriter, r *http.Request) {
	out := []model.IndexEntry{}
	for _, ad := range a.adapters {
		if ss, err := ad.Stops(); err == nil {
			for _, s := range ss {
				out = append(out, s.Entry())
			}
		}
	}
	src := a.states(a.now(), a.adapters...)
	stops, err := json.Marshal(out)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, minMaxAge, map[string]string{"error": "internal"})
		return
	}
	sum := sha256.New()
	sum.Write(stops)
	for _, s := range src {
		fmt.Fprintf(sum, "|%s=%s", s.ID, s.State)
		if s.State == model.Stale && s.UpdatedAt != nil {
			fmt.Fprintf(sum, "@%d", *s.UpdatedAt)
		}
	}
	key := hex.EncodeToString(sum.Sum(nil))
	a.v2StopsMu.Lock()
	b := a.v2Stops
	if b == nil || b.key != key {
		enc, err := encode(gzip.NewWriter(nil), map[string]any{"stops": json.RawMessage(stops), "sources": src})
		if err != nil {
			a.v2StopsMu.Unlock()
			writeJSON(w, http.StatusInternalServerError, minMaxAge, map[string]string{"error": "internal"})
			return
		}
		b = &v2StopsBody{key, enc}
		a.v2Stops = b
	}
	a.v2StopsMu.Unlock()
	if !model.Complete(src) {
		serveEncoded(w, r, b.enc, v2IncompleteAge, false)
		return
	}
	serveEncoded(w, r, b.enc, v2ListMaxAge, true)
}

func (a *App) handleV2Stop(w http.ResponseWriter, r *http.Request) {
	ad, id, ok := a.lookup(r, KindStop)
	if !ok {
		notFound(w, "unknown_stop")
		return
	}
	now := a.now()
	st, err := ad.Stop(id)
	if errors.Is(err, errUnknown) {
		notFound(w, "unknown_stop")
		return
	}
	lines, arr, us := []model.Line{}, []model.Arrival{}, []model.Usual{}
	if err == nil {
		for _, l := range st.Lines {
			if li, err := ad.LineInfo(l); err == nil {
				lines = append(lines, *li)
			}
		}
		if got, err := ad.Arrivals(r.Context(), id, now); err == nil && got != nil {
			arr = got
		}
		if u, ok := ad.(usualRower); ok {
			if got := u.Usual(id, now, arr); got != nil {
				us = got
			}
		}
	}
	ids := []string(nil)
	if st != nil {
		ids = st.Lines
	}
	src := a.statesOf(now, ad, ids)
	writeV2(w, r, stopMaxAge, src, map[string]any{"stop": st, "lines": lines, "arrivals": arr, "usual": us,
		"updated_at": now.Unix(), "sources": src})
}

func (a *App) handleV2Run(w http.ResponseWriter, r *http.Request) {
	ad, id, ok := a.lookup(r, KindRun)
	if !ok {
		notFound(w, "unknown_run")
		return
	}
	now := a.now()
	run, err := ad.Run(id, now)
	if errors.Is(err, errUnknown) {
		notFound(w, "unknown_run")
		return
	}
	ids := []string(nil)
	if run != nil {
		ids = []string{run.Line}
	}
	src := a.statesOf(now, ad, ids)
	writeV2(w, r, v2RunMaxAge, src, map[string]any{"run": run, "sources": src})
}

// v2Response is the body of /v2/vehicles and its tiles.
type v2Response[V any] struct {
	UpdatedAt    int64               `json:"updated_at"`
	NextUpdateAt int64               `json:"next_update_at"`
	Vehicles     []V                 `json:"vehicles"`
	Sources      []model.SourceState `json:"sources"`
}

// v2Snapshot is the encoded /v2/vehicles body and tiles, built like the /v1 snapshot.
type v2Snapshot struct {
	built    time.Time
	life     time.Duration // reused this long: vehiclesEvery, or v2IncompleteAge when incomplete
	complete bool
	all      *encoded
	tiles    map[tileKey]*encoded
	empty    *encoded
}

// adaptersIn are the Sources with these namespaces, in their usual order.
func (a *App) adaptersIn(ns ...string) []Adapter {
	var out []Adapter
	for _, ad := range a.adapters {
		if slices.Contains(ns, ad.Namespace()) {
			out = append(out, ad)
		}
	}
	return out
}

// busAdapters are the Sources of /v2/vehicles and its tiles: oasa only, so that map traffic
// never opens the suburban stream.
func (a *App) busAdapters() []Adapter { return a.adaptersIn(oasaNS) }

// handleV2RailVehicles: metro and tram, placed from the STASY timetable, built per request
// (about 60 trains). It never opens the suburban stream, so a map that shows the metro does
// not keep the railway.gov.gr stream open.
func (a *App) handleV2RailVehicles(w http.ResponseWriter, r *http.Request) {
	a.serveV2Trains(w, r, a.adaptersIn(stasyNS), stasyVehMaxAge)
}

// handleV2SuburbanVehicles: suburban trains from the railway.gov.gr stream, which this request
// opens (it closes 3 min after the last one). Clients load it only while their suburban layer
// is on and a corridor is in view.
func (a *App) handleV2SuburbanVehicles(w http.ResponseWriter, r *http.Request) {
	a.serveV2Trains(w, r, a.adaptersIn(subNS), subVehMaxAge)
}

// serveV2Trains builds one body of the trains of ads. moving is the max-age while there are
// trains (GPS trains move every second); with none, stasyVehMaxAge.
func (a *App) serveV2Trains(w http.ResponseWriter, r *http.Request, ads []Adapter, moving int) {
	now := a.now()
	vs := []model.Vehicle{}
	for _, ad := range ads {
		if got, err := ad.Vehicles(now); err == nil {
			vs = append(vs, got...)
		}
	}
	maxAge := stasyVehMaxAge
	if len(vs) > 0 {
		maxAge = moving
	}
	src := a.states(now, ads...)
	if !model.Complete(src) {
		maxAge = min(maxAge, v2IncompleteAge)
	}
	writeV2(w, r, maxAge, src, v2Response[model.Vehicle]{now.Unix(), now.Unix() + int64(maxAge), vs, src})
}

func (a *App) v2Vehicles() (*v2Snapshot, error) {
	a.v2Mu.Lock()
	defer a.v2Mu.Unlock()
	now := a.now()
	if s := a.v2Snap; s != nil && now.Sub(s.built) < s.life && !now.Before(s.built) {
		return s, nil
	}
	ads := a.busAdapters()
	vs := []model.Vehicle{}
	for _, ad := range ads {
		if got, err := ad.Vehicles(now); err == nil {
			vs = append(vs, got...)
		}
	}
	src := a.states(now, ads...)
	// An incomplete snapshot lives only as long as its max-age, so a recovered Source shows.
	life := vehiclesEvery
	if !model.Complete(src) {
		life = v2IncompleteAge * time.Second
	}
	next := now.Add(life).Unix()
	zw := gzip.NewWriter(nil)
	all, err := encode(zw, v2Response[model.Vehicle]{now.Unix(), next, vs, src})
	if err != nil {
		return nil, err
	}
	s := &v2Snapshot{built: now, life: life, complete: model.Complete(src), all: all, tiles: map[tileKey]*encoded{}}
	detail := map[tileKey][]model.Vehicle{}
	overview := map[tileKey][]model.LiteVehicle{}
	for _, v := range vs {
		k := tileKey{z: tileDetailZ}
		if k.x, k.y = tileOf(v.Position.Lat, v.Position.Lon, k.z); !inTileRange(k) {
			continue
		}
		detail[k] = append(detail[k], v)
		k = tileKey{z: tileOverviewZ}
		k.x, k.y = tileOf(v.Position.Lat, v.Position.Lon, k.z)
		overview[k] = append(overview[k], v.Lite())
	}
	for k, vs := range detail {
		if s.tiles[k], err = encode(zw, v2Response[model.Vehicle]{now.Unix(), next, vs, src}); err != nil {
			return nil, err
		}
	}
	for k, vs := range overview {
		if s.tiles[k], err = encode(zw, v2Response[model.LiteVehicle]{now.Unix(), next, vs, src}); err != nil {
			return nil, err
		}
	}
	if s.empty, err = encode(zw, v2Response[model.Vehicle]{now.Unix(), next, []model.Vehicle{}, src}); err != nil {
		return nil, err
	}
	a.v2Snap = s
	return s, nil
}

func (a *App) handleV2Vehicles(w http.ResponseWriter, r *http.Request) {
	a.serveV2Snapshot(w, r, func(s *v2Snapshot) *encoded { return s.all })
}

func (a *App) handleV2Tile(w http.ResponseWriter, r *http.Request) {
	k, ok := tileRequest(w, r)
	if !ok {
		return
	}
	a.serveV2Snapshot(w, r, func(s *v2Snapshot) *encoded {
		if e := s.tiles[k]; e != nil {
			return e
		}
		return s.empty
	})
}

// serveV2Snapshot serves prebuilt bytes like serveSnapshot; an incomplete snapshot is cached
// at most 5 s, with no ETag.
func (a *App) serveV2Snapshot(w http.ResponseWriter, r *http.Request, pick func(*v2Snapshot) *encoded) {
	s, err := a.v2Vehicles()
	if err != nil {
		a.log.Error("building /v2/vehicles", "err", err)
		writeJSON(w, http.StatusInternalServerError, minMaxAge, map[string]string{"error": "internal"})
		return
	}
	maxAge := clamp(int64(s.built.Add(s.life).Sub(a.now())/time.Second), minMaxAge, vehiclesMaxAge)
	if !s.complete {
		maxAge = min(maxAge, v2IncompleteAge)
	}
	serveEncoded(w, r, pick(s), maxAge, s.complete)
}

// sortLines orders a Stop's Lines by mode, then label (or name).
func sortLines(ids []string, info func(string) (mode, key string)) {
	sort.SliceStable(ids, func(i, j int) bool {
		mi, ki := info(ids[i])
		mj, kj := info(ids[j])
		if model.ModeRank(mi) != model.ModeRank(mj) {
			return model.ModeRank(mi) < model.ModeRank(mj)
		}
		return ki < kj
	})
}

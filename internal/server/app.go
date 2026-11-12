// Package server runs the live service: it keeps the static GTFS snapshot current, polls
// OASA through the scheduler, matches vehicles to trips and serves the contract API.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	rt "github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/angelospk/athens-transit-rt/internal/alerts"
	"github.com/angelospk/athens-transit-rt/internal/feed"
	"github.com/angelospk/athens-transit-rt/internal/geo"
	"github.com/angelospk/athens-transit-rt/internal/gtfs"
	"github.com/angelospk/athens-transit-rt/internal/history"
	"github.com/angelospk/athens-transit-rt/internal/livespeed"
	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/meta"
	"github.com/angelospk/athens-transit-rt/internal/municipal"
	"github.com/angelospk/athens-transit-rt/internal/oseth"
	"github.com/angelospk/athens-transit-rt/internal/quality"
	"github.com/angelospk/athens-transit-rt/internal/rail"
	"github.com/angelospk/athens-transit-rt/internal/release"
	"github.com/angelospk/athens-transit-rt/internal/sched"
	"github.com/angelospk/athens-transit-rt/internal/segspeed"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
	"github.com/angelospk/athens-transit-rt/internal/usual"
)

const (
	ExpiryWarningDays = 7
	feedMaxAge        = 5 * time.Minute // vehicles older than this leave the GTFS-RT feeds
	lineKeep          = 5 * time.Minute // results of a line that stopped being polled
)

type Config struct {
	Listen        string
	MetricsListen string // loopback address for /metrics ("" = off)
	StateDir      string
	RPS           float64
	Matcher       match.Kind
	ReleaseURL    string        // snapshot release base URL ("" = release.DefaultBaseURL)
	GTFSZip       string        // load this zip instead of the release snapshot (local use)
	GTFSCKANURL   string        // CKAN package_show URL of the city's GTFS ("" = the city's default)
	TelematicsURL string        // "" = the city's telematics API
	OSETHRPS      float64       // OSETH requests per second for Thessaloniki (0 = off; Athens ignores it)
	City          string        // "athens" or "thessaloniki" ("" = athens)
	OASTHSalt     string        // secret of the OASTH API token (Thessaloniki); never logged
	STASYURL      string        // STASY timetable catalogue ("" = rail.DefaultSTASYCatalog)
	SuburbanURL   string        // railway.gov.gr base URL ("" = rail.DefaultRailwayURL)
	MPassURL      string        // ose.mpass.ltd API base URL ("" = rail.DefaultMPassURL)
	Proxy         string        // proxy URL for OASA requests, e.g. socks5h://127.0.0.1:40001 ("" = direct)
	ReleaseCheck  time.Duration // how often to look for a new snapshot
	Sched         sched.Config
	// Fix history (docs/superpowers/specs/2026-10-06-fix-history.md): off when HistoryMaxBytes is 0.
	HistoryMaxBytes int64
	HistoryKeep     time.Duration
}

type world struct {
	feed      *gtfs.Feed
	manifest  *release.Manifest
	matcher   *match.Matcher
	mapper    *match.RouteMapper
	stopLines map[int32][]string // stop index -> the lines calling there

	modesOnce sync.Once
	modes     map[string]string // line -> mode (/v2), built on first use
	dirsOnce  sync.Once
	dirs      map[string]map[int32]int    // line -> shape -> direction (0/1), built on first use
	qstops    map[[2]int32][]quality.Stop // (shape, pattern) -> stops along it, live quality; guarded by App.matchMu
}

// lineData is the last published state of one line.
type lineData struct {
	updated  time.Time
	feed     *gtfs.Feed
	results  []match.Result
	vehicles []Vehicle
	board    []boardPos             // per result, for the stop boards
	daily    []int64                // per result, its run's shift to OASA's daily schedule (dailyShifts); nil: none
	dirs     map[int32]int          // shape -> direction (0/1) on feed, kept so a feed swap does not lose it
	quality  []quality.VehicleLevel // per result, its live level (rev 23)
}

type App struct {
	cfg    Config
	log    *slog.Logger
	now    func() time.Time
	pacer  *telematics.Pacer
	client *telematics.Client
	sched  *sched.Scheduler
	meta   *meta.Store
	fetch  *release.Fetcher
	city   City
	// lineAliasFn: Thessaloniki's spelling of a line id (the GTFS one); set with each feed.
	lineAliasFn atomic.Pointer[func(string) string]

	matchMu sync.Mutex // serialises matching (matchers are not concurrency-safe) and motion
	motion  *motion
	roads   *livespeed.Grid  // recent speeds per road piece, from every line
	live    *quality.Tracker // stop passes of the last minutes, for the live quality (rev 23)

	mu           sync.RWMutex
	w            *world
	lines        map[string]*lineData
	known        map[string]bool
	warming      map[string]bool // scheduled now, but its route codes are not known yet
	active       int
	lastPublish  time.Time
	lastOK       time.Time
	warnedDay    string
	lastRequests int64
	seenErrors   int64 // upstream errors already reported by warnErrors
	seenOSETH    int64

	rtMu    sync.Mutex
	rtBuilt time.Time
	rtFeeds map[string][]byte

	vehMu   sync.Mutex // lock order: vehMu, then mu
	vehSnap *vehiclesSnapshot

	v2StopsMu sync.Mutex // the /v2/stops body, rebuilt when its stops or Source states change
	v2Stops   *v2StopsBody

	adapters []Adapter  // the /v2 Sources, in the order of their "sources" entries
	v2Mu     sync.Mutex // lock order: v2Mu, then mu
	v2Snap   *v2Snapshot

	hist *history.Writer                // nil: no fix history
	segs atomic.Pointer[segspeed.Table] // typical speeds from the history; nil until built
	// usual holds the stop passes of past days (/v2/stops/{id} `usual`); nil without history.
	usual     atomic.Pointer[usual.Store]
	usualRows usualCache // the usual rows of the stops looked up this hour (live quality)
	// histBuild runs one history build at a time (typical speeds, usual passes): each holds
	// tens of MB while it reads.
	histBuild sync.Mutex

	stasy    *rail.STASYStore
	suburban *rail.Suburban
	trains   *rail.TrainStream // live Proastiakos positions, read only while clients ask

	// Second live source of Thessaloniki (OSETH), on its own budget; nil elsewhere or when off.
	oseth      *oseth.Client
	osethMu    sync.Mutex            // guards the OSETH state below
	osethPub   sync.Mutex            // one OSETH-aware publication at a time
	osethLines map[string]*osethLine // line -> its latest OSETH state
	osethLast  map[string]time.Time  // line -> start of its last OSETH poll
	osethRR    int                   // index of the last line picked, for the round-robin

	planner    *planner     // /v2/plan: the Timetable of the service day and the plan slots
	prefetched atomic.Value // string: the date whose daily schedules the prefetch has fetched

	alerts    *alerts.Poller // /v2/alerts, polled every 10 min
	alertHTTP *http.Client   // the alert sources

	municipal     *municipal.Poller // /v2/municipal, OSM via Overpass, weekly
	municipalBody municipalCache

	starts  *runStarts // buses leaving their start: seen flags of the daily timetable
	seenMu  sync.Mutex
	seen    map[string]bool // trips the matcher has assigned a vehicle to, today
	seenDay string

	daily      dailyFunc  // a request waits: urgent scheduler slots, at most maxQueued
	dailyBg    dailyFunc  // the prefetch: background slots, after the polls and the metadata
	dailyMu    sync.Mutex // guards dailyCache and dailyPrev only; never held during a request
	dailyCache map[string]dailyEntry
	// dailyPrev is the entry a newer day replaced in dailyCache: from 00:00 to 03:00 the planner
	// still plans the day before, while /schedule fetches the calendar day (#128).
	dailyPrev  map[string]dailyEntry
	dailyCalls map[string]*dailyCall // fetches in flight, by line

	stopArrivals func(ctx context.Context, stopCode string) ([]telematics.Arrival, error)
	arrMu        sync.Mutex // guards arrCache and arrCalls only; never held during a request
	arrCache     map[string]arrivalsEntry
	arrCalls     map[string]*arrivalsCall // OASA calls in flight, by stop

	queuedN atomic.Int32 // OASA calls queued by requests and not yet run or dropped

	mcpOnce    sync.Once
	mcpSrv     *mcp.Server
	apiOnce    sync.Once
	apiHandler http.Handler // the app's own REST handler, for the MCP tools
}

func New(cfg Config, log *slog.Logger) *App {
	a := &App{cfg: cfg, log: log, now: time.Now, lines: map[string]*lineData{}, known: map[string]bool{},
		roads: livespeed.New(), live: quality.NewTracker(), starts: newRunStarts()}
	a.motion = newMotion(a.roads)
	a.pacer = telematics.NewPacer(cfg.RPS)
	a.pacer.Log = log
	c, err := CityByID(cfg.City)
	if err != nil {
		log.Error("unknown city, serving Athens", "err", err)
		c, _ = CityByID("athens")
	}
	a.city = c
	tURL := cfg.TelematicsURL
	if tURL == "" {
		tURL = c.TelematicsURL
	}
	a.client = telematics.New(tURL, nil) // the scheduler paces every request
	a.client.GTFSStopIDs = c.GTFSStopIDs
	if c.ID == "thessaloniki" {
		// The same http.Client, so the proxy set below applies to the session requests too.
		a.client.Auth = telematics.NewOASTHAuth(tURL, cfg.OASTHSalt, a.client.HTTP)
	}
	a.client.MinGap = a.pacer.BaseInterval()
	a.client.Stopped = a.pacer.Stopped
	if cfg.Proxy != "" {
		// Validated by the serve command; the Transport dials socks5/socks5h and http proxies.
		if u, err := url.Parse(cfg.Proxy); err == nil {
			a.client.SetTransport(&http.Transport{Proxy: http.ProxyURL(u), MaxIdleConnsPerHost: 8,
				IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second})
		}
	}
	a.daily = a.dailyQueued
	a.dailyBg = a.dailyBackground
	if !c.Rail {
		// OASTH has no getDailySchedule (HTTP 400): timetables come from the GTFS, as when OASA's is down.
		none := func(context.Context, string) (telematics.DailySchedule, error) {
			return telematics.DailySchedule{}, errNoDailySchedule
		}
		a.daily, a.dailyBg = none, none
	}
	a.stopArrivals = a.stopArrivalsQueued
	onLine := a.onLine
	if c.OSETHURL != "" && cfg.OSETHRPS > 0 {
		a.oseth = oseth.New(c.OSETHURL, cfg.OSETHRPS)
		a.osethLines = map[string]*osethLine{}
		a.osethLast = map[string]time.Time{}
		onLine = a.onLineWithOSETH
	}
	a.sched = sched.New(cfg.Sched, a.pacer, a.client, onLine)
	a.meta = meta.Open(filepath.Join(cfg.StateDir, "telematics-meta.json"), a.client, a.sched.Submit,
		func() { a.refreshLines() })
	if c.ID == "thessaloniki" {
		a.meta.FetchAllStops() // every stop needs its OASTH arrival code (rt#86)
	}
	base := cfg.ReleaseURL
	if base == "" {
		base = release.DefaultBaseURL
	}
	a.fetch = &release.Fetcher{BaseURL: base, Dir: cfg.StateDir, HTTP: &http.Client{Timeout: 2 * time.Minute}}
	stasyURL := cfg.STASYURL
	if stasyURL == "" {
		stasyURL = rail.DefaultSTASYCatalog
	}
	a.stasy = &rail.STASYStore{CatalogURL: stasyURL, Dir: filepath.Join(cfg.StateDir, "stasy"),
		HTTP: &http.Client{Timeout: 5 * time.Minute}, Log: log, Now: func() time.Time { return a.now() }}
	a.suburban = &rail.Suburban{BaseURL: cfg.SuburbanURL, MPassURL: cfg.MPassURL,
		// Well under the 30 s WriteTimeout of the HTTP server: the MPass fallback and the response
		// still have to fit after a dead primary upstream.
		HTTP: &http.Client{Timeout: 8 * time.Second}, Now: func() time.Time { return a.now() }}
	// The stream has no end, so no client Timeout; only the wait for its headers is bounded.
	stream := http.DefaultTransport.(*http.Transport).Clone()
	stream.ResponseHeaderTimeout = 15 * time.Second
	a.trains = &rail.TrainStream{BaseURL: cfg.SuburbanURL, HTTP: &http.Client{Transport: stream}, Log: log,
		Now: func() time.Time { return a.now() }}
	a.adapters = []Adapter{&oasaAdapter{a}}
	if a.rail() {
		a.adapters = append(a.adapters, &stasyAdapter{a: a}, &proastiakosAdapter{a: a, board: a.suburban, trains: a.trains})
	}
	a.planner = newPlanner()
	a.alertHTTP = &http.Client{Timeout: alertTimeout}
	a.alerts = a.newAlertsPoller()
	a.municipal = a.newMunicipalPoller()
	return a
}

var errNoDailySchedule = errors.New("this city's telematics has no daily schedule")

// rail reports whether the city has the Athens-only layers (STASY, Proastiakos, alerts, municipal).
func (a *App) rail() bool { return a.city.Rail }

// SetFeed installs a static feed (new matcher state; published results stay until replaced).
func (a *App) SetFeed(f *gtfs.Feed, m *release.Manifest) {
	if !a.rail() {
		// Before the mapper: it reads line numbers and route codes from the store.
		a.setLineAlias(lineAlias(f.Lines()))
	}
	mapper := match.NewRouteMapper(f, a.meta.Stops)
	mapper.ShapeIDsAreRouteCodes = a.rail() // Athens: OASA route codes are often the GTFS shape ids
	w := &world{feed: f, manifest: m, mapper: mapper, matcher: match.New(f, mapper, a.cfg.Matcher), stopLines: stopLines(f)}
	a.matchMu.Lock()
	a.mu.Lock()
	a.w = w
	a.mu.Unlock()
	a.motion = newMotion(a.roads) // positions along the old feed's shapes mean nothing now
	a.live.Reset()                // so do its stop passes: trip and shape ids may be reused for other geometry
	// Trip ids of the old feed mean nothing either. Cleared before matchMu opens, so a poll that
	// starts right after cannot record its matches into a map that is dropped a moment later.
	a.forgetSeen()
	a.matchMu.Unlock()
	a.vehMu.Lock()
	a.vehSnap = nil
	a.vehMu.Unlock()
	a.v2Mu.Lock()
	a.v2Snap = nil
	a.v2Mu.Unlock()
	a.log.Info("static GTFS loaded", "version", a.gtfsVersion(), "trips", len(f.Trips), "lines", len(f.Lines()))
	a.warnExpiry(true)
	a.refreshLines()
}

func (a *App) world() *world {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.w
}

func (a *App) gtfsVersion() string { return worldVersion(a.world()) }

// worldVersion is the GTFS version of a world, "" without one.
func worldVersion(w *world) string {
	if w == nil {
		return ""
	}
	if w.manifest != nil && w.manifest.GTFSVersion != "" {
		return w.manifest.GTFSVersion
	}
	return w.feed.Version()
}

func (a *App) gtfsExpires() string {
	w := a.world()
	if w == nil || w.feed.FeedEnd().IsZero() {
		return ""
	}
	return w.feed.FeedEnd().Format(time.DateOnly)
}

// warnExpiry logs once a day when the static GTFS expires within ExpiryWarningDays.
func (a *App) warnExpiry(force bool) {
	w := a.world()
	if w == nil {
		return
	}
	end := w.feed.FeedEnd()
	if end.IsZero() {
		return
	}
	today := gtfs.Midnight(a.now())
	day := today.Format(time.DateOnly)
	if !force && day == a.warnedDay {
		return
	}
	a.warnedDay = day
	left := int(end.Sub(today).Hours() / 24)
	switch {
	case left < 0:
		a.log.Warn("static GTFS expired; no trips can be matched until OASA publishes a new one", "end", end.Format(time.DateOnly))
	case left <= ExpiryWarningDays:
		a.log.Warn("static GTFS expires soon", "end", end.Format(time.DateOnly), "days_left", left)
	}
}

// lineSpec is what to poll of a line at plan: the line when it has trips now; of its route codes,
// those whose shapes run around this time of day on any day (the GTFS puts the "every Thursday"
// variants of some lines on Wednesday, #20; an empty route code backs off in the scheduler), and
// those that map to no shape.
func lineSpec(f *gtfs.Feed, mapper *match.RouteMapper, line string, plan time.Time, routes []meta.RouteRef) sched.LineSpec {
	cands := f.CandidateTrips(line, plan, match.CandidateBefore, match.CandidateAfter)
	spec := sched.LineSpec{ID: line, Active: len(cands) > 0 && len(routes) > 0}
	var around map[int32]bool
	if spec.Active {
		around = f.ShapesAround(line, plan, match.CandidateBefore, match.CandidateAfter)
	}
	for _, r := range routes {
		on := spec.Active
		if on {
			if shapes := mapper.ShapesFor(line, r.Code); len(shapes) > 0 {
				on = false
				for s := range shapes {
					on = on || around[s]
				}
			}
		}
		spec.Routes = append(spec.Routes, sched.Route{Code: r.Code, LineCode: r.LineCode, Active: on})
	}
	return spec
}

// refreshLines recomputes which lines and route codes have scheduled service now.
func (a *App) refreshLines() {
	w := a.world()
	if w == nil {
		return
	}
	now := a.now()
	routes := a.meta.LineRoutes()
	known := map[string]bool{}
	for _, l := range w.feed.Lines() {
		known[l] = true
	}
	for _, l := range a.meta.LineIDs() {
		known[l] = true
	}
	var specs []sched.LineSpec
	active := 0
	warming := map[string]bool{}
	bootstrapping := a.meta.Incomplete() // routes of some lines are not known yet
	plan := w.feed.PlanningTime(now)     // which lines run now, even when the feed expired
	for line := range known {
		spec := lineSpec(w.feed, w.mapper, line, plan, routes[line])
		if len(routes[line]) == 0 && bootstrapping && len(w.feed.CandidateTrips(line, plan, match.CandidateBefore, match.CandidateAfter)) > 0 {
			warming[line] = true
		}
		if spec.Active {
			active++
		}
		specs = append(specs, spec)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].ID < specs[j].ID })
	a.sched.SetLines(specs)
	a.mu.Lock()
	a.known, a.active, a.warming = known, active, warming
	a.mu.Unlock()
}

// onLine matches one finished line poll and publishes it.
func (a *App) onLine(p sched.LinePoll) {
	a.matchMu.Lock()
	w := a.world()
	if w == nil {
		a.matchMu.Unlock()
		return
	}
	results := w.matcher.MatchLine(p.Line, p.Obs)
	a.noteSeen(w, results, p.Done)
	vehicles := make([]Vehicle, len(results))
	board := make([]boardPos, len(results))
	ver := ""
	if a.hist != nil {
		ver = a.gtfsVersion()
	}
	for i := range results {
		vehicles[i], board[i] = a.vehicleView(w, &results[i])
	}
	// After vehicleView: it records the departures the shifts need.
	daily, runs := a.dailyShifts(w.feed, p.Line, results, p.Done)
	for i, s := range daily {
		if s != 0 {
			vehicles[i].DelayS = ptr(results[i].Delay - int(s))
		}
	}
	levels := a.liveQuality(w, results, vehicles)
	if a.hist != nil {
		for i := range results {
			row := historyRow(w, p.Line, &results[i], ver)
			if i < len(runs) && !runs[i].start.IsZero() {
				row.RunStart, row.StartDelayS = runs[i].start.Unix(), ptr(runs[i].late)
			}
			a.hist.Add(row)
		}
	}
	a.matchMu.Unlock()

	a.mu.Lock()
	defer a.mu.Unlock()
	if p.Failed == p.Routes {
		return // keep the previous data; nothing new is known
	}
	a.lines[p.Line] = &lineData{updated: p.Done, feed: w.feed, results: results, vehicles: vehicles, board: board, daily: daily,
		dirs: w.shapeDirs(p.Line), quality: levels}
	a.lastPublish, a.lastOK = p.Done, p.Done
}

// Vehicle is one entry of the /v1/lines/{id} response (docs/CONTRACT.md).
type Vehicle struct {
	ID         string       `json:"id"`
	Lat        float64      `json:"lat"`
	Lon        float64      `json:"lon"`
	Bearing    *float64     `json:"bearing"`
	PositionAt int64        `json:"position_at"`
	RouteCode  string       `json:"route_code"`
	Variant    *string      `json:"variant"`
	TripID     *string      `json:"trip_id"`
	TripLabel  *string      `json:"trip_label"`
	DelayS     *int         `json:"delay_s"`
	NextStopID *string      `json:"next_stop_id"`
	Speed      *float64     `json:"speed"`       // m/s along the route
	Path       [][2]float64 `json:"path"`        // [lat, lon] of the route ahead, to the next stop
	PathBeyond [][2]float64 `json:"path_beyond"` // rev 4: on from path's end to the third stop
	PathStops  []int        `json:"path_stops"`  // rev 4: metres along path + path_beyond of each stop
	PathSpeeds []*float64   `json:"path_speeds"` // rev 6: typical m/s per 200 m step ahead; rev 7: with today's traffic
	AtTerminus bool         `json:"at_terminus"` // matched and waiting at the first stop of its trip
	AlongM     *int         `json:"along_m"`     // rev 20: whole metres along the variant's shape at position_at
}

func ptr[T any](v T) *T { return &v }

// vehicleView is the vehicle's contract entry and its place for the stop boards.
func (a *App) vehicleView(w *world, r *match.Result) (Vehicle, boardPos) {
	f := w.feed
	v := Vehicle{ID: r.VehicleID, Lat: r.Lat, Lon: r.Lon, PositionAt: r.Time.Unix(), RouteCode: r.RouteCode}
	if r.Bearing != 0 { // 0 means "unknown" more often than "due north"
		v.Bearing = ptr(r.Bearing)
	}
	var along float64
	if !r.Matched() {
		s := bestShape(f, w.mapper.ShapesFor(r.Line, r.RouteCode), r.Line)
		if s >= 0 {
			v.Variant = ptr(f.Shapes[s].ID)
		}
		v.Speed, v.Path, v.PathBeyond, v.PathStops, v.PathSpeeds, along = a.motionOf(w, r, s)
		v.AlongM = alongM(v.Variant, along)
		return v, boardPosOf(w, r, s, along, v.Speed)
	}
	t := f.Trip(r.Trip)
	if t.Shape >= 0 {
		v.Variant = ptr(f.Shapes[t.Shape].ID)
	}
	v.Speed, v.Path, v.PathBeyond, v.PathStops, v.PathSpeeds, along = a.motionOf(w, r, t.Shape)
	if !r.Waiting { // waiting at its first stop: it stands there, not on along the shape
		v.AlongM = alongM(v.Variant, along)
	}
	v.TripID = ptr(t.ID)
	v.TripLabel = ptr(TripLabel(f, t))
	v.DelayS = ptr(r.Delay)
	v.NextStopID = ptr(f.Stops[feed.NextStop(f, r).Stop].ID)
	v.AtTerminus = r.Waiting
	return v, boardPosOf(w, r, t.Shape, r.Along, v.Speed)
}

// alongM is the contract's along_m: whole metres along the variant's shape, nil when the vehicle
// has no variant or is not placed on it (along < 0).
func alongM(variant *string, along float64) *int {
	if variant == nil || along < 0 {
		return nil
	}
	return ptr(int(math.Round(along)))
}

// motionOf records the vehicle's position along its shape and returns its speed, the path
// ahead, its continuation, the stops on them, (matched, with history) typical speeds ahead and
// its position along the shape line (-1 when not placed). A matched trip whose stops fit its
// shape has its position from the matcher; others are projected onto the shape when the pass is
// clear.
func (a *App) motionOf(w *world, r *match.Result, shape int32) (speed *float64, path, beyond [][2]float64, at []int, steps []*float64, along float64) {
	pos := geo.XY(r.Lat, r.Lon)
	if r.Matched() {
		if g := w.matcher.Geometry(w.feed.Trip(r.Trip)); g != nil && g.FromShape {
			if geo.Dist(pos, g.Line.At(r.Along)) > motionMaxOffM {
				return nil, nil, nil, nil, nil, -1
			}
			// Waiting to start: only up to the first stop (usually nothing: it is there).
			stops := g.StopAlong[r.NextIndex:]
			if r.Waiting {
				stops = g.StopAlong[:1]
			}
			speed = a.motion.observeAs(r.VehicleID, g.Line, r.Along, r.Time, !r.Waiting)
			a.starts.observe(w.feed, r.VehicleID, r.Line, shape, r.Along, g.Line.Length(), r.Time)
			path, beyond, at = pathAhead(g.Line, r.Along, speed, stops)
			if tab := a.segs.Load(); shape >= 0 && !r.Waiting && tab.Feed() == worldVersion(w) {
				steps = tab.Steps(w.feed.Shapes[shape].ID, r.Along, g.Line.Length(), segspeed.Slot(r.Time, gtfs.Athens), stepFallback(speed))
			}
			if !r.Waiting {
				steps = a.roads.Blend(steps, g.Line, r.Along, r.Time.Unix()) // today's traffic, any line
			}
			return speed, path, beyond, at, steps, r.Along
		}
	}
	if shape < 0 {
		return nil, nil, nil, nil, nil, -1
	}
	line := w.matcher.ShapeLine(shape)
	if len(line.XY) < 2 {
		return nil, nil, nil, nil, nil, -1
	}
	cands := line.Candidates(pos[0], pos[1], motionMaxOffM, r.Bearing, r.Bearing != 0)
	s, ok := a.motion.pick(r.VehicleID, line, cands, r.Time)
	if !ok {
		if atTerminus(cands, line.Length()) { // a loop's start, put at both of its ends
			a.starts.observe(w.feed, r.VehicleID, r.Line, shape, 0, line.Length(), r.Time)
		}
		return nil, nil, nil, nil, nil, -1
	}
	speed = a.motion.observeAs(r.VehicleID, line, s, r.Time, !r.Waiting)
	a.starts.observe(w.feed, r.VehicleID, r.Line, shape, s, line.Length(), r.Time)
	path, _, _ = pathAhead(line, s, speed, nil)
	return speed, path, nil, nil, nil, s
}

// historyRow is one fix for the history: trip, shape and position along it when matched.
func historyRow(w *world, line string, r *match.Result, ver string) history.Row {
	row := history.Row{FixT: r.Time.Unix(), Line: line, RouteCode: r.RouteCode, Veh: r.VehicleID,
		Lat: r.Lat, Lon: r.Lon, GTFS: ver, SM: -1}
	if !r.Matched() {
		return row
	}
	t := w.feed.Trip(r.Trip)
	row.TripID, row.DelayS = t.ID, ptr(r.Delay)
	if t.Shape >= 0 {
		row.ShapeID = w.feed.Shapes[t.Shape].ID
	}
	if g := w.matcher.Geometry(t); g != nil && g.FromShape {
		row.SM = history.SM(r.Along)
	}
	return row
}

// bestShape picks the shape with the most trips of the line among a route code's shapes.
func bestShape(f *gtfs.Feed, shapes map[int32]bool, line string) int32 {
	if len(shapes) == 0 {
		return -1
	}
	count := map[int32]int{}
	for _, ti := range f.TripsForLine(line) {
		if s := f.Trips[ti].Shape; shapes[s] {
			count[s]++
		}
	}
	best := int32(-1)
	for s, n := range count {
		if s >= 0 && (best < 0 || n > count[best] || (n == count[best] && s < best)) {
			best = s
		}
	}
	return best
}

// TripLabel is "HH:MM FIRST STOP → LAST STOP" (GTFS 24:35 shows as 00:35).
func TripLabel(f *gtfs.Feed, t *gtfs.Trip) string {
	first, last := f.StopTime(t, 0), f.StopTime(t, f.NumStops(t)-1)
	mins := first.Dep / 60
	return fmt.Sprintf("%02d:%02d %s → %s", mins/60%24, mins%60, f.Stops[first.Stop].Name, f.Stops[last.Stop].Name)
}

var latinToGreek = strings.NewReplacer("A", "Α", "B", "Β", "E", "Ε", "Z", "Ζ", "H", "Η", "I", "Ι", "K", "Κ",
	"M", "Μ", "N", "Ν", "O", "Ο", "P", "Ρ", "T", "Τ", "Y", "Υ", "X", "Χ")

// CanonicalLine normalises a line id as users type it (Latin look-alikes for Greek letters).
func CanonicalLine(raw string) string {
	return latinToGreek.Replace(strings.ToUpper(strings.TrimSpace(raw)))
}

// canonLine is CanonicalLine in Athens. Thessaloniki's GTFS mixes Latin and Greek letters
// (01A, 39Σ), so there a line id takes the feed's spelling.
func (a *App) canonLine(raw string) string {
	if a.rail() {
		return CanonicalLine(raw)
	}
	if f := a.lineAliasFn.Load(); f != nil {
		return (*f)(raw)
	}
	return strings.ToUpper(strings.TrimSpace(raw))
}

// setLineAlias installs the GTFS spelling of telematics line ids, for the metadata and for ids
// in requests.
func (a *App) setLineAlias(f func(string) string) {
	a.lineAliasFn.Store(&f)
	a.meta.SetLineAlias(f)
}

// arrivalCode is the stop code to ask getStopArrivals for. In Thessaloniki the GTFS id of a stop
// is another stop's code, so a stop no route list has mapped yet is not asked at all.
func (a *App) arrivalCode(stop string) (string, error) {
	c, ok := a.meta.ArrivalCode(stop)
	if !ok && a.client.GTFSStopIDs {
		return "", fmt.Errorf("stop %s: its telematics code is not known yet", stop)
	}
	return c, nil
}

// gtfsRT returns the encoded feeds, rebuilt at most every 10 s.
func (a *App) gtfsRT(name string) ([]byte, bool) {
	a.rtMu.Lock()
	defer a.rtMu.Unlock()
	now := a.now()
	if a.rtFeeds == nil || now.Sub(a.rtBuilt) >= 10*time.Second {
		feeds, err := a.buildRT(now)
		if err != nil {
			a.log.Error("building GTFS-RT", "err", err)
			return nil, false
		}
		a.rtFeeds, a.rtBuilt = feeds, now
	}
	b, ok := a.rtFeeds[name]
	return b, ok
}

func (a *App) buildRT(now time.Time) (map[string][]byte, error) {
	w := a.world()
	var results []match.Result
	a.mu.RLock()
	ids := make([]string, 0, len(a.lines))
	for id := range a.lines {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	at := map[string]int{} // vehicle -> index in results: a vehicle that changed lines appears once
	for _, id := range ids {
		d := a.lines[id]
		if w == nil || d.feed != w.feed {
			continue
		}
		for _, r := range d.results {
			if now.Sub(r.Time) > feedMaxAge {
				continue
			}
			if i, ok := at[r.VehicleID]; !ok {
				at[r.VehicleID] = len(results)
				results = append(results, r)
			} else if r.Time.After(results[i].Time) {
				results[i] = r
			}
		}
	}
	a.mu.RUnlock()
	var vp, tu *rt.FeedMessage
	if w == nil {
		vp, tu = feed.Build(&gtfs.Feed{}, nil, now)
	} else {
		vp, tu = feed.Build(w.feed, results, now)
	}
	out := map[string][]byte{}
	for name, msg := range map[string]*rt.FeedMessage{"vehicle_positions": vp, "trip_updates": tu} {
		pb, js, err := feed.Encode(msg)
		if err != nil {
			return nil, err
		}
		out[name+".pb"], out[name+".json"] = pb, js
	}
	return out, nil
}

// Run loads the static feed, then polls and serves until ctx ends.
func (a *App) Run(ctx context.Context) error {
	if err := a.loadInitial(ctx); err != nil {
		return err
	}
	if a.meta.Due() {
		a.meta.Refresh()
	}
	if a.cfg.HistoryMaxBytes > 0 {
		h, err := history.Open(filepath.Join(a.cfg.StateDir, "history"), a.cfg.HistoryMaxBytes, a.cfg.HistoryKeep, a.now, a.log)
		if err != nil {
			return err
		}
		a.matchMu.Lock()
		a.hist = h
		a.matchMu.Unlock()
		defer h.Close()
		go a.dailyStats(ctx)
		go a.typicalSpeeds(ctx)
		go a.usualPasses(ctx)
	}
	if a.rail() {
		if n, err := a.restoreDaily(); err != nil {
			a.log.Warn("daily schedule: restore", "err", err)
		} else if n > 0 {
			a.log.Info("daily schedule: restored from the archive", "lines", n)
		}
		go a.prefetchDaily(ctx)
	}
	srv := &http.Server{Addr: a.cfg.Listen, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- srv.ListenAndServe() }()
	var msrv *http.Server
	if a.cfg.MetricsListen != "" {
		msrv = &http.Server{Addr: a.cfg.MetricsListen, Handler: a.MetricsHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- msrv.ListenAndServe() }()
	}
	go a.sched.Run(ctx)
	go a.housekeeping(ctx)
	if a.oseth != nil {
		a.log.Info("OSETH live source on", "rps", a.oseth.Pacer.BaseRPS(), "url", a.oseth.BaseURL)
		go a.osethRun(ctx)
	}
	if a.rail() {
		go a.stasy.Run(ctx)
		go a.trains.Run(ctx)
		go a.alerts.Run(ctx)
	}
	go a.planLoop(ctx)
	if a.rail() {
		go a.municipal.Run(ctx)
	}
	a.log.Info("listening", "addr", a.cfg.Listen, "metrics", a.cfg.MetricsListen, "rps", a.pacer.BaseRPS())
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if msrv != nil {
			msrv.Shutdown(sctx)
		}
		return srv.Shutdown(sctx)
	case err := <-errc:
		return err
	}
}

func (a *App) loadInitial(ctx context.Context) error {
	if a.cfg.GTFSZip != "" {
		f, err := gtfs.LoadZip(a.cfg.GTFSZip, gtfs.LoadOptions{})
		if err != nil {
			return err
		}
		a.SetFeed(f, nil)
		return nil
	}
	if a.ckanMode() {
		a.loadCKANLocal()
		for a.world() == nil {
			err := a.checkCKAN(ctx)
			if a.world() != nil {
				break
			}
			a.log.Warn("waiting for a GTFS slice", "err", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Minute):
			}
		}
		return nil
	}
	if f, m, err := a.fetch.LoadLocal(); err != nil {
		a.log.Warn("local snapshot unusable", "err", err)
	} else if f != nil {
		a.SetFeed(f, m)
	}
	for a.world() == nil {
		f, m, err := a.fetch.Check(ctx)
		if err == nil && f != nil {
			a.SetFeed(f, m)
			break
		}
		a.log.Warn("waiting for a GTFS snapshot", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Minute):
		}
	}
	return nil
}

func (a *App) housekeeping(ctx context.Context) {
	minute := time.NewTicker(time.Minute)
	defer minute.Stop()
	check := a.cfg.ReleaseCheck
	if check <= 0 {
		check = 30 * time.Minute
	}
	release := time.NewTicker(check)
	defer release.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-minute.C:
			a.refreshLines()
			if a.meta.Due() {
				a.meta.Refresh()
			}
			if a.city.ID == "thessaloniki" {
				a.meta.FetchAllStops() // routes whose stop list failed or is still missing
			}
			a.warnExpiry(false)
			a.forget()
			a.roads.Prune(a.now().Unix())
			a.live.Prune(a.now().Unix())
			a.logStats()
		case <-release.C:
			if a.cfg.GTFSZip != "" {
				continue
			}
			if a.ckanMode() {
				if err := a.checkCKAN(ctx); err != nil {
					a.log.Warn("GTFS slice check failed", "err", err)
				}
				continue
			}
			f, m, err := a.fetch.Check(ctx)
			switch {
			case err != nil:
				a.log.Warn("snapshot check failed", "err", err)
			case f != nil:
				a.SetFeed(f, m)
			}
		}
	}
}

// logStats writes one line a minute about the polling plan and its cost, and names the lines
// whose buses all lack a trip (a timetable that no longer fits, rt#51).
func (a *App) logStats() {
	st := a.sched.Stats()
	a.mu.RLock()
	vehicles, matched := 0, 0
	var unmatched []string  // lines whose buses all lack a trip (rt#51)
	why := map[string]int{} // "line:reason" -> unmatched buses (rt#116)
	for id, d := range a.lines {
		if a.now().Sub(d.updated) > feedMaxAge {
			continue
		}
		n := 0
		for i := range d.results {
			if d.results[i].Matched() {
				n++
			} else {
				why[id+":"+d.results[i].Why]++
			}
		}
		if len(d.results) > 0 && n == 0 {
			unmatched = append(unmatched, id)
		}
		vehicles += len(d.results)
		matched += n
	}
	a.mu.RUnlock()
	sort.Strings(unmatched)
	whys := make([]string, 0, len(why))
	for k, n := range why {
		whys = append(whys, fmt.Sprintf("%s=%d", k, n))
	}
	sort.Strings(whys)
	reqs := a.client.Requests()
	osethReqs, osethErrs := int64(0), int64(0)
	if a.oseth != nil {
		osethReqs, osethErrs = a.oseth.Requests(), a.oseth.Errors()
	}
	a.log.Info("stats", "requests", reqs, "requests_last_min", reqs-a.lastRequests, "errors", a.client.Errors(),
		"oseth_requests", osethReqs, "oseth_errors", osethErrs,
		"watched", st.LinesByTier[sched.Watched], "dense", st.LinesByTier[sched.Dense], "other", st.LinesByTier[sched.Other],
		"stretch_other", fmt.Sprintf("%.2f", st.Stretch[sched.Other]), "rps_now", fmt.Sprintf("%.2f", a.pacer.RPS()),
		"vehicles", vehicles, "matched", matched, "unmatched_lines", strings.Join(unmatched, ","),
		"unmatched_why", strings.Join(whys, ","), "road_cells", a.roads.Cells())
	a.lastRequests = reqs
	a.warnErrors("telematics", a.client.Errors(), a.client.LastError(), &a.seenErrors)
	if a.oseth != nil {
		a.warnErrors("oseth", osethErrs, a.oseth.LastError(), &a.seenOSETH)
	}
}

// warnErrors logs once a minute where new upstream errors come from and what the last one said
// (the counters alone hide the cause). Error texts carry the act and status, never the salt.
func (a *App) warnErrors(source string, total int64, last string, seen *int64) {
	if total > *seen {
		a.log.Warn("upstream errors", "source", source, "new", total-*seen, "total", total, "last", last)
	}
	*seen = total
}

// forget drops lines not polled for a while and old matcher state.
func (a *App) forget() {
	now := a.now()
	a.matchMu.Lock()
	if w := a.world(); w != nil {
		w.matcher.Forget(now)
	}
	a.motion.forget(now)
	a.matchMu.Unlock()
	a.mu.Lock()
	for id, d := range a.lines {
		if now.Sub(d.updated) > time.Hour {
			delete(a.lines, id)
		}
	}
	a.mu.Unlock()
}

// maxQueued bounds the OASA calls that requests may have waiting in the scheduler at once.
const maxQueued = 16

var errQueueFull = errors.New("telematics: request queue full")

// queued runs one OASA call as a background request of the scheduler: it takes a request slot
// (budget, backoff, MaxInFlight) like the polls. At most maxQueued wait at once; past that the
// caller is told at once. A call whose caller has left by its turn is dropped, not sent.
func queued[T any](ctx context.Context, a *App, call func(ctx context.Context) (T, error)) (T, error) {
	type answer struct {
		v   T
		err error
	}
	var zero T
	if a.queuedN.Add(1) > maxQueued {
		a.queuedN.Add(-1)
		return zero, errQueueFull
	}
	ch := make(chan answer, 1)
	a.sched.SubmitFirst(func(sctx context.Context) error { // a caller waits: before the metadata work
		defer a.queuedN.Add(-1)
		if ctx.Err() != nil {
			return nil // nobody waits for it any more
		}
		v, err := call(sctx)
		ch <- answer{v, err}
		return err
	})
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// dailyBackground fetches OASA's daily schedule of a line code in a background slot of the
// scheduler (FIFO with the metadata work, every few slots): no cap, the prefetch asks one at a time.
func (a *App) dailyBackground(ctx context.Context, code string) (telematics.DailySchedule, error) {
	type answer struct {
		d   telematics.DailySchedule
		err error
	}
	ch := make(chan answer, 1)
	a.sched.Submit(func(sctx context.Context) error {
		if ctx.Err() != nil {
			return nil
		}
		d, err := a.client.DailySchedule(sctx, code)
		ch <- answer{d, err}
		return err
	})
	select {
	case r := <-ch:
		return r.d, r.err
	case <-ctx.Done():
		return telematics.DailySchedule{}, ctx.Err()
	}
}

// dailyQueued fetches OASA's daily schedule of a line code through queued.
func (a *App) dailyQueued(ctx context.Context, code string) (telematics.DailySchedule, error) {
	return queued(ctx, a, func(sctx context.Context) (telematics.DailySchedule, error) {
		return a.client.DailySchedule(sctx, code)
	})
}

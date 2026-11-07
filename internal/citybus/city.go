package citybus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// maxInflight: stop boards waiting for an answer at once, per city.
const maxInflight = 3

// hotEvery: a watched line's board takes at most one read in this many.
const hotEvery = 4

// idleWait: with no live line to rotate over, the timetable-only checks run this often.
var idleWait = 30 * time.Second

// Conf is one citybus.gr site.
type Conf struct {
	ID     string `json:"id"`     // the code: the path prefix and the id namespace: /her/..., her:1235
	Slug   string `json:"slug"`   // the long name: the web's /{city} paths and the state dir
	Sub    string `json:"sub"`    // the citybus.gr subdomain
	Agency int    `json:"agency"` // the agency code of the REST paths
	Name   string `json:"name"`
}

// Cities: every non-empty citybus.gr site, in the order of the citybus.gr index page. The code is
// IATA where the city has its own airport, else three letters of the name.
var Cities = []Conf{
	{"agq", "agrinio", "agrinio", 130, "Αγρίνιο"},
	{"axd", "alexandroupoli", "alexandroupoli", 107, "Αλεξανδρούπολη"},
	{"art", "arta", "arta", 125, "Άρτα"},
	{"ver", "veroia", "veroia", 129, "Βέροια"},
	{"vol", "volos", "volos", 103, "Βόλος"},
	{"dra", "drama", "drama", 134, "Δράμα"},
	{"her", "heraklion", "irakleio", 110, "Ηράκλειο"},
	{"ioa", "ioannina", "ioannina", 106, "Ιωάννινα"},
	{"kva", "kavala", "kavala", 123, "Καβάλα"},
	{"kso", "kastoria", "kastoria", 115, "Καστοριά"},
	{"kat", "katerini", "katerini", 118, "Κατερίνη"},
	{"cfu", "corfu", "corfu", 101, "Κέρκυρα"},
	{"kzi", "kozani", "kozani", 121, "Κοζάνη"},
	{"kom", "komotini", "komotini", 105, "Κομοτηνή"},
	{"lam", "lamia", "lamia", 114, "Λαμία"},
	{"lra", "larissa", "larisa", 102, "Λάρισα"},
	{"mes", "mesologgi", "mesologgi", 109, "Μεσολόγγι"},
	{"mjt", "mitilini", "mitilini", 122, "Μυτιλήνη"},
	{"nao", "naousa", "naousa", 116, "Νάουσα"},
	{"xan", "xanthi", "xanthi", 104, "Ξάνθη"},
	{"pat", "patras", "patra", 112, "Πάτρα"},
	{"pto", "ptolemaida", "ptolemaida", 128, "Πτολεμαΐδα"},
	{"ser", "serres", "serres", 117, "Σέρρες"},
	{"cha", "chalkida", "chalkida", 133, "Χαλκίδα"},
	{"chq", "chania", "chania", 120, "Χανιά"},
	{"jkh", "chios", "chios", 127, "Χίος"},
	{"sal", "salamina", "salamina", 113, "Σαλαμίνα"},
	{"jsi", "skiathos", "skiathos", 135, "Σκιάθος"},
}

type cycleStats struct {
	Started float64 `json:"started"`
	Took    int64   `json:"took"`
	Boards  int     `json:"boards"`
	Empty   int     `json:"empty"`
}

// City is one citybus.gr city: its static model (from disk, refreshed daily), the stop-board
// rotation, and its line health. mu guards every field below it.
type City struct {
	Conf Conf
	up   fetcher
	dir  string
	// boards: stop boards waiting for an answer, from every path (rotation, checks, users).
	boards chan struct{}
	log    *slog.Logger
	now    func() time.Time

	loadMu sync.Mutex // one static load at a time

	mu          sync.Mutex
	model       *Model
	live        *liveCity
	raw         *RawCity
	health      *LineHealth
	probes      []string
	checkProbes map[string][]string // timetable-only line -> its probe stops
	skipped     map[string]bool     // lines with no live bus for Days
	watched     map[string]int64    // line -> watched until (unix s)
	hotAsked    map[string]int64    // stop -> last read for a watched line (unix s)
	userAsked   map[string]int64    // stop -> last user read of its board, failed or not (unix s)
	userWait    map[string]*pending // stop -> the user read in flight, joined by later callers
	svc         context.Context     // the service: user reads run on it, not on the request
	lastCycle   cycleStats
	healthSaved int64
	checkRound  int
}

func newCity(conf Conf, up fetcher, stateDir string, log *slog.Logger) (*City, error) {
	dir := filepath.Join(stateDir, conf.Slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	c := &City{Conf: conf, up: up, dir: dir, log: log.With("city", conf.ID), now: time.Now,
		checkProbes: map[string][]string{}, skipped: map[string]bool{}, watched: map[string]int64{}, hotAsked: map[string]int64{}, userAsked: map[string]int64{}, userWait: map[string]*pending{},
		svc: context.Background(), boards: make(chan struct{}, maxInflight)}
	c.health = loadHealth(filepath.Join(dir, "health.json"), c.unix())
	return c, nil
}

func (c *City) unix() int64 { return c.now().Unix() }

func (c *City) base() string { return fmt.Sprintf("/el/%d", c.Conf.Agency) }

func getJSON[T any](ctx context.Context, c *City, path string, pri Priority) (*T, error) {
	b, err := c.up.get(ctx, path, pri)
	if err != nil || b == nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &v, nil
}

// loadStatic: from disk when under 24 h old (unless force), else from citybus.gr (about 2
// requests per route). pri 2 (the daily reload) asks one request at a time, so the rotation on
// the same queue keeps every other slot; otherwise all are queued at once.
func (c *City) loadStatic(ctx context.Context, force bool, pri Priority) error {
	c.loadMu.Lock()
	defer c.loadMu.Unlock()
	file := filepath.Join(c.dir, "static.json")
	if !force {
		if b, err := os.ReadFile(file); err == nil {
			var raw RawCity
			if err := json.Unmarshal(b, &raw); err != nil {
				c.log.Warn("static.json unreadable, loading again", "err", err)
			} else {
				c.install(&raw)
				if c.unix()-raw.BuiltAt < 86400 {
					return nil
				}
			}
		}
	}
	lines, err := getJSON[[]RawLine](ctx, c, c.base()+"/lines", pri)
	if err != nil {
		return err
	}
	stops, err := getJSON[[]RawStop](ctx, c, c.base()+"/stops", pri)
	if err != nil {
		return err
	}
	// An empty answer never replaces a good model (or the file on disk).
	if lines == nil || len(*lines) == 0 {
		return errors.New("no lines from citybus.gr")
	}
	if stops == nil || len(*stops) == 0 {
		return errors.New("no stops from citybus.gr")
	}
	raw := &RawCity{Lines: *lines, Stops: *stops, Sequences: map[string][]RawSeq{}, Points: map[string][]RawPoints{},
		Trips: map[string][]RawTripRow{}, BuiltAt: c.unix()}
	c.mu.Lock()
	prev := c.raw
	c.mu.Unlock()
	if prev == nil {
		prev = &RawCity{}
	}
	var (
		mu      sync.Mutex
		missing int
		firstEr error
	)
	keep := func(err error) {
		mu.Lock()
		if firstEr == nil && err != nil {
			firstEr = err // a failed answer (not a 404) fails the load, as in the bridge
		}
		mu.Unlock()
	}
	var jobs []func()
	for _, l := range *lines {
		l := l
		jobs = append(jobs, func() {
			x, err := getJSON[[]RawPoints](ctx, c, fmt.Sprintf("/%d/lines/%s/points", c.Conf.Agency, url.PathEscape(l.Code)), pri)
			keep(err)
			mu.Lock()
			defer mu.Unlock()
			if x != nil {
				raw.Points[l.Code] = *x
			} else if p, ok := prev.Points[l.Code]; ok {
				raw.Points[l.Code] = p
			} else {
				raw.Points[l.Code] = []RawPoints{}
			}
		})
		for _, r := range l.Routes {
			r := r
			jobs = append(jobs, func() {
				x, err := getJSON[[]RawSeq](ctx, c, c.base()+"/routes/"+url.PathEscape(r.Code)+"/sequence", pri)
				keep(err)
				mu.Lock()
				defer mu.Unlock()
				if x != nil {
					raw.Sequences[r.Code] = *x
				} else if s, ok := prev.Sequences[r.Code]; ok {
					raw.Sequences[r.Code] = s
				} else {
					missing++
					raw.Sequences[r.Code] = []RawSeq{}
				}
			}, func() {
				x, err := getJSON[[]RawTripRow](ctx, c, c.base()+"/trips/route/"+url.PathEscape(r.Code), pri)
				keep(err)
				mu.Lock()
				defer mu.Unlock()
				if x != nil {
					raw.Trips[r.Code] = *x
				} else if t, ok := prev.Trips[r.Code]; ok {
					raw.Trips[r.Code] = t
				} else {
					raw.Trips[r.Code] = []RawTripRow{}
				}
			})
		}
	}
	if pri == priBackground {
		for _, j := range jobs {
			if j(); ctx.Err() != nil || firstEr != nil {
				break
			}
		}
	} else {
		var wg sync.WaitGroup
		for _, j := range jobs {
			wg.Add(1)
			go func() { defer wg.Done(); j() }()
		}
		wg.Wait()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if firstEr != nil {
		return firstEr
	}
	routes := 0
	for _, l := range *lines {
		routes += len(l.Routes)
	}
	if missing*10 > routes {
		return fmt.Errorf("%d of %d routes without a stop order from citybus.gr", missing, routes)
	}
	raw.Hash, raw.Version = contentHash(raw), athensClock(raw.BuiltAt).Date
	if prevHash := prev.Hash; prev.version() != "" {
		if prevHash == "" { // a file from before the hash was saved
			prevHash = contentHash(prev)
		}
		if prevHash == raw.Hash {
			raw.Version = prev.version()
		}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	if err := os.WriteFile(file+".tmp", b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(file+".tmp", file); err != nil { // never a half-written file
		return err
	}
	c.install(raw)
	return nil
}

func (c *City) install(raw *RawCity) {
	model := buildModel(raw)
	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.live
	c.raw, c.model, c.live = raw, model, newLiveCity(model)
	if old != nil {
		for _, b := range old.Boards {
			c.live.add(b)
		}
	}
	c.refreshProbesLocked(c.unix())
}

// refreshProbesLocked splits the lines in two: the lines with a live bus (rotated every cycle) and
// the timetable-only lines (one check probe per cycle, so a line that starts sending GPS comes
// back). It also sets the city's gap: one rotation in about 60 s.
func (c *City) refreshProbesLocked(now int64) {
	if c.model == nil {
		c.probes, c.checkProbes = nil, map[string][]string{}
		return
	}
	codes := make([]string, 0, len(c.model.Lines))
	for _, l := range c.model.Lines {
		codes = append(codes, l.Code)
	}
	c.health.track(codes, now)
	c.skipped = map[string]bool{}
	for _, l := range codes {
		if c.health.timetableOnly(l, now) {
			c.skipped[l] = true
		}
	}
	c.probes = probeStops(c.model, c.skipped)
	c.checkProbes = map[string][]string{}
	for l := range c.skipped {
		others := map[string]bool{}
		for _, x := range codes {
			if x != l {
				others[x] = true
			}
		}
		c.checkProbes[l] = probeStops(c.model, others)
	}
	c.up.pacer().setGap(time.Duration(gapFor(len(c.probes)+len(c.skipped)+1)) * time.Millisecond)
}

func (c *City) refreshProbes() {
	c.mu.Lock()
	c.refreshProbesLocked(c.unix())
	c.mu.Unlock()
}

// ready is true once the city has a model.
func (c *City) ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.model != nil
}

type liveAnswer struct {
	Vehicles []LiveRow `json:"vehicles"`
}

// board reads one stop board from citybus.gr into the live state.
func (c *City) board(ctx context.Context, stop string, pri Priority) (Board, error) {
	select {
	case c.boards <- struct{}{}:
	case <-ctx.Done():
		return Board{}, ctx.Err()
	}
	res, err := getJSON[liveAnswer](ctx, c, c.base()+"/stops/live/"+url.PathEscape(stop), pri)
	<-c.boards
	if err != nil {
		return Board{}, err
	}
	b := Board{Stop: stop, At: c.unix()}
	if res != nil {
		b.Rows = res.Vehicles
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live != nil {
		for _, s := range c.live.add(b) {
			if r := c.model.Routes[s.Route]; r != nil {
				c.health.saw(r.Line, b.At)
			}
		}
	}
	if b.At-c.healthSaved >= 60 {
		c.healthSaved = b.At
		if err := c.health.save(); err != nil {
			c.log.Warn("health.json not written", "err", err)
		}
	}
	return b, nil
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// rotate reads every probe stop in turn, forever; a watched line's probes first. Up to
// maxInflight boards wait for an answer at once: the city's pacer sets the pace, so a slow answer
// does not stretch the cycle.
func (c *City) rotate(ctx context.Context) {
	c.mu.Lock()
	c.svc = ctx
	c.mu.Unlock()
	// The first load may fail (citybus.gr down): try again, 1 min apart, until it works.
	for ctx.Err() == nil {
		if err := c.loadStatic(ctx, false, priWatched); err == nil {
			break
		} else if ctx.Err() == nil {
			c.log.Error("static load failed", "err", err)
			sleepCtx(ctx, time.Minute)
		}
		if c.ready() { // an old model from disk: rotate on it, the daily reload tries again
			break
		}
	}
	var (
		wg        sync.WaitGroup
		sem       = make(chan struct{}, maxInflight)
		failedAt  atomic.Int64 // unix ms of the last failed board
		reloading atomic.Bool
		retryAt   atomic.Int64 // unix s: no daily reload before this (after a failed one)
		i         int
		sinceHot  = hotEvery // reads since the last watched one
		cyMu      sync.Mutex
		cycle     = &cycleStats{Started: float64(c.now().UnixMilli()) / 1000}
	)
	read := func(code string, pri Priority, count bool) {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		cy := cycle
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			b, err := c.board(ctx, code, pri)
			if err != nil {
				if ctx.Err() == nil {
					failedAt.Store(time.Now().UnixMilli())
				}
				return
			}
			if count {
				cyMu.Lock()
				cy.Boards++
				if len(b.Rows) == 0 {
					cy.Empty++
				}
				cyMu.Unlock()
			}
		}()
	}
	for ctx.Err() == nil {
		now := c.unix()
		c.mu.Lock()
		builtAt := now
		if c.model != nil {
			builtAt = c.model.BuiltAt
		}
		c.mu.Unlock()
		// One daily reload at a time; a failed one is tried again an hour after it failed.
		if !reloading.Load() && now >= retryAt.Load() && now-builtAt > 86400 {
			reloading.Store(true)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer reloading.Store(false)
				if err := c.loadStatic(ctx, true, priBackground); err != nil && ctx.Err() == nil {
					c.log.Warn("daily reload failed", "err", err)
					retryAt.Store(c.unix() + 3600)
				}
			}()
		}
		// Watched lines take at most one read in hotEvery, so the rotation (and with it every other
		// line) never starves however many lines users watch.
		c.mu.Lock()
		probes := c.probes
		nChecks := len(c.checkProbes)
		c.mu.Unlock()
		hot := ""
		if sinceHot >= hotEvery-1 || len(probes) == 0 { // no rotation to starve: no quota
			hot = c.watchedProbe(now)
		}
		// Nothing live to rotate: only the timetable-only checks, every 30 s.
		if hot == "" && len(probes) == 0 {
			if nChecks > 0 {
				var cw sync.WaitGroup
				for _, s := range c.checks() {
					cw.Add(1)
					go func() { defer cw.Done(); c.board(ctx, s, priBackground) }()
				}
				cw.Wait()
				c.refreshProbes()
				c.prune()
			}
			sleepCtx(ctx, idleWait)
			continue
		}
		// Wait for room before the back-off check, as the bridge does.
		select {
		case sem <- struct{}{}:
			<-sem
		case <-ctx.Done():
		}
		if time.Now().UnixMilli()-failedAt.Load() < 5000 { // citybus.gr failing: back off
			sleepCtx(ctx, 5*time.Second)
		}
		if ctx.Err() != nil {
			break
		}
		if hot != "" {
			sinceHot = 0
			read(hot, priWatched, false)
		} else {
			sinceHot++
			read(probes[i%len(probes)], priBackground, true)
			i = (i + 1) % len(probes)
			// A full cycle ran: look again at which lines are live, and check each timetable-only
			// line once.
			if i == 0 {
				cyMu.Lock()
				done := *cycle
				nowF := float64(c.now().UnixMilli()) / 1000
				done.Took = int64(nowF - done.Started + 0.5)
				cycle = &cycleStats{Started: nowF}
				cyMu.Unlock()
				c.mu.Lock()
				c.lastCycle = done
				c.mu.Unlock()
				c.refreshProbes()
				for _, s := range c.checks() {
					read(s, priBackground, false)
				}
			}
		}
		c.prune()
	}
	wg.Wait()
}

func (c *City) prune() {
	c.mu.Lock()
	if c.live != nil {
		c.live.prune(c.unix())
	}
	c.mu.Unlock()
}

// checks: one probe stop per timetable-only line, the next one of its list each cycle.
func (c *City) checks() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	round := c.checkRound
	c.checkRound++
	lines := make([]string, 0, len(c.checkProbes))
	for l := range c.checkProbes {
		lines = append(lines, l)
	}
	sort.Strings(lines)
	var out []string
	for _, l := range lines {
		if p := c.checkProbes[l]; len(p) > 0 {
			out = append(out, p[round%len(p)])
		}
	}
	return out
}

// watchedProbe: the end probe stop of a route of a watched line not read in the last 20 s
// ("" when none).
func (c *City) watchedProbe(now int64) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.model == nil {
		return ""
	}
	lines := make([]string, 0, len(c.watched))
	for l, until := range c.watched {
		if until < now {
			delete(c.watched, l)
			continue
		}
		lines = append(lines, l)
	}
	sort.Strings(lines)
	for _, l := range lines {
		ln := c.model.LineByCode[l]
		if ln == nil {
			continue
		}
		for _, rc := range ln.Routes {
			r := c.model.Routes[rc]
			s := r.Stops[max(1, len(r.Stops)-2)]
			if c.hotAsked[s] > now-20 {
				continue
			}
			c.hotAsked[s] = now
			return s
		}
	}
	return ""
}

// watchLocked marks a line as watched for 10 min (a user opened its vehicles).
func (c *City) watchLocked(line string) { c.watched[line] = c.unix() + 600 }

// pending is a user read of a stop board in flight; b is set before done closes (nil on failure).
type pending struct {
	done chan struct{}
	b    *Board
}

// freshBoard: a stop board from the cache when under 15 s old, else one request at user
// priority; after 8 s the cached one (or none). A caller joins a read of the same stop that is
// in flight. A stop is asked at most once per 15 s, failed or not. The request runs on the
// service context (bounded to 20 s), not on the user's request: other callers share its answer.
func (c *City) freshBoard(code string) *Board {
	c.mu.Lock()
	var cached *Board
	if c.live != nil {
		if b, ok := c.live.Boards[code]; ok {
			cached = &b
		}
	}
	now := c.unix()
	if cached != nil && now-cached.At < 15 {
		c.mu.Unlock()
		return cached
	}
	p := c.userWait[code]
	if p == nil {
		if now-c.userAsked[code] < 15 {
			c.mu.Unlock()
			return cached
		}
		c.userAsked[code] = now
		p = &pending{done: make(chan struct{})}
		c.userWait[code] = p
		svc := c.svc
		go func() {
			ctx, cancel := context.WithTimeout(svc, 20*time.Second)
			defer cancel()
			if b, err := c.board(ctx, code, priUser); err == nil {
				p.b = &b
			}
			c.mu.Lock()
			delete(c.userWait, code)
			c.mu.Unlock()
			close(p.done)
		}()
	}
	c.mu.Unlock()
	t := time.NewTimer(8 * time.Second)
	defer t.Stop()
	select {
	case <-p.done:
		if p.b != nil {
			return p.b
		}
	case <-t.C:
	}
	return cached
}

// contentHash: sha256 of everything the model is built from (not BuiltAt, Version, Hash). Written
// part by part, map keys in order, so no copy of the whole city is held in memory.
func contentHash(raw *RawCity) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	_ = enc.Encode(raw.Lines)
	_ = enc.Encode(raw.Stops)
	hashMap(enc, raw.Sequences)
	hashMap(enc, raw.Points)
	hashMap(enc, raw.Trips)
	return hex.EncodeToString(h.Sum(nil))
}

func hashMap[T any](enc *json.Encoder, m map[string]T) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_ = enc.Encode(k)
		_ = enc.Encode(m[k])
	}
	_ = enc.Encode("|")
}

package citybus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/history"
)

// Config is the citybus kind of `atrt serve --city citybus`.
type Config struct {
	StateDir string
	Cities   []string // ids of Cities, in any order; served in the order of Cities
	// Fix history (off when HistoryMaxBytes is 0): one writer for every city, in
	// <state>/history (rt#142).
	HistoryMaxBytes int64
	HistoryKeep     time.Duration
}

// ParseCities reads --cities: "all", or codes or slugs separated by commas. The output is always
// the codes, deduplicated after mapping.
func ParseCities(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "all" {
		out := make([]string, len(Cities))
		for i, c := range Cities {
			out[i] = c.ID
		}
		return out, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, x := range strings.Split(s, ",") {
		x = strings.TrimSpace(x)
		if x == "" {
			continue
		}
		conf, ok := confOf(x)
		if !ok {
			return nil, fmt.Errorf("unknown citybus city %q", x)
		}
		if seen[conf.ID] {
			continue
		}
		seen[conf.ID] = true
		out = append(out, conf.ID)
	}
	if len(out) == 0 {
		return nil, errors.New("no citybus city given (--cities all or a list)")
	}
	return out, nil
}

// confOf: a city by its code or its slug.
func confOf(id string) (Conf, bool) {
	for _, c := range Cities {
		if c.ID == id || c.Slug == id {
			return c, true
		}
	}
	return Conf{}, false
}

// Service is every enabled citybus city behind one HTTP handler.
type Service struct {
	cities  map[string]*City
	order   []*City
	log     *slog.Logger
	hist    *history.Writer // the one fix history of every city; nil = off
	histDir string          // the directory of that history
}

// New builds the cities; Run starts their rotations.
func New(cfg Config, log *slog.Logger) (*Service, error) {
	hc := &http.Client{Timeout: 20 * time.Second}
	s := &Service{cities: map[string]*City{}, log: log}
	if cfg.HistoryMaxBytes > 0 {
		h, err := history.Open(filepath.Join(cfg.StateDir, "history"), cfg.HistoryMaxBytes, cfg.HistoryKeep, time.Now, log)
		if err != nil {
			return nil, err
		}
		s.hist, s.histDir = h, filepath.Join(cfg.StateDir, "history")
	}
	for _, conf := range Cities {
		on := false
		for _, id := range cfg.Cities {
			on = on || id == conf.ID
		}
		if !on {
			continue
		}
		c, err := newCity(conf, newClient(conf.Sub, hc), cfg.StateDir, log)
		if err != nil {
			if s.hist != nil {
				s.hist.Close()
			}
			return nil, err
		}
		c.hist = s.hist
		s.cities[conf.ID] = c
		s.order = append(s.order, c)
	}
	return s, nil
}

// close saves every city's run log and ends the fix history. Safe to call twice.
func (s *Service) close() {
	for _, c := range s.order {
		c.flush()
	}
	if s.hist != nil {
		s.hist.Close()
	}
}

// Run serves HTTP on listen and rotates every city until ctx ends.
func (s *Service) Run(ctx context.Context, listen string) error {
	var wg sync.WaitGroup
	for _, c := range s.order {
		wg.Add(1)
		go func() { defer wg.Done(); c.rotate(ctx) }()
	}
	if s.hist != nil {
		wg.Add(1)
		go func() { defer wg.Done(); s.dailyStats(ctx, s.histDir) }()
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sh)
	}()
	ids := make([]string, len(s.order))
	for i, c := range s.order {
		ids[i] = c.Conf.ID
	}
	s.log.Info("citybus serving", "listen", listen, "cities", strings.Join(ids, ","))
	err = srv.Serve(ln)
	wg.Wait()
	s.close() // after the rotations: no board writes to a closed history
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func writeJSON(w http.ResponseWriter, body any, status, maxAge int) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(maxAge))
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, code string, status int) {
	maxAge := 5
	if status == http.StatusNotFound {
		maxAge = 60
	}
	writeJSON(w, obj{"error": code}, status, maxAge)
}

func ok(w http.ResponseWriter, body obj, code string, maxAge int) {
	if body == nil {
		writeErr(w, code, http.StatusNotFound)
		return
	}
	writeJSON(w, body, http.StatusOK, maxAge)
}

var cityPath = regexp.MustCompile(`^/([a-z]+)(/.*)$`)

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "Date")
	switch r.Method {
	case http.MethodOptions:
		h.Set("Access-Control-Allow-Methods", "GET")
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet, http.MethodHead:
	default:
		writeErr(w, "method_not_allowed", http.StatusMethodNotAllowed)
		return
	}
	path := r.URL.Path
	if path == "/" || path == "/debug" {
		var all []obj
		for _, c := range s.order {
			all = append(all, c.debug())
		}
		writeJSON(w, obj{"cities": all}, http.StatusOK, 0)
		return
	}
	if path == "/v1/cities" {
		var list []obj
		for _, c := range s.order {
			list = append(list, obj{"id": c.Conf.ID, "slug": c.Conf.Slug, "name": c.Conf.Name, "ready": c.ready()})
		}
		writeJSON(w, obj{"cities": list}, http.StatusOK, 60)
		return
	}
	m := cityPath.FindStringSubmatch(path)
	if m == nil || s.cities[m[1]] == nil {
		writeErr(w, "unknown_city", http.StatusNotFound)
		return
	}
	defer func() {
		if e := recover(); e != nil {
			s.log.Error("panic", "path", path, "err", e)
			writeErr(w, "internal", http.StatusInternalServerError)
		}
	}()
	s.cities[m[1]].route(w, m[2])
}

var (
	reStaticLine   = regexp.MustCompile(`^/static/v1/lines/([^/]+)\.json$`)
	reStatsDay     = regexp.MustCompile(`^/v1/stats/days/([^/]+)$`)
	reV2LineVeh    = regexp.MustCompile(`^/v2/lines/([^/]+)/vehicles$`)
	reV2Line       = regexp.MustCompile(`^/v2/lines/([^/]+)$`)
	reV2Stop       = regexp.MustCompile(`^/v2/stops/([^/]+)$`)
	reV2Run        = regexp.MustCompile(`^/v2/runs/([^/]+)$`)
	reV1Schedule   = regexp.MustCompile(`^/v1/lines/([^/]+)/schedule$`)
	reV1Line       = regexp.MustCompile(`^/v1/lines/([^/]+)$`)
	reV1Stop       = regexp.MustCompile(`^/v1/stops/([^/]+)$`)
	reV1Tile       = regexp.MustCompile(`^/v1/vehicles/tiles/(9|13)/(\d+)/(\d+)$`)
	errUnknownPath = errors.New("not_found")
)

func dec(s string) string {
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}

func (c *City) route(w http.ResponseWriter, path string) {
	if c.statsRoute(w, path) {
		return // the stats files are written by the service, not by the model
	}
	switch path {
	case "/v1/status":
		writeJSON(w, c.status(), http.StatusOK, 10)
		return
	case "/v1/health":
		writeJSON(w, c.healthReport(), http.StatusOK, 60)
		return
	case "/debug":
		writeJSON(w, c.debug(), http.StatusOK, 0)
		return
	}
	if !c.ready() {
		if strings.HasPrefix(path, "/v2/") {
			c.mu.Lock()
			src := c.sourcesLocked()
			c.mu.Unlock()
			writeJSON(w, obj{"sources": src, "lines": []any{}, "stops": []any{}}, http.StatusOK, 5)
			return
		}
		writeErr(w, "warming_up", http.StatusServiceUnavailable)
		return
	}
	var m []string
	match := func(re *regexp.Regexp) bool { m = re.FindStringSubmatch(path); return m != nil }
	switch {
	case path == "/static/v1/lines.json":
		writeJSON(w, c.staticLines(), http.StatusOK, 3600)
	case match(reStaticLine):
		ok(w, c.staticLine(dec(m[1])), "unknown_line", 3600)
	case path == "/v2/lines":
		writeJSON(w, c.v2Lines(), http.StatusOK, 3600)
	case path == "/v2/stops":
		writeJSON(w, c.v2Stops(), http.StatusOK, 3600)
	case match(reV2LineVeh):
		ok(w, c.v2LineVehicles(dec(m[1])), "unknown_line", 5)
	case match(reV2Line):
		ok(w, c.v2LineDetail(dec(m[1])), "unknown_line", 3600)
	case match(reV2Stop):
		ok(w, c.v2StopBoard(dec(m[1])), "unknown_stop", 15)
	case match(reV2Run):
		ok(w, c.v2Run(dec(m[1])), "unknown_run", 15)
	case path == "/v2/alerts":
		writeJSON(w, obj{"alerts": []any{}, "updated_at": c.unix()}, http.StatusOK, 300)
	case match(reV1Schedule):
		body, maxAge := c.schedule(dec(m[1]))
		ok(w, body, "unknown_line", maxAge)
	case match(reV1Line):
		ok(w, c.v1Line(dec(m[1])), "unknown_line", 5)
	case match(reV1Stop):
		ok(w, c.v1StopBoard(dec(m[1])), "unknown_stop", 15)
	case match(reV1Tile):
		z, _ := strconv.Atoi(m[1])
		x, _ := strconv.Atoi(m[2])
		y, _ := strconv.Atoi(m[3])
		writeJSON(w, c.tile(z, x, y), http.StatusOK, 5)
	default:
		writeErr(w, errUnknownPath.Error(), http.StatusNotFound)
	}
}

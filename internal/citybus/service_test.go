package citybus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Ported from citybus-bridge test/health.test.ts and test/upstream.test.ts, plus the HTTP shape.

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fake answers REST paths from a map; nil = 404. hook runs first and may answer instead.
type fake struct {
	mu      sync.Mutex
	answers map[string]any
	hook    func(path string) (any, bool, error)
	p       *pacer
}

func (f *fake) pacer() *pacer { return f.p }

func (f *fake) get(ctx context.Context, path string, pri Priority) ([]byte, error) {
	if f.hook != nil {
		if v, done, err := f.hook(path); done || err != nil {
			if err != nil || v == nil {
				return nil, err
			}
			return json.Marshal(v)
		}
	}
	f.mu.Lock()
	v, ok := f.answers[path]
	f.mu.Unlock()
	if !ok || v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

func answersOf(raw *RawCity) map[string]any {
	a := map[string]any{"/el/1/lines": raw.Lines, "/el/1/stops": raw.Stops}
	for rc, s := range raw.Sequences {
		a["/el/1/routes/"+rc+"/sequence"] = s
	}
	for rc, t := range raw.Trips {
		a["/el/1/trips/route/"+rc] = t
	}
	for lc, p := range raw.Points {
		a["/1/lines/"+lc+"/points"] = p
	}
	return a
}

func testCity(t *testing.T, f *fake, dir string) *City {
	t.Helper()
	if f.p == nil {
		f.p = newPacer(time.Second)
	}
	c, err := newCity(Conf{ID: "t", Slug: "t", Sub: "t", Agency: 1, Name: "T"}, f, dir, quiet)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestBoardMarksLineLive(t *testing.T) {
	raw := linesRaw("1", "2")
	a := answersOf(raw)
	a["/el/1/stops/live/1E"] = liveAnswer{Vehicles: []LiveRow{{LineCode: "1", RouteCode: "10", Latitude: 35.3, Longitude: 25.12, DepartureMins: 3, VehicleCode: "v1"}}}
	c := testCity(t, &fake{answers: a}, t.TempDir())
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	if _, err := c.board(context.Background(), "1E", priUser); err != nil {
		t.Fatal(err)
	}
	h := c.healthReport()["lines"].([]obj)
	if h[0]["line"] != "1" || h[0]["last_live"] == nil || h[1]["last_live"] != nil {
		t.Fatalf("health %v", h)
	}
	for _, l := range h {
		if l["timetable_only"] != false {
			t.Fatalf("tracked under 7 days: %v", l)
		}
	}
	if _, err := os.Stat(filepath.Join(c.dir, "health.json")); err != nil {
		t.Fatal("health.json must be written after a board")
	}
}

func TestDailyReloadOneRequestAtATime(t *testing.T) {
	var inflight, most atomic.Int32
	f := &fake{answers: answersOf(linesRaw("1", "2"))}
	f.hook = func(string) (any, bool, error) {
		n := inflight.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		inflight.Add(-1)
		return nil, false, nil
	}
	c := testCity(t, f, t.TempDir())
	if err := c.loadStatic(context.Background(), true, priBackground); err != nil {
		t.Fatal(err)
	}
	if most.Load() != 1 {
		t.Fatalf("max in flight %d", most.Load())
	}
}

func TestFirstLoadAllAtOnce(t *testing.T) {
	var inflight, most atomic.Int32
	f := &fake{answers: answersOf(linesRaw("1", "2"))}
	f.hook = func(string) (any, bool, error) {
		n := inflight.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inflight.Add(-1)
		return nil, false, nil
	}
	c := testCity(t, f, t.TempDir())
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	if most.Load() < 2 {
		t.Fatalf("a first load queues every request at once (the pacer paces them); max in flight %d", most.Load())
	}
}

func TestRotationKeepsThreeBoardsInFlight(t *testing.T) {
	var boards, inflight, most atomic.Int32
	f := &fake{answers: answersOf(linesRaw("1", "2"))}
	f.hook = func(path string) (any, bool, error) {
		if !strings.Contains(path, "/stops/live/") {
			return nil, false, nil
		}
		boards.Add(1)
		n := inflight.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		inflight.Add(-1)
		return nil, true, nil
	}
	c := testCity(t, f, t.TempDir())
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.rotate(ctx); close(done) }()
	time.Sleep(220 * time.Millisecond)
	cancel()
	<-done
	if most.Load() != maxInflight || maxInflight != 3 {
		t.Fatalf("max in flight %d", most.Load())
	}
	if boards.Load() < 10 {
		t.Fatalf("boards %d: one at a time would give at most 6", boards.Load())
	}
}

func TestEachTimetableOnlyLineGetsOneCheckPerCycle(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	raw := linesRaw("1", "2", "3")
	f := &fake{answers: answersOf(raw)}
	live := regexp.MustCompile(`stops/live/(.+)$`)
	f.hook = func(path string) (any, bool, error) {
		if m := live.FindStringSubmatch(path); m != nil {
			mu.Lock()
			asked = append(asked, m[1])
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			return nil, true, nil
		}
		return nil, false, nil
	}
	dir := t.TempDir()
	c := testCity(t, f, dir)
	now := time.Now().Unix()
	c.health.Since = now - 8*D
	c.health.FirstSeen = map[string]int64{"1": now - 8*D, "2": now - 8*D, "3": now - 8*D}
	c.health.saw("1", now) // only line 1 is live
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(c.probes) != "[1C]" {
		t.Fatalf("probes %v", c.probes)
	}
	if g := c.up.pacer().gapNow(); g != time.Second {
		t.Fatalf("gap %v: 1 probe + 2 checks + 1 is under 60, so 1 s", g)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.rotate(ctx); close(done) }()
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	// One cycle: the probe of the live line and one check of each timetable-only line (they start
	// in goroutines, so their order within the cycle is free).
	if len(asked) < 3 {
		t.Fatalf("asked %v", asked)
	}
	first := append([]string(nil), asked[:3]...)
	sort.Strings(first)
	if fmt.Sprint(first) != "[1C 2C 3C]" {
		t.Fatalf("asked %v", asked)
	}
}

func TestReloadKeepsOldTripsAndNeverWritesModelWithoutStopOrders(t *testing.T) {
	dir := t.TempDir()
	var fail atomic.Value
	fail.Store("")
	f := &fake{answers: map[string]any{
		"/el/1/lines":              []RawLine{{Code: "1", Name: "L", Color: "#000000", TextColor: "#ffffff", Routes: []RawRouteRef{{Code: "10", Name: "R", Direction: 1}}}},
		"/el/1/stops":              []RawStop{{Code: "A", Name: "A", Latitude: 35.3, Longitude: 25.1, LineCodes: []string{"1"}}, {Code: "B", Name: "B", Latitude: 35.3, Longitude: 25.11, LineCodes: []string{"1"}}},
		"/1/lines/1/points":        []RawPoints{},
		"/el/1/routes/10/sequence": []RawSeq{{Sequence: 1, Code: "A"}, {Sequence: 2, Code: "B"}},
		"/el/1/trips/route/10":     []RawTripRow{{ID: 7, Day: 1, RouteCode: "10", StopCode: "A", TripTime: "08:00"}},
	}}
	f.hook = func(path string) (any, bool, error) {
		if s := fail.Load().(string); s != "" && strings.Contains(path, s) {
			return nil, true, nil // 404
		}
		return nil, false, nil
	}
	c := testCity(t, f, dir)
	ctx := context.Background()
	if err := c.loadStatic(ctx, true, priWatched); err != nil {
		t.Fatal(err)
	}
	if n := len(c.model.Routes["10"].Trips); n != 1 {
		t.Fatalf("trips %d", n)
	}
	fail.Store("/trips/")
	if err := c.loadStatic(ctx, true, priWatched); err != nil {
		t.Fatal(err)
	}
	if n := len(c.model.Routes["10"].Trips); n != 1 {
		t.Fatalf("a failed trips answer must keep the old trips; got %d", n)
	}
	// The old trips come from static.json on disk, not from memory: a new City (a restart) that
	// reloads while the trips fail still keeps them.
	c2 := testCity(t, f, dir)
	if err := c2.loadStatic(ctx, true, priWatched); err != nil {
		t.Fatal(err)
	}
	if n := len(c2.model.Routes["10"].Trips); n != 1 {
		t.Fatalf("after a restart a failed trips answer must keep the trips on disk; got %d", n)
	}
	// An unreadable file never costs a city its model: the reload stops instead of dropping the
	// old trips.
	file := filepath.Join(dir, "t", "static.json")
	if err := os.WriteFile(file, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.loadStatic(ctx, true, priWatched); err == nil {
		t.Fatal("a reload without a readable static.json must fail")
	}
	if n := len(c.model.Routes["10"].Trips); n != 1 {
		t.Fatalf("an unreadable static.json must keep the model; got %d trips", n)
	}
	if b, _ := os.ReadFile(file); string(b) != "{" {
		t.Fatal("a failed load must not write static.json")
	}
	if err := os.Remove(file); err != nil { // a first load: no old model to fall back on
		t.Fatal(err)
	}
	fail.Store("/sequence")
	c3 := testCity(t, f, dir)
	err := c3.loadStatic(ctx, true, priWatched)
	if err == nil || !strings.Contains(err.Error(), "stop order") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("a failed load must not write static.json")
	}
}

func TestStaticFromDiskUnder24h(t *testing.T) {
	dir := t.TempDir()
	f := &fake{answers: answersOf(lineRaw())}
	c := testCity(t, f, dir)
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	f2 := &fake{hook: func(string) (any, bool, error) { calls.Add(1); return nil, true, nil }}
	c2 := testCity(t, f2, dir)
	if err := c2.loadStatic(context.Background(), false, priWatched); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || c2.model == nil || len(c2.model.Routes) != 1 {
		t.Fatalf("calls %d model %v", calls.Load(), c2.model != nil)
	}
}

// ---- upstream ----

func TestPacerStartsOneJobPerGapNotWaitingForSlowOnes(t *testing.T) {
	p := newPacer(50 * time.Millisecond)
	t0 := time.Now()
	var mu sync.Mutex
	var starts []time.Duration
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.turn(context.Background(), priBackground)
			mu.Lock()
			starts = append(starts, time.Since(t0))
			mu.Unlock()
			time.Sleep(200 * time.Millisecond)
		}()
	}
	wg.Wait()
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	if starts[1]-starts[0] < 45*time.Millisecond || starts[2]-starts[0] >= 200*time.Millisecond {
		t.Fatalf("starts %v", starts)
	}
}

func TestPacerPriorityFirst(t *testing.T) {
	p := newPacer(30 * time.Millisecond)
	p.turn(context.Background(), priBackground) // the next slot is 30 ms away
	order := make(chan Priority, 3)
	var wg sync.WaitGroup
	for _, pri := range []Priority{priBackground, priBackground, priUser} {
		wg.Add(1)
		go func() { defer wg.Done(); p.turn(context.Background(), pri); order <- pri }()
		time.Sleep(2 * time.Millisecond)
	}
	wg.Wait()
	if first := <-order; first != priUser {
		t.Fatalf("first %v: a user request goes before the background", first)
	}
}

func TestPacerCancelledTicketGivesUpItsSlot(t *testing.T) {
	p := newPacer(20 * time.Millisecond)
	p.turn(context.Background(), priBackground)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- p.turn(ctx, priUser) }()
	time.Sleep(2 * time.Millisecond)
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("a cancelled turn returns its error")
	}
	t0 := time.Now()
	p.turn(context.Background(), priBackground)
	if d := time.Since(t0); d > 40*time.Millisecond {
		t.Fatalf("the next turn waited %v", d)
	}
}

func TestEachCityHasItsOwnPacer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/el/stops") {
			io.WriteString(w, "const token = 't';")
			return
		}
		io.WriteString(w, `{"ok":1}`)
	}))
	defer srv.Close()
	mkc := func(sub string, gap time.Duration) *client {
		c := newClient(sub, srv.Client())
		c.rest, c.pageURL = srv.URL, srv.URL+"/"+sub+"/el/stops"
		c.p.setGap(gap)
		return c
	}
	a, b := mkc("a", 400*time.Millisecond), mkc("b", time.Millisecond)
	t0 := time.Now()
	for i := range 5 {
		go a.get(context.Background(), fmt.Sprintf("/a/%d", i), priBackground) // city a: ~2 s of queue
	}
	var wg sync.WaitGroup
	for i := range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); b.get(context.Background(), fmt.Sprintf("/b/%d", i), priBackground) }()
	}
	wg.Wait()
	if d := time.Since(t0); d >= 300*time.Millisecond {
		t.Fatalf("city b took %v", d)
	}
}

func TestTokenReadOnceAndRenewedOnceOn401(t *testing.T) {
	var pages atomic.Int32
	var tok atomic.Value
	tok.Store("t1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/el/stops") {
			pages.Add(1)
			fmt.Fprintf(w, "const token = '%s';", tok.Load())
			return
		}
		if strings.HasPrefix(r.URL.Path, "/denied") || r.Header.Get("Authorization") != "Bearer "+tok.Load().(string) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("User-Agent") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/missing") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/broken") {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, `{"ok":1}`)
	}))
	defer srv.Close()
	c := newClient("x", srv.Client())
	c.rest, c.pageURL = srv.URL, srv.URL+"/el/stops"
	c.p.setGap(time.Millisecond)
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b, err := c.get(context.Background(), fmt.Sprintf("/p/%d", i), priBackground); err != nil || string(b) != `{"ok":1}` {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 || pages.Load() != 1 {
		t.Fatalf("bad %d pages %d", bad.Load(), pages.Load())
	}
	tok.Store("t2") // the server rotates its token
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b, err := c.get(context.Background(), fmt.Sprintf("/q/%d", i), priBackground); err != nil || string(b) != `{"ok":1}` {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 || pages.Load() != 2 {
		t.Fatalf("after rotation: bad %d pages %d", bad.Load(), pages.Load())
	}
	if b, err := c.get(context.Background(), "/missing", priBackground); b != nil || err != nil {
		t.Fatalf("404: %q %v", b, err)
	}
	st := c.statsNow()
	if st.Errors != 0 || st.ByStatus["404"] != 1 || st.ByStatus["401"] < 1 || st.LastError != "" {
		t.Fatalf("stats %+v", st)
	}
	if _, err := c.get(context.Background(), "/broken", priBackground); err == nil {
		t.Fatal("502: no error")
	}
	if st = c.statsNow(); st.Errors != 1 || !strings.HasSuffix(st.LastError, " HTTP 502") {
		t.Fatalf("after 502: %+v", st)
	}
	// A 401 again after the new token is an error, counted once.
	if _, err := c.get(context.Background(), "/denied", priBackground); err == nil {
		t.Fatal("401 twice: no error")
	}
	if st = c.statsNow(); st.Errors != 2 || !strings.HasSuffix(st.LastError, " HTTP 401") {
		t.Fatalf("after 401 twice: %+v", st)
	}
}

func TestSamePathInFlightIsOneRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/el/stops") {
			io.WriteString(w, "token = 'k'")
			return
		}
		hits.Add(1)
		time.Sleep(30 * time.Millisecond)
		io.WriteString(w, `[]`)
	}))
	defer srv.Close()
	c := newClient("x", srv.Client())
	c.rest, c.pageURL = srv.URL, srv.URL+"/el/stops"
	c.p.setGap(time.Millisecond)
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); c.get(context.Background(), "/same", priUser) }()
	}
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("hits %d", hits.Load())
	}
}

// ---- HTTP ----

func TestParseCities(t *testing.T) {
	all, err := ParseCities("all")
	if err != nil || len(all) != 28 || all[0] != "agq" || all[27] != "jsi" || all[6] != "her" {
		t.Fatalf("all: %v %v", all, err)
	}
	two, err := ParseCities("pat, her,pat")
	if err != nil || fmt.Sprint(two) != "[pat her]" {
		t.Fatalf("list: %v %v", two, err)
	}
	// A slug still names its city (the staging unit's CITIES predates the codes).
	slugs, err := ParseCities("patras,heraklion,her")
	if err != nil || fmt.Sprint(slugs) != "[pat her]" {
		t.Fatalf("slugs: %v %v", slugs, err)
	}
	if _, err := ParseCities("athens"); err == nil {
		t.Fatal("athens is not a citybus city")
	}
	if _, err := ParseCities(""); err == nil {
		t.Fatal("empty list must fail")
	}
	seenID, seenSlug := map[string]bool{}, map[string]bool{}
	for _, c := range Cities {
		if seenID[c.ID] || seenSlug[c.Slug] || !regexp.MustCompile(`^[a-z]{3}$`).MatchString(c.ID) ||
			!regexp.MustCompile(`^[a-z]+$`).MatchString(c.Slug) || seenID[c.Slug] || seenSlug[c.ID] ||
			c.Agency == 0 || c.Sub == "" {
			t.Fatalf("bad conf %+v", c)
		}
		seenID[c.ID], seenSlug[c.Slug] = true, true
	}
}

// The city code: IATA where the city has its own airport, else three letters of the name
// (Patras: GPA is Araxos, so pat). Harold, 2026-11-07.
func TestCityCodes(t *testing.T) {
	want := map[string]string{
		"agrinio": "agq", "alexandroupoli": "axd", "arta": "art", "veroia": "ver", "volos": "vol",
		"drama": "dra", "heraklion": "her", "ioannina": "ioa", "kavala": "kva", "kastoria": "kso",
		"katerini": "kat", "corfu": "cfu", "kozani": "kzi", "komotini": "kom", "lamia": "lam",
		"larissa": "lra", "mesologgi": "mes", "mitilini": "mjt", "naousa": "nao", "xanthi": "xan",
		"patras": "pat", "ptolemaida": "pto", "serres": "ser", "chalkida": "cha", "chania": "chq",
		"chios": "jkh", "salamina": "sal", "skiathos": "jsi",
	}
	if len(Cities) != len(want) {
		t.Fatalf("%d cities, want %d", len(Cities), len(want))
	}
	for _, c := range Cities {
		if want[c.Slug] != c.ID {
			t.Fatalf("%s: code %q, want %q", c.Slug, c.ID, want[c.Slug])
		}
	}
}

// The state dir keeps the slug, so a code change does not reset line health.
func TestStateDirIsSlug(t *testing.T) {
	dir := t.TempDir()
	c, err := newCity(Conf{ID: "her", Slug: "heraklion", Sub: "irakleio", Agency: 110, Name: "Ηράκλειο"}, &fake{}, dir, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if c.dir != filepath.Join(dir, "heraklion") {
		t.Fatalf("dir %s", c.dir)
	}
}

func TestCitiesListHasSlug(t *testing.T) {
	c := testCity(t, &fake{answers: map[string]any{}}, t.TempDir())
	c.Conf.ID, c.Conf.Slug = "her", "heraklion"
	code, body := getBody(t, serviceWith(t, c), "/v1/cities")
	list, _ := body["cities"].([]any)
	if code != 200 || len(list) != 1 {
		t.Fatalf("cities: %d %v", code, body)
	}
	row := list[0].(map[string]any)
	if row["id"] != "her" || row["slug"] != "heraklion" {
		t.Fatalf("row %v", row)
	}
}

func serviceWith(t *testing.T, c *City) *Service {
	return &Service{cities: map[string]*City{c.Conf.ID: c}, order: []*City{c}, log: quiet}
}

func getBody(t *testing.T, s http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("%s: no CORS header", path)
	}
	return rec.Code, out
}

func TestHTTPNamespacesIDsPerCity(t *testing.T) {
	raw := lineRaw()
	a := answersOf(raw)
	a["/el/1/stops/live/D"] = liveAnswer{Vehicles: []LiveRow{{LineCode: "1", RouteCode: "10", Latitude: 35.3, Longitude: 25.11, DepartureMins: 2, VehicleCode: "v1"}}}
	f := &fake{answers: a}
	c := testCity(t, f, t.TempDir())
	c.Conf.ID = "heraklion"
	s := serviceWith(t, c)

	if code, body := getBody(t, s, "/heraklion/v1/lines/1"); code != 503 || body["error"] != "warming_up" {
		t.Fatalf("warming up: %d %v", code, body)
	}
	if code, body := getBody(t, s, "/heraklion/v2/lines"); code != 200 || len(body["lines"].([]any)) != 0 {
		t.Fatalf("v2 warming up: %d %v", code, body)
	}
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	if _, err := c.board(context.Background(), "D", priBackground); err != nil {
		t.Fatal(err)
	}

	_, lines := getBody(t, s, "/heraklion/v2/lines")
	l0 := lines["lines"].([]any)[0].(map[string]any)
	if l0["id"] != "heraklion:1" || l0["source"] != "heraklion" || l0["label"] != "1" {
		t.Fatalf("v2 line %v", l0)
	}
	_, stops := getBody(t, s, "/heraklion/v2/stops")
	s0 := stops["stops"].([]any)[0].([]any)
	if s0[0] != "heraklion:A" || fmt.Sprint(s0[5]) != "[heraklion:1]" {
		t.Fatalf("v2 stop %v", s0)
	}
	code, veh := getBody(t, s, "/heraklion/v2/lines/heraklion:1/vehicles")
	if code != 200 {
		t.Fatalf("vehicles %d", code)
	}
	vs := veh["vehicles"].([]any)
	if len(vs) != 1 {
		t.Fatalf("vehicles %v", veh)
	}
	v := vs[0].(map[string]any)
	if v["id"] != "heraklion:v1" || v["line"] != "heraklion:1" || !strings.HasPrefix(v["next_stop"].(string), "heraklion:") {
		t.Fatalf("vehicle %v", v)
	}
	if c.watched["1"] == 0 {
		t.Fatal("opening a line's vehicles watches it")
	}
	if code, d := getBody(t, s, "/heraklion/v2/lines/heraklion:1"); code != 200 || d["line"].(map[string]any)["id"] != "heraklion:1" ||
		d["patterns"].([]any)[0].(map[string]any)["id"] != "heraklion:10" || d["stops"].(map[string]any)["heraklion:B"] == nil {
		t.Fatalf("line detail %d %v", code, d)
	}
	if code, _ := getBody(t, s, "/heraklion/v2/lines/oasa:1"); code != 404 {
		t.Fatalf("another namespace is unknown: %d", code)
	}
	code, st := getBody(t, s, "/heraklion/v2/stops/heraklion:B")
	if code != 200 || st["stop"].(map[string]any)["id"] != "heraklion:B" {
		t.Fatalf("stop board %d %v", code, st)
	}
	for _, x := range st["arrivals"].([]any) {
		a := x.(map[string]any)
		if run, _ := a["run"].(string); run != "" && !regexp.MustCompile(`^heraklion:run:7:\d{4}-\d{2}-\d{2}$`).MatchString(run) {
			t.Fatalf("run id %q", run)
		}
	}
	_, v1 := getBody(t, s, "/heraklion/v1/lines/1")
	if v1v := v1["vehicles"].([]any)[0].(map[string]any); v1v["id"] != "v1" || v1v["line"] != "1" || v1v["next_stop_id"] == nil {
		t.Fatalf("v1 keeps bare codes: %v", v1v)
	}
	if code, h := getBody(t, s, "/heraklion/v1/health"); code != 200 || h["days"] != float64(7) {
		t.Fatalf("health %d %v", code, h)
	}
	if code, st := getBody(t, s, "/heraklion/v1/status"); code != 200 || st["ok"] != true || st["lines_active"] != float64(1) {
		t.Fatalf("status %d %v", code, st)
	}
	x, y := tileOf(35.3, 25.11, 13)
	if _, tl := getBody(t, s, fmt.Sprintf("/heraklion/v1/vehicles/tiles/13/%d/%d", x, y)); len(tl["vehicles"].([]any)) != 1 {
		t.Fatalf("tile %v", tl)
	}
	if code, _ := getBody(t, s, "/patras/v2/lines"); code != 404 {
		t.Fatalf("a city not enabled: %d", code)
	}
	if code, _ := getBody(t, s, "/heraklion/v9/nothing"); code != 404 {
		t.Fatalf("unknown path: %d", code)
	}
	if code, d := getBody(t, s, "/"); code != 200 || len(d["cities"].([]any)) != 1 {
		t.Fatalf("debug %d %v", code, d)
	}
}

func TestHTTPRunAndSchedule(t *testing.T) {
	c := testCity(t, &fake{answers: answersOf(lineRaw())}, t.TempDir())
	c.Conf.ID = "patras"
	s := serviceWith(t, c)
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	today := athensClock(time.Now().Unix()).Date
	code, run := getBody(t, s, "/patras/v2/runs/patras:run:7:"+today)
	if code != 200 {
		t.Fatalf("run %d", code)
	}
	r := run["run"].(map[string]any)
	if r["line"] != "patras:1" || r["pattern"] != "patras:10" || len(r["stops"].([]any)) != 4 {
		t.Fatalf("run %v", r)
	}
	if code, _ := getBody(t, s, "/patras/v2/runs/heraklion:run:7:"+today); code != 404 {
		t.Fatalf("a run of another city: %d", code)
	}
	if code, _ := getBody(t, s, "/patras/v2/runs/patras:run:7:2000-01-01"); code != 404 {
		t.Fatalf("a run far from today: %d", code)
	}
	code, sc := getBody(t, s, "/patras/v1/lines/1/schedule")
	dirs := sc["directions"].([]any)
	if code != 200 || len(dirs) != 1 {
		t.Fatalf("schedule %d %v", code, sc)
	}
	d0 := dirs[0].(map[string]any)
	deps := d0["departures"].([]any)
	if d0["headsign"] != "A - E" || len(deps) != 1 || deps[0].(map[string]any)["t"] != "10:00" {
		t.Fatalf("schedule %v", d0)
	}
}

// ---- review round 1 (Codex) ----

func TestBoardsInFlightAreLimitedAcrossAllPaths(t *testing.T) {
	var inflight, most atomic.Int32
	f := &fake{answers: answersOf(linesRaw("1", "2"))}
	f.hook = func(path string) (any, bool, error) {
		if !strings.Contains(path, "/stops/live/") {
			return nil, false, nil
		}
		n := inflight.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inflight.Add(-1)
		return nil, true, nil
	}
	c := testCity(t, f, t.TempDir())
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); c.board(context.Background(), fmt.Sprintf("1%c", 'A'+i%5), priUser) }()
	}
	wg.Wait()
	if most.Load() != maxInflight {
		t.Fatalf("max in flight %d", most.Load())
	}
}

func TestUserBoardAskedOncePer15sEvenWhenItFails(t *testing.T) {
	var calls atomic.Int32
	f := &fake{answers: answersOf(lineRaw())}
	f.hook = func(path string) (any, bool, error) {
		if strings.Contains(path, "/stops/live/") {
			calls.Add(1)
			return nil, true, fmt.Errorf("HTTP 500")
		}
		return nil, false, nil
	}
	c := testCity(t, f, t.TempDir())
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if b, _ := c.freshBoard("B"); b != nil {
			t.Fatalf("board %v", b)
		}
		time.Sleep(50 * time.Millisecond) // the read runs behind the answer
	}
	if calls.Load() != 1 {
		t.Fatalf("calls %d", calls.Load())
	}
	if _, refreshing := c.freshBoard("B"); refreshing {
		t.Fatal("no read is in flight after the failed one")
	}
	if c.v1StopBoard("B") == nil {
		t.Fatal("a failed board still gives the timetable")
	}
}

func TestWatchedLinesNeverStarveTheRotation(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	codes := []string{"1", "2", "3", "4", "5", "6", "7", "8"}
	f := &fake{answers: answersOf(linesRaw(codes...))}
	live := regexp.MustCompile(`stops/live/(.+)$`)
	f.hook = func(path string) (any, bool, error) {
		if m := live.FindStringSubmatch(path); m != nil {
			mu.Lock()
			asked = append(asked, m[1])
			mu.Unlock()
			time.Sleep(3 * time.Millisecond)
			return nil, true, nil
		}
		return nil, false, nil
	}
	c := testCity(t, f, t.TempDir())
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	for _, l := range codes {
		c.watchLocked(l)
	}
	c.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.rotate(ctx); close(done) }()
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(asked) < 12 {
		t.Fatalf("asked %v", asked)
	}
	hot := 0
	for _, s := range asked[:12] {
		if strings.HasSuffix(s, "D") { // the watched end stop; probes are the C stops
			hot++
		}
	}
	if hot == 0 || hot > 4 { // at most one in hotEvery (4)
		t.Fatalf("%d watched reads in 12: %v", hot, asked[:12])
	}
}

func TestFlightWaiterCanGiveUp(t *testing.T) {
	var f flight
	release := make(chan struct{})
	go f.do(context.Background(), "k", func() ([]byte, error) { <-release; return nil, nil })
	time.Sleep(5 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := f.do(ctx, "k", func() ([]byte, error) { return nil, nil }); err == nil {
		t.Fatal("the waiter must return when its ctx ends")
	}
	close(release)
}

// ---- review round 2 (Codex) ----

func TestWatchedLineKeepsPollingWithNoRotation(t *testing.T) {
	old := idleWait
	idleWait = 5 * time.Millisecond
	defer func() { idleWait = old }()
	var mu sync.Mutex
	var asked []string
	codes := []string{"1", "2"}
	f := &fake{answers: answersOf(linesRaw(codes...))}
	live := regexp.MustCompile(`stops/live/(.+)$`)
	f.hook = func(path string) (any, bool, error) {
		if m := live.FindStringSubmatch(path); m != nil {
			mu.Lock()
			asked = append(asked, m[1])
			mu.Unlock()
			return nil, true, nil
		}
		return nil, false, nil
	}
	c := testCity(t, f, t.TempDir())
	now := time.Now().Unix()
	c.health.Since = now - 8*D
	c.health.FirstSeen = map[string]int64{"1": now - 8*D, "2": now - 8*D} // both timetable-only: no rotation
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	if len(c.probes) != 0 {
		t.Fatalf("probes %v", c.probes)
	}
	c.mu.Lock()
	c.watchLocked("1")
	c.watchLocked("2")
	c.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.rotate(ctx); close(done) }()
	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	seen := map[string]bool{}
	for _, s := range asked {
		seen[s] = true
	}
	if !seen["1D"] || !seen["2D"] {
		t.Fatalf("both watched lines must be read: %v", asked)
	}
}

func TestConcurrentUserReadsShareOneRequest(t *testing.T) {
	var calls atomic.Int32
	a := answersOf(lineRaw())
	f := &fake{answers: a}
	f.hook = func(path string) (any, bool, error) {
		if strings.Contains(path, "/stops/live/") {
			calls.Add(1)
			time.Sleep(50 * time.Millisecond)
			return liveAnswer{Vehicles: []LiveRow{}}, true, nil
		}
		return nil, false, nil
	}
	c := testCity(t, f, t.TempDir())
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var told atomic.Int32
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b, refreshing := c.freshBoard("B"); b == nil && refreshing {
				told.Add(1)
			}
		}()
	}
	wg.Wait()
	// No caller waits for the board (rt#136): each hears that the one read is on its way.
	if told.Load() != 4 {
		t.Fatalf("callers told to ask again: %d of 4", told.Load())
	}
	time.Sleep(150 * time.Millisecond)
	if b, refreshing := c.freshBoard("B"); calls.Load() != 1 || b == nil || refreshing {
		t.Fatalf("calls %d, board %+v, refreshing %v", calls.Load(), b, refreshing)
	}
}

// The static version (gtfs_version) names the first day the city's data looked like this: a daily
// reload of the same data keeps it, so the bot's alerts and the web's stop index stay valid (rt#121).
func TestStaticVersionChangesOnlyWithTheData(t *testing.T) {
	dir := t.TempDir()
	raw := lineRaw()
	f := &fake{answers: answersOf(raw)}
	c := testCity(t, f, dir)
	day := time.Date(2026, 11, 7, 12, 0, 0, 0, athensLoc)
	c.now = func() time.Time { return day }
	ctx := context.Background()
	version := func() (string, string) {
		return c.status()["gtfs_version"].(string), c.staticLines()["gtfs_version"].(string)
	}
	if err := c.loadStatic(ctx, true, priWatched); err != nil {
		t.Fatal(err)
	}
	if s, l := version(); s != "2026-11-07" || l != s {
		t.Fatalf("first load: status %q lines %q", s, l)
	}
	day = day.Add(24 * time.Hour)
	if err := c.loadStatic(ctx, true, priWatched); err != nil {
		t.Fatal(err)
	}
	if s, l := version(); s != "2026-11-07" || l != s {
		t.Fatalf("same data a day later: status %q lines %q, want 2026-11-07", s, l)
	}
	// A restart reads the version back from static.json.
	c2 := testCity(t, &fake{hook: func(string) (any, bool, error) { return nil, true, nil }}, dir)
	c2.now = func() time.Time { return day }
	if err := c2.loadStatic(ctx, false, priWatched); err != nil {
		t.Fatal(err)
	}
	if v := c2.status()["gtfs_version"]; v != "2026-11-07" {
		t.Fatalf("after a restart: %q", v)
	}
	// A stop moves: a new version.
	day = day.Add(24 * time.Hour)
	raw.Stops[2].Latitude += 0.001
	f.mu.Lock()
	f.answers = answersOf(raw)
	f.mu.Unlock()
	if err := c.loadStatic(ctx, true, priWatched); err != nil {
		t.Fatal(err)
	}
	if s, l := version(); s != "2026-11-09" || l != s {
		t.Fatalf("changed data: status %q lines %q, want 2026-11-09", s, l)
	}
}

// A static.json from before the version was saved: its build day is its version, and a reload of
// the same data keeps that day.
func TestStaticVersionFromAnOldFile(t *testing.T) {
	dir := t.TempDir()
	raw := lineRaw()
	raw.BuiltAt = time.Date(2026, 11, 6, 9, 0, 0, 0, athensLoc).Unix()
	if err := os.MkdirAll(filepath.Join(dir, "t"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(filepath.Join(dir, "t", "static.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	c := testCity(t, &fake{answers: answersOf(raw)}, dir)
	c.now = func() time.Time { return time.Date(2026, 11, 8, 9, 0, 0, 0, athensLoc) }
	if err := c.loadStatic(context.Background(), false, priWatched); err != nil { // over 24 h: reloads
		t.Fatal(err)
	}
	if v := c.status()["gtfs_version"]; v != "2026-11-06" {
		t.Fatalf("version %q, want 2026-11-06", v)
	}
}

// ---- fast stop boards (Harold, 2026-11-10): a stale board at once, hub stops every 30 s ----

// A board up to MaxAge (120 s) old shows at once; one read for a new one runs behind it.
func TestStaleBoardShownAtOnceAndRefreshedBehind(t *testing.T) {
	var calls atomic.Int32
	f := &fake{answers: answersOf(lineRaw())}
	f.hook = func(path string) (any, bool, error) {
		if strings.Contains(path, "/stops/live/") {
			calls.Add(1)
			time.Sleep(150 * time.Millisecond)
			return liveAnswer{Vehicles: []LiveRow{}}, true, nil
		}
		return nil, false, nil
	}
	c := testCity(t, f, t.TempDir())
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	old := c.unix() - 60
	c.mu.Lock()
	c.live.Boards["B"] = Board{Stop: "B", At: old}
	c.mu.Unlock()
	t0 := time.Now()
	b, refreshing := c.freshBoard("B")
	if time.Since(t0) > 50*time.Millisecond || b == nil || b.At != old || !refreshing {
		t.Fatalf("board %+v refreshing %v after %v", b, refreshing, time.Since(t0))
	}
	// A second caller meanwhile: the same stale board, no second read.
	if b2, r2 := c.freshBoard("B"); b2 == nil || b2.At != old || !r2 {
		t.Fatalf("second caller %+v %v", b2, r2)
	}
	time.Sleep(250 * time.Millisecond)
	c.mu.Lock()
	at := c.live.Boards["B"].At
	c.mu.Unlock()
	if calls.Load() != 1 || at <= old {
		t.Fatalf("calls %d, board at %d (was %d)", calls.Load(), at, old)
	}
	if b3, r3 := c.freshBoard("B"); b3 == nil || b3.At <= old || r3 {
		t.Fatalf("after the read %+v %v", b3, r3)
	}
}

// The HTTP answer says when a new board is on its way, so the client asks again soon.
func TestStopAnswerSaysRefreshing(t *testing.T) {
	f := &fake{answers: answersOf(lineRaw())}
	f.hook = func(path string) (any, bool, error) {
		if strings.Contains(path, "/stops/live/") {
			time.Sleep(100 * time.Millisecond)
			return liveAnswer{Vehicles: []LiveRow{}}, true, nil
		}
		return nil, false, nil
	}
	c := testCity(t, f, t.TempDir())
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.live.Boards["B"] = Board{Stop: "B", At: c.unix() - 60}
	c.mu.Unlock()
	if v1 := c.v1StopBoard("B"); v1 == nil || v1["refreshing"] != true {
		t.Fatalf("v1 %v", v1)
	}
	if v2 := c.v2StopBoard("t:B"); v2 == nil || v2["refreshing"] != true {
		t.Fatalf("v2 %v", v2)
	}
	time.Sleep(200 * time.Millisecond)
	if v1 := c.v1StopBoard("B"); v1 == nil || v1["refreshing"] != false {
		t.Fatalf("v1 after %v", v1)
	}
}

// The hub stops: the most lines first, then the most routes, then the code; at least 2 lines.
func TestHubStopsMostLinesFirst(t *testing.T) {
	m := &Model{Stops: map[string]*Stop{
		"a": {Code: "a", Lines: []string{"1", "2", "3"}},
		"b": {Code: "b", Lines: []string{"1", "2", "3", "4"}},
		"c": {Code: "c", Lines: []string{"1", "2"}},
		"d": {Code: "d", Lines: []string{"1", "2"}},
		"e": {Code: "e", Lines: []string{"1"}},
	}, Routes: map[string]*Route{
		"r1": {Code: "r1", Stops: []string{"d", "c"}},
		"r2": {Code: "r2", Stops: []string{"d"}},
	}}
	if got := hubStops(m, 3); !reflect.DeepEqual(got, []string{"b", "a", "d"}) {
		t.Fatalf("hubs %v", got)
	}
	if got := hubStops(m, 10); !reflect.DeepEqual(got, []string{"b", "a", "d", "c"}) {
		t.Fatalf("hubs %v (a 1-line stop is no hub)", got)
	}
}

// A hub is read when its board is 30 s old (or it has none), once in 30 s, in turn.
func TestDueHubEvery30s(t *testing.T) {
	c := testCity(t, &fake{answers: answersOf(lineRaw())}, t.TempDir())
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1_800_000_000, 0)
	c.now = func() time.Time { return clock }
	now := c.unix()
	c.mu.Lock()
	c.hubs = []string{"B", "C", "D"}
	c.live.Boards["C"] = Board{Stop: "C", At: now - 10} // fresh: not due
	c.mu.Unlock()
	if h := c.dueHub(now); h != "B" {
		t.Fatalf("first %q", h)
	}
	if h := c.dueHub(now); h != "D" {
		t.Fatalf("second %q", h)
	}
	if h := c.dueHub(now); h != "" {
		t.Fatalf("third %q", h)
	}
	if h := c.dueHub(now + 25); h != "C" { // C's board is 35 s old now
		t.Fatalf("C at +25: %q", h)
	}
	if h := c.dueHub(now + 30); h != "B" {
		t.Fatalf("B again at +30: %q", h)
	}
}

// The hub reads count in the pace: two a minute each, as if they were probes.
func TestHubsCountInTheGap(t *testing.T) {
	c := testCity(t, &fake{answers: answersOf(linesRaw("1", "2", "3", "4", "5", "6", "7", "8"))}, t.TempDir())
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	want := gapFor(len(c.probes) + len(c.skipped) + 1 + 2*len(c.hubs))
	if got := int(c.up.pacer().gapNow() / time.Millisecond); got != want {
		t.Fatalf("gap %d want %d (probes %d hubs %d)", got, want, len(c.probes), len(c.hubs))
	}
}

// A board older than MaxAge that prune has not dropped yet is never shown: it counts as no board.
func TestBoardOverMaxAgeIsNotShown(t *testing.T) {
	f := &fake{answers: answersOf(lineRaw())}
	f.hook = func(path string) (any, bool, error) {
		if strings.Contains(path, "/stops/live/") {
			return liveAnswer{Vehicles: []LiveRow{}}, true, nil
		}
		return nil, false, nil
	}
	c := testCity(t, f, t.TempDir())
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	old := c.unix() - MaxAge - 5
	c.mu.Lock()
	c.live.Boards["B"] = Board{Stop: "B", At: old}
	c.mu.Unlock()
	if b, r := c.freshBoard("B"); b != nil || !r {
		t.Fatalf("board %+v refreshing %v", b, r)
	}
	time.Sleep(100 * time.Millisecond)
	if b, r := c.freshBoard("B"); b == nil || b.At <= old || r {
		t.Fatalf("after the read: board %+v refreshing %v", b, r)
	}
}

// ---- estimated arrivals (rt#136 B): a stop with no board answers at once ----

// A stop with no board never waits for citybus.gr: the answer comes at once, with the tracked bus
// as an estimate and refreshing = true. Once the stop's own board is in, the board wins.
func TestStopWithNoBoardAnswersAtOnceWithEstimates(t *testing.T) {
	var calls atomic.Int32
	f := &fake{answers: answersOf(lineRaw())}
	f.hook = func(path string) (any, bool, error) {
		switch {
		case strings.HasSuffix(path, "/stops/live/B"): // the rotation saw v1, 1 min before B
			return liveAnswer{Vehicles: []LiveRow{{LineCode: "1", RouteCode: "10", Latitude: 35.3, Longitude: 25.105, DepartureMins: 1, VehicleCode: "v1"}}}, true, nil
		case strings.HasSuffix(path, "/stops/live/C"): // a slow board; citybus.gr gives v1 4 min
			calls.Add(1)
			time.Sleep(150 * time.Millisecond)
			return liveAnswer{Vehicles: []LiveRow{{LineCode: "1", RouteCode: "10", Latitude: 35.3, Longitude: 25.105, DepartureMins: 4, VehicleCode: "v1"}}}, true, nil
		}
		return nil, false, nil
	}
	c := testCity(t, f, t.TempDir())
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	if _, err := c.board(context.Background(), "B", priWatched); err != nil {
		t.Fatal(err)
	}
	live := func(arrivals any) []map[string]any {
		out := []map[string]any{}
		for _, a := range arrivals.([]obj) {
			if a["source"] == "live" {
				out = append(out, a)
			}
		}
		return out
	}
	t0 := time.Now()
	v1 := c.v1StopBoard("C")
	if d := time.Since(t0); d > 50*time.Millisecond {
		t.Fatalf("the answer waited %v for the board", d)
	}
	rows := live(v1["arrivals"])
	if v1["refreshing"] != true || len(rows) != 1 || rows[0]["estimate"] != true || rows[0]["vehicle"] != "v1" {
		t.Fatalf("v1 %v", v1)
	}
	if eta := rows[0]["eta_s"].(int64); eta < 170 || eta > 180 { // 1 min to B + 2 min B-C on the timetable
		t.Fatalf("eta %d", eta)
	}
	v2 := c.v2StopBoard("t:C")
	rows = live(v2["arrivals"])
	if v2["refreshing"] != true || len(rows) != 1 || rows[0]["estimate"] != true || rows[0]["vehicle"] != "t:v1" || rows[0]["status"] != "running" {
		t.Fatalf("v2 %v", v2)
	}
	time.Sleep(250 * time.Millisecond)
	v1 = c.v1StopBoard("C")
	rows = live(v1["arrivals"])
	if calls.Load() != 1 || v1["refreshing"] != false || len(rows) != 1 || rows[0]["estimate"] != false {
		t.Fatalf("with the board: calls %d, v1 %v", calls.Load(), v1)
	}
	if eta := rows[0]["eta_s"].(int64); eta < 230 || eta > 240 {
		t.Fatalf("board eta %d", eta)
	}
	rows = live(c.v2StopBoard("t:C")["arrivals"])
	if len(rows) != 1 || rows[0]["estimate"] != false {
		t.Fatalf("v2 with the board: %v", rows)
	}
}

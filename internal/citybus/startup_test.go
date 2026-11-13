package citybus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writeStatic puts raw as the city's static.json in dir, built at builtAt.
func writeStatic(t *testing.T, dir string, raw *RawCity, builtAt int64) {
	t.Helper()
	raw.BuiltAt = builtAt
	if err := os.MkdirAll(filepath.Join(dir, "t"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(filepath.Join(dir, "t", "static.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// One city at a time builds its model from disk (#185): all 28 at once peaked at about 330 MB.
func TestStaticLoadWaitsForTheDiskGate(t *testing.T) {
	dir := t.TempDir()
	c := testCity(t, &fake{}, dir)
	writeStatic(t, dir, lineRaw(), c.unix())
	diskGate <- struct{}{} // another city is building its model
	done := make(chan error, 1)
	go func() { done <- c.loadStatic(context.Background(), false, priWatched) }()
	select {
	case err := <-done:
		<-diskGate
		t.Fatalf("loaded while another city held the disk gate (err %v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	if c.ready() {
		<-diskGate
		t.Fatal("a model was installed while another city held the disk gate")
	}
	<-diskGate
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the load did not go on after the gate was free")
	}
	if !c.ready() {
		t.Fatal("no model after the load")
	}
}

// A stale city installs its file at once and only then waits for its turn to reload from
// citybus.gr: every city is ready before any network reload (#185).
func TestStaleCityIsReadyBeforeItsReload(t *testing.T) {
	dir := t.TempDir()
	c := testCity(t, &fake{answers: answersOf(lineRaw())}, dir)
	writeStatic(t, dir, lineRaw(), c.unix()-2*86400)
	fetchGate <- struct{}{} // another city is reloading
	done := make(chan error, 1)
	go func() { done <- c.loadStatic(context.Background(), false, priWatched) }()
	deadline := time.Now().Add(5 * time.Second)
	for !c.ready() {
		if time.Now().After(deadline) {
			<-fetchGate
			t.Fatal("a stale city must install its file before it waits for the reload")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case err := <-done:
		<-fetchGate
		t.Fatalf("reloaded while another city held the fetch gate (err %v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	<-fetchGate
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the reload did not go on after the gate was free")
	}
	if len(fetchGate) != 0 || len(diskGate) != 0 {
		t.Fatal("a finished load must release both gates")
	}
}

// A stop (ctx cancelled) while waiting for a gate ends the load and takes no slot.
func TestStaticLoadStopsWhileWaitingForAGate(t *testing.T) {
	dir := t.TempDir()
	c := testCity(t, &fake{}, dir)
	writeStatic(t, dir, lineRaw(), c.unix())
	diskGate <- struct{}{}
	defer func() { <-diskGate }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.loadStatic(ctx, false, priWatched) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled load must not wait for the gate")
	}
	if len(diskGate) != 1 {
		t.Fatalf("the gate holds %d slots, want only the test's own", len(diskGate))
	}
}

// A stale city starts its rotation on the file at once; its reload from citybus.gr runs in the
// background and does not hold up the live buses (#185).
func TestStaleCityRotatesBeforeItsReload(t *testing.T) {
	dir := t.TempDir()
	var boards atomic.Int32
	f := &fake{answers: answersOf(lineRaw()), p: newPacer(time.Millisecond)}
	f.hook = func(path string) (any, bool, error) {
		if strings.Contains(path, "/stops/live/") {
			boards.Add(1)
		}
		return nil, false, nil
	}
	c := testCity(t, f, dir)
	writeStatic(t, dir, lineRaw(), c.unix()-2*86400)
	fetchGate <- struct{}{} // another city is reloading, for as long as this test runs
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.rotate(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for boards.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	<-fetchGate
	if boards.Load() == 0 {
		t.Fatal("a stale city must read boards while its reload waits for the fetch gate")
	}
}

// ctxFetcher answers nothing until the request is cancelled: a reload that hangs.
type ctxFetcher struct{ p *pacer }

func (f ctxFetcher) pacer() *pacer { return f.p }
func (f ctxFetcher) get(ctx context.Context, path string, pri Priority) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A hung reload gives the fetch gate back after reloadLimit, so the other cities can reload.
func TestHungReloadReleasesTheFetchGate(t *testing.T) {
	old := reloadLimit
	reloadLimit = 50 * time.Millisecond
	defer func() { reloadLimit = old }()
	c, err := newCity(Conf{ID: "t", Slug: "t", Sub: "t", Agency: 1, Name: "T"}, ctxFetcher{p: newPacer(time.Second)}, t.TempDir(), quiet)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.loadStatic(context.Background(), true, priBackground) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hung reload must end after reloadLimit")
	}
	if len(fetchGate) != 0 {
		t.Fatal("a hung reload must give the fetch gate back")
	}
}

// A city with a model but no stop board yet does not know where its buses are: an empty answer
// would read as «no bus running» (night) and clear the web's map. It answers warming_up until
// its first board, empty or not (#185, athens-transit-map#78).
func TestNoBoardYetIsWarmingUp(t *testing.T) {
	a := answersOf(lineRaw())
	a["/el/1/stops/live/D"] = liveAnswer{} // night: a board with no bus
	c := testCity(t, &fake{answers: a}, t.TempDir())
	if err := c.loadStatic(context.Background(), true, priWatched); err != nil {
		t.Fatal(err)
	}
	s := serviceWith(t, c)
	state := func(body map[string]any) any {
		src, _ := body["sources"].([]any)
		if len(src) != 1 {
			return nil
		}
		return src[0].(map[string]any)["state"]
	}
	maxAge := func(path string) string {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Header().Get("Cache-Control")
	}
	for _, p := range []string{"/t/v1/vehicles/tiles/13/4680/3304", "/t/v1/lines/1"} {
		if code, body := getBody(t, s, p); code != 503 || body["error"] != "warming_up" {
			t.Fatalf("%s before the first board: %d %v", p, code, body)
		}
	}
	for _, p := range []string{"/t/v2/lines/t:1/vehicles", "/t/v2/lines"} {
		if code, body := getBody(t, s, p); code != 200 || state(body) != "warming_up" {
			t.Fatalf("%s before the first board: %d state %v", p, code, state(body))
		}
	}
	if cc := maxAge("/t/v2/lines"); cc != "public, max-age=5" {
		t.Fatalf("a warming_up answer is cached %q, want max-age=5", cc)
	}

	if _, err := c.board(context.Background(), "D", priUser); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/t/v1/vehicles/tiles/13/4680/3304", "/t/v1/lines/1"} {
		if code, body := getBody(t, s, p); code != 200 || body["vehicles"] == nil {
			t.Fatalf("%s after an empty board: %d %v", p, code, body)
		}
	}
	if code, body := getBody(t, s, "/t/v2/lines/t:1/vehicles"); code != 200 || state(body) != "ok" {
		t.Fatalf("v2 vehicles after an empty board: %d state %v", code, state(body))
	}
	if cc := maxAge("/t/v2/lines"); cc != "public, max-age=3600" {
		t.Fatalf("a complete /v2/lines is cached %q, want max-age=3600", cc)
	}
}

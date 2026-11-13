package citybus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"
)

// TestMeasureStartup measures the disk part of a start: read static.json, unmarshal, buildModel,
// newLiveCity, per city. Off unless ATRT_MEASURE_STATE points at a copy of the citybus state.
// ATRT_MEASURE_MODE: "serial" (one city at a time) or "parallel" (all at once, as rotate does).
// Research for docs/research/zero-gap-deploys.md; not a regression test.
func TestMeasureStartup(t *testing.T) {
	dir := os.Getenv("ATRT_MEASURE_STATE")
	if dir == "" {
		t.Skip("ATRT_MEASURE_STATE not set")
	}
	mode := os.Getenv("ATRT_MEASURE_MODE")
	var files []string
	for _, c := range Cities {
		f := filepath.Join(dir, c.Slug, "static.json")
		if _, err := os.Stat(f); err == nil {
			files = append(files, f)
		}
	}
	var peak uint64
	stop := make(chan struct{})
	var pw sync.WaitGroup
	pw.Add(1)
	go func() {
		defer pw.Done()
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				runtime.ReadMemStats(&m)
				if m.HeapInuse > peak {
					peak = m.HeapInuse
				}
			}
		}
	}()
	type res struct {
		city            string
		size            int
		read, json, mdl time.Duration
	}
	var mu sync.Mutex
	var out []res
	models := make([]*Model, len(files))
	one := func(i int, f string) {
		t0 := time.Now()
		b, err := os.ReadFile(f)
		if err != nil {
			t.Error(err)
			return
		}
		t1 := time.Now()
		var raw RawCity
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Error(err)
			return
		}
		n := len(b)
		b = nil
		t2 := time.Now()
		m := buildModel(&raw)
		_ = newLiveCity(m)
		models[i] = m
		t3 := time.Now()
		mu.Lock()
		out = append(out, res{filepath.Base(filepath.Dir(f)), n, t1.Sub(t0), t2.Sub(t1), t3.Sub(t2)})
		mu.Unlock()
	}
	start := time.Now()
	if mode == "parallel" {
		var wg sync.WaitGroup
		for i, f := range files {
			wg.Add(1)
			go func() { defer wg.Done(); one(i, f) }()
		}
		wg.Wait()
	} else {
		for i, f := range files {
			one(i, f)
		}
	}
	wall := time.Since(start)
	close(stop)
	pw.Wait()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	sort.Slice(out, func(i, j int) bool { return out[i].json+out[i].mdl > out[j].json+out[j].mdl })
	var sj, sm time.Duration
	for _, r := range out {
		sj += r.json
		sm += r.mdl
	}
	for _, r := range out[:min(6, len(out))] {
		t.Logf("%-14s %5.1f MB read %4dms json %5dms model %5dms", r.city, float64(r.size)/1e6, r.read.Milliseconds(), r.json.Milliseconds(), r.mdl.Milliseconds())
	}
	t.Logf("mode=%s cities=%d GOMAXPROCS=%d wall=%dms sum(json)=%dms sum(model)=%dms peakHeapInuse=%dMB liveHeapAfterGC=%dMB",
		mode, len(out), runtime.GOMAXPROCS(0), wall.Milliseconds(), sj.Milliseconds(), sm.Milliseconds(), peak>>20, m.HeapInuse>>20)
	runtime.KeepAlive(models)
}

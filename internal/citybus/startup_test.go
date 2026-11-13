package citybus

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

package histrelease

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/history"
)

var athens, _ = time.LoadLocation("Europe/Athens")

// hourFile writes one history hour (UTC name) with the given rows, gzipped unless plain.
func hourFile(t *testing.T, dir, hour string, plain bool, rows ...string) {
	t.Helper()
	body := history.Header + strings.Join(rows, "")
	if plain {
		if err := os.WriteFile(filepath.Join(dir, hour+".csv"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write([]byte(body))
	zw.Close()
	if err := os.WriteFile(filepath.Join(dir, hour+".csv.gz"), b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func row(fixT int64, veh string) string {
	return fmt.Sprintf("%d,kva:4,4,kva:%s,40.9,24.4,v1,,,-1,,0,\n", fixT, veh)
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func unix(s string) int64 {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, athens)
	if err != nil {
		panic(err)
	}
	return t.Unix()
}

// 2026-11-12 in Athens (UTC+2) is 2026-11-11T22:00Z to 2026-11-12T22:00Z.
func historyDir(t *testing.T) string {
	dir := t.TempDir()
	hourFile(t, dir, "2026-11-11T10", false, row(unix("2026-11-11 12:10"), "a"))
	hourFile(t, dir, "2026-11-11T21", false, row(unix("2026-11-11 23:59"), "a"))
	// 00:00 Athens on the 12th; a row of the 11th arrived late in this hour.
	hourFile(t, dir, "2026-11-11T22", false, row(unix("2026-11-11 23:59")+30, "late"), row(unix("2026-11-12 00:01"), "b"))
	hourFile(t, dir, "2026-11-12T21", false, row(unix("2026-11-12 23:58"), "c"))
	// 00:00 Athens on the 13th: a late row of the 12th and the first row of the 13th.
	hourFile(t, dir, "2026-11-12T22", false, row(unix("2026-11-12 23:59")+50, "late12"), row(unix("2026-11-13 00:05"), "d"))
	// The 14th starts: the writer ran past the end of the 13th.
	hourFile(t, dir, "2026-11-13T23", false, row(unix("2026-11-14 01:10"), "f"))
	return dir
}

func TestDayKeepsOnlyTheRowsOfTheLocalDay(t *testing.T) {
	dir := historyDir(t)
	gz, rows, err := Day(dir, "2026-11-12", athens)
	if err != nil {
		t.Fatal(err)
	}
	want := history.Header + row(unix("2026-11-12 00:01"), "b") + row(unix("2026-11-12 23:58"), "c") + row(unix("2026-11-12 23:59")+50, "late12")
	if got := gunzip(t, gz); got != want || rows != 3 {
		t.Fatalf("rows %d, got\n%s\nwant\n%s", rows, got, want)
	}
}

func TestDaysOnlyCompletePastDays(t *testing.T) {
	dir := historyDir(t)
	// 2026-11-11: history starts that day (partial). 2026-11-12: ends 22:00Z, settled at 02:00Z
	// on the 13th. 2026-11-13: not over.
	now := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	if got := Days(dir, athens, now); strings.Join(got, ",") != "2026-11-12" {
		t.Fatalf("days %v, want [2026-11-12]", got)
	}
	if got := Days(dir, athens, now.Add(-time.Minute)); len(got) != 0 {
		t.Fatalf("before settle: days %v, want none", got)
	}
	// The writer stopped before the day's end: no file from its end on, so not covered.
	short := t.TempDir()
	hourFile(t, short, "2026-11-11T10", false, row(unix("2026-11-11 12:10"), "a"))
	hourFile(t, short, "2026-11-12T20", false, row(unix("2026-11-12 22:10"), "b"))
	if got := Days(short, athens, now); len(got) != 0 {
		t.Fatalf("history ends before the day's end: days %v, want none", got)
	}
	// Pruned past the day's start: neither listed nor built.
	pruned := historyDir(t)
	os.Remove(filepath.Join(pruned, "2026-11-11T10.csv.gz"))
	os.Remove(filepath.Join(pruned, "2026-11-11T21.csv.gz"))
	if got := Days(pruned, athens, now); len(got) != 0 {
		t.Fatalf("pruned: days %v, want none", got)
	}
	if _, _, err := Day(pruned, "2026-11-12", athens); err == nil {
		t.Fatal("pruned: Day must fail")
	}
	// An hour of the day still plain (not gzipped): not ready.
	hourFile(t, dir, "2026-11-12T23", true, row(unix("2026-11-13 01:00"), "e"))
	if got := Days(dir, athens, now); len(got) != 0 {
		t.Fatalf("plain hour: days %v, want none", got)
	}
}

// fakeGitHub is the part of the GitHub releases API the uploader uses.
type fakeGitHub struct {
	mu       sync.Mutex
	releases map[string]int // tag -> id
	assets   map[int][]map[string]any
	uploads  []string
	auth     []string
}

func newFake() (*fakeGitHub, *httptest.Server) {
	f := &fakeGitHub{releases: map[string]int{}, assets: map[int][]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		p := r.URL.Path
		switch {
		case r.Method == "GET" && strings.HasPrefix(p, "/repos/o/r/releases/tags/"):
			id, ok := f.releases[strings.TrimPrefix(p, "/repos/o/r/releases/tags/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": id, "upload_url": "http://" + r.Host + fmt.Sprintf("/up/%d/assets{?name,label}", id)})
		case r.Method == "POST" && p == "/repos/o/r/releases":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["make_latest"] != "false" {
				http.Error(w, "history release must not be latest", 400)
				return
			}
			id := len(f.releases) + 1
			f.releases[body["tag_name"].(string)] = id
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"id": id, "upload_url": "http://" + r.Host + fmt.Sprintf("/up/%d/assets{?name,label}", id)})
		case r.Method == "GET" && strings.HasPrefix(p, "/repos/o/r/releases/") && strings.HasSuffix(p, "/assets"):
			var id int
			fmt.Sscanf(p, "/repos/o/r/releases/%d/assets", &id)
			if r.URL.Query().Get("page") != "1" {
				json.NewEncoder(w).Encode([]any{})
				return
			}
			json.NewEncoder(w).Encode(f.assets[id])
		case r.Method == "POST" && strings.HasPrefix(p, "/up/"):
			var id int
			fmt.Sscanf(p, "/up/%d/assets", &id)
			name := r.URL.Query().Get("name")
			b, _ := io.ReadAll(r.Body)
			if r.Header.Get("Content-Type") != "application/gzip" || len(b) == 0 {
				http.Error(w, "bad upload", 400)
				return
			}
			f.uploads = append(f.uploads, name)
			f.assets[id] = append(f.assets[id], map[string]any{"id": 100 + len(f.uploads), "name": name, "state": "uploaded"})
			w.WriteHeader(201)
			w.Write([]byte("{}"))
		default:
			http.Error(w, "unexpected "+r.Method+" "+p, 500)
		}
	}))
	return f, srv
}

func TestRunUploadsEachDayOnce(t *testing.T) {
	dir := historyDir(t)
	f, srv := newFake()
	defer srv.Close()
	u := &Uploader{API: srv.URL, Repo: "o/r", Token: "tok", Client: srv.Client()}
	now := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC) // 12th and 13th are complete
	up, err := Run(u, dir, athens, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(up, ",") != "2026-11-12,2026-11-13" {
		t.Fatalf("uploaded %v", up)
	}
	if strings.Join(f.uploads, ",") != "citybus-history-2026-11-12.csv.gz,citybus-history-2026-11-13.csv.gz" {
		t.Fatalf("assets %v", f.uploads)
	}
	if _, ok := f.releases["history-citybus-2026-11"]; !ok || len(f.releases) != 1 {
		t.Fatalf("releases %v, want one: history-citybus-2026-11", f.releases)
	}
	// A second run (the next timer tick) uploads nothing.
	up, err = Run(u, dir, athens, now.Add(time.Hour))
	if err != nil || len(up) != 0 || len(f.uploads) != 2 {
		t.Fatalf("second run: uploaded %v, err %v, assets %v", up, err, f.uploads)
	}
	for _, a := range f.auth {
		if a != "Bearer tok" {
			t.Fatalf("auth header %q", a)
		}
	}
}

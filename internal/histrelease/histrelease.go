// Package histrelease publishes the citybus.gr fix history as GitHub Release assets
// (staseis-rt#152): one gzipped CSV per local day, in a release per month
// (tag history-citybus-YYYY-MM). Only a day that is over, settled (stats.Settle) and fully in the
// history is uploaded, and a day whose asset exists is skipped, so a re-run uploads nothing twice.
package histrelease

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/history"
	"github.com/angelospk/athens-transit-rt/internal/stats"
)

const (
	hourLayout = "2006-01-02T15" // history file names, UTC
	dateLayout = "2006-01-02"
)

// Tag is the release of a day's month; Asset is the day's file name.
func Tag(date string) string   { return "history-citybus-" + date[:7] }
func Asset(date string) string { return "citybus-history-" + date + ".csv.gz" }

// Days lists the local days that are complete in the history directory: over for stats.Settle,
// every hour they need gzipped (stats.Inputs), and with a history file older than their start,
// so a day the history began in, or has partly pruned, is never published.
func Days(dir string, loc *time.Location, now time.Time) []string {
	entries, _ := os.ReadDir(dir)
	seen := map[string]bool{}
	var oldest time.Time
	for _, e := range entries {
		hour, _, _ := strings.Cut(e.Name(), ".")
		t, err := time.Parse(hourLayout, hour)
		if err != nil {
			continue
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
		seen[t.Add(-stats.Late).In(loc).Format(dateLayout)] = true
		seen[t.Add(time.Hour-time.Second).In(loc).Format(dateLayout)] = true
	}
	var days []string
	for d := range seen {
		start, end, err := stats.Bounds(d, loc)
		if err != nil || now.Before(end.Add(stats.Settle)) || !oldest.Before(start.UTC().Truncate(time.Hour)) {
			continue
		}
		if files, ready, err := stats.Inputs(dir, d, loc); err != nil || !ready || len(files) == 0 {
			continue
		}
		days = append(days, d)
	}
	sort.Strings(days)
	return days
}

// Day returns the gzipped CSV of one local day: the history header, then every row whose fix
// time is in the day, as written and in file order. rows is the number of rows.
func Day(dir, date string, loc *time.Location) (gz []byte, rows int, err error) {
	start, end, err := stats.Bounds(date, loc)
	if err != nil {
		return nil, 0, err
	}
	files, ready, err := stats.Inputs(dir, date, loc)
	if err != nil {
		return nil, 0, err
	}
	if !ready {
		return nil, 0, fmt.Errorf("history %s: an hour is not gzipped yet", date)
	}
	var b bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
	zw.Write([]byte(history.Header))
	from, to := start.Unix(), end.Unix()
	for _, path := range files {
		n, err := copyDay(zw, path, from, to)
		if err != nil {
			return nil, 0, fmt.Errorf("history %s: %s: %w", date, filepath.Base(path), err)
		}
		rows += n
	}
	if err := zw.Close(); err != nil {
		return nil, 0, err
	}
	return b.Bytes(), rows, nil
}

// copyDay copies the rows of one gzipped hour file with from <= fix_t < to.
func copyDay(w io.Writer, path string, from, to int64) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f) // multistream: every member
	if err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	n := 0
	for first := true; sc.Scan(); first = false {
		line := sc.Text()
		if first {
			if line+"\n" != history.Header {
				return 0, fmt.Errorf("unexpected header %q", line)
			}
			continue
		}
		head, _, _ := strings.Cut(line, ",")
		t, err := strconv.ParseInt(head, 10, 64)
		if err != nil || t < from || t >= to {
			continue
		}
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return 0, err
		}
		n++
	}
	return n, sc.Err()
}

// Uploader talks to the GitHub releases API of one repository.
type Uploader struct {
	API    string // https://api.github.com
	Repo   string // owner/name
	Token  string // contents: write on Repo
	Client *http.Client
}

type release struct {
	ID        int64  `json:"id"`
	UploadURL string `json:"upload_url"`
}

type asset struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

func (u *Uploader) do(method, rawURL string, body io.Reader, ctype string, size int64, out any) (int, error) {
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+u.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
		req.ContentLength = size
	}
	resp, err := u.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusNotFound && method == http.MethodGet {
		return resp.StatusCode, nil
	}
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("%s %s: HTTP %d: %.200s", method, rawURL, resp.StatusCode, data)
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(data, out)
	}
	return resp.StatusCode, nil
}

func (u *Uploader) repoURL(p string) string {
	return strings.TrimRight(u.API, "/") + "/repos/" + u.Repo + p
}

// release returns the release of tag, created (not marked latest) when missing.
func (u *Uploader) release(tag string) (*release, error) {
	var r release
	code, err := u.do(http.MethodGet, u.repoURL("/releases/tags/"+url.PathEscape(tag)), nil, "", 0, &r)
	if err != nil || code != http.StatusNotFound {
		return &r, err
	}
	body, _ := json.Marshal(map[string]any{
		"tag_name": tag, "name": "citybus.gr fix history " + strings.TrimPrefix(tag, "history-citybus-"),
		"body":        "Raw citybus.gr fix history: one gzipped CSV per day (Europe/Athens), all cities. Format: docs/citybus.md.",
		"make_latest": "false",
	})
	_, err = u.do(http.MethodPost, u.repoURL("/releases"), bytes.NewReader(body), "application/json", int64(len(body)), &r)
	return &r, err
}

// assets lists every asset of a release.
func (u *Uploader) assets(id int64) ([]asset, error) {
	var all []asset
	for page := 1; ; page++ {
		var list []asset
		if _, err := u.do(http.MethodGet, u.repoURL(fmt.Sprintf("/releases/%d/assets?per_page=100&page=%d", id, page)), nil, "", 0, &list); err != nil {
			return nil, err
		}
		all = append(all, list...)
		if len(list) < 100 {
			return all, nil
		}
	}
}

func (u *Uploader) upload(r *release, name string, data []byte) error {
	base, _, _ := strings.Cut(r.UploadURL, "{")
	_, err := u.do(http.MethodPost, base+"?name="+url.QueryEscape(name), bytes.NewReader(data), "application/gzip", int64(len(data)), nil)
	return err
}

// Run uploads every complete day whose asset is not in its month's release yet and returns the
// days uploaded. An asset left by a failed upload (state other than "uploaded") is deleted and
// uploaded again. One day's error does not stop the others.
func Run(u *Uploader, dir string, loc *time.Location, now time.Time) ([]string, error) {
	var done []string
	var errs []error
	rels := map[string]*release{}
	for _, d := range Days(dir, loc, now) {
		tag := Tag(d)
		r, ok := rels[tag]
		if !ok {
			var err error
			if r, err = u.release(tag); err != nil {
				errs = append(errs, err)
				continue
			}
			rels[tag] = r
		}
		list, err := u.assets(r.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		skip := false
		for _, a := range list {
			if a.Name != Asset(d) {
				continue
			}
			if a.State == "uploaded" {
				skip = true
			} else if _, err := u.do(http.MethodDelete, u.repoURL(fmt.Sprintf("/releases/assets/%d", a.ID)), nil, "", 0, nil); err != nil {
				errs = append(errs, err)
				skip = true
			}
		}
		if skip {
			continue
		}
		gz, rows, err := Day(dir, d, loc)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if rows == 0 {
			continue // no bus that day: no asset
		}
		if err := u.upload(r, Asset(d), gz); err != nil {
			errs = append(errs, fmt.Errorf("history %s: %w", d, err))
			continue
		}
		done = append(done, d)
	}
	return done, errors.Join(errs...)
}

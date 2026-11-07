package citybus

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

// citybus.gr access: one paced queue per city (a busy city never delays another), the page token
// per city, and one request for the same path however many callers wait for it.

const (
	restBase  = "https://rest.citybus.gr/api/v1"
	userAgent = "staseis.gr atrt-citybus (github.com/angelospk/athens-transit-rt)"
	// tokenTTL: the page token is about 48 h valid; it is read again after 6 h, or on a 401.
	tokenTTL = 6 * time.Hour
)

// Priority orders the queue of a city: 0 a user is waiting, 1 a watched line, 2 background.
type Priority int

const (
	priUser Priority = iota
	priWatched
	priBackground
)

type ticket struct {
	pri   Priority
	seq   uint64
	ready chan struct{}
	gone  atomic.Bool // the caller stopped waiting: the slot goes to the next one
}

type tickets []*ticket

func (t tickets) Len() int { return len(t) }
func (t tickets) Less(i, j int) bool {
	return t[i].pri < t[j].pri || (t[i].pri == t[j].pri && t[i].seq < t[j].seq)
}
func (t tickets) Swap(i, j int) { t[i], t[j] = t[j], t[i] }
func (t *tickets) Push(x any)   { *t = append(*t, x.(*ticket)) }
func (t *tickets) Pop() any {
	old := *t
	x := old[len(old)-1]
	*t = old[:len(old)-1]
	return x
}

// pacer gives one start slot per gap, highest priority first. The gap is between starts: a slow
// answer does not hold the queue.
type pacer struct {
	mu    sync.Mutex
	gap   time.Duration
	q     tickets
	seq   uint64
	last  time.Time
	wake  chan struct{}
	start sync.Once
}

func newPacer(gap time.Duration) *pacer { return &pacer{gap: gap, wake: make(chan struct{}, 1)} }

func (p *pacer) setGap(d time.Duration) { p.mu.Lock(); p.gap = d; p.mu.Unlock() }
func (p *pacer) gapNow() time.Duration  { p.mu.Lock(); defer p.mu.Unlock(); return p.gap }

func (p *pacer) queued() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.q)
}

// turn blocks until the caller may start one request.
func (p *pacer) turn(ctx context.Context, pri Priority) error {
	p.start.Do(func() { go p.run() })
	t := &ticket{pri: pri, ready: make(chan struct{})}
	p.mu.Lock()
	t.seq = p.seq
	p.seq++
	heap.Push(&p.q, t)
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
	select {
	case <-t.ready:
		return nil
	case <-ctx.Done():
		t.gone.Store(true)
		return ctx.Err()
	}
}

// run hands out the slots, forever (one goroutine per city; a pacer lives as long as its city).
func (p *pacer) run() {
	for {
		p.mu.Lock()
		n, wait := len(p.q), time.Until(p.last.Add(p.gap))
		p.mu.Unlock()
		if n == 0 {
			<-p.wake
			continue
		}
		if wait > 0 {
			// A new ticket may change nothing here: the next slot is fixed by the last start.
			time.Sleep(wait)
		}
		p.mu.Lock()
		var t *ticket
		for len(p.q) > 0 {
			if x := heap.Pop(&p.q).(*ticket); !x.gone.Load() {
				t = x
				break
			}
		}
		if t != nil {
			p.last = time.Now()
			close(t.ready)
		}
		p.mu.Unlock()
	}
}

// flight: one call per key at a time; later callers get the same answer.
type flight struct {
	mu sync.Mutex
	m  map[string]*call
}

type call struct {
	done chan struct{}
	body []byte
	err  error
}

// A waiter that gives up returns its ctx error; the call itself runs on with the first caller's ctx.
func (f *flight) do(ctx context.Context, key string, fn func() ([]byte, error)) ([]byte, error) {
	f.mu.Lock()
	if f.m == nil {
		f.m = map[string]*call{}
	}
	if c, ok := f.m[key]; ok {
		f.mu.Unlock()
		select {
		case <-c.done:
			return c.body, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	f.m[key] = c
	f.mu.Unlock()
	c.body, c.err = fn()
	f.mu.Lock()
	delete(f.m, key)
	f.mu.Unlock()
	close(c.done)
	return c.body, c.err
}

// Stats counts the upstream requests of one city.
type Stats struct {
	Requests int64            `json:"requests"`
	Errors   int64            `json:"errors"`
	ByStatus map[string]int64 `json:"byStatus"`
	// LastError is the latest failed request (network error or bad status), for diagnosis.
	LastError string `json:"last_error,omitempty"`
}

// fetcher is what a city needs from citybus.gr (a fake in tests).
type fetcher interface {
	// get returns the body of a REST path, nil on 404 (live: no bus due).
	get(ctx context.Context, path string, pri Priority) ([]byte, error)
	pacer() *pacer
}

// client is the citybus.gr access of one city.
type client struct {
	sub     string
	rest    string // REST base URL
	pageURL string // the stops page that holds the token
	http    *http.Client
	p       *pacer

	tokMu    sync.Mutex
	token    string
	tokUntil time.Time
	tokLoad  flight
	paths    flight

	stMu  sync.Mutex
	stats Stats
}

var errNotFound = errors.New("not found")

func newClient(sub string, hc *http.Client) *client {
	return &client{sub: sub, rest: restBase, pageURL: fmt.Sprintf("https://%s.citybus.gr/el/stops", url.PathEscape(sub)),
		http: hc, p: newPacer(time.Second), stats: Stats{ByStatus: map[string]int64{}}}
}

func (c *client) pacer() *pacer { return c.p }

func (c *client) statsNow() Stats {
	c.stMu.Lock()
	defer c.stMu.Unlock()
	s := Stats{Requests: c.stats.Requests, Errors: c.stats.Errors, ByStatus: map[string]int64{}, LastError: c.stats.LastError}
	for k, v := range c.stats.ByStatus {
		s.ByStatus[k] = v
	}
	return s
}

func (c *client) count(status int, err error, isErr bool) {
	c.stMu.Lock()
	defer c.stMu.Unlock()
	c.stats.Requests++
	if status > 0 {
		c.stats.ByStatus[fmt.Sprint(status)]++
	}
	if isErr {
		c.stats.Errors++
		msg := fmt.Sprintf("HTTP %d", status)
		if err != nil {
			msg = err.Error()
		}
		c.stats.LastError = time.Now().Format(time.RFC3339) + " " + msg
	}
}

var tokenRe = regexp.MustCompile(`token\s*=\s*'([^']+)'`)

// getToken returns the page token; one page read per city at a time.
func (c *client) getToken(ctx context.Context) (string, error) {
	c.tokMu.Lock()
	if c.token != "" && time.Now().Before(c.tokUntil) {
		t := c.token
		c.tokMu.Unlock()
		return t, nil
	}
	c.tokMu.Unlock()
	b, err := c.tokLoad.do(ctx, "token", func() ([]byte, error) {
		// Another caller may have read it while this one waited.
		c.tokMu.Lock()
		if c.token != "" && time.Now().Before(c.tokUntil) {
			t := c.token
			c.tokMu.Unlock()
			return []byte(t), nil
		}
		c.tokMu.Unlock()
		if err := c.p.turn(ctx, priUser); err != nil {
			return nil, err
		}
		body, status, err := c.do(ctx, c.pageURL, "")
		c.count(status, err, err != nil || status != http.StatusOK)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("stops page %d", status)
		}
		m := tokenRe.FindSubmatch(body)
		if m == nil {
			return nil, errors.New("token not found in page")
		}
		c.tokMu.Lock()
		c.token, c.tokUntil = string(m[1]), time.Now().Add(tokenTTL)
		c.tokMu.Unlock()
		return m[1], nil
	})
	return string(b), err
}

func (c *client) dropToken(t string) {
	c.tokMu.Lock()
	if c.token == t { // others may have renewed it already
		c.token = ""
	}
	c.tokMu.Unlock()
}

func (c *client) do(ctx context.Context, u, token string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 256<<20))
	return body, res.StatusCode, err
}

func (c *client) get(ctx context.Context, path string, pri Priority) ([]byte, error) {
	b, err := c.paths.do(ctx, path, func() ([]byte, error) { return c.fetch(ctx, path, pri, false) })
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	return b, err
}

func (c *client) fetch(ctx context.Context, path string, pri Priority, retried bool) ([]byte, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.p.turn(ctx, pri); err != nil {
		return nil, err
	}
	body, status, err := c.do(ctx, c.rest+path, token)
	bad := err != nil || (status != http.StatusOK && status != http.StatusNotFound && (status != http.StatusUnauthorized || retried))
	c.count(status, err, bad)
	switch {
	case err != nil:
		return nil, err
	case status == http.StatusUnauthorized && !retried:
		c.dropToken(token)
		return c.fetch(ctx, path, pri, true)
	case status == http.StatusNotFound:
		return nil, errNotFound
	case status != http.StatusOK:
		return nil, fmt.Errorf("HTTP %d %s", status, path)
	}
	return body, nil
}

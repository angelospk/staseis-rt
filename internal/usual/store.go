package usual

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The estimate (spec "Estimator"; thresholds from the study).
const (
	TrainDays  = 28            // a query learns from the days of its day type this far back
	MaxWaitS   = 3 * 3600      // a "next pass" further away is no pass
	MinDays    = 3             // a time needs this many days with a pass,
	MaxSpreadS = 180           // whose waits differ by at most this much (IQR),
	MinGaps    = 6             // and a headway needs this many gaps in the hour
	fileExt    = ".pass"       // day files: YYYYMMDD.pass
	magic      = "ATRTPASS1\n" // file format version
	dateLayout = "20060102"    // day file names
	maxHeader  = 8 << 20       // a larger header is a damaged file (real ones: ~0.2 MB)
	maxBlock   = 1 << 20       // so is a larger stop block (real ones: ≤ ~20 KB)
	hourS      = 3600          // seconds per hour
	minHeadway = 1             // minutes; a shorter median gap shows as 1
)

// Row is the usual service of one line and headsign at a stop now. Days is the number of
// training days with a pass within MaxWaitS. NextAt (and SpreadS, the IQR of those waits) is
// set only when the days agree; the headways are the median gap in this local hour and the
// next one, minutes.
type Row struct {
	Line, Headsign     string
	DayType            string
	Days               int
	HeadwayMin         *int
	NextHourHeadwayMin *int
	NextAt             *int64
	SpreadS            *int
}

// WriteDay writes d as dir/YYYYMMDD.pass (via a temporary file and a rename). It sorts
// d.Passes in place (by stop, key and time), so a day of passes is not held twice.
func WriteDay(dir string, d *Day) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	start, _ := dayBounds(d.Date)
	// Passes by stop, then key, then time.
	ps := d.Passes
	sort.Slice(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		if a.Stop != b.Stop {
			return d.Stops[a.Stop] < d.Stops[b.Stop]
		}
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		return a.T < b.T
	})
	var stops []string
	var blocks [][]byte
	for i := 0; i < len(ps); {
		j := i
		for j < len(ps) && ps[j].Stop == ps[i].Stop {
			j++
		}
		var blk []byte
		var series [][2]int // [from, to) in ps
		for k := i; k < j; {
			l := k
			for l < j && ps[l].Key == ps[k].Key {
				l++
			}
			series = append(series, [2]int{k, l})
			k = l
		}
		blk = binary.AppendUvarint(blk, uint64(len(series)))
		for _, se := range series {
			blk = binary.AppendUvarint(blk, uint64(ps[se[0]].Key))
			blk = binary.AppendUvarint(blk, uint64(se[1]-se[0]))
			prev := start
			for _, p := range ps[se[0]:se[1]] {
				blk = binary.AppendUvarint(blk, uint64(max(p.T-prev, 0)))
				prev = max(p.T, prev)
			}
		}
		stops = append(stops, d.Stops[ps[i].Stop])
		blocks = append(blocks, blk)
		i = j
	}
	var h []byte
	h = binary.AppendUvarint(h, uint64(start))
	h = binary.AppendUvarint(h, uint64(len(d.Outages)))
	for _, o := range d.Outages {
		h = binary.AppendUvarint(h, uint64(o[0]))
		h = binary.AppendUvarint(h, uint64(o[1]))
	}
	h = binary.AppendUvarint(h, uint64(len(d.Keys)))
	for _, k := range d.Keys {
		h = appendString(h, k.Line)
		h = appendString(h, k.Headsign)
	}
	h = binary.AppendUvarint(h, uint64(len(stops)))
	for i, s := range stops {
		h = appendString(h, s)
		h = binary.AppendUvarint(h, uint64(len(blocks[i])))
	}
	var buf bytes.Buffer
	buf.WriteString(magic)
	buf.Write(binary.AppendUvarint(nil, uint64(len(h))))
	buf.Write(h)
	for _, b := range blocks {
		buf.Write(b)
	}
	path := filepath.Join(dir, d.Date.Format(dateLayout)+fileExt)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func appendString(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// Store serves queries from the day files of a directory. Only each file's index is in memory.
type Store struct {
	dir string

	mu     sync.RWMutex
	days   map[time.Time]*dayFile // by service date
	intern map[string]string      // shares line, headsign and stop strings between days
}

type dayFile struct {
	date    time.Time
	f       *os.File
	start   int64
	outages [][2]int64
	keys    []Key
	stops   []string // sorted
	offs    []int64  // block offset of stops[i] in f; offs[len(stops)] = end
}

// OpenStore loads the index of every day file in dir (created if needed). Damaged files are
// skipped.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, days: map[time.Time]*dayFile{}, intern: map[string]string{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), fileExt)
		if !ok {
			continue
		}
		if d, err := time.Parse(dateLayout, name); err == nil {
			s.Load(d)
		}
	}
	return s, nil
}

// Has reports whether the day of date is loaded.
func (s *Store) Has(date time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.days[date]
	return ok
}

// Days is the number of days loaded.
func (s *Store) Days() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.days)
}

// TrainingDays is the number of loaded days a Query at now learns from: the days of its day
// type within TrainDays before its service date. 0 for a nil store.
func (s *Store) TrainingDays(now time.Time) int {
	if s == nil {
		return 0
	}
	sd := ServiceDate(now)
	typ := DayType(sd)
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for d := range s.days {
		if d.Before(sd) && !d.Before(sd.AddDate(0, 0, -TrainDays)) && DayType(d) == typ {
			n++
		}
	}
	return n
}

// Load (re)loads the day file of date.
func (s *Store) Load(date time.Time) error {
	f, err := os.Open(filepath.Join(s.dir, date.Format(dateLayout)+fileExt))
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.readIndex(f, date)
	if err != nil {
		f.Close()
		return fmt.Errorf("%s: %w", f.Name(), err)
	}
	if old := s.days[date]; old != nil {
		old.f.Close()
	}
	s.days[date] = d
	return nil
}

func (s *Store) str(v string) string {
	if w, ok := s.intern[v]; ok {
		return w
	}
	s.intern[v] = v
	return v
}

var errBad = errors.New("damaged day file")

func (s *Store) readIndex(f *os.File, date time.Time) (*dayFile, error) {
	br := bufio.NewReader(f)
	m := make([]byte, len(magic))
	if _, err := io.ReadFull(br, m); err != nil || string(m) != magic {
		return nil, errBad
	}
	hl, err := binary.ReadUvarint(br)
	if err != nil || hl > maxHeader {
		return nil, errBad
	}
	h := make([]byte, hl)
	if _, err := io.ReadFull(br, h); err != nil {
		return nil, errBad
	}
	r := &reader{b: h}
	d := &dayFile{date: date, f: f, start: int64(r.uv())}
	n := r.uv()
	for i := uint64(0); i < n && r.ok(); i++ {
		d.outages = append(d.outages, [2]int64{int64(r.uv()), int64(r.uv())})
	}
	n = r.uv()
	for i := uint64(0); i < n && r.ok(); i++ {
		d.keys = append(d.keys, Key{s.str(r.str()), s.str(r.str())})
	}
	n = r.uv()
	off := int64(len(magic) + len(binary.AppendUvarint(nil, hl)) + len(h))
	for i := uint64(0); i < n && r.ok(); i++ {
		d.stops = append(d.stops, s.str(r.str()))
		d.offs = append(d.offs, off)
		off += int64(r.uv())
	}
	d.offs = append(d.offs, off)
	if !r.ok() || !sort.StringsAreSorted(d.stops) {
		return nil, errBad
	}
	if st, err := f.Stat(); err != nil || st.Size() != off {
		return nil, errBad
	}
	return d, nil
}

// reader decodes a header; any error sticks.
type reader struct {
	b   []byte
	err bool
}

func (r *reader) ok() bool { return !r.err }

func (r *reader) uv() uint64 {
	if r.err {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err = true
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *reader) str() string {
	n := r.uv()
	if r.err || n > uint64(len(r.b)) {
		r.err = true
		return ""
	}
	v := string(r.b[:n])
	r.b = r.b[n:]
	return v
}

// Prune drops the days before date, from memory and from disk.
func (s *Store) Prune(date time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for d, df := range s.days {
		if d.Before(date) {
			df.f.Close()
			os.Remove(df.f.Name())
			delete(s.days, d)
		}
	}
}

func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for d, df := range s.days {
		df.f.Close()
		delete(s.days, d)
	}
}

// passes reads one stop's passes of a day, by key: empty for a stop with none that day, ok
// false when its block cannot be read.
func (d *dayFile) passes(stop string) (map[Key][]int64, bool) {
	out := map[Key][]int64{}
	i := sort.SearchStrings(d.stops, stop)
	if i == len(d.stops) || d.stops[i] != stop {
		return out, true
	}
	n := d.offs[i+1] - d.offs[i]
	if n <= 0 || n > maxBlock {
		return nil, false
	}
	b := make([]byte, n)
	if _, err := d.f.ReadAt(b, d.offs[i]); err != nil {
		return nil, false
	}
	r := &reader{b: b}
	ns := r.uv()
	for k := uint64(0); k < ns && r.ok(); k++ {
		ki, cnt := r.uv(), r.uv()
		if ki >= uint64(len(d.keys)) || cnt > uint64(len(r.b)) { // each time takes ≥ 1 byte
			return nil, false
		}
		ts := make([]int64, 0, cnt)
		t := d.start
		for j := uint64(0); j < cnt && r.ok(); j++ {
			t += int64(r.uv())
			ts = append(ts, t)
		}
		out[d.keys[ki]] = ts
	}
	if !r.ok() {
		return nil, false
	}
	return out, true
}

// span is one training day's passes at a stop, continued by the next day's file when there is
// one, so a lookup near DayStartH sees past it. After until nothing is known (no file):
// blind like an outage.
type span struct {
	date    time.Time
	ps      map[Key][]int64
	outages [][2]int64
	until   int64
}

// spanOf reads the span of day d at stop; ok false when d's block cannot be read. Call with
// s.mu held.
func (s *Store) spanOf(d *dayFile, stop string) (*span, bool) {
	ps, ok := d.passes(stop)
	if !ok {
		return nil, false
	}
	_, end := dayBounds(d.date)
	sp := &span{date: d.date, ps: ps, outages: d.outages, until: end}
	if next := s.days[d.date.AddDate(0, 0, 1)]; next != nil {
		if nps, ok := next.passes(stop); ok {
			for k, ts := range nps {
				sp.ps[k] = append(sp.ps[k], ts...)
			}
			sp.outages = append(append([][2]int64(nil), d.outages...), next.outages...)
			_, sp.until = dayBounds(next.date)
		}
	}
	return sp, true
}

// blind reports whether [from, to] is not fully seen: an outage, or past the known files.
func (sp *span) blind(from, to int64) bool {
	if to > sp.until {
		return true
	}
	for _, o := range sp.outages {
		if o[0] <= to && from < o[1] {
			return true
		}
	}
	return false
}

// keyStats gathers one key's observations over the training days.
type keyStats struct {
	usable     int     // days whose lookup was not hit by an outage
	waits      []int64 // seconds to the next pass, days with one
	gaps, next []int64 // gaps in this hour and the next one
}

// Query is the usual service at a stop at now, from the earlier days of the same day type
// (TrainDays back). Only lines with a headway or a next time are listed, by line and headsign.
func (s *Store) Query(stop string, now time.Time) []Row {
	sd := ServiceDate(now)
	typ := DayType(sd)
	qs := wallSecs(now)
	hour := qs / hourS
	s.mu.RLock()
	var days []*dayFile
	for d, df := range s.days {
		if d.Before(sd) && !d.Before(sd.AddDate(0, 0, -TrainDays)) && DayType(d) == typ {
			days = append(days, df)
		}
	}
	var spans []*span
	keys := map[Key]bool{}
	for _, df := range days {
		sp, ok := s.spanOf(df, stop)
		if !ok {
			continue // unreadable: not a day without buses
		}
		for k := range sp.ps {
			keys[k] = true
		}
		spans = append(spans, sp)
	}
	s.mu.RUnlock()
	stats := map[Key]*keyStats{}
	for k := range keys {
		stats[k] = &keyStats{}
	}
	for _, sp := range spans {
		q := wallClock(sp.date, qs).Unix()
		h0 := wallClock(sp.date, hour*hourS).Unix()
		h1 := wallClock(sp.date, (hour+1)*hourS).Unix()
		h2 := wallClock(sp.date, (hour+2)*hourS).Unix()
		for k, st := range stats {
			ts := sp.ps[k]
			i := sort.Search(len(ts), func(i int) bool { return ts[i] >= q })
			if i < len(ts) && ts[i]-q <= MaxWaitS {
				if !sp.blind(q, ts[i]) {
					st.usable++
					st.waits = append(st.waits, ts[i]-q)
				}
			} else if !sp.blind(q, q+MaxWaitS) {
				st.usable++
			}
			st.gaps = append(st.gaps, gaps(sp, ts, h0, h1)...)
			st.next = append(st.next, gaps(sp, ts, h1, h2)...)
		}
	}
	var out []Row
	for k, st := range stats {
		r := Row{Line: k.Line, Headsign: k.Headsign, DayType: typ, Days: len(st.waits),
			HeadwayMin: headway(st.gaps), NextHourHeadwayMin: headway(st.next)}
		if len(st.waits) >= MinDays && 2*len(st.waits) > st.usable {
			w := floats(st.waits)
			if spread := quantile(w, 0.75) - quantile(w, 0.25); spread <= MaxSpreadS {
				at := now.Unix() + int64(math.Round(quantile(w, 0.5)))
				sp := int(math.Round(spread))
				r.NextAt, r.SpreadS = &at, &sp
			}
		}
		if r.HeadwayMin != nil || r.NextAt != nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Headsign < out[j].Headsign
	})
	return out
}

// gaps are the gaps after each pass in [from, to) of one span, up to MaxWaitS, outages left out.
func gaps(sp *span, ts []int64, from, to int64) []int64 {
	var out []int64
	for i := sort.Search(len(ts), func(i int) bool { return ts[i] >= from }); i+1 < len(ts) && ts[i] < to; i++ {
		if g := ts[i+1] - ts[i]; g <= MaxWaitS && !sp.blind(ts[i], ts[i+1]) {
			out = append(out, g)
		}
	}
	return out
}

func headway(gaps []int64) *int {
	if len(gaps) < MinGaps {
		return nil
	}
	m := max(int(math.Round(quantile(floats(gaps), 0.5)/60)), minHeadway)
	return &m
}

func floats(v []int64) []float64 {
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = float64(x)
	}
	sort.Float64s(out)
	return out
}

// quantile of sorted values, linear between ranks (as numpy's default).
func quantile(v []float64, p float64) float64 {
	pos := p * float64(len(v)-1)
	lo := int(math.Floor(pos))
	if lo+1 >= len(v) {
		return v[len(v)-1]
	}
	return v[lo] + (pos-float64(lo))*(v[lo+1]-v[lo])
}

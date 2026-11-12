// Package model is the one transit model of docs/CONTRACT-rev8-draft.md (/v2): Line, Stop,
// Pattern, Vehicle, Arrival, Run and Source state, the same for every mode. Adapters fill it from
// their Source; the handlers only encode it. Terms: docs/GLOSSARY.md.
package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Modes.
const (
	Bus      = "bus"
	Trolley  = "trolley"
	Metro    = "metro"
	Tram     = "tram"
	Suburban = "suburban"
)

// ModeRank orders Lines on a Stop: by mode, then label/name.
func ModeRank(mode string) int {
	switch mode {
	case Bus:
		return 0
	case Trolley:
		return 1
	case Metro:
		return 2
	case Tram:
		return 3
	case Suburban:
		return 4
	}
	return 5
}

// Source states.
const (
	OK          = "ok"
	WarmingUp   = "warming_up"
	Stale       = "stale"
	Indicative  = "indicative"
	Down        = "down"
	SourceLive  = "live"      // Arrival source: GPS or a reported delay set the time
	SourceTable = "timetable" // Arrival source: the timetable only
)

// Arrival and Run statuses.
const (
	Scheduled = "scheduled"
	Running   = "running"
	Delayed   = "delayed"
)

// DelayedFrom is the delay from which a running Arrival or Run is "delayed" (every Source).
const DelayedFrom = 5 * 60

// StatusOf is "delayed" from DelayedFrom, else "running"; "scheduled" without a live signal.
func StatusOf(live bool, delay *int) string {
	switch {
	case !live:
		return Scheduled
	case delay != nil && *delay >= DelayedFrom:
		return Delayed
	}
	return Running
}

type Line struct {
	ID        string  `json:"id"`
	Mode      string  `json:"mode"`
	Label     *string `json:"label"`
	Name      string  `json:"name"`
	NameEn    *string `json:"name_en"`
	Color     string  `json:"color"`
	TextColor string  `json:"text_color"`
	Source    string  `json:"source"`
}

type Stop struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	NameEn *string  `json:"name_en"`
	Code   string   `json:"code,omitempty"` // rev 21: the code on the stop sign (GTFS stop_code); OASA only
	Lat    float64  `json:"lat"`
	Lon    float64  `json:"lon"`
	Modes  []string `json:"modes"`
	Lines  []string `json:"lines"`
	Source string   `json:"source"`
}

// IndexEntry is the compact row of /v2/stops: [id, name, lat, lon, modes, lines].
type IndexEntry struct {
	ID       string
	Name     string
	Lat, Lon float64
	Modes    []string
	Lines    []string
}

func (e IndexEntry) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{e.ID, e.Name, e.Lat, e.Lon, nonNil(e.Modes), nonNil(e.Lines)})
}

func (e *IndexEntry) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) != 6 {
		return fmt.Errorf("stop index entry: %d fields, want 6", len(raw))
	}
	for i, dst := range []any{&e.ID, &e.Name, &e.Lat, &e.Lon, &e.Modes, &e.Lines} {
		if err := json.Unmarshal(raw[i], dst); err != nil {
			return err
		}
	}
	return nil
}

// Entry is the Stop's index row.
func (s Stop) Entry() IndexEntry { return IndexEntry{s.ID, s.Name, s.Lat, s.Lon, s.Modes, s.Lines} }

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

type Pattern struct {
	ID        string       `json:"id"`
	Headsign  string       `json:"headsign"`
	Direction int          `json:"direction"`
	Stops     []string     `json:"stops"`
	Shape     [][2]float64 `json:"shape"`
}

type Position struct {
	Kind    string   `json:"kind"` // gps | estimate
	Lat     float64  `json:"lat"`
	Lon     float64  `json:"lon"`
	Bearing *float64 `json:"bearing"`
	At      int64    `json:"at"`
}

const (
	GPS      = "gps"
	Estimate = "estimate"
)

// Motion is how a client moves the marker between snapshots (contract rev 8 §4 Vehicle).
type Motion struct {
	Path       [][2]float64 `json:"path"`
	PathBeyond [][2]float64 `json:"path_beyond"`
	PathStops  []int        `json:"path_stops"`
	Speed      *float64     `json:"speed"`
	PathSpeeds []*float64   `json:"path_speeds"`
	Knots      [][2]int64   `json:"knots"` // [metres, unix time]; estimates only
}

type Vehicle struct {
	ID       string   `json:"id"`
	Line     string   `json:"line"`
	Run      *string  `json:"run"`
	Mode     string   `json:"mode"`
	Headsign *string  `json:"headsign"`
	Label    *string  `json:"label"`
	Position Position `json:"position"`
	DelayS   *int     `json:"delay_s"`
	NextStop *string  `json:"next_stop"`
	// AtTerminus: matched and waiting at the first stop of its Run (bus only; false otherwise).
	AtTerminus bool `json:"at_terminus"`
	// Direction: the direction (0/1) of the Pattern it runs, nil when unknown.
	Direction *int    `json:"direction"`
	Motion    *Motion `json:"motion"`
	// Quality (rev 23): the live level against the usual service; null where it is not measured.
	Quality *VehicleQuality `json:"quality"`
	Source  string          `json:"source"`
}

// VehicleQuality (rev 23) is a vehicle's live level on the scale of docs/QUALITY.md. Basis names
// the measure: "headway" (gap to the bus ahead, frequent directions) or "schedule" (delay_s,
// sparse ones); null when the level is unknown for want of a measure.
type VehicleQuality struct {
	Level         string  `json:"level"` // normal | worse | much_worse | unknown
	Basis         *string `json:"basis"`
	GapS          *int    `json:"gap_s"`
	UsualHeadwayS *int    `json:"usual_headway_s"`
	Stop          *string `json:"stop"`
	Bunched       bool    `json:"bunched"`
	Early         bool    `json:"early"`
}

// Segment (rev 23) is the live level of one stop-to-stop segment of a Pattern: the median speed
// of the buses that crossed it lately against its usual speed at this hour and day type.
type Segment struct {
	Pattern    string   `json:"pattern"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Level      string   `json:"level"`
	Ratio      *float64 `json:"ratio"`
	Speed      float64  `json:"speed"`       // m/s
	UsualSpeed *float64 `json:"usual_speed"` // m/s
	N          int      `json:"n"`
}

// LiteVehicle is a Vehicle without motion (vehicle tiles at zoom 9: the key is absent).
type LiteVehicle struct {
	ID         string   `json:"id"`
	Line       string   `json:"line"`
	Run        *string  `json:"run"`
	Mode       string   `json:"mode"`
	Headsign   *string  `json:"headsign"`
	Label      *string  `json:"label"`
	Position   Position `json:"position"`
	DelayS     *int     `json:"delay_s"`
	NextStop   *string  `json:"next_stop"`
	AtTerminus bool     `json:"at_terminus"`
	Direction  *int     `json:"direction"`
	Source     string   `json:"source"`
}

func (v Vehicle) Lite() LiteVehicle {
	return LiteVehicle{v.ID, v.Line, v.Run, v.Mode, v.Headsign, v.Label, v.Position, v.DelayS, v.NextStop, v.AtTerminus, v.Direction, v.Source}
}

// Usual (rev 19) is the usual service of a Line and headsign at a Stop, learnt from the passes
// of the past weeks (map#24): how often a bus usually passes in this local hour, and, only when
// the past weeks agree and today's timetable has a run near it, when the next one usually
// passes.
type Usual struct {
	Line               string `json:"line"`
	Headsign           string `json:"headsign"`
	DayType            string `json:"day_type"`
	Days               int    `json:"days"`
	HeadwayMin         *int   `json:"headway_min"`
	NextHourHeadwayMin *int   `json:"next_hour_headway_min"`
	NextAt             *int64 `json:"next_at"`
	SpreadS            *int   `json:"spread_s"`
}

type Arrival struct {
	Line         string  `json:"line"`
	Stop         string  `json:"stop"`
	Headsign     string  `json:"headsign"`
	At           int64   `json:"at"`
	ScheduledAt  *int64  `json:"scheduled_at"`
	DelayS       *int    `json:"delay_s"`
	Source       string  `json:"source"`
	Run          *string `json:"run"`
	Vehicle      *string `json:"vehicle"`
	VehicleLabel *string `json:"vehicle_label"`
	Platform     *string `json:"platform"`
	Status       string  `json:"status"`
}

type RunStop struct {
	Stop        string  `json:"stop"`
	ScheduledAt *int64  `json:"scheduled_at"`
	At          int64   `json:"at"`
	DelayS      *int    `json:"delay_s"`
	Source      string  `json:"source"`
	Platform    *string `json:"platform"`
	Passed      bool    `json:"passed"`
}

type Run struct {
	ID           string    `json:"id"`
	Line         string    `json:"line"`
	Headsign     string    `json:"headsign"`
	Pattern      *string   `json:"pattern"`
	Vehicle      *string   `json:"vehicle"`
	VehicleLabel *string   `json:"vehicle_label"`
	Status       string    `json:"status"`
	Stops        []RunStop `json:"stops"`
}

type SourceState struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	UpdatedAt *int64 `json:"updated_at"`
}

// Complete reports whether a response drawing on these Sources may be cached long: no Source
// is warming up or down (contract rev 8 §5, "incomplete responses"). An indicative Source is
// complete: an expired timetable standing in is a steady state, often for weeks, not a load
// that will finish in seconds.
func Complete(sources []SourceState) bool {
	for _, s := range sources {
		if s.State == WarmingUp || s.State == Down {
			return false
		}
	}
	return true
}

// Namespace is the part of an id before its first ':' ("oasa" of "oasa:040").
func Namespace(id string) string {
	ns, _, _ := strings.Cut(id, ":")
	return ns
}

// RunID is "<ns>:run:<native>:<YYYY-MM-DD>" for a run on the service day of day (Athens).
func RunID(ns, native string, day time.Time, loc *time.Location) string {
	return ns + ":run:" + native + ":" + day.Add(12*time.Hour).In(loc).Format(time.DateOnly)
}

// ParseRunID splits a run id of namespace ns into its native id and service date (noon of it,
// in loc). ok is false for any other shape.
func ParseRunID(ns, id string, loc *time.Location) (native string, noon time.Time, ok bool) {
	rest, found := strings.CutPrefix(id, ns+":run:")
	if !found {
		return "", time.Time{}, false
	}
	i := strings.LastIndexByte(rest, ':')
	if i <= 0 {
		return "", time.Time{}, false
	}
	d, err := time.ParseInLocation(time.DateOnly, rest[i+1:], loc)
	if err != nil || d.Format(time.DateOnly) != rest[i+1:] {
		return "", time.Time{}, false
	}
	// time.Date, not midnight + 12 h: on a DST change day that would be 11:00 or 13:00.
	return rest[:i], time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, loc), true
}

func Ptr[T any](v T) *T { return &v }

// CheckKnots reports how knots break the contract's rules (Vehicle, motion.knots), or "": the
// first knot is at metre 0, times strictly increase, metres never decrease.
func CheckKnots(knots [][2]int64) string {
	if len(knots) == 0 {
		return "no knots"
	}
	if knots[0][0] != 0 {
		return fmt.Sprintf("first knot at %d m", knots[0][0])
	}
	for i := 1; i < len(knots); i++ {
		if knots[i][1] <= knots[i-1][1] {
			return fmt.Sprintf("knot %d: time %d not after %d", i, knots[i][1], knots[i-1][1])
		}
		if knots[i][0] < knots[i-1][0] {
			return fmt.Sprintf("knot %d: %d m before %d m", i, knots[i][0], knots[i-1][0])
		}
	}
	return ""
}

// Package citybus serves the atrt API for the cities on citybus.gr (e-analysis), all in one
// process, one path prefix per city: /her/v2/lines ... It is the Go port of the
// citybus-bridge prototype (Bun/TS, mini PC; report: athens-transit-map
// docs/research/citybus-prototype.md).
//
// citybus.gr has no fleet call. Each city rotates over "probe" stop boards (stops/live/{code}),
// one paced queue per city, and moves the buses it saw on along their route shape between two
// sightings. No position is older than MaxAge.
package citybus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	_ "time/tzdata" // Europe/Athens without the system zoneinfo (distroless image)
)

// Num is a JSON number that citybus.gr sends as a number or as a numeric string. null and ""
// read as 0.
type Num float64

func (n *Num) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if string(b) == "null" {
		*n = 0
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			*n = 0
			return nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("citybus: number %q: %w", s, err)
		}
		*n = Num(f)
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*n = Num(f)
	return nil
}

// ---- raw citybus.gr answers (also the on-disk static.json, same field names as the bridge) ----

type RawRouteRef struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Direction int    `json:"direction"` // 1 or 2
}

type RawLine struct {
	Code      string        `json:"code"`
	Name      string        `json:"name"`
	Color     string        `json:"color"`
	TextColor string        `json:"textColor"`
	Routes    []RawRouteRef `json:"routes"`
}

type RawStop struct {
	Code       string   `json:"code"`
	Name       string   `json:"name"`
	Latitude   Num      `json:"latitude"`
	Longitude  Num      `json:"longitude"`
	LineCodes  []string `json:"lineCodes"`
	RouteCodes []string `json:"routeCodes"`
}

type RawSeq struct {
	Sequence int    `json:"sequence"`
	Code     string `json:"code"`
}

type RawPoint struct {
	Sequence  int `json:"sequence"`
	Latitude  Num `json:"latitude"`
	Longitude Num `json:"longitude"`
}

type RawPoints struct {
	RouteCode   string     `json:"routeCode"`
	RoutePoints []RawPoint `json:"routePoints"`
}

type RawTripRow struct {
	ID        int64  `json:"id"`
	Day       int    `json:"day"` // 0 = Sunday
	RouteCode string `json:"routeCode"`
	StopCode  string `json:"stopCode"`
	TripTime  string `json:"tripTime"` // "HH:MM"
}

// RawCity is everything the static model is built from.
type RawCity struct {
	Lines     []RawLine               `json:"lines"`
	Stops     []RawStop               `json:"stops"`
	Sequences map[string][]RawSeq     `json:"sequences"` // routeCode -> stops
	Points    map[string][]RawPoints  `json:"points"`    // lineCode -> shapes per route
	Trips     map[string][]RawTripRow `json:"trips"`     // routeCode -> rows
	BuiltAt   int64                   `json:"builtAt"`   // unix s
	// Version: the Athens day of the first build with this data (Hash), so a daily reload of the
	// same data keeps it. Files from before it was saved have neither (see version).
	Version string `json:"version,omitempty"`
	Hash    string `json:"hash,omitempty"`
}

// version is the city's gtfs_version: Version, or for an older file the day it was built.
func (r *RawCity) version() string {
	if r.Version != "" {
		return r.Version
	}
	if r.BuiltAt == 0 {
		return ""
	}
	return athensClock(r.BuiltAt).Date
}

// LiveRow is one bus on a stop board (stops/live/{code}).
type LiveRow struct {
	LineCode         string `json:"lineCode"`
	RouteCode        string `json:"routeCode"`
	Latitude         Num    `json:"latitude"`
	Longitude        Num    `json:"longitude"`
	DepartureMins    Num    `json:"departureMins"`
	DepartureSeconds Num    `json:"departureSeconds"`
	VehicleCode      string `json:"vehicleCode"`
}

// ---- the model ----

// LL is a [lat, lon] point.
type LL [2]float64

// NoTime marks a stop of a trip with no time.
const NoTime = -1

type Stop struct {
	Code     string
	Name     string
	Lat, Lon float64
	Lines    []string
}

type Trip struct {
	ID    string
	Day   int   // 0 = Sunday
	Times []int // seconds after midnight per stop index of the route; NoTime when none
}

type Route struct {
	Code  string
	Line  string
	Name  string // headsign, without a trailing dot
	Dir   int    // 0 or 1 (citybus direction 1 -> 0, 2 -> 1)
	Stops []string
	Shape []LL
	Cum   []float64 // metres along Shape at each point
	StopM []float64 // metres along Shape of each stop
	Trips []Trip
}

type Line struct {
	Code      string
	Name      string
	Color     string
	TextColor string
	Routes    []string
}

type Model struct {
	Lines      []*Line
	LineByCode map[string]*Line
	Stops      map[string]*Stop
	Routes     map[string]*Route
	BuiltAt    int64
	Version    string // gtfs_version (RawCity.version)
}

// ---- live ----

// Board is one stop board answer; At is the unix s of the answer.
type Board struct {
	Stop string
	At   int64
	Rows []LiveRow
}

// Sighting is the newest place a bus was seen at.
type Sighting struct {
	ID     string
	Route  string
	Lat    float64
	Lon    float64
	Seen   int64   // unix s
	M      float64 // metres along the route shape
	ToStop string  // the stop of the board
	ToM    float64 // its metre
	ETA    int64   // unix s due there
	Speed  *float64
}

// Placed is where a bus is at a given time.
type Placed struct {
	S       Sighting
	Route   *Route
	Lat     float64
	Lon     float64
	M       float64
	At      int64  // the sighting time for "gps", the asked time for "estimate"
	Kind    string // "gps" or "estimate"
	Bearing *float64
	NextIdx int
}

// Motion is the path ahead for the client's mover; nil fields are JSON null.
type Motion struct {
	Path       []LL
	PathBeyond []LL
	PathStops  []int
	Speed      *float64
}

// Clock is a time in Athens.
type Clock struct {
	Date     string // YYYY-MM-DD
	Day      int    // 0 = Sunday
	Sec      int64  // seconds after midnight
	Midnight int64  // unix s of that midnight
}

// Arrival is one bus due at a stop.
type Arrival struct {
	Line      string
	Route     string
	Headsign  string
	At        int64
	Scheduled *int64
	Live      bool
	Vehicle   string // "" when none
	Trip      string // "" when none
	Date      string // service date of a timetable run
	Lat, Lon  float64
	Seen      int64
}

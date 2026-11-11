// Package oseth is a client for the (undocumented) OSETH Telematics API and the merge of its
// sightings with OASTH's, for Thessaloniki.
//
// The API answers per route and shape (both are GTFS ids):
// https://oseth.com.gr/el/telematics-api/route/{route_id}/info?shapeId={shape_id}&language=el
package oseth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/angelospk/athens-transit-rt/internal/match"
	"github.com/angelospk/athens-transit-rt/internal/telematics"
)

const (
	// MaxRPS is the highest rate accepted for this host; the source is a public website.
	MaxRPS = 2
)

// Vehicle is one vehicle of a route info answer.
type Vehicle struct {
	ID                string
	Lat, Lon, Bearing float64
	ShapeID           string
}

// NormID is the comparison form of a vehicle id: no spaces, no leading zeros. OASTH and OSETH
// spell the same bus differently ("0104" and "104").
func NormID(s string) string {
	s = strings.TrimSpace(s)
	t := strings.TrimLeft(s, "0")
	if t == "" {
		if s == "" {
			return ""
		}
		return "0"
	}
	return t
}

// Client calls the API. The pacer keeps a budget for this host, apart from the OASA one.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Pacer   *telematics.Pacer

	requests atomic.Int64
	errors   atomic.Int64
	lastErr  atomic.Pointer[string]
}

// New builds a client of baseURL paced at rps requests per second.
func New(baseURL string, rps float64) *Client {
	if rps <= 0 {
		rps = 1
	}
	c := &Client{BaseURL: strings.TrimRight(baseURL, "/"), Pacer: telematics.NewPacer(min(rps, MaxRPS))}
	// Redirects are not followed: one request slot must stay one request.
	c.HTTP = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return c
}

// Requests counts calls made since the client was created.
func (c *Client) Requests() int64 { return c.requests.Load() }

// Errors counts failed calls (transport, HTTP status, error answers).
func (c *Client) Errors() int64 { return c.errors.Load() }

// LastError is the text of the newest counted failure ("" before the first one).
func (c *Client) LastError() string {
	if e := c.lastErr.Load(); e != nil {
		return *e
	}
	return ""
}

// flex decodes a JSON string or number into a string (the API is not consistent).
type flex string

func (f *flex) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*f = ""
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*f = flex(v)
		return nil
	}
	*f = flex(b)
	return nil
}

// num decodes a JSON number or a numeric string.
type num float64

func (n *num) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*n = num(v)
	return nil
}

type routeInfo struct {
	Data       json.RawMessage `json:"data"`
	Error      string          `json:"error"`
	StatusCode int             `json:"status_code"`
}

// RouteVehicles returns the vehicles OSETH reports on one route and shape.
func (c *Client) RouteVehicles(ctx context.Context, routeID, shapeID string) ([]Vehicle, error) {
	if err := c.Pacer.Wait(ctx); err != nil {
		return nil, err
	}
	c.requests.Add(1)
	start := time.Now()
	vs, err := c.routeVehicles(ctx, routeID, shapeID)
	c.Pacer.Report(time.Since(start), err)
	if err != nil {
		c.errors.Add(1)
		s := err.Error()
		c.lastErr.Store(&s)
	}
	return vs, err
}

func (c *Client) routeVehicles(ctx context.Context, routeID, shapeID string) ([]Vehicle, error) {
	q := url.Values{"shapeId": {shapeID}, "language": {"el"}}
	u := c.BaseURL + "/route/" + url.PathEscape(routeID) + "/info?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", telematics.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("route %s: %w", routeID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("route %s: HTTP %d", routeID, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("route %s: %w", routeID, err)
	}
	var ri routeInfo
	if err := json.Unmarshal(body, &ri); err != nil {
		return nil, fmt.Errorf("route %s: %w", routeID, err)
	}
	if ri.StatusCode != http.StatusOK {
		// TM100002: no trip for this route and shape now. A normal empty answer, not a failure (#123).
		if strings.Contains(ri.Error, `"TM100002"`) {
			return nil, nil
		}
		return nil, fmt.Errorf("route %s: status %d %s", routeID, ri.StatusCode, ri.Error)
	}
	data := bytes.TrimSpace(ri.Data)
	if len(data) == 0 || data[0] != '{' {
		return nil, fmt.Errorf("route %s: data is not an object", routeID)
	}
	var d struct {
		Vehicles []struct {
			ID        flex `json:"id"`
			Latitude  num  `json:"latitude"`
			Longitude num  `json:"longitude"`
			Bearing   num  `json:"bearing"`
		} `json:"vehicles"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("route %s: %w", routeID, err)
	}
	out := make([]Vehicle, 0, len(d.Vehicles))
	for _, v := range d.Vehicles {
		id := NormID(string(v.ID))
		if id == "" || !inGreece(float64(v.Latitude), float64(v.Longitude)) {
			continue // no id, or a fix without a position (0,0)
		}
		out = append(out, Vehicle{ID: id, Lat: float64(v.Latitude), Lon: float64(v.Longitude),
			Bearing: float64(v.Bearing), ShapeID: shapeID})
	}
	return out, nil
}

// inGreece is a loose bounding box; a vehicle without a fix reports 0,0.
func inGreece(lat, lon float64) bool { return lat >= 34 && lat <= 42 && lon >= 19 && lon <= 30 }

// Sighting is one OSETH position, timed by when the server fetched it: the API carries no
// timestamp of its own.
type Sighting struct {
	Vehicle
	At time.Time
}

// Merge folds OSETH sightings into an OASTH poll. cur and prev are keyed by NormID; prev is the
// previous fetch of the same line.
//
// A sighting replaces the OASTH fix of the same vehicle only when it is newer than that fix and
// has moved since both. OSETH-only vehicles are added only when they moved since prev and their
// shape maps to an OASTH route: a bus that stands still for a long time, or a vehicle seen
// once, is a ghost more often than a real one. The result is a new slice, OASTH order first.
func Merge(oasth []match.Obs, cur, prev map[string]Sighting, routeFor func(shapeID string) (routeCode, lineCode string, ok bool), minMove float64) []match.Obs {
	out := make([]match.Obs, 0, len(oasth)+len(cur))
	seen := make(map[string]bool, len(oasth))
	for _, o := range oasth {
		id := NormID(o.VehNo)
		seen[id] = true
		if s, ok := cur[id]; ok && s.At.After(o.Time) &&
			metres(s.Lat, s.Lon, o.Lat, o.Lon) >= minMove && movedFrom(prev, id, s, minMove) {
			v := o.Vehicle
			v.Lat, v.Lon, v.Heading, v.Time, v.TimeErr = s.Lat, s.Lon, s.Bearing, s.At, nil
			out = append(out, match.Obs{Vehicle: v, LineCode: o.LineCode})
			continue
		}
		out = append(out, o)
	}
	added := make([]string, 0, len(cur))
	for id, s := range cur {
		if seen[id] || !movedFrom(prev, id, s, minMove) {
			continue
		}
		if _, _, ok := routeFor(s.ShapeID); !ok {
			continue
		}
		added = append(added, id)
	}
	sort.Strings(added)
	for _, id := range added {
		s := cur[id]
		routeCode, lineCode, _ := routeFor(s.ShapeID)
		out = append(out, match.Obs{Vehicle: telematics.Vehicle{VehNo: s.ID, RouteCode: routeCode,
			Lat: s.Lat, Lon: s.Lon, Heading: s.Bearing, Time: s.At}, LineCode: lineCode})
	}
	return out
}

// movedFrom reports whether id was in prev and s is at least minMove metres away from it.
func movedFrom(prev map[string]Sighting, id string, s Sighting, minMove float64) bool {
	p, ok := prev[id]
	return ok && metres(s.Lat, s.Lon, p.Lat, p.Lon) >= minMove
}

// metres is the haversine distance between two fixes.
func metres(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371000.0
	rad := math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

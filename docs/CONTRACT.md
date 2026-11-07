# athens-transit contract v1

Shared interface between the backend (`athens-transit-rt`, Go, on a home machine)
and the frontend (`athens-transit-map`, Svelte, on GitHub Pages).
Source of truth: `athens-transit-rt/docs/CONTRACT.md`. The frontend keeps a copy.

## Ownership rules

- The backend owns this contract. v1 changes are **additive only** (new optional
  fields, new endpoints). A breaking change needs a `/v2` path.
- The frontend never edits the backend repo. It asks for a change with a GitHub
  issue on `angelospk/athens-transit-rt`, label `contract`.
- Each change to this file bumps the `Revision` line below and is copied to the
  frontend repo by whoever made it (backend thread opens the copy as a commit or
  issue on the frontend repo).

Revision: 21 (2026-11-07): an OASA Stop (and the `stop` of `/v1/stops/{stop_id}`) gets `code`, the code on the stop sign (GTFS
`stop_code`, e.g. `040054` for `oasa:40054`); missing when the source has none
(rev 20: vehicles of `/v1/lines/{line_id}`, `/v1/vehicles` and its `z` 13 tiles
get `along_m`, their metres along the static shape of their `variant`, and each variant of
`static/v1/lines/{line_id}.json` gets `length_m`, so a client can move a marker along the whole
shape instead of stopping at the end of `path_beyond`
([client rule](#along-the-static-shape-rev-20));
rev 19: `/v2/stops/{id}` and `/v1/stops/{stop_id}` get `usual`, the usual
service of each OASA line and headsign at the stop, learnt from the past weeks;
rev 18: `GET /v2/municipal`, the municipal bus routes and stops of OpenStreetMap;
rev 17: the daily statistics files (`/v1/stats/days/{date}`, the `stats`
branch) get `start_delay`, how late each line's runs of OASA's daily schedule left their first
stop ([API.md](API.md#start-delays-start_delay)); days without it are as before;
rev 16: `GET /v2/plan`, trips from a point to a point with walks and transfers
([§9](#9-trip-planner-v2plan-rev-16));
rev 15: OASA Patterns and bus Vehicles get both directions, 0 and 1 (the
GTFS has no `direction_id`, so all were 0); metro M2 `headsign` is where the train goes, not
where it starts; rev 14: on an expired GTFS, `delay_s` (and a stop board's `scheduled`) of a
bus seen leaving on a clear run of OASA's daily schedule is against that run; rev 13: `GET /v2/alerts`, service disruptions (OASA notices and strikes); rev 12: `direction` on `/v2` Vehicles; STASY Pattern directions follow branches (M3 airport);
rev 11: `/v2/stops/{id}` bus Arrivals include OASA's own predictions; rev 10: live responses send `Access-Control-Expose-Headers: Date`; rev 9: `at_terminus` on `/v1/lines/{line_id}` vehicles and `/v2` Vehicles; rev 8: `GET /v1/stops/{stop_id}` (a stop board); the unified `/v2` API
([below](#unified-api-v2-rev-8)); rev 7: `path_speeds` also follow today's traffic (same shape and units); rev 6: `path_speeds` on vehicles (typical speeds ahead); rev 5: `GET /v1/vehicles/tiles/{z}/{x}/{y}`; rev 4: `path_beyond` and `path_stops` on vehicles; rev 3: `GET /v1/vehicles`; `speed` and
`path` on vehicles)

## Hosts

| Host | Served by | Content |
|---|---|---|
| `https://transit.haroldpoi.dev` | Home machine → Cloudflare Tunnel → Cloudflare cache | Live data (small, changes every ~30 s) |
| `https://angelospk.github.io/staseis-rt/static/v1/` | GitHub Pages of the backend repo | Static map data (changes only when OASA publishes a new GTFS) |
| `https://raw.githubusercontent.com/angelospk/staseis-rt/stats/v1/` | `stats` branch of the backend repo, copied daily from `/v1/stats` | Daily network statistics ([API.md](API.md#daily-statistics)) |
| `https://bus.haroldpoi.dev` | GitHub Pages of the frontend repo | The map app |

All JSON is UTF-8. Line ids are the OASA telematics `LineID` in Greek uppercase
(`040`, `550`, `Α1`, `Χ96`), percent-encoded in URLs. The backend also accepts the
Latin look-alikes (`A1` → `Α1`). Times are Unix seconds (integers).
CORS: every live and static response has `Access-Control-Allow-Origin: *`. Live responses
also send `Access-Control-Expose-Headers: Date` (rev 10), so a browser `fetch()` can read the
server's `Date`. A copy from the browser cache keeps its old `Date`: use it as a lower bound on
server time.

## Live API (`transit.haroldpoi.dev`)

### `GET /v1/lines/{line_id}`

Vehicles of one line. One URL per line so all users share one cached copy.
The frontend combines lines in the browser. Max 5 lines per client (enforced in UI).

```json
{
  "line": "040",
  "updated_at": 1791100000,
  "next_update_at": 1791100030,
  "vehicles": [
    {
      "id": "52134",
      "lat": 37.9755, "lon": 23.7348,
      "bearing": 180,
      "position_at": 1791099988,
      "route_code": "2045",
      "variant": "2045",
      "trip_id": "12345-WINTER",
      "trip_label": "00:35 ΠΕΙΡΑΙΑΣ → ΣΥΝΤΑΓΜΑ",
      "delay_s": 140,
      "next_stop_id": "400012",
      "at_terminus": false,
      "speed": 6.4,
      "path": [[37.9755, 23.7348], [37.97601, 23.73502], [37.9781, 23.7356]],
      "path_beyond": [[37.9781, 23.7356], [37.98145, 23.73668], [37.98466, 23.73771]],
      "path_stops": [298, 682, 1050],
      "path_speeds": [6.2, 3.1, 4.8, 5.5, null, 7.0, 6.1, 5.9],
      "along_m": 4210
    }
  ]
}
```

- `trip_id`, `trip_label`, `delay_s`, `next_stop_id` are `null` when the vehicle is
  not matched to a scheduled trip. `bearing` may be `null`.
- `at_terminus` (rev 9): `true` when the vehicle is matched and waiting at the first stop of
  its trip (within 100 m of it, before the scheduled departure); its `delay_s` is then a
  departure delay, not a running delay. `false` otherwise. Older responses lack the key:
  treat a missing key as `false`.
- `delay_s` against an expired GTFS (rev 14): when the bus was seen leaving its first stop on a run of
  OASA's daily schedule that is clear (no run without a bus nearer to its departure), it is the
  delay against that run: the trip's times move to the run's departure. Otherwise it is against
  the matched GTFS trip. The same holds for stop boards (`delay_s`, `scheduled`) and `/v2`
  Vehicles and Runs. GTFS-RT keeps the delay against the GTFS trip.
- `variant` keys into `static/v1/lines/{line_id}.json` → `variants` (for shape + stops).
  It may be `null` if no static variant matches.
- `speed` (rev 3): smoothed speed along the route in m/s, one decimal, 0..20. It is the
  median slope over the vehicle's recent fixes (up to 5, within 300 s) as distance along
  its shape; moving less than 20 m over them gives `0` (standing). `null` when unknown:
  fewer than 2 fixes ≥ 20 s apart on the same shape (first sighting, new run, long gap
  between polls), position on the shape ambiguous (a loop or out-and-back passes there
  twice and history cannot tell which), more than 150 m from the shape, no shape.
- `path` (rev 3): the route ahead as `[lat, lon]` pairs (5 decimals), from the vehicle's
  projected point on its shape (at `position_at`) to its next stop when matched,
  otherwise `max(speed × 150 s, 300 m)` ahead; at most 1.5 km and never past the
  shape's end. Simplified (5 m), so a straight street has 2 points. `null` when there is
  nothing to move along: no shape, > 150 m from it, ambiguous position, or already at the
  next stop / the shape's end. Use: move the marker along `path` at `speed` from
  `position_at`, stop at the last point and wait for the next update. The static
  `shape` is not needed for this.
- `path_beyond` (rev 4): matched vehicles only, the route on from the end of `path` (from
  the vehicle's point when `path` is `null` because it is at its next stop) to its third
  stop ahead; at most 1.5 km from the vehicle and never past the shape's end. Same format
  as `path`; its first point is the last point of `path`. `null` when unmatched, ambiguous,
  waiting to start, past the last stop, or when `path` already reaches the 1.5 km limit.
- `path_stops` (rev 4): the stops on `path` followed by `path_beyond`, as whole metres along
  those points (joined, the shared point once), ascending; the next stop is the end of
  `path`. `null` when not matched or no stop is on them. Use (rev 4 clients): drive
  `path` then `path_beyond` from `position_at`, slowing down and waiting a little at each
  of `path_stops`.
- `path_speeds` (rev 6): matched vehicles only, the typical speed ahead in m/s (one decimal,
  at least 0.1) for consecutive 200 m steps from the vehicle's point along its shape: step `i`
  covers metres `200·i` to `200·(i+1)` ahead, the last one may be shorter (shape's end), at
  most 8 steps (1.6 km). It comes from the last 14 days of fixes of vehicles on the same shape,
  per 200 m of the shape, day type (weekday, Saturday, Sunday) and local hour, falling back to
  all hours; waits at stops and lights are included. Rev 7: where buses of any line crossed
  the same road in the same direction in the last 20 minutes (at least 2 buses, per 100 m of
  road), that road counts at 0.7 × their speed + 0.3 × the typical one (their speed alone
  without history). A step with neither is `null`. `null` when unmatched, waiting to start,
  or nothing ahead is known. Clients need no change. Use (rev 6 clients): move along `path` then `path_beyond` from `position_at` at
  the step speeds (a `null` step at `speed`, or 1.5 m/s without one), instead of the stop
  waits; stop at the end of the points, as before. Metres are along the shape; the 5 m
  simplified points are a few metres shorter, so a client may simply walk them.
- `along_m` (rev 20): whole metres along the shape of `variant` (its static `shape`) at
  `position_at`: where `path` starts. `null` when the vehicle is not placed on that shape: no
  `variant`, no shape, more than 150 m from it, the position is ambiguous (a loop or
  out-and-back passes there twice and history cannot tell which), or the vehicle is waiting at its
  first stop (`at_terminus`: it stands, so keep the rev 4 rule). It is set at the next stop and
  at the shape's end, where `path` is `null`. On a loop or out-and-back shape it says which pass
  the vehicle is on: do not project the GPS point onto the shape yourself. Use: see
  [Along the static shape](#along-the-static-shape-rev-20).
- Headers: `Cache-Control: public, max-age=<seconds until next_update_at, min 5, max 300>`.
  The frontend refetches a line at `next_update_at + 1..3 s` (random jitter), never faster
  than every 5 s, and at most every 60 s while the tab is hidden.
- Asking for a line marks it "watched" for ~10 min: the backend polls it at the top
  priority tier (~30 s), within a capped share of the OASA request budget.
- `404` `{"error":"unknown_line"}` for an unknown line. `503` `{"error":"warming_up"}`
  before the first poll of that line finished.
- `429` may come from Cloudflare rate limiting (per IP). The frontend backs off ≥ 10 s.

### `GET /v1/vehicles` (rev 3)

Every vehicle the backend knows now, from all lines, for a citywide map. Data comes from
the normal polling (watched lines ~30 s, busy ~60 s, others ~150 s); this endpoint does
**not** mark any line watched and causes no OASA request, so positions here can be up
to ~2.5 min staler than `/v1/lines/{id}` for lines nobody watches.

```json
{
  "updated_at": 1791100000,
  "next_update_at": 1791100030,
  "vehicles": [
    {
      "line": "040",
      "id": "52134",
      "lat": 37.9755, "lon": 23.7348,
      "bearing": 180,
      "position_at": 1791099988,
      "variant": "2045",
      "delay_s": 140,
      "speed": 6.4,
      "path": [[37.9755, 23.7348], [37.9781, 23.7356]],
      "path_beyond": [[37.9781, 23.7356], [37.98466, 23.73771]],
      "path_stops": [297, 1049],
      "path_speeds": [6.2, 3.1, 4.8, null],
      "along_m": 4210
    }
  ]
}
```

- Same field meanings as `/v1/lines/{id}` (`along_m` too); `line` is the line id. Coordinates are rounded
  to 5 decimals. No `route_code`, `trip_id`, `trip_label`, `next_stop_id`: fetch
  `/v1/lines/{line}` when the user selects a vehicle.
- Vehicles with a GPS fix older than 5 min are left out. A vehicle seen on two lines
  appears once (newest fix). Sorted by `line`, then `id`. `vehicles` may be `[]`.
- One snapshot for everyone, built at most every 10 s (`updated_at` = build time,
  `next_update_at` = earliest next build). Refetch at `next_update_at + 1..3 s`.
- Headers: `Cache-Control: public, max-age=<seconds until next_update_at, min 5, max 10>`
  (the 5 s floor can keep a copy up to 5 s past `next_update_at`). `ETag`;
  `If-None-Match` gives `304`. In a browser, let the HTTP cache revalidate (plain
  `fetch`); do not set `If-None-Match` from JS: it is not a CORS-safelisted header and
  there is no `OPTIONS` preflight support. gzip when asked (~50 KB for 1500 vehicles).

### `GET /v1/vehicles/tiles/{z}/{x}/{y}` (rev 5)

The `/v1/vehicles` snapshot cut into map tiles, so a client loads only what its map view shows.
Tiles are XYZ (Web Mercator, "slippy map") tiles, the scheme of OSM/MapLibre raster tiles. Each
tile is one fixed URL shared by all clients, so Cloudflare caches it like `/v1/vehicles`.
Example: `/v1/vehicles/tiles/13/4636/3160` (around Syntagma).

| `z` | tile size (Athens) | content | use at MapLibre zoom |
|---|---|---|---|
| `9` | ~62 km | vehicles without `path`, `path_beyond`, `path_stops`, `path_speeds`, `along_m` (the keys are absent) | < 13 |
| `13` | ~3.9 km | the same vehicle entries as `/v1/vehicles` | ≥ 13 |

```json
{
  "updated_at": 1791187860,
  "next_update_at": 1791187890,
  "vehicles": [
    {
      "line": "040", "id": "44537",
      "lat": 37.95614, "lon": 23.71609,
      "bearing": 52, "position_at": 1791187838,
      "variant": "5512", "delay_s": -35, "speed": 0
    }
  ]
}
```

- Body: the shape of `/v1/vehicles` (same fields, order and rules), with only the vehicles whose
  `lat`/`lon` is in that tile. A vehicle is in exactly one tile per zoom; its paths may run on
  into other tiles. `z` 13 entries are byte-identical to `/v1/vehicles`; `z` 9 entries have
  `line, id, lat, lon, bearing, position_at, variant, delay_s, speed` only.
- Same snapshot as `/v1/vehicles`: one build, at most every 10 s, gives `/v1/vehicles` and all
  tiles the same `updated_at` and `next_update_at`. No OASA request, no line marked watched.
- Tiles exist only for the box lat 37.5..38.5, lon 22.9..24.5: `z` 9 x 288..290, y 196..198;
  `z` 13 x 4617..4653, y 3145..3174. A tile in the box with no vehicle → `200` with
  `"vehicles":[]`. A vehicle is in a tile only when its `z` 13 tile is one of these (then its `z` 9
tile is too); one farther out is in no tile (none seen so far).
- Tile of a point (the usual slippy-map formula, `n = 2^z`):
  `x = floor((lon + 180) / 360 × n)`,
  `y = floor((1 − asinh(tan(lat × π/180)) / π) / 2 × n)`.
- `404` `{"error":"not_found"}` (`max-age=60`) for any other `z`, `x`/`y` outside the box, numbers
  not in plain decimal (`04636`, `+4636`), percent-encoded digits, or a trailing `/`. `400`
  `{"error":"query_not_allowed"}` (`no-store`) for any query string, even a bare `?`: each
  variant would be another Cloudflare cache entry. Never add cache busters.
- Headers as `/v1/vehicles`: `Cache-Control: public, max-age=<seconds until next_update_at,
  min 5, max 10>`, `ETag`, `If-None-Match` → `304`, gzip when asked, `Vary: Accept-Encoding`.
  Let the browser HTTP cache handle them (plain `fetch`); a tile asked again within its max-age
  then costs no request.

Client rules:

1. Tile set: take the map bounds, pad by 300 m (≈ 30 s of driving, so a vehicle that will drive
   into view is loaded), clamp to the box, choose `z` = 13 at MapLibre zoom ≥ 13 else 9, and take
   every tile from the north-west corner's tile to the south-east corner's tile.
2. On `moveend` (fires once the user stops panning or zooming): recompute the set, fetch tiles
   not loaded, drop vehicles of tiles that left the set. When `z` changes, replace the whole set.
3. Refresh all tiles of the set at `max(min(next_update_at) + 1..3 s, last refresh + 5 s)` (a
   cached copy can carry a `next_update_at` up to 5 s in the past), and at most every 60 s
   while the tab is hidden.
4. Merge all tiles by vehicle `id`: newer `position_at` wins, on a tie the response with the
   newer `updated_at`. Caches can mix two builds for a few seconds, so a vehicle that just
   crossed a tile edge can be in two tiles, or in none until the next refresh.
5. Fetch `/v1/lines/{line}` for `route_code`, `trip_id`, `trip_label`, `next_stop_id` when the
   user selects a vehicle, as with `/v1/vehicles`.
6. Cloudflare rate-limits per IP (`429`, back off ≥ 10 s). A desktop view at MapLibre 13 is ~15
   tiles; do not refetch tiles that are still fresh.

### `GET /v1/stops/{stop_id}` (rev 8)

The buses on their way to one stop and about when they get there (a stop board), from the
line polls the backend already makes: no OASA request of its own. The lines with a live bus
in `arrivals` are marked watched, as a line request does (polled about every 30 s for 10 min);
the others keep their usual rate. `stop_id` is the GTFS `stop_id`, as in
`static/v1/lines/{line_id}.json` → `stops`.

```json
{
  "stop": {"id": "61000", "name": "ΑΚΑΔΗΜΙΑ", "lat": 37.9799, "lon": 23.7333},
  "lines": ["022", "025", "026", "608"],
  "arrivals": [
    {
      "line": "608", "headsign": "ΝΕΚΡ. ΖΩΓΡΑΦΟΥ - ΑΚΑΔΗΜΙΑ - ΓΑΛΑΤΣΙ", "variant": "5346",
      "vehicle": "61191", "eta_s": 25, "at": 1793019999, "delay_s": -177,
      "position_at": 1793019859, "source": "live", "trip_id": "960_day_1_1625_1443",
      "scheduled": 1793020176, "stops_away": 1, "distance_m": 444
    },
    {
      "line": "026", "headsign": "ΒΟΤΑΝΙΚΟΣ", "variant": "5420",
      "vehicle": "", "eta_s": 526, "at": 1793020500, "delay_s": 0,
      "position_at": 0, "source": "schedule", "trip_id": "1032_day_1_2402_1540",
      "scheduled": 1793020500, "stops_away": null, "distance_m": null
    }
  ],
  "updated_at": 1793019974
}
```

- `lines`: every line with a trip that calls here, sorted (lines with no bus coming too).
  `updated_at`: when the answer was computed.
- `source: "live"`: a bus of those lines with a fix from the last 5 min whose route passes the
  stop ahead of it: its next pass (the first one on a loop), never a stop where its trip ends.
  A bus seen on two lines counts on the line of its newest fix only. Matched buses are placed
  by their trip; unmatched ones on the shape of their `variant` with that variant's stops, when
  the pass is clear (as for `speed`), else left out.
- `at`: `position_at` plus the time to drive the `distance_m` metres along the shape: per 200 m
  the typical speed of the last 14 days at that day type and hour (as `path_speeds`), where
  there is none the timetable's running time (matched) or 3.5 m/s (unmatched); waits at stops
  and lights are included. A matched bus waiting at its first stop leaves at its scheduled
  departure, or now when that is past. An unmatched bus standing at its first stop has no
  time and is left out. A bus not past the stop by its last fix is never overdue: late, it is
  due now (`at` = now). `eta_s` = `at` − now.
- `delay_s`: the matched bus's delay (positive = late); `0` when unmatched. `trip_id` and
  `scheduled` (the trip's timetable time at the stop) are `null` when unmatched.
  `stops_away`: `1` when the stop is the bus's next one.
- `source: "schedule"`: a trip of those lines that has not left its first stop yet, passes the
  stop within 60 min and has no matched bus; `at` = `scheduled`, `vehicle` `""`, `delay_s` and
  `position_at` `0`, `stops_away` and `distance_m` `null`. None while the static GTFS is expired
  (`gtfs_expires` in `/v1/status` is past): buses still match it, but its times may not be
  OASA's any more. A client that shows only buses filters on `source`.
- Up to 60 min ahead, sorted by `at`, at most 3 rows per `line` and `variant`. `arrivals` may
  be `[]`.
- `usual` (rev 19): the usual service of each line and headsign at the stop, as
  [`/v2` Usual](#usual-rev-19-v2stopsid) but with `line` as in `arrivals` (`"608"`). The guard
  of `next_at` uses today's timetable (OASA's daily schedule while the GTFS is expired) even
  when `arrivals` has no `schedule` rows. `[]` while the server has no history.
- Headers: `Cache-Control: public, max-age=15`. Refetch every 15-30 s while shown.
- `404` `{"error":"unknown_stop"}` (`max-age=60`) for an id not in the current GTFS. `503`
  `{"error":"warming_up"}` before the static GTFS is loaded.

### `GET /v1/status`

```json
{ "ok": true, "gtfs_version": "2026-10-06", "gtfs_expires": "2027-01-06",
  "updated_at": 1791100000, "lines_active": 312, "budget_rps": 4 }
```

`Cache-Control: public, max-age=10`. The frontend shows a banner if `ok` is false or
`gtfs_expires` is in the past.

### GTFS-Realtime feeds (whole network, for journey planners)

- `GET /v1/gtfs-rt/vehicle_positions.pb`
- `GET /v1/gtfs-rt/trip_updates.pb`
- `.json` variants of both, for debugging.

`Cache-Control: public, max-age=15`. The frontend does not use these.

## Static data (`.../athens-transit-rt/static/v1/`)

Published by GitHub Actions in the backend repo when OASA publishes a new GTFS.

### `lines.json`

```json
{
  "gtfs_version": "2026-10-06",
  "lines": [
    { "id": "040", "name": "ΠΕΙΡΑΙΑΣ - ΣΥΝΤΑΓΜΑ", "name_en": "PIRAEUS - SYNTAGMA",
      "color": "#1a73e8", "text_color": "#ffffff", "kind": "bus" }
  ]
}
```

`kind` is `bus` or `trolley`.

### `lines/{line_id}.json`

```json
{
  "id": "040",
  "variants": {
    "2045": {
      "headsign": "ΣΥΝΤΑΓΜΑ",
      "direction": 0,
      "shape": [[37.9420, 23.6466], [37.9431, 23.6480]],
      "stops": ["400012", "400013"]
    }
  },
  "stops": {
    "400012": { "name": "ΠΛ. ΚΑΡΑΪΣΚΑΚΗ", "lat": 37.9420, "lon": 23.6466 }
  }
}
```

`shape` is `[lat, lon]` pairs, rounded to 5 decimals (not simplified). `variants[*].stops` is ordered.
`length_m` (rev 20): the length of `shape` in the server's metres, the unit of a vehicle's
`along_m`, whole metres. Files built before rev 20 lack it.

#### Along the static shape (rev 20)

How a client moves a vehicle with `along_m` (rev 20). It replaces the rev 4/6 rule (drive `path`
then `path_beyond`) when the client has the vehicle's variant shape; else keep that rule.

1. Load `lines/{line}.json` for the lines of the vehicles in view only, once per
   `gtfs_version` of `lines.json` (one line is 2-3 KB gzipped; the files change only with the
   GTFS). A vehicle whose `variant` is not in the file: use the rev 4/6 rule.
2. Scale: the server measures metres on its own projection, a client on its own (haversine, an
   ellipsoid), so lengths differ by up to ~0.1 % (≈ 70 m on a 50 km shape). Use
   `s = along_m × L / length_m`, where `L` is the shape's length as the client measures it
   (factor 1 when `length_m` is missing or not above 0). Then a point is within ~15 m of the server's
   (measured on all 515 Athens shapes, 2026-11-07). Scale `path_stops` and the 200 m steps of
   `path_speeds` by the same factor.
3. Move from `s` at `position_at` along the shape at `path_speeds` (step `i` covers
   `s + 200·i` to `s + 200·(i+1)`; a `null` step and everything after the last step at `speed`,
   or 1.5 m/s without one), or without `path_speeds` at `speed` slowing down and waiting at
   `path_stops` (and the variant's later stops); stop at the shape's end. No 1.5 km limit.
4. A new fix with the same `variant`: glide from the shown point to the new `s`; never jump
   backwards for a small step back (GPS noise, a few metres). A new `variant` or trip (or
   `along_m` far behind): treat it as a new plan, as for a new `path`.

## Unified API `/v2` (rev 8)

Implemented 2026-10-29 (branch `feat/v2-unified`; live once deployed). One model, Line / Stop /
Vehicle / Arrival / Run, for every mode. `/v1` above is unchanged. Terms:
[GLOSSARY.md](GLOSSARY.md). Choices the draft left open and the places where the code differs
from the draft are in [DECISIONS-phase2.md](DECISIONS-phase2.md); the ones a client sees:

- **proastiakos is never `warming_up`** and `/v2/lines/{id}` never answers `line: null` for a
  suburban Line: corridors, stations and paths are embedded in the backend (decision 20).
- **proastiakos Stop ids** are the station ids of the embedded corridor table; the other
  railway.gov.gr ids of a station (Αεροδρόμιο `…cec7`, Άγιος Γεώργιος `…cfeb`) are permanent
  aliases (21).
- **Suburban Arrivals** are only runs of the three corridors (CHALKIDA, KIATO, PIRAIR), by the
  board's own corridor field (22). A board delay is `live` only for a run under way; a run not
  started is `timetable` (23). A train on the stream with no delay keeps `timetable` but names its
  `vehicle` (24). ose.mpass is not used by `/v2`: with no board and its last fetch failed, the Source is `down` (25).
- **Suburban stale**: board over 30 min old and its refresh failing, or the open stream silent
  for 2 min (26).
- **Suburban Vehicles** far (> 300 m) from their corridor keep their GPS position with
  `motion: null` (27). Direction 0 is the corridor file order (PIRAIR-0 is Αεροδρόμιο →
  Πειραιάς, CHALKIDA-0 Χαλκίδα → Αθήνα) (28). `/v2/runs/proastiakos:…` does not open the stream
  and answers only for today (29).
- **An unmatched bus on its way is still an Arrival** (`run`, `scheduled_at`, `delay_s` null),
  as `/v1/stops/{id}` lists it (2). A Run's last stop is not an Arrival (15).
- **`status: delayed`** from 5 minutes of delay in every mode (3). oasa Stop `name_en` and stasy
  Line `name_en` are `null`: the sources have no English names (5, 14). stasy Stop `name` and
  `name_en` come from a hand-checked table built from OpenStreetMap
  (`internal/railgeo/stasy_stops.json`, `scripts/stasy-stop-names.py`): metro "Σύνταγμα",
  not STASY's "ΣΥΝΤΑΓΜΑ"; tram names as STASY writes them. A Stop the table lacks (new or
  renamed in the feed) keeps the STASY name with `name_en: null` and is logged; a renamed Stop
  gets a permanent alias in the table (13, review #9 items 4 and 7). A point of the feed that
  is no station is merged into its Stop by an alias too: ΣΥΝΔΕΣΗ ΠΡΟΑΣΤΙΑΚΟΥ, where M3
  airport trains move onto the suburban tracks 91 m past Δουκίσσης Πλακεντίας, is not in
  `/v2/stops`; a Pattern or Run lists Πλακεντίας once, arriving at the station and leaving at
  the point's departure, and its Arrivals are Πλακεντίας Arrivals.
- `/v2/vehicles/rail` is metro and tram only, cached 15 s. Suburban trains are in
  `/v2/vehicles/suburban`, cached 5 s while trains are in it, else 15 s (review #9, item 11).

### 1. Goal and rules

- One model, **Line / Stop / Vehicle / Arrival**, for bus, trolley, metro (M1-M3), tram (T6, T7)
  and suburban (Proastiakos). The app and the web render it with one stop card, one line card and
  one vehicle marker for every mode.
- **Additive only.** Every rev 1-7 endpoint and field stays as it is (app v0.2.0 is out). The new
  model lives under a new path prefix, `/v2/` (Harold, 2026-10-29). `/v1` is not deprecated.
- **The backend owns the unification.** Each Source has one adapter. An adapter turns upstream
  data into the model below. A Source change (new URL, new format, a GTFS instead of a JSON
  board) changes only its adapter; responses keep their shape and their ids.
- **Missing data is labelled, never hidden.** Every optional field is present with `null` when
  not known (keys never disappear, except where noted for tile zoom 9). Clients render the same
  card and say "χωρίς ζωντανά δεδομένα" / "κατά το πρόγραμμα" from `source` and `position.kind`.

### 2. Sources and adapters

| Source id | Upstream | Modes | Adapter (today's code it wraps) | Live signal |
|---|---|---|---|---|
| `oasa` | OASA static GTFS + telematics API | bus, trolley | `internal/gtfs`, `internal/telematics`, `internal/match`, `internal/server/stops.go` | GPS every 30-150 s; delay from the matcher |
| `stasy` | STASY static GTFS (data.gov.gr CKAN) + OSM track geometry | metro, tram | `internal/rail/stasy*.go` | none: timetable only |
| `proastiakos` | railway.gov.gr stations, schedule boards, train-stream (SSE); ose.mpass is not used by `/v2` (25) | suburban | `internal/rail/proastiakos.go`, `trainstream.go` | GPS + delay while the stream is open |

New per adapter (phase 2): a stable id scheme (§3), Line names and colours, geometry, and an
alias table for ids that the upstream renames.

Geometry is in the backend (`internal/railgeo`, embedded). Until phases 3-4 the clients still bundle their copies: `athens-transit-map/public/metro.json`
(OSM tracks, built by `scripts/metro*.ts`) and `athens-transit-app/assets/suburban.json`
(corridor paths). The backend serves the same geometry as Pattern `shape`; clients stop bundling
it in phases 3-4.

### 3. Identity

All ids are opaque strings `<namespace>:<native>`, URL-safe after percent-encoding. Clients store
and compare them; they never parse them.

| Entity | `oasa` | `stasy` | `proastiakos` |
|---|---|---|---|
| Line | `oasa:040`, `oasa:Α1` (telematics LineID) | `stasy:M1`, `stasy:T6` (route_short_name) | `proastiakos:PIRAIR` (corridor) |
| Stop | `oasa:400012` (GTFS stop_id = OASA stop code) | `stasy:<folded-name>-<metro\|tram>`, e.g. `stasy:συνταγμα-metro` | `proastiakos:<primary station id>` |
| Vehicle | `oasa:52134` (vehicle no.) | `stasy:run:<trip_id>:<date>` (the Run itself) | `proastiakos:<train no.>` |
| Run | `oasa:run:<trip_id>:<date>` | `stasy:run:<trip_id>:<date>` | `proastiakos:run:<train no.>:<date>` |

- **Namespace = the numbering authority, not the fetch path.** If railway.gov.gr is replaced by
  a Hellenic Train GTFS, the adapter maps the new native ids to the old `proastiakos:` ids.
- **STASY stops are keyed by folded name + mode, not by `stop_id`.** STASY lists one `stop_id`
  per platform and may renumber them in a new feed; a name is what the passenger saves. All
  platforms with one folded name and one mode become one Stop.
- **Aliases.** Each adapter keeps a small table `old id → current id` (renamed stop, Αεροδρόμιο's
  second station id). A request with an alias id answers with the current object; its `id`
  field is the current id, so clients can rewrite stored Favourites. **Aliases are permanent**
  (never removed), and a published Line or Stop id is **never reused** for another Line or Stop,
  so a saved Alert always resolves or fails with `404`, never to the wrong place.
- Run ids (`oasa:run:<trip_id>:<date>`, `stasy:run:<trip_id>:<date>`,
  `proastiakos:run:<train no.>:<date>`; on an expired OASA GTFS a run of OASA's daily schedule is
  `oasa:run:<trip_id>@<HHMM>:<date>`, its departure and the GTFS trip that gives its stop times) and `stasy:run:*` Vehicle ids live only for their service
  day. Clients never store Vehicle or Run ids.
- `/v1` ids map 1:1: `/v1` line `040` = `oasa:040`; `/v1` stop `400012` = `oasa:400012`.

### 4. Objects

#### Line

```json
{
  "id": "proastiakos:PIRAIR",
  "mode": "suburban",
  "label": null,
  "name": "Πειραιάς – Αεροδρόμιο",
  "name_en": "Piraeus – Airport",
  "color": "#2e7d32",
  "text_color": "#ffffff",
  "source": "proastiakos"
}
```

- `mode`: `bus` | `trolley` | `metro` | `tram` | `suburban`. A new mode is an additive change;
  clients show an unknown mode with a generic icon.
- `label`: short public code or `null`. Badge rule: `label` present → coloured badge with the
  label; `null` → mode icon on `color` with `name` (truncated).
- `name`: always present. `name_en`: `null` when the Source has none.
- `color`, `text_color`: always present; the adapter supplies a default per mode when the Source
  has none.

#### Stop

```json
{
  "id": "stasy:συνταγμα-metro",
  "name": "Σύνταγμα",
  "name_en": "Syntagma",
  "lat": 37.97527, "lon": 23.73552,
  "modes": ["metro"],
  "lines": ["stasy:M2", "stasy:M3"],
  "source": "stasy"
}
```

- `lat`/`lon`: one point (mean of the platforms for merged STASY stops). 5 decimals.
- `modes`: the modes of `lines`, sorted; a bus stop can be `["bus","trolley"]`.
- `lines`: Line ids that call here, sorted by mode then label/name.
- `name_en`: `null` when unknown (OASA has it for most stops; STASY and railway.gov.gr do).
- `code` (rev 21): the code on the stop sign, GTFS `stop_code` (`oasa:40054` is `040054`, and a
  few terminals have short codes such as `18`). Only OASA Stops have it; the field is missing
  otherwise.

#### Vehicle

```json
{
  "id": "stasy:run:1012345:2026-10-29",
  "line": "stasy:M2",
  "run": "stasy:run:1012345:2026-10-29",
  "mode": "metro",
  "headsign": "Ελληνικό",
  "label": null,
  "position": {"kind": "estimate", "lat": 37.9722, "lon": 23.7325, "bearing": 205, "at": 1793205600},
  "delay_s": null,
  "next_stop": "stasy:ακροπολη-metro",
  "at_terminus": false,
  "direction": 0,
  "motion": {
    "path": [[37.9722, 23.7325], [37.96882, 23.72963]],
    "path_beyond": [[37.96882, 23.72963], [37.96421, 23.72651]],
    "path_stops": [450, 1010],
    "speed": null,
    "path_speeds": null,
    "knots": [[0, 1793205600], [450, 1793205660], [450, 1793205690], [1010, 1793205760]]
  },
  "source": "stasy"
}
```

- `label`: what the passenger reads on the vehicle: bus fleet no. (`52134`), suburban train no.
  (`1226`); `null` for metro/tram (no physical train is known).
- `position.kind`: `gps` | `estimate`. `position.at`: the GPS fix time, or for an estimate the
  build time of the snapshot. `bearing` may be `null`.
- `delay_s`: `null` when there is no live signal (always for metro/tram).
- `run`: Run id, `null` when the Vehicle is not matched to a Run.
- `next_stop`: Stop id, `null` when unmatched.
- `at_terminus` (rev 9): `true` when a bus or trolley is matched and waiting at the first Stop
  of its Run (the `/v1` meaning); always `false` for metro, tram and suburban. Present in tiles
  at zoom 9 too.
- `direction` (rev 12): the `direction` (0/1) of the Pattern the Vehicle runs, so a client can
  put it on one of the Line's two tracks without a `/v2/runs` call. Metro and tram: always set.
  Bus and trolley: the matched trip's direction, else the direction of the trip the bus was
  placed on, else `null`. Suburban: the direction of its Run when it has one, else of where it
  is placed on its corridor, else `null`.
  Present in tiles at zoom 9 too. Older responses lack the key: treat a missing key as `null`.
- `motion`: how the client moves the marker between snapshots. `null` when there is nothing to
  move along (standing with no Run, no geometry).
  - `path`, `path_beyond`, `path_stops`, `speed`, `path_speeds`: the rev 3-7 meanings, unchanged
    for buses (1.5 km caps included); for suburban they run along the rail geometry of §2.
    `/v2` Vehicles have no `along_m` (rev 20): they name no shape (no `variant`) to measure it on.
  - For **estimates** (metro/tram) the 1.5 km caps do not apply: `path` runs to the next Stop
    and `path_beyond` to the third Stop ahead, whatever the distance.
  - `knots` (new, estimates only; `null` for GPS vehicles): `[metres along path + path_beyond,
    unix time]` pairs. The first knot is `[0, position.at]`. Each Stop ahead gives two knots,
    its arrival and its departure, with the same metres (one knot when the two times are equal).
    Metres never decrease; times strictly increase. The last knot is at the end of
    `path_beyond` (or of `path`).
  - **Client rule, one for all modes:** if `knots` is present, place the marker by time: before
    the first knot at metre 0, between two knots linearly in metres (equal metres = dwell at the
    Stop), after the last knot at its metres until the next snapshot. Otherwise use the rev 6
    speed rule (`path_speeds`, then `speed`, then 1.5 m/s) and stop at the end of the points.
    This is the "trains glide along the track" behaviour. Test cases for a client's mover:
    [`fixtures/v2/motion-cases.json`](fixtures/v2/motion-cases.json) (time → metres, checked
    against a reference in `internal/model/motion_test.go`); the backend checks these rules on
    every train of its test feed through a service day.
- `source`: the Source id.

#### Arrival

```json
{
  "line": "proastiakos:PIRAIR",
  "stop": "proastiakos:6928c3c150ab5bc72b39cee5",
  "headsign": "Αεροδρόμιο",
  "at": 1793205900,
  "scheduled_at": 1793205780,
  "delay_s": 120,
  "source": "live",
  "run": "proastiakos:run:1226:2026-10-29",
  "vehicle": "proastiakos:1226",
  "vehicle_label": "1226",
  "platform": "2",
  "status": "running"
}
```

- `at`: the expected time (leaving time for rail; see the Glossary). Always present.
- `scheduled_at`: the timetable time; `null` when the Source has no timetable for it: an OASA bus
  on its way but matched to no trip is listed with `scheduled_at`, `run` and `delay_s` `null`, as
  `/v1/stops/{id}` lists it (2).
- `delay_s`: the **observed** delay of the Vehicle (or the Source's reported delay for the
  Run), else `null`. Normally `at = scheduled_at + delay_s`, but `at` is clamped to now (a bus
  already due), so clients must not compute one from the other.
- `source`: `live` | `timetable`. `live` only when GPS or a reported delay set `at`. Suburban
  precedence: stream delay (stream open) → board delay (board ≤ 10 min old) → timetable; the
  first two give `live`.
- `run`: Run id, so the card can open the Run even before it starts; `null` for an OASA bus
  matched to no trip and for an OASA prediction (below).
- OASA predictions (rev 11): an OASA stop also lists OASA's own predictions (`getStopArrivals`)
  for the buses the backend has no Arrival of, as `source: live`, `at` = OASA's minutes from its
  last answer (whole minutes, at most 2 min old, refreshed in the background every 30 s while
  the stop is asked for), `vehicle` and `vehicle_label` set; `scheduled_at`, `delay_s` and `run`
  `null`. A bus both list appears once, with the backend's time. Such a bus may also be the one
  that will run a `timetable` Arrival of the same line: OASA does not say which trip it runs.
- `vehicle`, `vehicle_label`: `null` when no Vehicle is on the Run yet or none is known. Metro/tram:
  `vehicle` is set only while the Run is under way; `vehicle_label` is always `null`.
- `platform`: `null` unless the Source gives one (only railway.gov.gr today).
- `status`: `scheduled` | `running` | `delayed`. Cancelled Runs are left out, as today.

#### Usual (rev 19, `/v2/stops/{id}`)

(`/v1/stops/{stop_id}` has the same rows, `line` without `oasa:`.)

The usual service of an OASA Line and headsign at the Stop, learnt from the bus passes of the
past weeks ([athens-transit-map#24](https://github.com/angelospk/athens-transit-map/issues/24),
study: `docs/superpowers/specs/2026-11-03-usual-headway.md`). It is for a line with no `live`
Arrival: it says how often a bus usually comes, never that one is coming.

```json
"usual": [
  {"line": "oasa:309Β", "headsign": "ΑΥΛΑΚΙ", "day_type": "weekday", "days": 9,
   "headway_min": 15, "next_hour_headway_min": 25,
   "next_at": 1793742000, "spread_s": 120}
]
```

- One row per Line and headsign that has a `headway_min` or a `next_at`, by line then headsign;
  also for a line with a `live` Arrival. `[]` for metro, tram and suburban Stops, and while the
  server has no history yet. A row may name a headsign no Arrival has (a run that is not on
  the board now).
- Learnt from the days of the same `day_type` (`weekday` | `saturday` | `sunday`; Greek public
  holidays count as `sunday`) of the last 28 days, at the same local time of day. Service days
  run 04:00-04:00. Minutes when the server saw no bus at all (an outage) are left out.
- `headway_min`: median gap between buses in this local hour (whole minutes, ≥ 1), from at
  least 6 gaps; `next_hour_headway_min`: the same for the next hour. `null` when unknown.
- `next_at`: the usual time of the next pass, now + the median wait of the past days. Set only
  when at least 3 days had a bus within 3 h, more than half of the days that could be read did,
  their waits differ by at most 3 min (`spread_s`, the IQR, seconds), and an Arrival of the same
  line and headsign (today's timetable, or a bus) is within 15 min of it. Else `null`, and
  `spread_s` too. It is an estimate: show it as one, and not when the line has a `live` Arrival.
- `days`: the past days with a bus within 3 h of now.

#### Source state (in every list response)

```json
"sources": [
  {"id": "oasa", "state": "ok", "updated_at": 1793205590},
  {"id": "stasy", "state": "indicative", "updated_at": 1793205000},
  {"id": "proastiakos", "state": "warming_up", "updated_at": null}
]
```

`state`: `ok` | `warming_up` | `stale` (last good data served) | `indicative` (expired
timetable, the same weekday stands in) | `down` (no data at all). Freshness is per Source,
because their signals differ:

| Source | `stale` when | `indicative` when | `down` when |
|---|---|---|---|
| `oasa` | no successful line poll for 5 min (as `/v1/status` `ok:false`) | static GTFS expired and no OASA daily schedule of today cached for the response's lines (any line for the lists) | no data since start |
| `stasy` | not reported yet: the timetable store does not expose its last check (19) | timetable expired (today's `indicative:true`) | no timetable loaded |
| `proastiakos` | board older than 30 min and its last refresh failed, or the stream open and silent for 2 min (a stream that never sent an event is not stale) (26) | never | no board, none cached, and the last board fetch failed (an idle start with no board yet is `ok`) (25) |

`updated_at`: the last good data of that Source (GPS fix, stream event, board fetch, timetable
load). Only the
Sources that the response draws on are listed. Clients show a one-line note per non-`ok` Source;
they never drop the card.

### 5. Endpoints (`/v2`)

All: UTF-8 JSON, `Access-Control-Allow-Origin: *`, gzip, times in Unix seconds, `ETag` +
`If-None-Match` → `304` where cached ≥ 60 s. **No query strings** on the cached map endpoints
(tiles), as in rev 5. Errors keep the rev 7 shape `{"error": "<code>"}`.

| Endpoint | Body | Cache (`max-age`) | Side effect upstream |
|---|---|---|---|
| `GET /v2/lines` | `{lines: [Line], sources}` all modes | 3600 | none |
| `GET /v2/lines/{id}` | `{line: Line, patterns: [Pattern], stops: {id: Stop}, sources}` | 3600 | none |
| `GET /v2/lines/{id}/vehicles` | `{line, updated_at, next_update_at, vehicles: [Vehicle], sources}` | until `next_update_at`, 5-300 | `oasa`: marks the line watched (as `/v1/lines/{id}`); `proastiakos`: opens the stream |
| `GET /v2/stops` | `{stops: [StopIndexEntry], sources}` every Stop of every mode | 3600 | none |
| `GET /v2/stops/{id}` | `{stop: Stop, lines: [Line], arrivals: [Arrival], usual: [Usual] (rev 19), updated_at, sources}` | 15 | `oasa`: raises its lines' priority; `proastiakos`: opens the stream |
| `GET /v2/vehicles` | `{updated_at, next_update_at, vehicles: [Vehicle], sources}` bus + trolley | until `next_update_at`, 5-30 | none |
| `GET /v2/vehicles/tiles/{z}/{x}/{y}` | as `/v2/vehicles`, tile-cut as rev 5 | as above | none |
| `GET /v2/vehicles/rail` | as `/v2/vehicles`, metro + tram (~60 vehicles, one body) | until `next_update_at`, 5-30 | none |
| `GET /v2/vehicles/suburban` | as `/v2/vehicles`, suburban trains (~10, one body) | until `next_update_at`, 5-30 | `proastiakos`: opens the stream |
| `GET /v2/runs/{id}` | `{run: Run, sources}` | 15 | none |
| `GET /v2/alerts` (rev 13) | `{alerts: [Alert], updated_at}` | 300 | none (polled in the background) |
| `GET /v2/plan?from=&to=&after=&last=&n=` (rev 16) | `{service_day, …, itineraries: [Itinerary], walk_only, lines, stops, sources}` ([§9](#9-trip-planner-v2plan-rev-16)) | 60 | none (reads only what is already loaded) |
| `GET /v2/municipal` (rev 18) | `{routes: [MunicipalRoute], stops: {id: MunicipalStop}, updated_at, attribution}` | 86400 | none (OpenStreetMap, polled weekly) |

Shapes not shown above:

```json
// Pattern
{"id": "oasa:2045", "headsign": "Σύνταγμα", "direction": 0,
 "stops": ["oasa:400012", "oasa:400013"], "shape": [[37.9420, 23.6466], [37.9431, 23.6480]]}

// StopIndexEntry (compact: ~9000 stops, target < 250 KB gzip)
["oasa:400012", "Πλ. Καραϊσκάκη", 37.94200, 23.64660, ["bus"], ["oasa:040", "oasa:049"]]

// Run (also before it starts: vehicle null, every stop passed:false)
{"id": "stasy:run:1012345:2026-10-29", "line": "stasy:M2", "headsign": "Ελληνικό",
 "pattern": "stasy:M2-0", "vehicle": "stasy:run:1012345:2026-10-29", "vehicle_label": null,
 "status": "running",
 "stops": [{"stop": "stasy:ακροπολη-metro", "scheduled_at": 1793205660, "at": 1793205660,
            "delay_s": null, "source": "timetable", "platform": null, "passed": false}]}
```

```json
// Alert (rev 13): a service disruption
{"id": "oasa:14909", "source": "oasa",
 "title": "Μερική προσωρινή τροποποίηση της λεωφορειακής γραμμής 112 στον Δήμο Αγ. Δημητρίου",
 "url": "https://www.oasa.gr/blog/...", "published": 1793336520,
 "from": "2026-10-31", "until": "2026-11-01",
 "modes": ["bus"], "lines": ["oasa:112"], "stops": ["oasa:40054"], "urgent": false}
```

```json
// MunicipalRoute (rev 18): one route relation of OpenStreetMap, one direction or a loop
{"id": "osm:r20462642", "ref": "Β1", "name": "Δημοτικό Λεωφορείο Β1: Πολύδροσο → Κοιμητήριο",
 "municipality": "Δήμος Χαλανδρίου",
 "timetable_url": "https://www.chalandri.gr/yphresies/ypodomes-erga/dimotiki-sygkoinonia/",
 "shape": [[[38.02, 23.80], [38.03, 23.81]]], "stops": ["osm:n201", "osm:n203"]}
// MunicipalStop
{"name": "Πολύδροσο", "lat": 38.02, "lon": 23.80}
```

Rules:

- `/v2/municipal` (rev 18): the bus routes that municipalities run (δημοτική συγκοινωνία), from
  OpenStreetMap through the Overpass API: at start, then every 7 days (every hour after a
  failure). No OASA request. About 37 routes of 12 municipalities, ~50 KB gzip. **No timetable,
  no live data**: clients show the route, its stops and a link to `timetable_url`.
  - A route is an OSM bus or minibus route relation in Attica whose `operator` starts with
    «Δήμος» or whose `network` says «Δημοτική». `shape`: its ways as lines of `[lat, lon]`
    (5 decimals); a way that continues the last line is joined to it, so a route has one line or
    a few. A route with no way is left out.
  - `ref`: the line's short name, `null` when OSM has none. `municipality`: the operator with
    its spaces normalised («Δήμος Παπάγου-Χολαργού»). `timetable_url`: that municipality's page
    (timetable, PDF or notices), `null` for a municipality the backend does not know.
  - `stops`: Stop ids (`osm:n<node id>`) in route order (a stop visited twice, such as the
    start and end of a loop, is listed twice), the members with a `stop` role, or the
    platforms when the route has none; `[]` when OSM has neither (10 routes today). `stops`
    (the map) holds every listed Stop; `name` is `null` when OSM has none.
  - Order: by `municipality`, then `ref`, then `name`. A failed poll keeps the last data.
    Before the first good poll: `routes: []`, `stops: {}`, `updated_at: null`, `max-age=5`,
    no `ETag`; clients then show nothing. After it: `max-age=86400` with an `ETag`.
  - Licence: ODbL. Clients show `attribution` («© OpenStreetMap contributors (ODbL)») wherever
    they show the layer.
- `/v2/alerts` (rev 13): service disruptions from two sources, polled every 10 min.
  - `source: "oasa"`: the OASA news feed (`oasa.gr/feed`), every item except tenders
    (category «Διαγωνισμοί»). Buses and trolleys only. `lines`: the line codes that follow
    «γραμμή/γραμμών» in the title, as `/v2` Line ids, only those the GTFS knows. `stops`: the
    «ΚΣ <code>» stop codes in the text (the code on the stop sign, GTFS `stop_code`; «ΚΣ 040054»
    is Stop `oasa:40054`), as Stop ids. A code the GTFS does not know (e.g. a temporary stop) is
    left out; a code that two stops share gives both. `from`/`until`: the
    earliest and latest date (Athens, `YYYY-MM-DD`) in the opening paragraph of the notice
    (up to «ως εξής»); `null` when it names none. `modes`: `bus`, plus `trolley` when one of
    the lines is a trolley line; both when `lines` is empty (a network-wide notice).
    `urgent: false`. Each notice page is read once. `url`: the notice page; when oasa.gr
    serves another notice there (its permalink redirects to a similar one), the text comes from
    the WordPress API and `url` is the news list `https://www.oasa.gr/blog/`.
  - `source: "athenstransport"`: strikes only (category «Απεργία») from the athenstransport.com
    news site, which also covers metro, tram and trains. We show its title and link, never its
    text. `modes` from its categories; none of them means every mode. `lines`, `stops` `[]`,
    `from`/`until` `null`, `urgent: true`.
  - An Alert is listed for 7 days after `published`, or until `until` when that is later.
    Newest `published` first. A source that fails keeps its last Alerts. `updated_at`: the
    last poll with no failure; `null` (and `alerts: []`, `max-age=5`) before the first one.
  - Clients show an Alert from `from` to `until`; with no dates, for 3 days after `published`.
    Always link `url`: the title alone does not say what changes.
- `/v2/stops/{id}` arrivals: soonest first, at most 2 h ahead, at most 4 per Line + headsign, at
  most 30 in all. Bus arrivals: those of rev 7 (`/v1/stops/{id}`, `live`), plus the timetable
  Arrivals of Runs no matched bus has started yet (`timetable`, `vehicle: null`; Harold,
  2026-10-29). Metro/tram: the STASY
  timetable. Suburban: the board, with the stream's delay when it is open.
- `/v2/vehicles` and tiles carry bus and trolley only, like rev 3/5: map traffic must not keep
  the railway.gov.gr stream open. Metro and tram vehicles come from `/v2/vehicles/rail`, which
  opens nothing upstream. Suburban vehicles come from `/v2/vehicles/suburban`, which a client
  loads only while its suburban layer is on and a corridor is in view; that request is the
  explicit suburban demand. Clients merge all three into one vehicles layer (one marker
  component). Tile zoom 9 entries drop `motion`
  (key absent), as rev 5 drops the path keys.
- Metro/tram vehicles are placed when `/v2/vehicles/rail` is built (per request, cached 15 s);
  their `knots` keep them moving.
- Pattern `direction` (rev 12): every Line has two directions, 0 and 1, never more. Suburban:
  the corridor direction. OASA (rev 15): OASA's GTFS has no `direction_id`, so the direction
  comes from where each Pattern runs: a Pattern whose first and last Stops lie nearer the ends of
  the busiest Pattern than its swapped ends runs its way (a short turn included); 0 is the way
  towards the place after the last " - " of the Line name, the way `/v1/lines/{id}/schedule`
  calls 0; a loop Line is 0. When neither the name nor the headsigns tell the places apart (no
  " - " in the name, or headsigns that name both or neither), the busiest Pattern's way is 0 and
  may then differ from `/v1`. Metro/tram: the most common Pattern is 0; each other
  Pattern is 0 or 1 by the order of two Stops it shares with a Pattern already decided, so a
  branch that shares one Stop with the main Pattern (M3 airport) still gets the right one. M3:
  every Pattern towards ΕΛ. ΒΕΝΙΖΕΛΟΣ or Δ. ΠΛΑΚΕΝΤΙΑΣ is 1, every one towards ΔΗΜΟΤΙΚΟ ΘΕΑΤΡΟ
  is 0 (before rev 12 the airport Patterns could get 0 in both directions).
- `/v2/runs/{id}` replaces `/v1/rail/stasy/trips/{id}` and `/v1/rail/suburban/trains/{no}` for
  the line card. It answers for any Run of the service day, started or not (`vehicle: null`
  before it starts). OASA: the matched trip's stops, timetable time plus observed delay.
  `404 {"error":"unknown_run"}` outside the service day (rev 16: a Run that `/v2/plan` returned
  answers until 03:00 after its service day, §9). Proastiakos: the service day of a Run, and the
  date in its id, runs 03:00 to 03:00 Athens time, so at 00:30 a train that left at 23:13 is still
  the Run of the evening before (#84). From 00:00 to 03:00 the Runs of the day to come answer too,
  and a Stop's arrivals list both days.
- Unknown ids: `404` `unknown_line` / `unknown_stop` / `unknown_run`. A Source still loading:
  `200` with the Source `warming_up` in `sources`, empty lists, and `null` for a single object
  that Source should fill (e.g. `line: null` in `/v2/lines/{id}` for a suburban Line before the
  corridor table loads, never `404` then). Never `503`, so one slow Source does not blank a card
  that also shows other Sources.
- **Incomplete responses are cached at most 5 s** (`max-age=5`, no `ETag`): any response with a
  Source `warming_up` or `down`. The long `max-age` above applies only to complete responses, so a
  Source that recovers shows within seconds, not after an hour. `stale` and `indicative` are
  complete: they are steady states (an expired timetable can stand in for weeks), and the
  `sources` block already says so.
- **`/v2/stops` has a strong `ETag`.** Its bytes are built once and kept while the stops and the
  Source states stay the same; an `ok` Source's `updated_at` does not change them, so the body
  carries the `updated_at` of its build. Clients keep the index and send `If-None-Match`; a `304`
  costs no body.

### 6. Mode × field availability

`✓` from the Source, `≈` estimate, `—` missing (`null`), `↻` only while the suburban stream is open.

| Field | bus / trolley (`oasa`) | metro / tram (`stasy`) | suburban (`proastiakos`) |
|---|---|---|---|
| Line `label` | ✓ | ✓ | — (badge falls back) |
| Line `color` | ✓ (GTFS / default) | ✓ GTFS `route_color` | adapter table |
| Stop `name_en` | — (`null`: not in the OASA GTFS) | ✓ name table (OSM, checked by hand) | ✓ |
| Pattern `shape` | ✓ GTFS | ✓ OSM track | ✓ corridor path |
| Vehicle `position.kind` | `gps` | `estimate` | `gps` ↻ |
| Vehicle `label` | ✓ fleet no. | — | ✓ train no. |
| Vehicle `delay_s` | ✓ when matched | — | ✓ ↻ |
| Vehicle `motion.path*` | ✓ when matched | ≈ from timetable + track | ≈ snapped to track, ahead at `speed` |
| Vehicle `motion.speed` | ✓ | — | ✓ (`speed_kmh` / 3.6) |
| Vehicle `motion.path_speeds` | ✓ | — | — |
| Vehicle `motion.knots` | — | ≈ | — |
| Arrival `source` | `live`; `timetable` before a bus starts its Run | `timetable` | `live` ↻ or board ≤ 10 min, else `timetable` |
| Arrival `scheduled_at` | ✓ | ✓ | ✓ |
| Arrival `delay_s` | ✓ | — | ✓ ↻ / board (≤ 10 min old) |
| Arrival `vehicle_label` | ✓ | — | ✓ |
| Arrival `platform` | — | — | ✓ when the board has it |
| Run (`/v2/runs/{id}`) | ✓ when matched | ✓ | ✓ |

### 7. Which adapter fills what

- **`oasa`**: Line, Pattern, Stop from the OASA GTFS (today's static `lines.json` /
  `lines/{id}.json` data); Vehicle from the matcher's vehicles (today's `/v1/lines/{id}` fields
  renamed: `bearing`, `position_at` → `position`; `variant` → Pattern id; `trip_label` dropped
  (the card builds it); `path*` → `motion`); Arrival from `internal/server/stops.go`, plus
  OASA's `getStopArrivals` for buses it does not have (rev 11). On an
  expired GTFS the timetable Arrivals and their Runs come from OASA's daily schedule
  (`getDailySchedule`, cached and prefetched once a day; a request never asks OASA for it): the
  run's departure plus the stop's offset in the GTFS trip of that direction that starts nearest
  in time. A line not cached yet has no timetable Arrivals (live ones only).
- **`stasy`**: Line from `routes.txt` (+ default names "Γραμμή 1" … when the feed leaves them
  empty); Stop by folding `stops.txt` names per mode; Pattern from the distinct stop sequences of
  `trips.txt` + the OSM track cut between stops; Vehicle from `STASY.Vehicles` (`from`/`to`
  become `next_stop`, `path`, `knots`); Arrival from the departures index; `indicative` →
  Source state.
- **`proastiakos`**: Line from the corridor table (names and colours in the adapter, as
  `suburban.json` has them); Stop from `corridor-stations` (ids merged, aliases kept); Pattern
  from the corridor path and its ordered stations; Vehicle from the train-stream (snapped to the
  corridor path; `course` → `bearing`); Arrival from the board + stream delay; with no board and none
  cached, no Arrivals, and the Source is `down` once the board fetch has failed (ose.mpass has no corridor, so it cannot give a
  Line).

### 8. Not in rev 8

- Cross-mode interchange grouping (Πειραιάς M1 + suburban + tram + buses as one place): not
  planned. Separate Stops; the Stop card lists nearby Stops of other modes (Harold, 2026-10-29).
- Rail vehicles inside the shared tiles: rejected (keeps the suburban stream open for bus-only
  map traffic, ~80 KB/s upstream; Codex review 2026-10-29).
- Server-side arrival alerts or push: arrival alerts stay client-side and poll `/v2/stops/{id}`.
  (Service disruptions are `/v2/alerts`, rev 13; a different thing.)
- MCP tools keep their names; they may switch to the `/v2` model inside without a contract change.

### 9. Trip planner `/v2/plan` (rev 16)

Implemented 2026-11-03 for issue #44 (PR #64; live once deployed), for the app's evening section «Τελευταία για σπίτι». Trips from a point to
a point: a walk to a Stop, one to four rides of any Mode with walks between them, and a walk to
the end. Design and limits: [superpowers/specs/2026-11-03-trip-planner.md](superpowers/specs/2026-11-03-trip-planner.md).

```
GET /v2/plan?from=37.97527,23.73552&to=37.87390,23.75400&after=1793227800&last=true&n=3
```

| Parameter | Required | Meaning |
|---|---|---|
| `from`, `to` | yes | `lat,lon`, decimal degrees. Outside Attica (lat 37.6-38.4, lon 23.2-24.2): `400 outside_area`. |
| `after` | no | Unix seconds; default now. No option leaves `from` before it. It must lie before the end of the current service day (below), and in that day or at most 1 h before now: else `400 bad_after`. |
| `last` | no | `true`: the last `n` options of the service day that leave at or after `after`. Default `false`: the first `n` options that leave at or after `after`. |
| `n` | no | 1-5, default 3. |

```json
{
  "service_day": "2026-11-03",
  "from": {"lat": 37.97527, "lon": 23.73552},
  "to": {"lat": 37.8739, "lon": 23.754},
  "after": 1793227800,
  "last": true,
  "itineraries": [
    {
      "depart_at": 1793230200, "arrive_at": 1793233329, "duration_s": 3129,
      "transfers": 1, "walk_m": 610, "modes": ["metro", "bus"],
      "legs": [
        {"kind": "walk", "from": {"stop": null, "lat": 37.97527, "lon": 23.73552},
         "to": {"stop": "stasy:συνταγμα-metro", "lat": 37.97527, "lon": 23.73552},
         "depart_at": 1793230200, "arrive_at": 1793230320, "distance_m": 0,
         "shape": [[37.97527, 23.73552], [37.97527, 23.73552]]},
        {"kind": "ride", "line": "stasy:M2", "mode": "metro", "headsign": "Ελληνικό",
         "run": "stasy:run:1012345:2026-11-03",
         "from": {"stop": "stasy:συνταγμα-metro", "lat": 37.97527, "lon": 23.73552},
         "to": {"stop": "stasy:ελληνικο-metro", "lat": 37.89260, "lon": 23.74710},
         "depart_at": 1793230320, "arrive_at": 1793231340, "hops": 10, "source": "timetable",
         "shape": [[37.97527, 23.73552], [37.9681, 23.7290], [37.8926, 23.7471]]},
        {"kind": "walk", "from": {"stop": "stasy:ελληνικο-metro", "lat": 37.8926, "lon": 23.7471},
         "to": {"stop": "oasa:330032", "lat": 37.8921, "lon": 23.7480},
         "depart_at": 1793231340, "arrive_at": 1793231539, "distance_m": 95,
         "shape": [[37.8926, 23.7471], [37.8921, 23.748]]},
        {"kind": "ride", "line": "oasa:122", "mode": "bus", "headsign": "Σαρωνίδα",
         "run": "oasa:run:13180045-ΕΣ-...@2340:2026-11-03",
         "from": {"stop": "oasa:330032", "lat": 37.8921, "lon": 23.748},
         "to": {"stop": "oasa:330140", "lat": 37.8752, "lon": 23.7531},
         "depart_at": 1793231700, "arrive_at": 1793232900, "hops": 9, "source": "timetable",
         "shape": [[37.8921, 23.748], [37.8840, 23.7502], [37.8752, 23.7531]]},
        {"kind": "walk", "from": {"stop": "oasa:330140", "lat": 37.8752, "lon": 23.7531},
         "to": {"stop": null, "lat": 37.8739, "lon": 23.754},
         "depart_at": 1793232900, "arrive_at": 1793233329, "distance_m": 515,
         "shape": [[37.8752, 23.7531], [37.8739, 23.754]]}
      ]
    }
  ],
  "walk_only": null,
  "lines": {"stasy:M2": {"id": "stasy:M2", "mode": "metro", "...": "a Line"}},
  "stops": {"stasy:συνταγμα-metro": {"id": "stasy:συνταγμα-metro", "...": "a Stop"}},
  "sources": [{"id": "oasa", "state": "indicative", "updated_at": 1793205590}]
}
```

Rules:

- **Service day**: 03:00 to 03:00 Athens time, as OASA's daily schedule counts it. `service_day`
  is its date. At 01:30 the service day is yesterday's, and its runs after midnight (bus times
  "24:15", the night lines) are in it. The planner holds one service day at a time; it changes
  to the next one after 03:00, once that day's timetables are loaded. A ride's `run` keeps the
  date its Source gives it (§3), so a run that leaves after midnight may carry the next
  calendar date. `/v2/runs/{id}` answers for every Run a plan returns until 03:00 after its
  service day, also when the date in the id is yesterday's.
- **Itinerary**: `legs` alternate walk and ride, and always start and end with a walk (a walk
  of 0 m when a point is on a Stop). `depart_at` is the start of the first walk, `arrive_at` the
  end of the last one. `transfers` = rides − 1. `walk_m` is the sum of the walks. `modes`: the
  Modes of the rides, in ride order, without repeats.
- **Ride leg**: `line`, `mode`, `headsign` as in the Line and Run; `run` is the Run id
  (§3), so the client can open `/v2/runs/{id}` for its live state; `hops` is the number of
  stops the ride moves past, the last one included. `source` is always `timetable` in rev 16:
  times are the timetable's, never live. A bus on an expired GTFS has the times of OASA's
  daily schedule (departure from the first stop plus the stop's offset in the GTFS trip that
  stands for it, as `/v2/stops/{id}` timetable Arrivals have).
- **Walk leg**: `distance_m` is the straight line × 1.3, whole metres; the walk takes
  `distance_m` / 1.2 m/s (about 4.3 km/h), rounded up to whole seconds. A metro or suburban
  Stop adds 120 s to a walk that starts or ends there (the way through the station). Walks from
  `from` and to `to`: every Stop up to 1000 m away (straight line). Walks between two rides:
  Stops up to 500 m apart. A change at the same Stop is a walk of 0 m that takes 0 s, or 120 s
  at a metro or suburban Stop (to another platform; the 120 s is counted once).
- **Leg times**: a walk's `arrive_at` is its `depart_at` plus its time. A ride's `depart_at`
  and `arrive_at` are the timetable's. A walk after a ride starts at the ride's `arrive_at`.
  The gap between a walk's `arrive_at` and the next ride's `depart_at` is waiting: at least
  60 s at a transfer (the transfer buffer), and 0 s before the first ride (the first walk
  starts as late as it can).
- **Which options**: at most 3 transfers (4 rides). An option is left out when another leaves
  `from` at the same time or later, arrives at `to` at the same time or earlier, and has no
  more transfers. At equal times and transfers only one is returned; which one is not fixed.
  `last=true` gives the `n` options that leave latest (some may arrive after 03:00);
  `last=false` the `n` options that leave first, looking at most 3 h past `after`. The `n` are
  picked first (ties: earlier arrival, then fewer transfers); the list is then sorted by
  `depart_at`, then `arrive_at`, then `transfers`. Fewer than `n` (or none) when
  fewer exist: `200` with `itineraries: []` after the last run of the day.
- **`shape`** (every leg, for the route map): `[[lat, lon], …]`, 5 decimals, at least 2 points.
  A ride: its Pattern `shape` cut from the board Stop to the alight Stop (the stops' nearest
  points on it, in ride order); when the Pattern has no shape, the stops' own points in order.
  A walk: in rev 16 the straight line from its start to its end (clients draw it dotted); a
  later rev may give the path along streets with the same key. The first and last point of a
  leg are its `from` and `to` points.
- **Live state and Plan B (client rules)**: the plan is timetable only. For the first ride the
  client reads `/v2/stops/{from.stop}` and takes the Arrival with the ride's `run` (else the
  first one of the same `line` and `headsign` at or after the ride's `depart_at`); "leave in X"
  is that Arrival's `at` − the first walk's time − now. When the user would miss it, the next
  itinerary of the list is Plan B; for a fresh list the client asks again with `after` = now.
  Local notifications ("leave now") are the app's own; the backend sends no push.
- **`walk_only`**: `{"distance_m": …, "duration_s": …}` when `from` and `to` are at most
  1000 m apart (straight line), else `null`. It is never one of the `itineraries`.
- **`lines`, `stops`**: every Line and Stop that a leg names, so the client needs no other call.
- **Data** (only what the backend already holds; a plan request never asks an upstream):
  - `oasa`: the GTFS trips while the GTFS is current; on an expired GTFS, the runs of OASA's
    daily schedule as the prefetch cached them. A line with no cached daily schedule has no
    rides until it is cached.
  - `stasy`: the STASY timetable (a typical day of the same weekday stands in when it has
    expired, never a holiday of the feed: `indicative`).
  - `proastiakos`: the schedule boards `/v2` already holds. No board: no suburban rides.
  - `sources` lists all three with the §4 states. `oasa` is `warming_up` when its GTFS is
    expired and no daily schedule of the service day is cached yet.
- **Errors**: `400` `bad_from`, `bad_to`, `bad_after`, `bad_n`, `outside_area`; `429 busy` with
  `Retry-After: 2` when too many plans run at once (a plan costs CPU; the other endpoints are
  not slowed). Never `503`.
- **Cache**: `max-age=60` when complete; `max-age=5` when a Source is `warming_up` or `down`
  (§5). No `ETag`. Clients round `from` and `to` to 4 decimals (about 10 m) and `after` to the
  minute, so equal questions hit the same cache entry.
- **Not in rev 16**: live delays in a plan; the "risky" mark of a ride whose line often drops
  its last runs (needs the seen/missed stats, about 2-3 weeks from 2026-10-30); a Mode filter;
  `arrive_by`; fares; accessibility; any day but the current service day.

## Fixtures

`docs/fixtures/` in the backend repo holds one example of every response above
(`line-040.json`, `vehicles.json`, `vehicles-tile-13-4636-3160.json`,
`vehicles-tile-9-289-197.json`, `stop-61000.json`, `status.json`, `lines.json`, `lines-040.json`). The frontend
develops against copies of these until the live API is up. `/v2` examples: `docs/fixtures/v2/`.

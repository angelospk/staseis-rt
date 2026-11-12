# staseis-rt API

Live positions and GTFS-Realtime for OASA buses and trolleys in Athens. Free to use, no key.
This page is for external users. Interactive reference: <https://angelospk.github.io/staseis-rt/>
(source: [`openapi/openapi.yaml`](openapi/openapi.yaml)). Full field rules: [`CONTRACT.md`](CONTRACT.md).

> **Ελληνικά, σύντομα.** Δημόσιο API χωρίς κλειδί. GTFS-Realtime για όλο το δίκτυο στο
> `/v1/gtfs-rt/`, JSON ανά γραμμή στο `/v1/lines/{γραμμή}`. Σεβαστείτε το caching: τα δεδομένα
> ανανεώνονται κάθε 30-150 s, άρα πιο συχνά αιτήματα δεν φέρνουν νέα δεδομένα.

Base URL: `https://transit.haroldpoi.dev`

The instance runs on one home machine (OASA does not answer data-centre addresses), behind
the Cloudflare cache. There is no uptime guarantee. For anything important, run your own
instance (see [Self-hosting](../README.md#self-hosting-docker)).

## Endpoints

The list of endpoints, their parameters and response schemas is the OpenAPI 3.1 spec
[`openapi/openapi.yaml`](openapi/openapi.yaml), with an interactive reference ("try it") at
<https://angelospk.github.io/staseis-rt/>. This page has usage notes and examples.

Live: `/v1/lines/{line_id}`, `/v1/lines/{line_id}/schedule`, `/v1/vehicles`,
`/v1/vehicles/tiles/{z}/{x}/{y}`, `/v1/gtfs-rt/{vehicle_positions,trip_updates}.{pb,json}`,
`/v1/stops/{stop_id}`, `/v1/stops/{stop_id}/arrivals`, `/v1/status`, `/v1/stats`,
`/v1/stats/days/{YYYY-MM-DD}`, `/v1/rail/stasy/departures`,
`/v1/rail/stasy/vehicles`, `/v1/rail/stasy/trips/{id}`, `/v1/rail/suburban/stations`,
`/v1/rail/suburban/stations/{id}/departures`, `/v1/rail/suburban/vehicles`,
`/v1/rail/suburban/trains/{no}`, `/healthz`; the unified model for every mode under `/v2`
([below](#one-model-for-every-mode-v2)). Static
data (line list, shapes, stops) is on GitHub Pages:
`https://angelospk.github.io/staseis-rt/static/v1/lines.json` and
`.../static/v1/lines/{line_id}.json`.

All responses are UTF-8, send `Access-Control-Allow-Origin: *`, and are gzip-compressed when
the request allows it. Live responses also send `Access-Control-Expose-Headers: Date`, so
browser code can read the server's `Date`. Times are Unix seconds.

## Examples

```bash
# Health and GTFS version
curl -s https://transit.haroldpoi.dev/v1/status
# {"ok":true,"gtfs_version":"2026-07-08","gtfs_expires":"2026-10-06","updated_at":1791229867,"lines_active":236,"budget_rps":4}

# Vehicles of line 040
curl -s https://transit.haroldpoi.dev/v1/lines/040

# All vehicles, with speed (m/s) and the route ahead
curl -s --compressed https://transit.haroldpoi.dev/v1/vehicles | jq '.vehicles | length'

# Vehicles around Syntagma only (one ~4 km map tile)
curl -s --compressed https://transit.haroldpoi.dev/v1/vehicles/tiles/13/4636/3160 | jq '.vehicles | length'

# Whole-network feeds
curl -s --compressed -o vehicle_positions.pb https://transit.haroldpoi.dev/v1/gtfs-rt/vehicle_positions.pb
curl -s --compressed https://transit.haroldpoi.dev/v1/gtfs-rt/trip_updates.json | jq '.entity | length'
```

Python:

```python
import requests
from google.transit import gtfs_realtime_pb2  # pip install gtfs-realtime-bindings

feed = gtfs_realtime_pb2.FeedMessage()
feed.ParseFromString(requests.get("https://transit.haroldpoi.dev/v1/gtfs-rt/vehicle_positions.pb").content)
for e in feed.entity[:5]:
    v = e.vehicle
    print(v.vehicle.id, v.trip.route_id, v.trip.trip_id, v.position.latitude, v.position.longitude)
```

## Per-line JSON

`line_id` is the OASA line number in Greek uppercase (`040`, `550`, `Α1`, `Χ96`),
percent-encoded. Latin look-alikes also work (`A1` → `Α1`).

```json
{
  "line": "040",
  "updated_at": 1791229815,
  "next_update_at": 1791229873,
  "vehicles": [
    {
      "id": "44537",
      "lat": 37.974551, "lon": 23.734165,
      "bearing": 41,
      "position_at": 1791229792,
      "route_code": "3923",
      "variant": "5513",
      "trip_id": "938_day_1_1303_2300",
      "trip_label": "23:00 ΣΥΝΤΑΓΜΑ → ΠΕΙΡΑΙΑΣ",
      "delay_s": 0,
      "next_stop_id": "10341"
    }
  ]
}
```

- `trip_id`, `trip_label`, `delay_s` and `next_stop_id` are `null` when the vehicle is not
  matched to a scheduled trip. `bearing` and `variant` may be `null`.
- `at_terminus` is `true` when the vehicle is matched and waiting at the first stop of its
  trip; its `delay_s` is then a departure delay.
- Refetch at `next_update_at`, not earlier: until then the answer does not change.
- Asking for a line raises its polling priority for about 10 minutes (about every 30 s). If
  nobody asked for it recently, it is polled at once: `next_update_at` is then a few seconds
  away.
- `speed` is the speed along the route in m/s (0..20, `0` = standing, `null` = unknown).
  `path` is the route ahead as `[lat, lon]` pairs, from the vehicle's point on its shape to
  the next stop (or 300 m-1.5 km ahead), or `null`. `path_beyond` continues it to the third
  stop ahead, and `path_stops` gives the stops on both as metres along them (or `null`).
  `path_speeds` (contract rev 6) is the typical speed per 200 m ahead, from the last 14 days of
  fixes on the same shape at that day type and hour (or `null`).
  See [`CONTRACT.md`](CONTRACT.md).

## All vehicles

`GET /v1/vehicles` returns every vehicle with a GPS fix from the last 5 minutes, one snapshot
for everyone, rebuilt at most every 10 s. Each vehicle has `line`, `id`, `lat`, `lon`,
`bearing`, `position_at`, `variant`, `delay_s`, `speed`, `path`, `path_beyond`, `path_stops` and `path_speeds`
(same meanings as above).
It does not raise any line's polling priority, so lines nobody watches update about every
150 s. Send `If-None-Match` with the last `ETag` to get `304` when nothing changed.

## Vehicles of a map area

`GET /v1/vehicles/tiles/{z}/{x}/{y}` returns the part of the same snapshot that lies in one
XYZ (Web Mercator, slippy map) tile, in the same JSON shape. `z` is `9` (~62 km tiles, no
`path`, `path_beyond`, `path_stops`, `path_speeds`; for a zoomed-out map) or `13` (~3.9 km tiles, the full
entries). Tiles exist for lat 37.5..38.5, lon 22.9..24.5 (`z` 9: x 288..290, y 196..198;
`z` 13: x 4617..4653, y 3145..3174); others are `404`. Each vehicle is in the one tile of its
position. Use plain URLs: a query string gives `400`, so every client shares the cached copy.
A map view at MapLibre zoom 13 needs 2-15 tiles of 1-8 KB (gzip) instead of the 60+ KB of
`/v1/vehicles`. Client rules: [`CONTRACT.md`](CONTRACT.md#get-v1vehiclestileszxy-rev-5).

## A stop and its arrivals

`GET /v1/stops/{stop_id}` returns the stop (`id`, `name`, `lat`, `lon`, and `code`, the code on the
stop sign, since rev 21, missing when the stop has none), the `lines` that call
there and `arrivals`, soonest first, up to 60 min ahead, at most 3 per line and variant. Each has
`line`, `headsign`, `variant`, `vehicle`, `eta_s`, `at`, `delay_s`, `position_at`, `source`,
`trip_id`, `scheduled`, `stops_away` and `distance_m`.

- `source: live`: a bus of those lines that still has the stop ahead, from a fix of the last
  5 minutes. `at` is its fix plus the driving time to the stop (typical speeds per 200 m of the
  shape, else the timetable's running time); an unmatched bus is placed on its shape and has no
  `trip_id`. A bus not past the stop by its last fix is never overdue: late, it is due now.
- `source: schedule`: a trip that has not left yet and has no bus (`vehicle` is `""`). None
  while the static GTFS is expired.

The request raises the polling priority of the stop's lines that have a live bus in
`arrivals`; a line with none keeps its usual rate. No OASA request of its own. Cached 15 s.
`warming` (rev 22) is `true` while a line of the stop is polled now but has no poll yet (after a
restart): an empty `arrivals` then does not mean no bus is coming; cached 5 s.
Full rules: [`CONTRACT.md`](CONTRACT.md#get-v1stopsstop_id-rev-8).

```bash
curl -s https://transit.haroldpoi.dev/v1/stops/61000 | jq -r '.arrivals[] | [.line, .source, .eta_s/60|floor] | @tsv'
```

## One model for every mode (`/v2`)

`/v2` serves buses, trolleys, metro (M1-M3), tram (T6, T7) and the suburban railway
(Proastiakos) in one model: Line, Stop, Pattern, Vehicle, Arrival, Run. `/v1` stays as it is.
Full field rules: [`CONTRACT.md`](CONTRACT.md#unified-api-v2-rev-8); terms:
[`GLOSSARY.md`](GLOSSARY.md); examples: [`fixtures/v2/`](fixtures/v2/).

| Endpoint | What | Cached |
|---|---|---|
| `GET /v2/lines` | every Line of every mode | 1 h |
| `GET /v2/lines/{id}` | one Line, its Patterns (stop order + shape) and Stops | 1 h |
| `GET /v2/lines/{id}/vehicles` | the Line's vehicles now | until `next_update_at` |
| `GET /v2/stops` | every Stop, compact rows `[id, name, lat, lon, modes, lines]` | 1 h |
| `GET /v2/stops/{id}` | one Stop, its Lines and the next Arrivals (2 h, 4 per Line + headsign, 30 in all); OASA stops add OASA's own predictions for buses the backend does not have | 15 s |
| `GET /v2/vehicles`, `/v2/vehicles/tiles/{z}/{x}/{y}` | buses and trolleys (as `/v1/vehicles` and its tiles) | until `next_update_at` |
| `GET /v2/vehicles/rail` | metro and tram trains | 15 s |
| `GET /v2/vehicles/suburban` | suburban trains (opens the railway.gov.gr stream) | 5 s with trains, else 15 s |
| `GET /v2/runs/{id}` | one Run (a trip of one day) with every stop and its time | 15 s |
| `GET /v2/alerts` | service disruptions (rev 13): OASA notices and strikes | 5 min |
| `GET /v2/plan` | trips from a point to a point with transfers, from the timetables (rev 16) | 1 min |
| `GET /v2/municipal` | municipal bus routes and stops from OpenStreetMap (rev 18), no times | 1 day |

- **Ids** are `<source>:<native>` and must be percent-encoded in a URL: `oasa:040`,
  `stasy:M2`, `stasy:συνταγμα-metro`, `proastiakos:KIATO`,
  `proastiakos:6928c3c150ab5bc72b39cee6`, `oasa:run:<trip_id>:<YYYY-MM-DD>` (a run of OASA's daily
  schedule on an expired GTFS: `oasa:run:<trip_id>@<HHMM>:<YYYY-MM-DD>`),
  `proastiakos:run:<train no.>:<YYYY-MM-DD>`. An old id of a renamed Stop still answers, with
  the current `id` in the body. Unknown ids: `404` `unknown_line` / `unknown_stop` /
  `unknown_run`.
- **`sources`** in every body says how fresh each Source is (`ok`, `warming_up`, `stale`,
  `indicative`, `down`). A body with a Source `warming_up` or `down` is cached 5 s, with no
  `ETag`; a complete one cached 1 h has an `ETag` and answers `304` (`/v2/stops`: a strong one
  that holds while the stops stay the same, so keep the index and send `If-None-Match`).
- **`position.kind`** is `gps` for buses and suburban trains, `estimate` for metro and tram (from
  the timetable; `motion.knots` moves them between snapshots). Arrival **`source`** is `live`
  when GPS or a reported delay set `at`, else `timetable`. When the OASA GTFS has expired, bus
  `timetable` Arrivals and their Runs come from OASA's daily schedule of today (departure plus the
  GTFS stop offsets); `oasa` is `indicative` only for lines whose daily schedule is not cached.
- Suburban trains are only in `/v2/vehicles/suburban`, so map traffic (buses, metro, tram) does
  not keep the railway.gov.gr stream open. Load it only while a suburban layer is on. A suburban
  Stop or Line vehicles request opens the stream too; it closes
  3 minutes after the last such request.

```bash
# Next departures at Σύνταγμα (metro), soonest first
curl -s "https://transit.haroldpoi.dev/v2/stops/$(jq -rn '"stasy:συνταγμα-metro"|@uri')" \
  | jq -r '.arrivals[] | [.line, .headsign, (.at|localtime|strftime("%H:%M")), .source] | @tsv'

# Every metro and tram train now, then every suburban train
curl -s --compressed https://transit.haroldpoi.dev/v2/vehicles/rail | jq -r '.vehicles[] | [.line, .headsign, .position.kind] | @tsv'
curl -s --compressed https://transit.haroldpoi.dev/v2/vehicles/suburban | jq -r '.vehicles[] | [.line, .label, .position.kind] | @tsv'
```

### Service disruptions (`/v2/alerts`)

`GET /v2/alerts` is the one feed that is not a map: it lists the service disruptions of the
next days, polled in the background every 10 min from two sources. `source: oasa` is an OASA
notice of its news feed, buses and trolleys only: `lines` and `stops` are the ones the GTFS
knows (`oasa:<code>` ids), `from`/`until` the dates of the notice (`null` when it names none).
`source: athenstransport` is a strike from athenstransport.com, which also covers metro, tram
and trains: `modes` from its categories (every mode when it names none), `urgent: true`. An
Alert is listed for 7 days after `published`, or until `until` when that is later. A source
that fails keeps its last Alerts; `updated_at` is the last poll with no failure (`null` and an
empty list before the first). Show an Alert from `from` to `until`, and 3 days after
`published` when it has no dates; always link `url`.

```bash
# Every Alert now, newest first
curl -s https://transit.haroldpoi.dev/v2/alerts \
  | jq -r '.alerts[] | [.source, .from, .until, (.modes|join(",")), .title] | @tsv'
```

### Trip planner (`/v2/plan`)

`GET /v2/plan?from=<lat,lon>&to=<lat,lon>[&after=<unix>][&last=true][&n=1-5]` plans trips inside
Attica: a walk to a Stop, one to four rides of any mode with walks between them, and a walk to
the end. It reads the timetables only (`source: timetable`; open a ride's `run` with
`/v2/runs/{id}` for its live state). `last=true` gives the last `n` options of the service day
(03:00 to 03:00 Athens time), else the first `n` within 3 h of `after` (default now). An option
is left out when another leaves no earlier, arrives no later and has no more transfers. Walks are
the straight line × 1.3 at 1.2 m/s (+120 s at a metro or suburban Stop), up to 1000 m at the ends
and 500 m between rides, with at least 60 s of waiting before each later ride. `walk_only` is the
walk when the points are at most 1000 m apart. Every leg has a `shape` for a route map. Errors:
`400` `bad_from`, `bad_to`, `outside_area`, `bad_after`, `bad_n`; `429 busy` (`Retry-After: 2`)
when two plans are already running. Full rules: [`CONTRACT.md` §9](CONTRACT.md#9-trip-planner-v2plan-rev-16);
example: [`fixtures/v2/plan.json`](fixtures/v2/plan.json).

```bash
# The last 3 trips of the day from Σύνταγμα to Γλυφάδα that leave after 23:30
curl -s "https://transit.haroldpoi.dev/v2/plan?from=37.9753,23.7355&to=37.8739,23.7540&last=true&after=$(date -d 23:30 +%s)" \
  | jq -r '.itineraries[] | [(.depart_at|localtime|strftime("%H:%M")), (.arrive_at|localtime|strftime("%H:%M")),
      ([.legs[] | select(.kind=="ride") | .line] | join(" → "))] | @tsv'
```

### Municipal buses (`/v2/municipal`)

`GET /v2/municipal` lists the bus routes that municipalities run (δημοτική συγκοινωνία), which
OASA does not have: about 37 routes of 12 municipalities, from OpenStreetMap (ODbL), refreshed
weekly from the Overpass API. Each route has its `shape` (lines of `[lat, lon]`), its `stops`
(`osm:n<id>`, `[]` when OSM has none), its `municipality` and `timetable_url`, the
municipality's own page. There are no times and no live positions. Show `attribution`
(«© OpenStreetMap contributors (ODbL)») with the layer. Before the first poll after a start the
lists are empty and `updated_at` is `null`.

```bash
# Every municipal route, with its timetable page
curl -s --compressed https://transit.haroldpoi.dev/v2/municipal \
  | jq -r '.routes[] | [.municipality, .ref, .name, (.stops|length), .timetable_url] | @tsv'
```

## OASA's predictions at a stop

`GET /v1/stops/{stop_id}/arrivals` gives OASA's own predictions for a stop, soonest first:

```bash
curl -s https://transit.haroldpoi.dev/v1/stops/240084/arrivals
# {"stop_id":"240084","stop_name":"ΠΛ. ΔΑΒΑΚΗ","updated_at":1793210650,
#  "arrivals":[{"line":"5","route_code":"1992","vehicle":"88019","minutes":28}]}
```

`stop_id` is the GTFS `stop_id` (the OASA stop code), as in the stops of
`static/v1/lines/{line_id}.json`. The predictions come from OASA (`getStopArrivals`), so they
work without a GTFS trip match, also while the static GTFS is expired and `/v1/stops/{stop_id}`
has no matched buses to list. `line` is empty when the route code is not in OASA's line list.

Each stop costs one OASA call per 30 s: answers are cached 30 s per stop, requests for the same
stop at the same moment share one call, and `Cache-Control` lasts until that cache expires
(`max-age` 5-30). The calls share the server's OASA budget with the line polls but get at most
one request slot in four, so the vehicle positions keep at least three quarters of it. At most
16 such calls wait at once; past that the answer is `503 busy`. When OASA or the queue cannot
answer, the stop's last good answer up to 2 minutes old is served instead, with its own
`updated_at` and `max-age=5`. Unknown stop ids get `404` without asking OASA.

## Schedules

Three read-only endpoints add timetables to the live positions. None of them raises any polling
priority.

### Line schedule (OASA buses, trolleys)

`GET /v1/lines/{line_id}/schedule` returns the trips of a line on a day, today in Athens time by
default. `?date=YYYY-MM-DD` reads another day, up to 7 days ahead.

```json
{
  "line": "040", "date": "2026-10-28", "expired": false, "source": "oasa-daily",
  "directions": [
    {"direction": 0, "headsign": "ΠΕΙΡΑΙΑΣ",
     "departures": [{"t": "05:30", "trip": "", "seen": true}]}
  ]
}
```

- `source`: today's timetable is OASA's daily schedule (`getDailySchedule`, `oasa-daily`) when
  OASA answers; the server fetches every line's once a day in the background, and a request falls
  back to the GTFS (`gtfs`) when it is not there yet. Other days are always from the GTFS.
- `t` is the departure from the trip's first stop in GTFS style: a trip after midnight shows
  `24:10`.
- `directions` groups the trips by `direction_id` and headsign. `trip` is the GTFS `trip_id`
  (empty for `oasa-daily`: the daily schedule has no trip ids).
- `seen` (today only): did the run leave?
  - `gtfs`: `true` when the matcher has assigned a vehicle to that trip since midnight.
  - `oasa-daily`: `true` when a bus of the line was seen leaving the start of a shape that ends
    where the direction does (last fix within 300 m of the start, then 600 m or more along it within
    15 min), from 3 min before to 20 min after the run's time; each start counts for one run.
    `false` when no bus left in that window and 20 min have passed. Missing when not known yet,
    or when the run was due before the server started watching (after a restart).
  - `citybus` (the citybus.gr cities, [citybus.md](citybus.md)): `true` when a bus that carries the
    run's code was seen past its first stop. Never `false`: only some buses show, so a run with no
    bus seen has no `seen` key. `live: true` (this source only) while that bus was sighted in the
    last 120 s.
- `expired` is `true` when the day is outside the static GTFS validity. The trips are then those
  of the same weekday inside the feed, so the timetable may not be the one OASA runs.
- `404 {"error":"unknown_line"}` for a line the feed does not have, `400 {"error":"bad_date"}`
  for a malformed or out-of-range date. Cached 60 s.

### Metro and tram departures (STASY)

`GET /v1/rail/stasy/departures?stop=<name>&n=6` returns the next `n` (1-20, default 6)
departures per line and headsign at every stop with that name, from now. The server loads the
STASY GTFS (metro M1-M3, tram T6/T7) at start and checks it once a day.

```json
{"stop": "ΣΥΝΤΑΓΜΑ", "indicative": false,
 "departures": [{"line": "M2", "route_color": "#e30613", "headsign": "ΕΛΛΗΝΙΚΟ", "t": "14:05", "in_s": 240}]}
```

- The name is matched without case, accents or final sigma: `συνταγμα`, `ΣΥΝΤΑΓΜΑ` and
  `Σύνταγμα` are the same stop.
- `in_s` is the seconds from now to the departure.
- `headsign` is the feed's `trip_headsign`, or the last stop of the trip when the feed leaves it
  empty (it leaves most of them empty).
- `indicative: true` when the STASY timetable has expired and a typical day of the same weekday
  inside it was used instead (never a holiday of the feed): show those times as indicative.
- `503 {"error":"warming_up"}` until the first load finishes, `404 {"error":"unknown_stop"}`,
  `400 {"error":"missing_stop"}`. Cached 30 s.

`GET /v1/rail/stasy/vehicles` returns every metro and tram train running now by the timetable.
STASY has no live data, so these are estimates: each train is between the stop it left (`from`,
at its departure) and the next one (`to`, at its arrival), times in unix seconds. Move it between
the two on your own clock. A train standing at a stop has that stop as both `from` (its arrival)
and `to` (its departure).

```json
{"updated_at": 1793205600, "indicative": false,
 "vehicles": [{"trip": "1012345", "line": "M2", "route_color": "#e30613", "headsign": "ΕΛΛΗΝΙΚΟ",
               "from": {"name": "ΣΥΝΤΑΓΜΑ", "lat": 37.975, "lon": 23.735, "at": 1793205540},
               "to": {"name": "ΑΚΡΟΠΟΛΗ", "lat": 37.969, "lon": 23.729, "at": 1793205660}}]}
```

`GET /v1/rail/stasy/trips/{id}` returns one run (`trip` of a vehicle) with every stop and its
departure in `stops` (same fields as `from`; the last stop has its arrival). A run past midnight is yesterday's while it still runs.
`404 {"error":"unknown_trip"}`. Both: `503 {"error":"warming_up"}` until the first load; vehicles
cached 15 s, trips 60 s.

### Proastiakos stations and departures

`GET /v1/rail/suburban/stations` lists the suburban stations with their corridors (`id`, `name`,
`corridors`), from railway.gov.gr, cached 24 h.

`GET /v1/rail/suburban/stations/{id}/departures` returns the next 10 trains at one station
(`?n=` 1-20).

```json
{"station": "Πειραιάς",
 "departures": [{"train": "1203", "from": "Άγιοι Ανάργυροι", "to": "Κιάτο", "t": "05:32",
                 "delay_min": null, "platform": "4", "status": "scheduled"}]}
```

- `t` is the scheduled departure in Athens time. `status` is the state of the run (`scheduled`,
  `in_progress`, `delayed`, `completed`); cancelled runs are left out.
- The server keeps one cached board per day and serves it when railway.gov.gr fails. Without a
  cached board it falls back to the ose.mpass screen of the station, for the stations whose
  screen number is known (the Attica ones).
- `delay_min` is the live delay of the train when the vehicles stream (below) is open, else the
  board's (up to 10 minutes old).
- `502 {"error":"upstream_unavailable"}` when neither source answers. Cached 60 s.

`GET /v1/rail/suburban/vehicles` returns the trains in service with their last GPS position,
from the railway.gov.gr train tracker. The server opens the tracker's stream on the first
request and closes it 3 minutes after the last one, so the first request after a pause gets
`503 {"error":"warming_up"}` for a few seconds. Cached 5 s.

```json
{"updated_at": 1793205600,
 "trains": [{"train": "1226", "corridor": "PIRAIR", "service": "Suburban", "lat": 38.0487,
             "lon": 23.7314, "course": 10.5, "speed_kmh": 80.4, "delay_min": 0,
             "from": "Πειραιάς", "to": "Αεροδρόμιο", "next_station": "Κάτω Αχαρναί",
             "position_at": 1793205590}]}
```

- Trains of all Greece are listed; `corridor` tells the line. Locomotives with no run are left out.
- `position_at` is the time of the GPS fix; some fixes are minutes old. `course` is degrees from
  north, `null` when unknown.

`GET /v1/rail/suburban/trains/{no}` returns today's run of a train with every stop: the run under
way, else the next one, else the last.

```json
{"train": "1226", "from": "Πειραιάς", "to": "Αεροδρόμιο", "status": "in_progress", "delay_min": 0,
 "stops": [{"id": "6928c3…", "name": "Πειραιάς", "t": "17:05", "at": 1793203500,
            "platform": "2", "delay_min": 0}]}
```

- `delay_min` of the run is the live one when the stream is open.
- `404 {"error":"unknown_train"}`, `502 {"error":"upstream_unavailable"}`. Cached 30 s.

## Daily statistics

A server that runs with `--history` sums each finished day (Europe/Athens, 00:00-24:00) about
4-5 h after midnight (it waits for the trips that run past midnight). `GET /v1/stats` returns `{"version":1,"latest":"2026-11-03","days":[...]}`;
`GET /v1/stats/days/{date}` returns one day (`400` for a malformed date, `404` when missing).
Only days from 2026-10-30 are listed and served (`404` before it): the seen/missed trip counts
of earlier days are not reliable (no daily OASA schedule, seen-flag bugs).
The same files are on the `stats` branch of this repository; prefer them (its `v1/days` keeps
the earlier files, but `index.json` and `latest.json` skip them):
`https://raw.githubusercontent.com/angelospk/staseis-rt/stats/v1/latest.json`,
`.../stats/v1/index.json`, `.../stats/v1/days/{date}.json`.

| Field | Meaning |
|---|---|
| `date`, `tz` | the local day and its time zone |
| `generated_at` | when the file was written |
| `first_fix`, `last_fix` | first and last fix time of the day (a day the history started late, or with an outage, has fewer hours with fixes) |
| `files`, `fixes`, `vehicles`, `lines` | history archives read, GPS fixes, distinct vehicles, distinct lines |
| `hours[h]` | 24 entries by local hour: `fixes`, `vehicles`, `lines`, `dist_m`, `time_s` |
| `by_line[]` | every line with a fix: `line`, `fixes`, `vehicles`, `dist_m`, `time_s` |
| `trips` | scheduled trips of the day and how many ran (below); missing when the server had no static GTFS of the day's history |
| `start_delay` | how late the runs of OASA's daily schedule left (below); missing when no run of the day was measured (every day before the field shipped) |
| `headways` | regularity (headway adherence) of the high-frequency lines of OASA's daily schedule (below); missing when no direction qualifies (every day before the field shipped) |

Speed in km/h = `dist_m / time_s * 3.6`; `time_s` 0 means no speed. Sums, not averages, so
several days add up exactly. A speed sample is two consecutive fixes of one vehicle on the
same line, 5-120 s apart and at most 25 m/s, counted in the hour and line of the later fix;
the distance is the straight line between them (a little short on curves) and stops and
traffic lights count. A fix counts for the day and hour of its own fix time; a fix that
reaches the server more than 3 h after the end of its day is not counted. A day without any
history file (server down all day) has no file and is not listed. `vehicles` counts vehicles that sent a
fix, which depends on the polling: a line nobody watches is polled about every 150 s. In the
hour repeated when clocks go back, both hours are summed into one entry.

### Trips that ran (`trips`)

| Field | Meaning |
|---|---|
| `gtfs` | the static GTFS version (`gtfs_version` of `/v1/status`) the trips come from |
| `gtfs_expired` | `true` when the day is outside that feed's validity. The lines with an archived OASA daily schedule of the day are then planned from it (`schedule_source`); the others take the trips of the same weekday inside the feed, which may not be the timetable OASA really ran |
| `schedule_source` | where the planned trips come from: `oasa_daily` (OASA's daily schedule of the day), `gtfs`, or `mixed` (some lines each). Missing on days written before 2026-11 (they are `gtfs`) |
| `min_coverage` | share of a trip's route a vehicle must cover for the trip to count as run (0.5) |
| `planned`, `run`, `partial`, `missed`, `unmeasured` | trips the day planned, and how they split: covered at least `min_coverage`; seen but covered less; never seen; no usable shape, so not measured (`run + partial + missed + unmeasured = planned`) |
| `by_line[]` | every line with a planned trip: `line`, `schedule_source` (`oasa_daily` or `gtfs`), the same five counts, `missed_trips` (by start time; `null` when none): GTFS `trip_id`s on a `gtfs` line, and `go HH:MM` / `come HH:MM` (local departure time of OASA's Go or Come run) on an `oasa_daily` line |

A day plans every trip whose scheduled start falls in it (a trip at 24:10 of Monday's service
counts for Tuesday). A history row counts for a trip when the matcher assigned the vehicle to
that `trip_id` (same GTFS version) between 30 min before its scheduled start and 30 min after
its scheduled end. Coverage is the distance along the shape between the first and the last of
those rows, divided by the shape length between the trip's first and last stop. Share of trips
run = `run / (planned - unmeasured)`.

A line planned from OASA's daily schedule (`oasa_daily`) has one planned trip per run: the
day's runs that leave before midnight, and the runs at 24:00 and later of the day before. The
runs have no `trip_id`, so they are paired with the buses that ran: the matched rows of one
vehicle on the GTFS trips whose shape ends where the run's direction does (by name, as the
`seen` flags of the schedule) are one bus run, whatever trip each row has (the matcher moves a
bus between those trips at the stand and on the way). A new bus run starts when the vehicle is
more than half of the route behind its previous fix (back at the start) or after 30 min without
a fix. A bus run left at its last fix within 300 m of its first stop, or at its first fix when
it was first seen further on. In time order, each run takes the first unused bus that left from
5 min before to 30 min after it; a run without one is missed. The bus's coverage decides `run`
or `partial`: each fix counts as its share of its own trip's shape, so trips on different
shapes add up. A GTFS trip that
matches both directions (a loop) pairs with neither. A direction with no GTFS shape of its own
has its runs without a bus `unmeasured`. A bus no run takes is not counted. The server
keeps each day's daily schedules (all lines, about 20 KB gzip a day) under its state directory
(`daily/YYYY-MM-DD.json.gz`), so the stats of a day can be written again later.

Limits: the matcher assigns each vehicle to the closest scheduled trip, so a late bus can take
the slot of a cancelled one (`run` is an upper bound of the trips really run); a bus without
working telematics looks like a missed trip (`missed` is an upper bound of cancellations); an
outage of the server (an hour with few `fixes` in `hours[]`) makes its trips missed; with an
expired feed and no daily schedule of the line, a line or trip the old timetable has but OASA no
longer runs counts as missed; with the daily schedule, a late bus can take the run after a
cancelled one the same way. Rows of
another GTFS version are ignored; when they are most of the day's rows, `trips` is left out.

### Start delays (`start_delay`)

```json
"start_delay": {"runs": 6, "by_line": [
  {"line": "040", "runs": 4, "mean_s": 203, "p90_s": 600, "sum_s": 810, "minutes": [[-1, 1], [1, 1], [3, 1], [10, 1]]}
]}
```

| Field | Meaning |
|---|---|
| `runs` | runs measured that day, all lines |
| `by_line[]` | every line with a measured run, by id: `line`, `runs`, `mean_s` (rounded), `p90_s` (nearest rank: the smallest delay that 90% of the runs did not exceed), `sum_s`, `minutes` |
| `minutes` | histogram: `[minute, runs]` pairs by minute, minute = floor(delay / 60) (`-1` is 1-60 s early). Add the pairs of several days and read a p90 of the period from them (to the minute) |

A run counts when the server saw its bus leave the first stop on a run of OASA's daily schedule
that is clear (no free run nearer to that departure; the same rule as `delay_s`, contract rev 14)
and the bus then matched the line. Its start delay is the time it left minus the run's scheduled
departure (negative: early), as precise as the polling of the line (about 30 s on a watched line, up to 150 s otherwise). A run counts once (line, vehicle and departure), for the day of its
scheduled departure. Only while the static GTFS is expired (the server pairs buses with daily
runs only then). Runs whose departure was not seen (the bus appeared on its way), lines whose daily
schedule did not load, and short-headway runs that two buses could have taken are not measured,
so `runs` is a sample, not every run.

### Headway regularity (`headways`)

```json
"headways": {"from_h": 7, "to_h": 21, "at": 0.5, "by_line": [
  {"line": "040", "dir": 0, "n": 96, "mean_headway_s": 512, "median_headway_s": 480, "cv_h": 0.412, "wait_min": 4.99, "ewt_min": 0.72, "off_headway_share": 0.208, "los": "D"}
]}
```

The measures follow TCQSM 4th edition (TCRP Research Report 262, 2026, chapter 5), with one
difference: the reference is the mean **observed** headway, because the scheduled headway at
mid-route is not known. So `cv_h` measures how even the buses ran, not whether they kept the
promised frequency.

| Field | Meaning |
|---|---|
| `from_h`, `to_h` | local hours of the window: passes from 07:00 to before 21:00 count |
| `at` | the reference point: share of the route (0.5 = mid-route) a bus passes |
| `by_line[]` | one entry per direction of a line (`dir` 0 Go, 1 Come), by line then `dir` |
| `n` | headways measured (at least 8) |
| `mean_headway_s` | mean observed headway (at most 600 s: only directions at 10 min or less are listed, as in TCQSM Exhibit 5-9) |
| `median_headway_s` | median observed headway |
| `cv_h` | headway adherence: population stddev of the headways / `mean_headway_s` |
| `wait_min` | average wait in minutes of a passenger who arrives at random: 0.5 · mean · (1 + cv_h²) (TCQSM Eq. 5-1) |
| `ewt_min` | excess wait in minutes: `wait_min` minus half the mean headway (0 for perfectly even buses) |
| `off_headway_share` | share of headways off the mean by more than half of it, either bunched (too short) or a gap (too long); measured, not derived from `cv_h` |
| `los` | band of `cv_h`, TCQSM Exhibit 5-9: A ≤ 0.21 (like clockwork), B ≤ 0.30 (slightly off headway), C ≤ 0.39 (often off headway), D ≤ 0.52 (irregular, some bunching), E ≤ 0.74 (frequent bunching), F above (most bunched). The 4th edition gives the bands without letters; the letters are ours |

Days written before 2026-11-13 used the median as the reference and 12 min as the limit: they
have `ref_headway_median_s` and no `median_headway_s`, `wait_min` or `off_headway_share`.

A pass is the time a bus run of a daily direction (same bus runs as `trips`) crosses `at` of the
route, interpolated between two fixes at most 5 min apart; a run first seen past the point has
no pass. Headways are the gaps between consecutive passes of the direction in the window. Only
lines planned from OASA's daily schedule (`oasa_daily`). A bus without working telematics has no pass,
so two headways merge into one: `mean_headway_s`, `cv_h`, `wait_min`, `ewt_min` and
`off_headway_share` are biased, usually up, but a missing bus can also hide a bunch and lower them.

## GTFS-Realtime details

- `trip_id`, `route_id` and `stop_id` refer to the official OASA static GTFS
  ([osy_gtfs.zip on data.gov.gr](https://data.gov.gr/dataset/fb049bb1-aea6-4443-95fa-8b941dd6a057/resource/119db488-16ea-4c76-b560-41c472872390/download/osy_gtfs.zip)). Use the version in `/v1/status`.
- The feeds are `FULL_DATASET`. `header.timestamp` is the build time, or the newest vehicle fix if that is later.
- VehiclePositions has one entity per vehicle (`vehicle-{id}`). A vehicle that is not matched
  to a trip has a trip descriptor with only `route_id`.
- TripUpdates has one entity per matched trip, with one `stop_time_update` (the next stop)
  that carries the current delay. Consumers propagate it to later stops.
- Matching: each vehicle is assigned to a scheduled trip by its position along the route
  (Hungarian assignment, with departure time and the last 15 minutes of positions to keep
  trips stable when buses bunch). OASA itself publishes no trip ids.

### Validation

On 2026-10-05 (20:06-20:08 UTC, 5 samples 30 s apart, 451 vehicles), the
[MobilityData GTFS Realtime validator](https://github.com/MobilityData/gtfs-realtime-validator)
reported:

| Feed | Errors | Warnings |
|---|---|---|
| TripUpdates | 0 | 0 |
| VehiclePositions | 0 | W006, W009 for unmatched vehicles only (19 of 451, about 4%) |

The warnings come from the `route_id`-only trip descriptor of unmatched vehicles. It is
intended: the line is known even when the trip is not.

To repeat it (batch mode, needs the static GTFS that the feed uses):

```bash
mkdir -p rt/vp && curl -sL -o rt/gtfs.zip https://data.gov.gr/dataset/fb049bb1-aea6-4443-95fa-8b941dd6a057/resource/119db488-16ea-4c76-b560-41c472872390/download/osy_gtfs.zip
for i in 1 2 3 4 5; do
  curl -s -o "rt/vp/VehiclePositions-$(date -u +%Y-%m-%dT%H-%M-%SZ).pb" https://transit.haroldpoi.dev/v1/gtfs-rt/vehicle_positions.pb
  sleep 30
done
docker run --rm -v "$PWD/rt:/w" --entrypoint java ghcr.io/mobilitydata/gtfs-realtime-validator -Xmx5g \
  -cp /app/gtfs-realtime-validator-webapp-1.0.0-SNAPSHOT.jar edu.usf.cutr.gtfsrtvalidator.lib.Main \
  -gtfs /w/gtfs.zip -gtfsRealtimePath /w/vp -sort name
# results: rt/vp/*.pb.results.json (empty list = no errors and no warnings)
```

## MCP (assistants)

A read-only [Model Context Protocol](https://modelcontextprotocol.io) server lets Claude, ChatGPT
and other assistants ask about the buses directly. URL: `https://<host>/mcp` (Streamable HTTP,
stateless, no authentication, read-only). Names are Greek, times are Europe/Athens.

| Tool | Returns |
|---|---|
| `search_lines` | lines whose number or name contains the text (accents and case ignored), up to 20 |
| `line_now` | live buses of a line with delay and next stop name |
| `line_timetable` | departures of a line for a day (default today) |
| `find_stops` | stops whose name contains the text, with id and position, up to 15 |
| `stop_arrivals` | OASA's own predictions at a stop (minutes, line, vehicle), cached 30 s |
| `network_day` | statistics of a day: speed, trips run and missed, slowest and most-missed lines |
| `suburban_departures` | next Proastiakos trains at a station, live |
| `metro_departures` | next metro and tram departures at a station (STASY timetable) |

- Claude: Settings > Connectors > Add custom connector, and paste the URL.
- ChatGPT: turn on developer mode, add a connector (MCP server) and paste the URL.

## Errors and limits

| Status | Body | Meaning |
|---|---|---|
| `404` | `{"error":"unknown_line"}` | no such line |
| `404` | `{"error":"unknown_stop"}` | no such stop |
| `404` | `{"error":"not_found"}` | unknown path or feed name |
| `404` | `{"error":"unknown_stop"}` | no such stop in the GTFS |
| `503` | `{"error":"warming_up"}` | the line was not polled yet after a start; retry in a few seconds |
| `503` | `{"error":"feed_unavailable"}` | the GTFS-RT feed is not built yet after a start |
| `503` | `{"error":"busy"}` | stop arrivals: the server's OASA request queue is full and nothing recent is cached; retry shortly |
| `502` | `{"error":"upstream_unavailable"}` | OASA (or the rail source) failed and nothing recent is cached |
| `400` | `{"error":"bad_from"}` etc. | `/v2/plan`: a bad parameter (`bad_from`, `bad_to`, `outside_area`, `bad_after`, `bad_n`) |
| `429` | `{"error":"busy"}` | `/v2/plan`: two plans are running; retry after `Retry-After` (2 s) |
| `429` | (from Cloudflare) | more than about 30 requests per 10 s from one IP; wait at least 10 s |

- `/v1/status` reports `ok: false` when no line poll succeeded in the last 5 minutes. During an
  OASA outage the last good data stays served; check `updated_at`.
- When the static GTFS has expired (`gtfs_expires` in the past), vehicles still appear with
  their line, but `trip_id` is `null` until OASA publishes a new GTFS.
- Please poll the GTFS-RT feeds at most every 15 s and each line at most once per
  `next_update_at`. Faster polling gets the same cached data.

## Licenses

Code: MIT ([`LICENSE`](../LICENSE)). Data: derived from OASA open data (the static GTFS on
[data.gov.gr](https://data.gov.gr/dataset/fb049bb1-aea6-4443-95fa-8b941dd6a057) and the OASA
telematics positions); use it under the terms of those sources.

# Changelog

The public instance (`https://transit.haroldpoi.dev`) runs `main`; a push to `main` deploys it.
Version tags (`v*`) build an image with that version (`ghcr.io/angelospk/athens-transit-rt:<version>`)
for self-hosting. API reference: [docs/API.md](docs/API.md), OpenAPI 3.1:
[docs/openapi/openapi.yaml](docs/openapi/openapi.yaml), viewer with try-it:
<https://angelospk.github.io/staseis-rt/>. Contract for clients: [docs/CONTRACT.md](docs/CONTRACT.md).

## Unreleased

### Live quality (contract rev 23)
- `/v2` bus and trolley Vehicles get `quality`: their level now against the usual service
  (`normal | worse | much_worse | unknown`). On a frequent direction (usual headway at the stop
  ≤ 10 min) it is the gap to the bus ahead at the last stop passed against the usual headway
  (`gap_s`, `usual_headway_s`, `bunched`); on a sparse one the delay against the timetable
  (`early`). `/v2/lines/{id}/vehicles` gets `segments`: the median speed of the buses of any line
  that crossed each stop-to-stop stretch in the last 15 min against its usual speed at this hour
  (`ratio`, `speed`, `usual_speed`, `n`). Athens and Thessaloniki, from the fixes already polled
  (no new OASA request); citybus.gr has neither (#168). `atrt quality-replay` rates an hour of the
  fix history the same way ([#173](https://github.com/angelospk/staseis-rt/issues/173)).

### citybus.gr cities
- `atrt serve --city citybus --cities all|<list>`: one process serves the 28 cities of citybus.gr,
  one path per city (`/heraklion/v2/lines`), with ids namespaced per city (`heraklion:1235`). Go port
  of the citybus-bridge prototype: a paced stop-board rotation per city (about 1 req/s each), no
  position older than 120 s, line health per city (`/{city}/v1/health`). Staging only; see
  [docs/citybus.md](docs/citybus.md) ([#103](https://github.com/angelospk/athens-transit-rt/issues/103)).

- citybus.gr cities: the run of a bus is read from its vehicle code (`20261110_4_0100000_17_20`:
  the 17:20 run of route 4). `/v1/lines/{code}/schedule` marks a run a bus was seen on with
  `seen: true`, and with `live: true` while the bus is there; `seen: false` is no longer sent (it
  made every run look missed). Vehicles get `trip_id`, `trip_label` (v1) and `run` (v2);
  `/v2/runs/{id}` says `running` with its `vehicle`
  ([#141](https://github.com/angelospk/staseis-rt/issues/141)).
- citybus.gr cities: `--history` keeps the sightings of every city in `<state>/history`, in the
  fix-history CSV format ([#142](https://github.com/angelospk/staseis-rt/issues/142)).

### Statistics
- citybus.gr cities: the daily stats gain two TCQSM 4th-edition blocks. `frequency` gives each
  line and direction its mean scheduled headway from 07:00 to 21:00 and its Exhibit 5-6 band, from
  the timetable only (also in the cities with no live buses). `origin_otp` gives the on-time
  performance at the route origin (Exhibit 5-8, 0 to +5 min): the scheduled departure is the time
  in the vehicle code, the actual one the crossing of shape metre 100 between two sightings at most
  60 s apart; early departures are counted apart and are not on time. Per line and per city, with
  the city's band. Athens and Thessaloniki days do not change
  ([#167](https://github.com/angelospk/staseis-rt/issues/167)).
- `headways` in the daily stats follows TCQSM 4th edition (TCRP Research Report 262): `cv_h` divides
  by the mean observed headway (was the median), only directions at a mean headway of 10 min or less
  are listed (was a median of 12 min), and each direction gains `wait_min` (TCQSM Eq. 5-1),
  `off_headway_share` (headways off the mean by more than half) and `median_headway_s`, which
  replaces `ref_headway_median_s` ([#162](https://github.com/angelospk/staseis-rt/issues/162)).
- Thessaloniki daily stats gain the `headways` block. OASTH has no daily schedule, so the passes
  come from the GTFS trips: each trip's mid-route pass from its matched fixes, by line and GTFS
  `direction_id` ([#164](https://github.com/angelospk/staseis-rt/issues/164)).
- The `trips` block of the daily stats plans from OASA's daily schedule when the static GTFS has
  expired: one planned trip per OASA run, paired with the buses that left near its time, instead
  of the trips of the summer timetable. A line without a daily schedule keeps the GTFS plan.
  `trips.schedule_source` and `by_line[].schedule_source` (`oasa_daily`, `gtfs`, `mixed`) tell
  which; `missed_trips` of an `oasa_daily` line are `go HH:MM` / `come HH:MM`. The server now
  keeps every day's daily schedules in `<state>/daily/YYYY-MM-DD.json.gz` (about 20 KB a day,
  no pruning), from the prefetch cache: no new OASA request
  ([#99](https://github.com/angelospk/staseis-rt/issues/99)).
- `oasa_daily` trips: a bus run is now all rows of a vehicle on a direction, not one (vehicle,
  trip id) pair. The matcher moves a bus between trip ids at the stand and on the way, so one run
  was split into short pieces that took scheduled starts as `partial` (11-07: 28% partial against
  6% on the GTFS plan). Replay of 2026-10-27 with the same starts: partial 33.8% to 7.4% (GTFS
  5.7%) ([#131](https://github.com/angelospk/staseis-rt/issues/131)).
- Daily stats files get `start_delay` (contract rev 17): per line, how late the runs of OASA's
  daily schedule left their first stop (runs measured, mean, p90, and a minute histogram so
  several days add up). Measured for buses seen leaving on a clear run (#55); days without it
  are unchanged. The fix history gets two trailing columns for it, `run_start` and
  `start_delay_s` ([#72](https://github.com/angelospk/athens-transit-rt/issues/72)).

### Live API
- An OASA Stop gets `code`, the code on the stop sign (GTFS `stop_code`, e.g. `040054` for
  `oasa:40054`), also in the `stop` of `/v1/stops/{stop_id}`, so a client can show the number a
  rider sees on the pole (contract rev 21).
- The minute stats log adds `unmatched_why`: unmatched buses counted by line and reason, e.g.
  `87P:too_early=1`. Reasons: `no_shape`, `no_trip`, `off_route`, `too_early`, `too_late`,
  `backwards`, `taken` (#116). The API does not change.
- The minute stats log adds an `upstream errors` warning when new upstream errors happened:
  the source (`telematics` or `oseth`), how many, and the text of the last one. Before, only
  the counters showed, without the cause.
- Vehicles of `/v1/lines/{id}`, `/v1/vehicles` and `z` 13 tiles get `along_m` (contract rev 20):
  whole metres along the static shape of their `variant`, and each variant of
  `static/v1/lines/{line}.json` gets `length_m`. Clients can then move a marker along the whole
  shape between fixes instead of stopping at the end of `path_beyond`; they scale `along_m` by
  their own shape length / `length_m` (within ~15 m on every Athens shape). About +5 % gzip on
  `/v1/vehicles`. `path` and `path_beyond` are unchanged
  ([#108](https://github.com/angelospk/athens-transit-rt/issues/108)).
- `/v2/stops/{id}` and `/v1/stops/{stop_id}` get `usual` (contract rev 19): per OASA line and headsign, how often a bus
  usually passes in this hour of this kind of day, and, when the past weeks agree (≥ 3 days,
  waits within 3 min) and today's timetable has a run near it, the usual time of the next pass.
  Learnt nightly from the fix history on disk (one ~1.2 MB file of stop passes per day in
  `<state>/usual`), so no new OASA calls. For the stop card's lines with no live bus
  ([map#24](https://github.com/angelospk/athens-transit-map/issues/24)).
- `GET /v2/plan` (contract rev 16): a trip planner for the app's evening section. From a point
  to a point, the first `n` options after `after`, or with `last=true` the last `n` of the
  service day: walks, one to four rides of any mode (bus, trolley, metro, tram, suburban) and the
  walks between them, from the timetables the backend already holds (no new load on OASA, STASY
  or railway.gov.gr). RAPTOR in process (`internal/plan`), rebuilt for each service day. Every
  leg has a `shape`; every ride's `run` opens with `/v2/runs/{id}`, also after midnight until
  03:00 ([#44](https://github.com/angelospk/athens-transit-rt/issues/44)).
- `GET /v2/municipal` (contract rev 18): the bus routes that 12 municipalities run, with their
  stops where OpenStreetMap has them and a link to each municipality's timetable page. From
  OpenStreetMap (ODbL) through the Overpass API, weekly; no times, no live data, no OASA request
  ([#59](https://github.com/angelospk/athens-transit-rt/issues/59)).
- `/v2` OASA Patterns and bus Vehicles have both directions (contract rev 15): OASA's GTFS has
  no `direction_id`, so every Pattern was 0 (`oasa:040`: 5512 and 5513 both 0). The direction now
  comes from where each Pattern runs, and 0 is the way `/v1/lines/{id}/schedule` calls 0
  ([#45](https://github.com/angelospk/athens-transit-rt/issues/45)).
- Metro M2 headsigns name where the train goes: STASY names every M2 trip after its first stop,
  so `/v1` and `/v2` showed «→ Ελληνικό» on trains leaving Ελληνικό. Such a headsign is now the
  trip's last stop ([#34](https://github.com/angelospk/athens-transit-rt/issues/34)).
- `/v2/alerts`: `stops` now lists the Stops of an OASA notice. Its «ΚΣ 040054» codes are the
  codes on the stop signs (GTFS `stop_code`), not stop ids, so none matched before; «ΚΣ 040054»
  is Stop `oasa:40054` ([#54](https://github.com/angelospk/athens-transit-rt/issues/54)).
- While the GTFS is expired, a bus whose variant has no trip around now on today's weekday is
  matched against the trips of that variant on the nearest weekday that has some. OASA moved
  some variants to other weekdays since the 2026-07-08 feed (309Β runs 5863 on Tuesdays too):
  on 2026-11-03, 30 of 48 unmatched buses had this cause. The local `/metrics` now counts
  vehicles and unmatched vehicles per line ([#51](https://github.com/angelospk/athens-transit-rt/issues/51)).
  The minute `stats` log line also names the lines whose buses all lack a trip
  (`unmatched_lines=054,309Β`), so such a line shows up in the log without anyone spotting it
  on the map ([#51](https://github.com/angelospk/athens-transit-rt/issues/51)).
- Bus delays against OASA's daily schedule while the GTFS is expired (contract rev 14): a bus seen leaving on a
  daily run is late or early against that run, not against the old GTFS trip (`/v1/lines`, stop
  boards, `/v2` Vehicles and Runs). Buses not seen leaving, or whose run is unclear, keep the GTFS
  delay; GTFS-RT and the history keep it too ([#55](https://github.com/angelospk/athens-transit-rt/issues/55)).
- Suburban stops and runs no longer wait for the 743 KB schedule board: once today's board is
  older than 10 min, requests get it at once while a background refresh downloads the new one
  (same for the 24 h station table). Only a cold cache still fetches inside the request
  ([#47](https://github.com/angelospk/athens-transit-rt/issues/47)).
- `GET /v2/alerts` (contract rev 13): service disruptions, polled in the background every 10 min
  from OASA's news feed (buses and trolleys; the Line ids and Stop codes the GTFS knows, and the
  dates of the notice) and from athenstransport.com's strike posts (every mode, `urgent`). An
  Alert is listed for 7 days, or until the notice's last day; a source that fails keeps its last
  Alerts ([#43](https://github.com/angelospk/athens-transit-rt/issues/43)).
- `/v2` Vehicles carry `direction` (contract rev 12): the direction of the Pattern they run, so
  the web can draw each rail line as two tracks and put each train on its own. Metro and tram
  always have it; buses when matched or placed on a trip; suburban trains from their Run, else
  from where they are placed on their corridor (a train standing at a station now also moves
  along its Run's direction). Metro Pattern directions now follow branches: on M3 the trips to the airport
  (ΕΛ. ΒΕΝΙΖΕΛΟΣ) are direction 1 like those to Δ. ΠΛΑΚΕΝΤΙΑΣ; before, some got 0 in both
  directions ([#31](https://github.com/angelospk/athens-transit-rt/issues/31)).
- Fresher bus positions (#30): the whole-city snapshot (`/v1/vehicles`, `/v2/vehicles`, tiles)
  is rebuilt at most every 10 s instead of 30 s (`max-age` at most 10), and the OASA budget is
  6 req/s by default (max 6, was 4 and 5). With that budget, lines nobody is looking at should
  be polled about every 150 s instead of ~300 s. A 403 or 429 from OASA stops all OASA
  requests for 10 minutes, logs `OASA refused requests`, and restarts at the slowest pace.
- Daily statistics start at 2026-10-30: `/v1/stats` lists no earlier day and
  `/v1/stats/days/{date}` answers `404` for one, because their seen/missed trip counts are not
  reliable (no daily OASA schedule, seen-flag bugs). The `stats` workflow writes `index.json`
  and `latest.json` of the `stats` branch the same way; the earlier day files stay there
  ([#32](https://github.com/angelospk/athens-transit-rt/issues/32)).

### Rail
- An expired STASY timetable no longer borrows a holiday: the stand-in day is a typical day of
  the same weekday (trip count within 5% of that weekday's median), not simply the feed's last
  week. Wednesdays after 31/10/2026 read the 28/10 holiday timetable (1172 trips against 1661)
  ([#97](https://github.com/angelospk/athens-transit-rt/issues/97)).

## v0.2.0 (2026-10-30)

### Live API
- `/v2` metro Stops Δημοτικό Θέατρο, Μανιάτικα, Νίκαια and Παλλήνη have proper names (they kept
  STASY's capitals), and the daily live check accepts ΚΑΤ, an acronym
  ([#39](https://github.com/angelospk/athens-transit-rt/issues/39)).
- Docs: CONTRACT.md's `/v2` text now says what the code does (unmatched buses are Arrivals;
  ose.mpass is not used; when `stasy` and `proastiakos` are `stale`). The draft contract is a
  record of what changed; MIGRATION and UI-SPEC carry their real status, and MIGRATION's
  hosting section describes the VPS and its automatic deploys. A test keeps the error codes of
  the code equal to the OpenAPI Error enum and every Source state in the contract.
- Daily `live-check` workflow: `scripts/check-openapi-live.py` now has a case for every path of
  the spec (the `/v1/stops`, `/v1/rail` and `/v2` paths were missing) and checks the /v2 data:
  Lines of every mode, Arrivals at a bus and a metro Stop, no Source down, metro Stop names in
  the name table, and the rail trains' `motion.knots` rules. Every response schema is closed
  for the check, nested objects too (a part of an `allOf` through its whole): a field the spec
  does not list fails. A failure opens an issue, with the log's last lines when the check stops
  before its summary. The
  OpenAPI parameter of `/v1/rail/suburban/trains/{no}` is quoted (`"no"`): YAML 1.1 read it as
  the boolean `false`.
- `/v2` metro and tram Stops have proper names and English names: "Σύνταγμα" / "Syntagma"
  instead of STASY's "ΣΥΝΤΑΓΜΑ" and `null` (all 126 Stops have `name_en`). They come from a
  hand-checked table built from OpenStreetMap (`internal/railgeo/stasy_stops.json`,
  regenerate with `scripts/stasy-stop-names.py`). A Stop new to the STASY feed keeps STASY's
  name, has no English name and is logged at load; a renamed Stop gets a permanent alias there.
  ΣΥΝΔΕΣΗ ΠΡΟΑΣΤΙΑΚΟΥ, the point where M3 airport trains move onto the suburban tracks, is
  merged into Δουκίσσης Πλακεντίας: its id is an alias of that Stop, it is not in `/v2/stops`,
  Patterns and Runs list Πλακεντίας once, and a train stands there from its arrival at the
  station to its departure from the point.
- `/v2/stops/{id}` (OASA stops) lists OASA's own predictions (`getStopArrivals`) as `live`
  Arrivals for the buses the backend has no Arrival of, so a client needs no OASA proxy for them
  (contract rev 11). They are refreshed in the background, at most one OASA call per stop every
  30 s through the shared request queue; a request never waits for OASA
  ([#22](https://github.com/angelospk/athens-transit-rt/issues/22)).
- Seen flags: a daily direction now finds its shape when one name abbreviates a word of the
  other ("ΣΤ. ΚΗΦΙΣΙΑΣ" and "ΣΤΑΘΜΟΣ ΚΗΦΙΣΙΑΣ", "Α. ΠΑΤΗΣΙΑ" and "ΑΝΩ ΠΑΤΗΣΙΑ"). Fixes the
  seen flags of 025, 11, 530, Β15, 022, 406 and 891; no other direction changes
  ([#21](https://github.com/angelospk/athens-transit-rt/issues/21)).
- Buses on a weekly variant route ("every Thursday, street market") are polled again. The OASA
  GTFS puts these variants of Α3, 447, 731, 120, 856 and others on the wrong weekday, so on
  the real day their route code was never asked and the buses of that leg were missing from the
  map and from the seen flags. A route code is now polled when its shapes run at this time of
  day on any weekday; an empty one backs off to every 5 min as before
  ([#20](https://github.com/angelospk/athens-transit-rt/issues/20)).
- Live responses send `Access-Control-Expose-Headers: Date` (contract rev 10), so a browser
  `fetch()` can read the server time and a new tab sees a stopped feed on its first response
  ([#3](https://github.com/angelospk/athens-transit-rt/issues/3)).
- `at_terminus` (contract rev 9) on `/v1/lines/{id}` vehicles and on `/v2` Vehicles: `true`
  when a bus is matched and waiting at the first stop of its trip, so clients can show
  "waiting at the terminus" and leave its departure delay out of the line's running delay
  ([#1](https://github.com/angelospk/athens-transit-rt/issues/1)).
- `docs/fixtures/v2/motion-cases.json`: shared test cases for the `motion.knots` client rule
  (where a marker stands at a time), so the app and the web map test their movers against the
  same cases. The backend checks the knot rules on every train of its test feed through a day.
- `/v2/vehicles/rail` is metro and tram only and opens nothing upstream. Suburban trains moved
  to the new `/v2/vehicles/suburban`, which opens the railway.gov.gr stream: a map showing the
  metro no longer keeps that stream (~80 KB/s) open.
- `/v2` caching: a Source that is `indicative` (expired timetable) or `stale` no longer makes a
  response incomplete, so `/v2/lines`, `/v2/stops` and `/v2/lines/{id}` keep `max-age=3600` with
  an `ETag` while the OASA GTFS is expired, and the `/v2/vehicles` snapshot keeps its full life.
  Only `warming_up` and `down` cut a response to `max-age=5`. `/v2/stops` (about 760 KB raw) is
  built and gzipped once and served with a strong `ETag` while the stops and Source states stay
  the same.
- **`/v2`: one model for every mode** (contract rev 8): buses, trolleys, metro, tram and the
  suburban railway as Line, Stop, Pattern, Vehicle, Arrival and Run, with `<source>:<native>`
  ids, permanent aliases and a `sources` freshness block in every body. Endpoints:
  `/v2/lines`, `/v2/lines/{id}`, `/v2/lines/{id}/vehicles`, `/v2/stops`, `/v2/stops/{id}`,
  `/v2/vehicles`, `/v2/vehicles/tiles/{z}/{x}/{y}`, `/v2/vehicles/rail`, `/v2/runs/{id}`. Bus
  Arrivals add the timetable trips no bus has started; metro and tram trains are placed from the
  timetable on their OSM track and glide between snapshots (`motion.knots`); suburban trains come
  from the railway.gov.gr stream, snapped to their corridor. Rail geometry is now embedded in the
  backend. `/v1` is unchanged. Choices made on the way: `docs/DECISIONS-phase2.md`.
- `/v2` on an expired OASA GTFS: bus timetable Arrivals and `/v2/runs` come from OASA's daily
  schedule of today (cached, never asked per request): the departure plus the stop's offset in
  the nearest GTFS trip of that direction. Run id `oasa:run:<trip_id>@<HHMM>:<date>`. `oasa` is
  `indicative` only for lines without a cached daily schedule.
- `GET /v1/stops/{stop_id}` (contract rev 8): arrival times now come from each bus's position
  and the typical driving speed to the stop (else the timetable's running time) instead of the
  timetable time plus delay; unmatched buses placed on their shape are listed too, and while
  the static GTFS is valid so are the trips that have not left yet (`source: schedule`). New
  fields `source`, `trip_id`, `scheduled`, `stops_away`, `distance_m`; up to 60 min ahead
  (was 2 h), at most 3 per line and variant. Existing fields are unchanged. Stop boards by
  @imertz (#6).
- `GET /v1/stops/{stop_id}/arrivals`: OASA's own predictions for a stop (line, route code,
  vehicle, minutes), soonest first, cached 30 s per stop with `Cache-Control` until that cache
  expires. The OASA calls take at most one request slot in four, so the line polls keep at least
  three quarters of the budget; when OASA or the queue cannot answer, the last good answer up to
  2 minutes old is served. Shares its cache with the MCP tool `stop_arrivals`.

## v0.1.0 (2026-10-28)

First tagged version. Everything below is live on the public instance.

### Live API
- Every active OASA route polled under one global budget (4–5 req/s), with backoff on errors and
  slow answers; background jobs report only their errors to the pacer, so a slow prefetch no longer
  holds the pace down.
- Vehicles matched to scheduled trips, with smoothed `speed`, the `path` ahead, `path_stops` and
  `path_speeds` (contract rev 7).
- `path_speeds` follow today's traffic: recent passes of every line per 100 m piece of road and
  heading, blended into the typical speed of the hour (`segspeed`).
- Vehicle tiles for whole-network maps (`/v1/vehicles/tiles/{z}/{x}/{y}`), GTFS-Realtime
  VehiclePositions and TripUpdates.

### Timetables
- `GET /v1/lines/{id}/schedule`: today from OASA's daily schedule (`getDailySchedule`,
  `source: oasa-daily`), the real timetable even while the static GTFS is expired; other days and
  fallback from the GTFS (`source: gtfs`, `expired` when outside its validity).
- Every line's daily schedule is prefetched once a day through the scheduler's background slots.
- `seen` per run for the daily schedule too (new): the daily schedule has no trip ids, so the
  server watches buses leave the start of their shape (within 300 m, then 600 m along within
  15 min) and gives each start to the run due from 3 min before to 20 min after it. `false` once
  20 min pass without one. Loop lines, late or out-of-order fixes and a feed swap are handled.
  A bus whose route code changes late, at the end of one shape and up to 1.5 km into the next
  (909, Χ96), counts too. Direction names match GTFS headsigns without spaces, dots or hyphens
  and with Latin look-alike letters read as Greek; a loop line takes every shape.

### Rail
- Metro and tram: `GET /v1/rail/stasy/departures?stop=<name>` (STASY GTFS timetable, not live).
- Suburban rail (Proastiakos): `GET /v1/rail/suburban/stations` and
  `/v1/rail/suburban/stations/{id}/departures` (live departures and delays).

### Statistics
- `/v1/stats` and `/v1/stats/days/{date}`: trips planned, run, partial, missed per line; long-term
  views on the web map.

### MCP
- `/mcp`: a read-only MCP server (Streamable HTTP) with tools for lines, buses, stops, timetables,
  statistics, metro and suburban rail. One OASA call per stop, bounded queue, read deadlines.

### Clients
- Web map: <https://bus.haroldpoi.dev> ([athens-transit-map](https://github.com/angelospk/athens-transit-map)).
- Android app: [athens-transit-app](https://github.com/angelospk/athens-transit-app) (signed APKs on
  its releases page).

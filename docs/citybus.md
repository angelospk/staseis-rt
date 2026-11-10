# citybus.gr cities (`atrt serve --city citybus`)

One process serves every city on citybus.gr (e-analysis), one path prefix per city. It is the Go
port of the `citybus-bridge` prototype (Bun/TS, mini PC; report: athens-transit-map
`docs/research/citybus-prototype.md`). Issue: #103.

```sh
atrt serve --city citybus --cities all --listen 127.0.0.1:8103 --state ~/.local/state/atrt-citybus
atrt serve --city citybus --cities pat,her ...   # a subset
```

## API

The same shapes as the bridge (the atrt contract): `/{city}/v1/status`, `/{city}/v1/health`,
`/{city}/v1/vehicles/tiles/{9|13}/{x}/{y}`, `/{city}/v1/lines/{code}`, `/{city}/v1/lines/{code}/schedule`,
`/{city}/v1/stops/{code}`, `/{city}/v2/lines`, `/{city}/v2/stops`, `/{city}/v2/lines/{id}`,
`/{city}/v2/lines/{id}/vehicles`, `/{city}/v2/stops/{id}`, `/{city}/v2/runs/{id}`, `/{city}/v2/alerts`,
`/{city}/static/v1/lines.json`, `/{city}/static/v1/lines/{code}.json`. `/v1/cities` lists the enabled
cities; `/` and `/{city}/debug` show the rotation state.

- `/v2` ids are namespaced per city: `her:1235` (stop), `her:01` (line),
  `her:0932` (pattern), `her:run:<trip>:<YYYY-MM-DD>` (run). The bridge used `oasa:`.
- `/v1` keeps bare codes, as in Athens.
- A stop answer (`/v1/stops/{code}`, `/v2/stops/{id}`) never waits for citybus.gr (#136). With a
  board of the stop in the cache (up to 120 s old) its rows are the live arrivals. With none, the
  live arrivals are estimates from the buses the city already tracks, marked `"estimate": true`
  (`source` stays `live`): the time the bus is due at the stop it was last seen heading for
  (citybus.gr's own minutes) plus or minus the timetable's ride to the asked stop, or distance over
  speed where the timetable has no times; up to 60 min ahead. `"refreshing": true` says a read of
  the stop's own board is in flight: the client asks again in about 2 s, and the board then wins.
  Measured on Kavala (2026-11-10, 381 bus-stop pairs against citybus.gr's own boards): median
  error 22 s, 90 % within 52 s, the worst 95 s.
- `gtfs_version` (`/v1/status`, `static/v1/lines.json`) is the Athens day of the first build with
  the city's current data (a sha256 of it, saved in `static.json`). A daily reload of the same data
  keeps it, so the bot's alerts and the web's stop index (`trips-<city>.json`) stay valid.
- A city's 3-letter code (`her`) is IATA where the city has its own airport, else three letters of
  the name. The slug (`heraklion`) stays for the web's `/{city}` paths and the state dir, so a code
  does not reset the state on disk.
- City codes, in the order of the citybus.gr index: agq, axd, art, ver, vol, dra, her, ioa, kva,
  kso, kat, cfu, kzi, kom, lam, lra, mes, mjt, nao, xan, pat, pto, ser, cha, chq, jkh, sal, jsi
  (`citybus.Cities`).

## Load on citybus.gr

- Static data: about 2 requests per route plus 2 per city, once a day; cached in
  `<state>/<city>/static.json` (same format as the bridge, so its files can be copied).
- Live: one paced queue per city. gap = 60 s / (probes + timetable-only lines + 1), 250-1000 ms;
  up to 3 stop boards in flight. So about 1 req/s per city, whatever the number of users. A user's
  stop board asks once per 15 s at most, first in the city's queue.
- No position older than 120 s (`MaxAge`).
- Line health per city in `<state>/<city>/health.json` (same format as the bridge): a line with no
  live bus for 7 tracked days leaves the rotation and gets one check board per cycle.
- Public since 2026-11-07 (staseis-rt.haroldpoi.dev), without e-analysis consent: Harold's decision. The prototype report (section 6) asked for consent first.

## Rollout on the mini PC (staging, tailnet only)

Historical: the mini PC staging no longer runs (2026-11-07). Production is the `atrt-citybus` container
on the Oracle VPS, behind `staseis-rt.haroldpoi.dev`.

User service `atrt-citybus`, `127.0.0.1:8103`, tailnet `https://minipc.tail11f3d4.ts.net:10449/<city>/...`.
Cities were moved from the bridge one at a time, in index order. Each city ran ≥ 10 min in service
hours before the next was added. Columns: probes, gap, the longest cycle and the oldest position
seen in the window, upstream errors (HTTP other than 200/404, or network), and the process CPU / RSS
with that many cities on.

| # | City | Window (Athens) | Lines | Probes | Gap ms | Max cycle s | Oldest pos. s | Errors | Buses (max) | CPU % | RSS MB | Bridge before: cycle s / oldest s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | agrinio | 2026-11-07 06:15–06:25 | 14 | 73 | 810 | 59 | – | 0 | 0 | 0.1 | 27 | – |
| 2 | alexandroupoli | 2026-11-07 06:26–06:36 | 18 | 39 | 1000 | 39 | 17 | 0 | 1 | 0.3 | 38 | 39 / 4 |
| 3 | arta | 2026-11-07 06:39–06:49 | 9 | 42 | 1000 | 42 | 110 | 0 | 2 | 0.4 | 44 | – |
| 4 | veroia | 2026-11-07 06:51–07:01 | 19 | 19 | 1000 | 19 | 15 | 0 | 2 | 0.5 | 49 | – |
| 5 | volos | 2026-11-07 07:03–07:13 | 12 | 49 | 1000 | 49 | 115 | 0 | 12 | 0.6 | 67 | – |
| 6 | drama | 2026-11-07 07:15–07:25 | 13 | 32 | 1000 | 32 | – | 0 | 0 | 0.7 | 73 | – |
| 7 | heraklion | 2026-11-07 07:26–07:36 | 24 | 55 | 1000 | 55 | 113 | 0 | 14 | 0.9 | 100 | 55 / 104 |
| 8 | ioannina | 2026-11-07 07:43–07:53 | 16 | 64 | 923 | 59 | 115 | 0 | 11 | 1.0 | 121 | – |
| 9 | kavala | 2026-11-07 07:55–08:05 | 19 | 25 | 1000 | 25 | 117 | 0 | 8 | 1.2 | 129 | – |
| 10 | kastoria | 2026-11-07 08:08–08:18 | 18 | 24 | 1000 | 24 | – | 0 | 0 | 0.8 | 137 | – |
| 11 | katerini | 2026-11-07 12:47–12:57 | 14 | 75 | 789 | 59 | 119 | 0 | 9 | 1.3 | 154 | – |
| 12 | corfu | 2026-11-07 13:00–13:10 | 12 | 53 | 1000 | 53 | 113 | 0 | 9 | 1.2 | 160 | – |
| 13 | kozani | 2026-11-07 13:14–13:24 | 16 | 48 | 1000 | 48 | 110 | 0 | 5 | 1.3 | 174 | – |
| 14 | komotini | 2026-11-07 13:26–13:36 | 14 | 24 | 1000 | 24 | 119 | 0 | 5 | 1.5 | 180 | – |
| 15 | lamia | 2026-11-07 13:38–13:48 | 13 | 24 | 1000 | 24 | 119 | 0 | 13 | 1.7 | 189 | – |
| 16 | larissa | 2026-11-07 13:52–14:02 | 20 | 67 | 882 | 59 | 119 | 0 | 20 | 1.5 | 212 | – |
| 17 | mesologgi | 2026-11-07 14:04–14:14 | 6 | 20 | 1000 | 20 | 19 | 0 | 1 | 1.9 | 215 | – |
| 18 | mitilini | 2026-11-07 14:17–14:27 | 8 | 36 | 1000 | 36 | 96 | 0 | 5 | 1.8 | 223 | – |
| 19 | naousa | 2026-11-07 14:28–14:38 | 6 | 16 | 1000 | 18 | – | 0 | 0 | 2.0 | 229 | – |
| 20 | xanthi | 2026-11-07 14:40–14:50 | 15 | 24 | 1000 | 24 | 24 | 0 | 4 | 2.2 | 233 | – |
| 21 | patras | 2026-11-07 14:51–15:01 | 30 | 61 | 967 | 59 | 115 | 0 | 22 | 2.4 | 261 | 59 / 78 |
| 22 | ptolemaida | 2026-11-07 15:03–15:13 | 12 | 38 | 1000 | 38 | 108 | 0 | 1 | 2.2 | 263 | – |
| 23 | serres | 2026-11-07 15:15–15:25 | 20 | 37 | 1000 | 37 | 37 | 0 | 2 | 2.3 | 267 | – |
| 24 | chalkida | 2026-11-07 15:28–15:38 | 13 | 42 | 1000 | 42 | – | 0 | 0 | 2.3 | 276 | – |
| 25 | chania | 2026-11-07 15:40–15:50 | 13 | 37 | 1000 | 37 | 107 | 0 | 11 | 2.4 | 288 | – |
| 26 | chios | 2026-11-07 15:51–16:01 | 2 | 16 | 1000 | 16 | – | 0 | 0 | 2.6 | 294 | – |
| 27 | salamina | 2026-11-07 16:03–16:13 | 8 | 26 | 1000 | 26 | 24 | 0 | 4 | 2.6 | 301 | – |
| 28 | skiathos | 2026-11-07 16:14–16:24 | 1 | 10 | 1000 | 10 | – | 0 | 0 | 2.5 | 314 | – |

`–` in Oldest: no bus was live in the window. `–` in the bridge column: no bridge snapshot was
kept for that city. All 28 cities passed with 0 upstream errors and
0 refused requests (about 600 requests per city per window). With 28 cities on, the process used
2.5 % CPU and 314 MB RSS; RSS grows about 10 MB per city.

- katerini, first window 2026-11-07 08:23–08:33: FAIL with 320 network errors, at the same moment in all cities on, no HTTP error status. Cause: the mini PC network (DNS), not citybus.gr. Measured again after the network came back: PASS.
- Cities with more than ~55 probes (agrinio, ioannina, katerini, larissa, patras) run a cycle of 59 s, at the
  60 s limit; for them the gap drops below 1000 ms (789-967 ms) to keep the cycle under 60 s.
- The Bun bridge (`citybus-bridge`) is stopped and disabled since the last city moved.

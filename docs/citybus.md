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
- Never deploy this publicly before e-analysis agrees (prototype report, section 6).

## Rollout on the mini PC (staging, tailnet only)

User service `atrt-citybus`, `127.0.0.1:8103`, tailnet `https://minipc.tail11f3d4.ts.net:10449/<city>/...`.
Cities were moved from the bridge one at a time, in index order. Each city ran ≥ 10 min in service
hours before the next was added. Columns: probes, gap, the longest cycle and the oldest position
seen in the window, upstream errors (HTTP other than 200/404, or network), and the process CPU / RSS
with that many cities on.

| # | City | Window (Athens) | Lines | Probes | Gap ms | Max cycle s | Oldest pos. s | Errors | Buses (max) | CPU % | RSS MB |
|---|---|---|---|---|---|---|---|---|---|---|---|

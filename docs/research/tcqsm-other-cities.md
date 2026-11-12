# TCQSM measures for the citybus.gr cities: what the data allows

Read-only feasibility study, 2026-11-12. Question: for the 28 citybus.gr cities (the only non-Athens,
non-Thessaloniki cities the backend serves), which TCQSM 4th-edition quality-of-service measures can
we compute with the data we already collect? No code was changed.

## Ranked: build these first

1. **On-time performance at the origin** (TCQSM Exhibit 5-8, origin window 0 to +5 min). Possible in
   21 cities today. The scheduled departure is in the citybus.gr vehicle code itself (`…_17_20`), so
   no matching is needed. On 2026-11-12, 5,302 of 6,252 trip-slot runs (85 %) had their departure
   (shape metre 100) bracketed by two sightings ≤ 60 s apart. `runs.go` already finds the 100 m
   crossing for the "seen" mark, so most of the logic exists.
2. **Frequency bands** (Exhibit 5-6). Possible in 27 cities (Veroia partial), including the 4 timetable-only ones,
   because it needs only the timetable. Each line gets a band from its scheduled daytime headway.
   It is cheap and it gives context to every other measure: 211 of 226 lines (93 %) run less often
   than every 15 min.
3. **Mid-route and terminal on-time performance, plus excess wait on long-headway lines**
   (Exhibit 5-8; TCQSM excess wait = actual minus scheduled departure). This uses the same
   pass-time logic as item 1, applied at stops further down the route. Mid-route passes are
   resolved ≤ 60 s for 91–100 % of runs in every city with trip-slot codes (at 25, 50 and 75 %). It is partial because the GPS lag of the
   stop boards is unknown (see Caveats), and that lag matters more for the −1 min early limit.

Not recommended now: **headway regularity** (cv_h). Only Patras has lines with a daytime mean
headway ≤ 10 min (2 lines). Five cities reach ≥ 6 trips/h in their busiest hour only.

## Method

- Data: the shared fix history on oracle-vm (`atrt-citybus-state` volume, read with `sudo tar`,
  nothing written). History starts 2026-11-11 09:00 Athens time, so the only full service day is
  **Thursday 2026-11-12**, 00:00 to about 19:00 Athens time (the copy was made then). The numbers
  come from one weekday. Treat them as orders of magnitude.
- Timetable: each city's `static.json` (citybus.gr trips; `day` uses the Go weekday, Thursday = 4).
  A trip's departure is its earliest stop time. A route needs ≥ 4 departures in 07:00–21:00 to count:
  routes with 2–3 trips (school runs, duplicate entries 1 min apart) are not a frequency. The
  headway of a line is the smallest daytime mean headway, `(last − first) / (n − 1)`, of its
  routes. "Peak" is the busiest clock hour of any route of the line.
- Observed: one vehicle code = one run (trip slot `YYYYMMDD_route_…_HH_MM`) or one device id (a long
  number, no run). The departure resolution is the gap between the last sighting before shape metre
  100 and the first after it, for a crossing between −10 and +60 min of the scheduled departure in
  the code. Mid-route and terminal use the crossing of 25/50/75/95 % of the furthest metre that
  the run reached.
- Scripts: kept in the session scratchpad (`/tmp/tcq/an.py`, `/tmp/tcq/mid.py`). They are not in
  the repo.

## City × measure

Legend: **F** feasible with the data as collected, **P** partial (see the reason per measure),
**N** no.

Columns: *Codes*: vehicle codes seen on 2026-11-12. *Slot*: codes of the trip-slot form (the run
and its departure are known). *Dep ≤60 s*: slot runs whose origin departure was resolved ≤ 60 s.
*Lines*: lines with a frequency (≥ 4 daytime departures). *≤10 / 11–15 / >15*: lines by daytime
mean scheduled headway (min). *Peak ≤10*: lines with ≥ 6 departures in their busiest hour.
M1 headway regularity, M2 frequency bands, M3o OTP origin, M3m OTP mid-route and terminal,
M4 long-headway excess wait and budgeted wait, M5 link delay against the hour's usual.

| City | Codes | Slot | Dep ≤60 s | Lines | ≤10 / 11–15 / >15 | Peak ≤10 | M1 | M2 | M3o | M3m | M4 | M5 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| agrinio | 89 | 0 | 0 | 0 | 0 / 0 / 0 | 0 | N | N | N | N | N | P |
| alexandroupoli | 218 | 212 | 199/212 | 11 | 0 / 0 / 11 | 0 | N | F | F | P | P | P |
| arta | 153 | 146 | 119/146 | 5 | 0 / 0 / 5 | 0 | N | F | F | P | P | P |
| veroia | 39 | 39 | 28/39 | 0 | 0 / 0 / 0 | 0 | N | P | F | N | P | P |
| volos | 747 | 745 | 674/745 | 11 | 0 / 2 / 9 | 1 | P | F | F | P | P | P |
| drama | 0 | 0 | 0 | 8 | 0 / 0 / 8 | 0 | N | F | N | N | N | N |
| heraklion | 631 | 620 | 529/620 | 18 | 0 / 3 / 15 | 5 | P | F | F | P | P | P |
| ioannina | 475 | 460 | 386/460 | 6 | 0 / 0 / 6 | 1 | P | F | F | P | P | P |
| kavala | 194 | 188 | 154/188 | 8 | 0 / 0 / 8 | 0 | N | F | F | P | P | P |
| kastoria | 0 | 0 | 0 | 7 | 0 / 0 / 7 | 0 | N | F | N | N | N | N |
| katerini | 255 | 235 | 197/235 | 9 | 0 / 0 / 9 | 0 | N | F | F | P | P | P |
| corfu | 316 | 311 | 262/311 | 11 | 0 / 0 / 11 | 0 | N | F | F | P | P | P |
| kozani | 154 | 150 | 104/150 | 10 | 0 / 0 / 10 | 0 | N | F | F | P | P | P |
| komotini | 170 | 166 | 152/166 | 6 | 0 / 0 / 6 | 0 | N | F | F | P | P | P |
| lamia | 273 | 261 | 243/261 | 11 | 0 / 0 / 11 | 0 | N | F | F | P | P | P |
| larissa | 989 | 982 | 864/982 | 15 | 0 / 4 / 11 | 2 | P | F | F | P | P | P |
| mesologgi | 42 | 42 | 29/42 | 1 | 0 / 0 / 1 | 0 | N | F | F | P | P | P |
| mitilini | 147 | 112 | 87/112 | 5 | 0 / 0 / 5 | 0 | N | F | F | P | P | P |
| naousa | 0 | 0 | 0 | 2 | 0 / 0 / 2 | 0 | N | F | N | N | N | N |
| xanthi | 115 | 115 | 92/115 | 8 | 0 / 0 / 8 | 0 | N | F | F | P | P | P |
| patras | 970 | 856 | 700/856 | 26 | 2 / 3 / 21 | 4 | F | F | F | P | P | P |
| ptolemaida | 17 | 1 | 0/1 | 8 | 0 / 0 / 8 | 0 | N | F | N | N | N | N |
| serres | 106 | 104 | 92/104 | 8 | 0 / 0 / 8 | 0 | N | F | F | P | P | P |
| chalkida | 14 | 0 | 0 | 11 | 0 / 0 / 11 | 0 | N | F | N | N | N | N |
| chania | 310 | 288 | 206/288 | 12 | 0 / 0 / 12 | 0 | N | F | F | P | P | P |
| chios | 66 | 66 | 55/66 | 1 | 0 / 0 / 1 | 0 | N | F | F | P | P | P |
| salamina | 157 | 153 | 130/153 | 7 | 0 / 0 / 7 | 0 | N | F | F | P | P | P |
| skiathos | 0 | 0 | 0 | 1 | 0 / 1 / 0 | 0 | N | F | N | N | N | N |

Totals: 226 lines with a frequency: 2 at ≤ 10 min, 13 at 11–15 min, 211 above 15 min. 13 lines in
5 cities reach ≥ 6 trips in their busiest hour.

## Per measure

**M1 Headway regularity** (cv_h, wait_min, off_headway_share; Exhibit 5-9, headways ≤ 10 min).
- F only for Patras (2 lines). P for Heraklion, Ioannina, Larissa, Volos, Patras: only their peak
  hour is ≤ 10 min, so a direction would have about 6–8 headways a day. That is at or below
  `HeadwayMinN = 8`.
- The Athens code (`internal/stats/headway.go`) uses OASA's daily directions (`plan.Daily`). The
  citybus plan has trips only (`stats.SourceTimetable`), so the pass logic needs a citybus
  direction key (the route code would do).
- Sighting gaps (median 16–25 s, p90 19–58 s per city) are fine for headways.

**M2 Frequency bands** (Exhibit 5-6: ≤5, 6–10, 11–15, 16–30, 31–59, 60, >60 min). This uses the
timetable only. The daytime mean understates a peak: a line at 15 min in the peak and 60 min at
midday reads as 16–30. Report the band per clock hour, as TCQSM does, rather than one daily value.
- N **Agrinio**: its `static.json` has no trips at all. Its 89 buses are all device ids.
- P **Veroia**: the timetable has trips for days 0, 1, 2 and 6 only (Sun, Mon, Tue, Sat). It has
  no Wed–Fri trips, so on 2026-11-12 no run matched the timetable (0 of 39). This looks like an
  upstream or loader gap. Check it before you trust Veroia's `trips` stats.
- The 4 timetable-only cities (Drama, Kastoria, Naousa, Skiathos) get F here. They had no rows on
  2026-11-12, which confirms they have no live buses.

**M3o On-time performance, origin** (0 to +5 min). F where trip-slot codes exist. The departure
time is read from the code, not from a match, so Veroia works without its timetable. The median
departure deviation (crossing of metre 100 minus the code's time) was 0.3–2.6 min per city, which
is plausible.
- N: Agrinio, Chalkida and Ptolemaida see only device-id buses (no run, no departure time), and the
  4 timetable-only cities have no buses.
- Device ids matched to a run: 163 of 394 on 2026-11-12 (41 %; 53 % without Agrinio, which has no
  timetable). The note "about 74 %" in docs/citybus.md was not reproduced on this day. Patras has
  114 device ids (81 matched) next to 856 slot codes.
- The 100 m crossing is "left the stand", not "doors closed". The 0 to +5 window has no slack for
  early departures, so a bias of 20–60 s matters (see Caveats).

**M3m On-time performance, mid-route and terminal** (−1 to +5 min mid-route, ≤ +5 at the
terminal). The timetable has a time per stop (for example Kavala route 04: about 43 stops per trip).
Passes at 25/50/75 % of the route were resolved ≤ 60 s for 91–100 % of runs in every city with trip-slot codes (at 25, 50 and 75 %).
It is P because:
- UNVERIFIED: whether citybus.gr's per-stop times are real timepoints or values interpolated by the
  operator. If interpolated, TCQSM says to use timepoints only.
- At 95 % of the route, Xanthi resolves only 70 of 102 runs ≤ 60 s. Buses may drop off the boards
  before the terminal, so terminal OTP needs a per-city check.
- Heraklion has 620 slot codes but only 517 match a timetable trip, and Arta 146 against 101. A
  code time that is no trip of the timetable has no stop times.

**M4 Long-headway excess wait and budgeted wait.** Excess wait (actual − scheduled departure) is
M3 without the bands, so it has the same coverage as M3o/M3m. 211 of 226 lines are > 15 min, so
almost every line is in scope. Budgeted wait (p95 − p2) needs ≥ 250 points per line, stop and
period:
- A busy route has about 50 runs a day (Kavala 04, Larissa). One route-stop-period (for example
  weekday 07–09) gets about 5–8 runs a day, so 250 points need 30–50 weekdays.
- History is kept for 30 days (`--history-days`, `cmd/atrt/serve.go:41`). The 500 MB cap is not
  the limit: about 11 MB a day for all cities. No `history-citybus-*` GitHub Release exists yet
  (`gh release list`, 2026-11-12), so older days are not archived.
- So excess wait is P now. Budgeted wait is N until the releases run, or until each day stores a
  daily summary (quantile sketch or sorted deviations) that later days can pool.

**M5 Stop-to-stop link delay against the usual for the hour** (no TCQSM table). There is plenty of
raw data: 4,400–59,000 moving sighting pairs ≤ 60 s apart per city per day, with `s_m` on the
shape. It is P because:
- The "usual" needs a baseline of several weeks per link and hour, and the same 30-day limit
  applies as for M4.
- `fix_t` is the time the board was read, not the GPS time. Link times are therefore smeared by the
  GPS refresh of each bus (unknown, ≤ 120 s by `MaxAge`). Over a link of 1–3 stops that is a large
  error. Pool links into route segments of several minutes.
- Pass interpolation depends on correct stop metres (`StopM`). The monotone fit (2026-11-10 audit)
  left 10 patterns with bad upstream data.
- Agrinio is P (device ids still give `s_m`). The 4 timetable-only cities and the
  near-empty Chalkida and Ptolemaida are N.

## Caveats

- One weekday only (Thursday 2026-11-12, until about 19:00 Athens time). Line counts come from the
  timetable and do not depend on this. Observed shares can change by day.
- Time base: rows carry the board-read time. Consecutive sightings of one bus were 16–25 s apart
  (median per city, p90 19–58 s). That matches the "8–56 s" from the brain note. A position on a
  board can be up to 120 s old, and the real GPS lag per city is unmeasured. Before OTP is
  published, measure the lag once: compare board times with citybus.gr's own `/v2` position
  timestamps, if it has them (UNVERIFIED), or with a ground-truth ride.
- oracle-vm: 310 MB available RAM, 606 MB swap in use (`free -m`, 2026-11-12). New stats must
  stream the hourly files as `stats.RunFiltered` does. Do not load a day per city into memory.

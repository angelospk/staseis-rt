# Quality of service: status per city

The single place to learn what we measure about bus service quality, what each city gets,
what the user sees, and what comes next. **Keep it current.** Any change to stats, quality
measures or their display updates this file in the same PR or commit (see "How to update").

Last update: 2026-11-13.

## Sources

- **TCQSM 4th edition** (Transit Capacity and Quality of Service Manual, TCRP Research Report
  262, 2026, doi 10.17226/29449). Pre-publication draft, text on the mini PC at
  `~/docs/tcqsm-4th-ed-29449.md`. Chapter 5: Exhibit 5-6 (frequency), 5-8 (on-time), 5-9
  (headway adherence), Eq. 5-1 (wait time).
- A call with a transit-operations researcher (2026-11-12). Advice: measure headway
  regularity on frequent lines and schedule adherence on sparse ones; show delay per
  stop-to-stop segment as a coloured strip; for transfers, list every departure passively and
  never ask which bus the user wants (privacy). He offered to share formulas, and asked for
  inter-stop travel times on line 608 (for his simulator).

## Measures (definitions used in the code)

| Measure | Applies to | Definition | Field (daily stats) |
|---|---|---|---|
| Headway regularity | lines with a mean observed headway ≤ 10 min, 07:00–21:00, at mid-route | `cv_h` = population stddev / mean headway; `wait_min` = 0.5·mean·(1 + cv_h²); `ewt_min` = wait − mean/2; `off_headway_share` = share with \|h − mean\| > mean/2 | `headways` |
| Frequency band | every line with ≥ 4 daytime departures | mean scheduled headway 07:00–21:00, bands ≤5, 5–10, 11–15, 16–30, 31–59, 60, >60 min | `frequency` |
| Origin on-time (OTP) | lines with a timetable and a known scheduled departure | on time = departure 0 to +5 min late; early counts as not on time and is reported apart | `origin_otp` |
| Start delay | Athens (OASA daily schedule) | delay of each run at its first stop: mean, p90, histogram | `start_delay` |
| Trips run | Athens, Thessaloniki, citybus | planned trips and how many ran (coverage ≥ 0.5) | `trips` |

The reference for regularity is the **observed** headway: the scheduled one at mid-route is
not known. So it measures evenness, not whether the promised frequency was kept. Say so when
you cite TCQSM.

## Status per city

| Measure | Athens (OASA) | Thessaloniki (OASTH) | citybus.gr (28 cities) |
|---|---|---|---|
| Headway regularity | **live** (#162, PR #163, from 2026-11-13); shown on the web line card | **live** (#164, PR #166): GTFS trips, ~5 directions qualify | not built: only 2 Patras lines are ≤ 10 min (#165) |
| Frequency band | not built (easy, from the plan) | not built (easy, from GTFS) | **live** (#167, PR #169, merged 2026-11-12): 27 cities |
| Origin OTP | not built; `start_delay` has the data | not checked | **live** (PR #169): 21 cities; shown on the web line card. 2026-11-12: Patras 73.1 %, Larissa 79.0 %, Komotini 92.5 % |
| Mid-route / terminal OTP, excess wait | planned: needs a pass at every stop | not checked | blocked by #168 (board GPS lag unmeasured) |
| Segment delay (now vs usual) | planned; usual speed exists (`internal/segspeed`, 200 m bins × hour × day type) | not checked | not checked |

City facts that change what is possible:

- **Athens**: OASA daily schedule archived per day (`internal/server/dailyarchive.go`); GPS for
  buses. Metro and tram have no tracking, only the timetable.
- **Thessaloniki**: no daily schedule; the plan is the GTFS trips (`direction_id` + `shape`),
  switched on by `stats.Plan.GTFSHeadways`.
- **citybus.gr**: a row carries the time the stop board was read, not the GPS time (positions
  ≤ 120 s old; lag unmeasured, #168). The scheduled departure is in the vehicle code
  (`…_17_20`). About 74 % of device-id buses match a run. Most lines run less often than every
  15 min (211 of 226). Drama, Kastoria, Naousa and Skiathos are timetable-only (no live buses).
  Agrinio has no timetable; Veroia has none for Wednesday to Friday.
- **Open data check (Patras)**: 155 of 728 runs (21 %) looked early on 2026-11-12. That is
  suspect (board lag would make buses look late, not early). Check it before OTP is shown to
  passengers.

## Resources (oracle-vm, measured 2026-11-13)

- RAM is the limit: about 348 MB free. Container `atrt-citybus` uses 80 MB of 400 MB; one stats
  pass over all 28 cities peaks at about 90 MB RSS. Stream hourly files (`stats.RunFiltered`);
  never load a whole day per city.
- Disk is not a limit (174 GB free). citybus history is about 11 MB/day, kept 30 days.
  Stats JSON is 2–12 KB per city-day; the #169 blocks add 0.1–5 KB.
- **Gap**: no `history-citybus-*` GitHub Release exists yet, although
  `deploy/atrt-history-release.service` exists. Without it, history older than 30 days is lost
  and past days cannot be recomputed (`atrt stats --force`).
- Day files are written once after the day ends. A changed formula applies to new days only,
  unless old days are rewritten with `--force` (production; ask Harold).

## What the user sees (web `athens-transit-map`, app `athens-transit-app`)

Today (web, design v3 of 2026-11-13, mockup `athens-transit-map/docs/mockups/quality-v3.html`;
`src/lib/quality.ts`, `src/lib/ui/QualityPill.svelte`, `src/lib/ui/LineCard.svelte`): the line card
shows one neutral pill under its header, only when there is data (no grey pill). It reads yesterday's
day file of the city; until that file is written (mornings, until about 13:00) it reads the day before
and says «Προχθές» instead of «Χθες». Neither file: no pill.

- The level is a 3-bar meter, not a dot: 3 green bars = normal, 2 yellow (`--late1`), 1 red
  (`--late3`); the unlit bars are muted. The words keep `--fg` / `--muted`, for 4.5:1 in light and dark.
- Frequent direction (a `headways` entry for the line and the shown direction; the card's direction
  is matched to the stats `dir` through the line's timetable headsign): `cv_h` ≤ 0.39 three bars,
  ≤ 0.74 two, above one. Text «Χθες · αναμονή ~{wait_min}′ (κανονικά ~{mean/2}′)», to the half
  minute; no brackets at three bars.
- Sparse citybus.gr line (`origin_otp.by_line`, per line, ≥ 5 measured runs): ≥ 80 % three bars,
  70–79 % two, < 70 % one. Text «Χθες · N στα 10 δρομολόγια στην ώρα».
- A tap opens a panel under the pill. Sparse: the day's split at the origin (νωρίτερα / στην ώρα /
  πάνω από 5′ αργά, with counts), the on-time share of the last 7 days with a dashed 80 % line, and
  the note «Στην ώρα: 0 έως 5′ μετά το πρόγραμμα. Όποιο φεύγει νωρίτερα μετράει εκτός, γιατί όποιος
  έρχεται στην ώρα το χάνει.» Frequent: the wait of the last 7 days with a dashed line at the
  even-service wait (mean/2), and the passes and mean headway of the day. The 7 day files load
  only when the panel opens; a day without data is an empty slot.
- Athens and Thessaloniki sparse lines: no pill (no OTP yet). Day files before 2026-11-12 have no
  `wait_min`: no pill for that day.
- The direction control under the pill is always the two-segment control; the header shows the
  line chip and the line name.

The app shows none of it yet. The web stats view (`src/lib/ui/StatsView.svelte`) shows trips run, buses on the road, network speed and slowest/fastest lines.

Agreed design (Harold, 2026-11-13; mockup `athens-transit-map/docs/mockups/quality-v2.html`, refined
by v3 above: neutral copy, level meter instead of words like «Αραιά»;
the first, rejected one is `quality-v1-rejected.html`: too much information, unclear words):

- **One scale, three levels, the same meaning everywhere**: green = normal, yellow = a little
  worse, red = much worse; grey = unknown. Always compared with the **usual** for the same
  hour and day type.
- Plain words, no percentages, no A–F letters, no jargon ("φάλαγγα", "εκτός ρυθμού" rejected).
  Wording: line «Κανονικά / Αραιά (αργεί) / Πολύ αραιά (αργεί πολύ)», vehicle «Στην ώρα του /
  Λίγο πίσω / Πολύ πίσω», segment «Κίνηση κανονική / Πιο αργά από συνήθως / Πολύ αργά».
- **Line** (badge on the line card): starts with **yesterday**, from the daily stats. Frequent
  line: the average wait vs the usual one. Sparse line: share of runs on time.
- **Vehicle** (marker colour, replaces the delay colours over time): frequent line = gap to the
  bus ahead vs the usual headway; sparse line = delay vs the timetable.
- **Segment** (between two stops, on the line's stop strip): speed now vs the usual speed.

Goal (later): show **today against the typical range**, like "usually busy" charts: the usual
band for this hour and where today is now.

## Roadmap

1. ~~Merge PR #169 (citybus frequency + origin OTP)~~ done 2026-11-12; check the Patras early share.
2. ~~Web: line badge "χθες" on the line card from the daily stats (Athens, Thessaloniki,
   citybus)~~ done 2026-11-13 (web 1f0fd68).
3. Backend: live "now vs usual" per segment (`segspeed` baseline) and per vehicle (gap to the bus
   ahead on frequent lines).
4. Web and app: vehicle and segment colours on the new scale; retire the delay colours.
5. Backend: a pass at every stop → mid-route/terminal OTP, segment delay history, the 608
   inter-stop travel times for the researcher.
6. "Today vs typical" ranges.

Side work: #168 (citybus board GPS lag), the missing citybus history release.

## How to update

- Change the "Last update" date.
- Update the status table cell of each city and measure you touched, with the issue or PR.
- New measure: add a row to "Measures" with its exact formula and field name.
- New user-facing decision: add it to "What the user sees", with the date and the mockup path.
- Keep it short: this file says what is true now; history lives in git and the issues.

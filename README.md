# athens-transit-rt daily statistics

One file per day (Europe/Athens) of the OASA buses and trolleys seen by
[athens-transit-rt](https://github.com/angelospk/athens-transit-rt): GPS fixes,
vehicles and travel speed per hour and per line, and how many scheduled trips ran
(`trips`). Updated daily by the `stats` workflow.

- `v1/latest.json`: the newest day
- `v1/index.json`: all days from 2026-10-30 (earlier files in `v1/days` are kept but
  not listed: their seen/missed trip counts are not reliable)
- `v1/days/YYYY-MM-DD.json`: one day
- `v1/thessaloniki/`: the same files for the OASTH buses of Thessaloniki, from
  2026-11-07 (no `start_delay`: it comes from OASA's daily schedule)

Speed (km/h) = `dist_m / time_s * 3.6`. Fields and method: `docs/API.md` on `main`.
Raw URL: `https://raw.githubusercontent.com/angelospk/staseis-rt/stats/v1/latest.json`

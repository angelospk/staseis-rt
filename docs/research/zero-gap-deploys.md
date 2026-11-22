# Deploys without a visible gap (oracle-vm)

Research for #185 (2026-11-13). Question: why does each backend merge leave Thessaloniki and the
28 citybus.gr cities without live buses for 20–30 minutes, and what is the smallest change that
removes that gap?

## Summary

- **The gap is not start-up work. It is memory.** One start of `atrt-citybus` needs about
  7 CPU-seconds (on one Ryzen 7840HS core; the VM is slower), but all 28 cities read and build
  their `static.json` at the same time. This takes **330 MB at peak, against 120 MB one city at a
  time.** oracle-vm (952 MB RAM) has no room for 330 MB: it was already 750 MB into swap on
  2026-11-13. The start pushes the host into swap thrash (kswapd, 15–50 MB/s of reads, 70 % CPU
  steal), and every tenant stalls, Thessaloniki included.
- **Thessaloniki starts in under 4 s** (78 MB, static GTFS + timetable). It goes dark because the
  same merge restarts both containers in the same second, on the same thrashing host.
- **The 13:40 UTC outage was not a restart.** At 14:00 both containers had been up for 2 h 39 min
  (since 11:22). The #182 deploy hung in `docker pull` from 12:40 to 13:24 (ghcr i/o timeout),
  then tried again. The host was thrashing without a deploy: both atrt processes held 25 MB in RAM
  and 70–95 MB in swap. Other load at that time: an SSH session with a 418 MB peak and 7 CPU-min
  (ended 14:00), `moodboard` restarted 7 times in 3 h (peaks up to 209 MB), `a1-catcher` every
  ~4 min (3–4 CPU-s each).
- **Recommendation (smallest combination):** (1) load the cities one at a time (prototype on
  branch `research/zero-gap-deploys`, 30 lines); (2) one deploy lock for both containers, a time
  limit on `docker pull`, and no deploy while the host is in swap thrash; (3) the web keeps the
  last buses, marked stale, while the API does not answer. A bigger host (A1) removes the cause;
  the rest only stops a deploy from making it worse.

## 1. Where the start-up time goes

Measured on the mini PC, on a copy of the production state (static files, health, runs; no
history), taken 2026-11-13 14:00 UTC. The binary is `main` at 7beb007.

### Disk part of a citybus start (read, unmarshal, `buildModel`, `newLiveCity`)

Test `TestMeasureStartup` (prototype branch). 28 cities, 172.7 MB of `static.json` (Patras
19.7 MB, Heraklion 16.5 MB).

| Mode | CPU | Wall | Peak heap in use | Heap after GC |
|---|---|---|---|---|
| One city at a time | 1 core | 6.8 s | 118 MB | 41 MB |
| All at once (today) | 1 core | 6.6 s | 389 MB | 55 MB |
| All at once (today) | 16 cores | 1.1 s | 434 MB | 58 MB |

The CPU work is the same; only the peak changes. `buildModel` is 70 % of it (4.6 of 6.5 s), JSON
the rest. Heraklion is the slowest city (0.5 s).

### A whole start in a container (prod flags, 0.25 CPU, live polling of citybus.gr)

| Variant | Memory limit | Peak (cgroup) | Swap | 28 cities ready | Lines with a live bus |
|---|---|---|---|---|---|
| Today | 400 MB | 329 MB | 0 | ~34 s | 47 at 34 s |
| One at a time | 400 MB | 121 MB | 0 | 30 s | 64 at 30 s, 76 at 60 s |
| Today | 250 MB | 251 MB (limit) | 80 MB | ~50 s | — |
| One at a time | 250 MB | 131 MB (197 MB later) | 0 | 30 s | 66 at 30 s, 83 at 60 s |

250 MB is about what oracle-vm really has free for this container. With today's code the start
fills it and swaps; one city at a time fits with room to spare. After a city is ready, its buses
show within one board rotation (about 60 s; Patras `last_cycle.took` 58 s).

On oracle-vm the same CPU work takes several times longer (E2.1.Micro: a 1/8 OCPU baseline, 70 %
steal seen today). That is minutes, not 27 minutes; the rest is the swap thrash.

### Two more costs at every start

- **Stale static files.** At the copy, 10 of 28 `static.json` were over 24 h old, because the
  daily reloads fail on the slow host (`daily reload failed ... context deadline exceeded`, 9
  cities between 13:02 and 13:58). A stale city installs its file, then also reloads from
  citybus.gr at once, holding the old file and the new rows in memory. Today all stale cities do
  this together.
- **Thessaloniki** loads in under 4 s (78 MB, 0.25 CPU). Live buses come from polling at
  `--rps 3` over about 164 lines: one round is about 55 s (computed from the rate, not measured:
  no OASTH salt on the mini PC).

## 2. Options

Steady state on oracle-vm (2026-11-13 14:00): `atrt-citybus` about 150 MB (34 MB in RAM, 118 MB
in swap), `atrt-thess` about 106 MB (32 + 74), dockerd + containerd 153 MB + 132 MB swap. The host
has no free RAM before a deploy starts.

| Option | Removes the gap? | RAM / CPU on oracle-vm | Effort |
|---|---|---|---|
| (a) Blue-green, second container until healthy | Yes, if it fits | +120 MB (one at a time) to +330 MB (today) for minutes, plus 2× upstream polling (citybus.gr, OASTH) during the overlap | Proxy flip in Caddy + scripts |
| (b) Warm start: save boards, vehicles, models on stop | Saves ~60 s per city after the fix below | Disk only | Medium: a snapshot format to keep in sync with the live state |
| (c) Cities one at a time | Removes the thrash (330 → 120 MB peak) | Same CPU, spread over ~30 s locally | 30 lines, prototype done |
| (d) Deploy rules: one lock, pull limit, no deploy under thrash | Removes the stacking (two starts + pull + other load) | None | Script change on the VM |
| (e) Web keeps the last buses (stale) on 5xx / timeout | Hides the last 1–3 min for open tabs; no help for a new visitor | None on the VM | Web change |
| (f) Bigger host | Yes, and the daily reload failures too | A1 (Always Free, 4 OCPU / 24 GB) if `a1-catcher` gets one; the mini PC is off at times | Move + DNS |

**(a) does not fit.** A second citybus instance needs 120 MB at best on a host that is already in
swap; today's code needs 330 MB. It also doubles the load on citybus.gr and OASTH for the overlap.
Worth it only on a bigger host.

**(b) is not worth it yet.** After (c), the cities are ready in about 30 s locally (a few minutes
on the VM) and buses fill in one rotation. A snapshot of the live state saves that one rotation
and adds a format to keep in step with `liveCity`.

**(e)** On a 503 `warming_up` the web poller does not call `onData`
(`athens-transit-map/src/lib/poller.ts:150`), so an open tab keeps its data. Not checked: whether
the map layer then hides buses by age, and what a timeout (not a 503) does. A new visitor sees an
empty map either way.

## 3. Recommendation

1. **(c) Cities one at a time.** Branch `research/zero-gap-deploys`, `internal/citybus/city.go`:
   `diskGate` (one city at a time reads and builds its file) and `fetchGate` (one city at a time
   reloads from citybus.gr). Every city first installs its file from disk, so all 28 are ready
   before any network reload. Existing tests pass. Needs a PR and a review (backend rule).
2. **(d) Deploy rules** (on oracle-vm; needs Άγγελος's yes):
   - both scripts take one lock (`flock /run/lock/atrt-deploy.lock`): Thessaloniki first, citybus
     after Thessaloniki answers, never both at once;
   - `timeout 300 docker pull` (today a pull hung for 44 min);
   - skip the run (the timer tries again in 10 min) when `MemAvailable` < 250 MB or the 1-min
     load average > 4;
   - the citybus health check waits until `/v1/cities` reports all cities `ready` (up to 5 min),
     not only an answer.
3. **(e) Web**: keep the last buses, marked stale, on 5xx and on timeouts. Separate issue in
   `athens-transit-map`.
4. **(f)** Keep `a1-catcher` running. With A1, add (a) blue-green: then it fits.

Expected result of 1 + 2: a deploy costs about 120 MB of transient memory instead of 330 MB, and
never runs on a thrashing host or together with the other container. Buses come back in about
one minute per city plus the start (estimate for the VM: 2–5 min, not measured there). Item 3
hides that minute for open tabs.

What this does not fix: the host is over-committed without any deploy (750 MB in swap, the daily
static reloads time out). Only less load (other tenants) or a bigger host fixes that.

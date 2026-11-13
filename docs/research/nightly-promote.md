# Nightly promotion: one gate for every change (backend and web)

Design and plan, 2026-11-14. Status: **draft, nothing built.** Implementation starts slice by
slice, only after Άγγελος approves this plan. Builds on PR #187 (oracle-vm deploy scripts with
lock, pull limit, busy-host skip) and PR #186 (`docs/research/zero-gap-deploys.md`).

## Decisions (Άγγελος, final)

1. Every change goes through the **same gate**. A hotfix is a fast lane through the gate
   (explicit SHA, shorter smoke set), never a bypass.
2. Backend and web both promote **only at night** (~01:30 Athens), except the hotfix lane.
3. The «what changed» modal opens by itself (no badge). Mockup first, build only after approval.
4. After each promotion, a daytime check verifies that **all cities** work. Assertions live in
   code. A cheap model may only write the human report. Failure: a request to the origin T3
   thread. Success: silent (log only).

Cut (do not design): `flags.json`, Shorebird, staged store rollout, nightly screenshots, a
nightly branch in `preview.yml`.

## Today (verified 2026-11-13/14)

| Where | What happens on a push to `main` | Evidence |
|---|---|---|
| rt, GitHub | `image.yml` builds and pushes `:latest` + `:sha-<short>`; it does not wait for `ci.yml` | `.github/workflows/image.yml:2-5,28` |
| MVPS (`ssh atrt-vps`, Athens, Alexandros's host) | `atrt-deploy.timer` every 10 min runs `/usr/local/bin/atrt-deploy --auto`: pull `:latest`, recreate on a new image id, wait up to 180 s for `"ok":true`, else roll back to `atrt:previous`; skips 03:00-06:00 Athens | read on the host, read-only (user `angelos`, no docker access) |
| oracle-vm | `atrt-thess-deploy.timer`, `atrt-citybus-deploy.timer` every 10 min: pull `:latest`, recreate | PR #187 `deploy/oracle/*` |
| web, GitHub | `deploy.yml` builds and deploys GitHub Pages on every push; also at 12:30 and 16:30 UTC; `thess-stops.yml:45` and `citybus-stops.yml:52` push data to `main` and dispatch `deploy.yml` | `athens-transit-map/.github/workflows/` |

So every merge goes live within minutes, and nothing checks the running result except
`transit-health` (hourly, oracle-vm user timer, ntfy) and `live-check.yml` (daily 08:23 UTC,
OpenAPI shape).

Problems found in the current scripts (each one has a slice below):

- **B1** `:previous` means "the image that ran before this run", not "the last healthy image".
  oracle-vm never rolls back (`atrt-*-deploy:20/29` only tag it): a failed image keeps running and
  becomes `:previous` at the next deploy. MVPS rolls back to it; if that also fails, nothing
  healthy is kept.
- **B2** MVPS: after a rollback the running image is the old one, so the next timer run sees the
  failed image as "new" and deploys it again: up to ~3 min of outage every 10 min until a new
  image appears.
- **B3** `atrt-thess-deploy:30` treats any HTTP 200 as healthy; it must require `"ok":true`.
- **B4** `atrt-deploy-common.sh:23` (busy host) exits 0 without deploying; nobody can tell
  "skipped" from "deployed".
- **B5** Same image + running container skips readiness (`atrt-*-deploy:14/17`); fine for a
  timer, but the promotion must confirm from outside, not trust the timer.
- **W1** A rerun of an old web deploy rebuilds from today's API and stats (`scripts/seo.ts:439`):
  rollback must redeploy the last good **artifact**, not rebuild a SHA.
- **W2** A non-Athens city whose API fails is dropped from the SEO pages with a green build
  (`scripts/seo.ts:392-396`).
- **W3** `getJSON` returns `{status: 0}` on a network error with no console error
  (`src/lib/api.ts:12-23`): a smoke test that only watches the console passes on a dead API.
- **W4** `preview.yml` builds with other env (no `VITE_TURNSTILE_SITE_KEY`, Thessaloniki base,
  `SEO_LEGACY=0`, `SEO_CITYBUS_STOPS=0`): the gate must test the **production** build.
- `public/sw.js` has no fetch handler: no stale-bundle risk after a promotion.

## Architecture

```
                         push to main (any time)
                                 |
        rt: ci.yml + image.yml   |   web: ci (test, check) only, no deploy
        image ghcr ...@sha256:D  |
                                 v
 01:37 Athens  rt promote.yml (gate) ------------------------------------+
   candidate = main HEAD (nightly) | input SHA (hotfix, rollback)        |
   checks: ci.yml green on SHA, image.yml green, digest D resolved,       |
           runner smoke of D (boot, /v1/status JSON, /v2/lines)          |
   pass -> write promoted.json {sha, digest D, contract_rev, lane, at}   |  fail -> nothing
           to release "promote" (asset)                                  |  promoted; ntfy
                                 |                                       |
        hosts' 10-min timers read promoted.json (not :latest)            |
   MVPS atrt (Athens)   oracle-vm atrt-thess -> then atrt-citybus        |
   run ...@sha256:D; health fail -> last-good digest; mark D failed      |
                                 |                                       |
 02:00-03:00  rt promote.yml confirm job: public /v1/status rev == sha   |
              on all 3 backends (bounded 60 min; timeout = ntfy only)    |
                                 |
 02:17 Athens  web promote.yml (gate)
   needs: production contract_rev >= web's MIN_CONTRACT_REV on all hosts
   build production artifact once; seo city report not worse than last;
   browser smoke on the built dist against production API;
   pass -> deploy that artifact to Pages; verify Pages serves version.json sha
           and its assets; record promoted-web.json {sha, run_id, artifact}
                                 |
 07:40 + 13:10 Athens  web promote-check.yml (all cities, deterministic)
   fail -> ntfy + issue (label promote-check) -> mini PC relay -> origin thread
   pass -> log only
```

Order is always **backend first, web after the backend is confirmed**, for promotion and for
rollback.

## The gate

One workflow per repo: `promote.yml`, `workflow_dispatch` + `schedule`. Lanes:

| Lane | Trigger | Candidate | Smoke set |
|---|---|---|---|
| `nightly` | schedule | `main` HEAD | full |
| `hotfix` | dispatch, `sha` + `reason` required | that SHA; must be an ancestor of `origin/main` (merged, reviewed) | short |
| `rollback` | dispatch, `sha` required | a SHA that was promoted before (in the history of `promoted.json`) | short; skips "newer than current" |
| `data` (web only) | dispatch by data jobs, schedule 12:37/16:37 UTC | the **promoted** web SHA + fresh data files | full web smoke |

Nothing to promote (candidate == promoted) exits 0 silently.

### Backend gate checks (rt `promote.yml`)

1. Candidate SHA resolved; for `hotfix`: `git merge-base --is-ancestor $SHA origin/main`.
2. `ci.yml` conclusion `success` on that exact SHA (`gh run list --workflow ci.yml --commit
   <full sha>`; full SHA required). `image.yml` `success` on the same SHA.
3. Digest: `docker buildx imagetools inspect ghcr.io/angelospk/athens-transit-rt:sha-<short>
   --format '{{json .Manifest.Digest}}'`. The tag `sha-<short>` is immutable in practice (one
   build per SHA); the digest is what hosts run.
4. Runner smoke (both lanes): `docker run ...@D serve --city athens --listen 127.0.0.1:8095
   --state /tmp/s --telematics-url http://127.0.0.1:9` (OASA unreachable on purpose: GitHub runners
   are outside Greece). Within 120 s: `/v1/status` answers JSON, `/v2/lines` has ≥ 100 lines (the
   static snapshot comes from the GitHub release). Full set adds the same for
   `--city thessaloniki` (`/v2/lines` ≥ 30) and `--city citybus --cities agq`. **UNVERIFIED:**
   that `serve` answers `/v2/lines` without OASA or the OASTH salt; slice 2 starts with this spike.
   If it does not, the smoke is "process stays up 60 s and `/v1/status` answers JSON".
5. `contract_rev` of the candidate (from `/v1/status`, slice 1) is ≥ the promoted one, unless
   lane `rollback`.
6. Pass: write `promoted.json` (below) to the release `promote` (asset replaced; history kept as
   `promoted-history.jsonl`, last 60 lines). Fail: write nothing, ntfy (secret `NTFY_TOPIC` in
   repo secrets, never in a file), job red.

`promoted.json`:

```json
{"sha": "<40 hex>", "digest": "sha256:<64 hex>", "contract_rev": 23, "lane": "nightly",
 "promoted_at": "2026-11-15T01:37:12+02:00", "run_url": "https://github.com/..."}
```

Hosts read it from
`https://github.com/angelospk/staseis-rt/releases/download/promote/promoted.json` (public repo).
They run `ghcr.io/angelospk/athens-transit-rt@<digest>`, never a mutable tag.

### Web gate checks (web `promote.yml`)

1. Candidate SHA (lane rules above). `bun run test`, `bun run check` green on it (same steps as
   today's `deploy.yml`).
2. **Contract**: `src/lib/config.ts` exports `MIN_CONTRACT_REV`. Every production backend
   (`transit.haroldpoi.dev`, `transit-thess.haroldpoi.dev`, `staseis-rt.haroldpoi.dev/<code>`)
   reports `contract_rev ≥ MIN_CONTRACT_REV`. Else skip tonight (fail closed, ntfy). This also
   makes "web after backend" automatic.
3. **Production build once**, production env (the `deploy.yml` env: `VITE_FEEDBACK_URL`,
   `VITE_TURNSTILE_SITE_KEY`; no preview overrides). It writes `dist/version.json`
   `{sha, built_at, min_contract_rev}`. This exact `dist` is what gets deployed (W4).
4. **City report** (W2): `seo.ts` already returns `{cities: [{id, stops, lines, skipped?}]}`; the
   build writes it to `dist/seo-report.json`. Fail when a city has `skipped`, or when a city's
   `lines` is < 90 % of the last promoted report (downloaded from the `promote-web` release).
5. **Browser smoke** on `dist` served locally (`vite preview`) against the production API, with
   Playwright (new dev dependency, pinned). Per page, bounded retry (3 tries, 10 s apart):
   - no `pageerror`, no failed request to an own asset;
   - map canvas visible and non-empty size;
   - LineCard opens for a fixed line and shows its stop list (static data, works at night);
   - a request to the city API returned HTTP 200 (W3: watch network responses, not the console).
   No live buses required (01:30 is outside service). Full set: Athens, Thessaloniki, 3 citybus
   cities (patras, heraklion, volos). Short set (hotfix): Athens + Thessaloniki.
6. Pass: upload the artifact with `actions/upload-pages-artifact`, deploy with
   `actions/deploy-pages`, then verify (bounded 10 min): production `/version.json` has the new
   SHA and every `<script>`/`<link>` asset in production `index.html` answers 200. Timeout = ntfy,
   **no automatic rollback**. Record `promoted-web.json` `{sha, run_id, artifact_id, at}` and the
   seo report in the release `promote-web`.
7. Keep the artifact: an extra `actions/upload-artifact` copy of `dist`, `retention-days: 90`
   (the maximum for a public repo), so rollback has it (W1).

### Data jobs (web)

`thess-stops.yml`, `citybus-stops.yml` and the 12:30/16:30 UTC stats rebuilds today deploy
`main` HEAD, which may hold unpromoted code. New rule: they dispatch `promote.yml` lane `data`:
checkout the **promoted** SHA, copy only the data files from `main`
(`public/trips-*.json`, the Thessaloniki stop index; an allowlist of paths), run the full web
gate, deploy. Code never moves outside the nightly or hotfix lanes. (See decision D3.)

## Promote and rollback per host

### MVPS (Athens, container `atrt`, Alexandros's host)

`atrt-deploy --auto` changes (slice 5, Alexandros installs):

- Source: `promoted.json` digest instead of `:latest`. Pull `REPO@digest` (timeout 300 s).
- **Window expiry**: in `--auto` mode deploy only when `now - promoted_at < 6 h`, or lane is
  `hotfix`/`rollback` (< 6 h as well). An older promotion that was never deployed (host down all
  night) is skipped and logged; it waits for the next promotion or a manual run. Keeps the
  03:00-06:00 skip.
- `last-good` (B1): file `/var/lib/atrt-deploy/atrt.last-good` holds a digest, written only after
  the new container passed the health check **and** is still `"ok":true` 10 min later (next timer
  run checks it). Rollback runs `last-good`, never "the image before this run".
- `failed` memo (B2): a digest that failed its health check goes to
  `/var/lib/atrt-deploy/atrt.failed`; `--auto` never retries it. Only a new promotion or
  `atrt-deploy --force` deploys again.
- Status file (B4): `/var/lib/atrt-deploy/atrt.json`
  `{state: deployed|up_to_date|skipped|failed|rolled_back, digest, reason, at}`.
- The **only automatic rollback** in the whole design is this one: health check fails right after
  a start → run `last-good`. Conditions: `last-good` exists, differs from the failed digest, and
  the state directory is compatible (rule below). If `last-good` also fails: stop, status
  `failed`, alert. Never a second rollback step.

### oracle-vm (`atrt-thess`, `atrt-citybus`)

Same changes in `deploy/oracle/atrt-deploy-common.sh` (PR #187 files, after #187 merges):
`promoted.json` source, digest pull, window expiry, `last-good`, `failed` memo, status file per
container, rollback to `last-good` (new: oracle-vm has none today). Plus B3: thess health needs
`"ok":true`. Order stays: thess first, citybus after thess runs the promoted digest. A busy host
(`host_check`) writes `skipped` with the reason; the window expiry bounds how late it may deploy.

### State compatibility

A rollback runs the old binary on state the new one wrote (`atrt-state`, `atrt-thess-state`,
`atrt-citybus-state`: GTFS snapshot, history CSV, `static.json`). Rule for every backend PR:
**a change to a state file format must stay readable by the previous release** (add fields, do not
rename or re-encode), or the PR carries the label `state-format` and its promotion uses lane
`hotfix` with a written rollback note. The PR template gets one checkbox. Not automated in v1.

### Web

- Promote: the web gate above. Production = GitHub Pages, one deploy per promotion.
- Rollback: `rollback.yml` (dispatch, input `run_id`, default = the previous entry of
  `promoted-web.json` history): download the kept `dist` artifact of that run, re-upload with
  `upload-pages-artifact`, `deploy-pages`, verify `version.json`. No rebuild (W1). **UNVERIFIED:**
  `actions/download-artifact` with `run-id` + `github-token` across runs, then
  `upload-pages-artifact`; slice 7c checks it on a dry run.
- Backend rollback first, web after. The web must keep client fallbacks for the previous
  backend (optional fields per contract rev, `docs/openapi/openapi.yaml`); `MIN_CONTRACT_REV`
  makes the gate refuse a web that needs a newer backend than production runs.

### Manual rollback (a person decides)

1. Backend: dispatch rt `promote.yml` lane `rollback`, `sha=<last good>`. Hosts take it within
   10 min (`failed` memo does not block an older digest; window expiry counts from the new
   `promoted_at`).
2. Web, only if needed: dispatch web `rollback.yml`.

## Daytime check of all cities

`promote-check.yml` in the web repo (next to `ops/transit-health`, whose rules it reuses).
Schedule `40 7 * * *` and `10 13 * * *` with `timezone: "Europe/Athens"`, plus dispatch. It runs
only when a promotion happened in the last 24 h (else exit 0: transit-health covers quiet days).

Deterministic assertions (TypeScript, `ops/promote-check/`, unit-tested with fixtures):

| Check | Per | Pass rule |
|---|---|---|
| status | Athens, Thessaloniki, each citybus city (`/<code>/v1/status`) | HTTP 200, JSON, `ok: true` |
| rev | each backend | `rev == promoted.json sha` (all three hosts on the promoted build) |
| contract | each backend | `contract_rev == promoted.json contract_rev` |
| live | each city | in service hours (`config.json` `serviceHours`): `minVehicles[city]` vehicles (transit-health `evaluateCity`), else `lines_active ≥ 1` |
| freshness | each city | transit-health `feed_old`, `source` codes absent |
| web | bus.haroldpoi.dev | `/version.json` sha == `promoted-web.json` sha; `/`, `/thessaloniki/`, one citybus page answer 200 |

Output: `report.json` (artifact) with one line per failed check
`{city, check, expected, got}`. Exit 1 on any failure.

- Pass: log only (job summary). No ntfy, no message.
- Fail: ntfy (one message, short), one issue in `athens-transit-rt` with label `promote-check`
  (comment on the open one if it exists, as `live-check.sh` does), job red.
- 13:10 re-check: if 07:40 failed and 13:10 passes, comment «recovered» and close the issue.
  A city that fails only at 07:40 is often an upstream that starts late; the re-check separates
  that from a broken build.

"Service is running" is approximated by fixed service hours and per-city minimums (as
transit-health does today), not by the GTFS calendar. Holidays with no service can give a false
`live` failure; the 13:10 re-check and the triage in the origin thread handle it.

### Failure routing to the origin thread

GitHub cannot reach T3 on the mini PC. A small relay does:

- `promote-relay` (mini PC, systemd **user** timer, 07:55 and 13:25 Athens): `gh run list -R
  angelospk/athens-transit-map --workflow promote-check.yml -L 1 --json
  databaseId,conclusion,createdAt`; if `failure` and the run id is not in its state file, download
  `report.json` and run `t3 thread send <ORIGIN_THREAD_ID> -` with a fixed template: what failed,
  the run link, the issue link, «investigate; do not deploy without Άγγελος». The thread id lives
  in `~/.config/promote-relay/env`, not in the repo.
- Mini PC off: ntfy and the issue still fire; the relay sends the missed failure at its next run
  (state file keeps the last relayed run id).

### Does the cheap model add value?

Honest answer: **not in v1.** On success there is nothing to write. On failure, `report.json`
already names the city, the check, expected and got, and the message goes to an agent thread
(Opus) that investigates anyway. A DeepSeek Flash step would add a second, less reliable
summary, a provider dependency and a privacy question (free models may log prompts), for a
report a template writes. Proposal: no model in v1. Revisit after 2-3 weeks of real failures if
the origin thread spends many turns on triage that a cheap model could pre-sort (for example
"citybus.gr down for every city" vs "one city broken"); even that pre-sort is a few lines of code.

## Timezone and DST

- Verified (GitHub docs, `workflow-syntax#onschedule` and `events-that-trigger-workflows#schedule`,
  read 2026-11-14): `schedule` takes `timezone: "<IANA name>"` next to `cron`; default UTC. During
  a spring-forward gap a skipped time advances to the next valid time. The docs say nothing about
  the repeated hour at fall-back.
- All crons here use `timezone: "Europe/Athens"` and avoid 03:00-04:00 local (the DST hour).
- Avoid minute 0 (GitHub delays or drops runs at the top of the hour under load): 01:37, 02:17,
  07:40, 13:10.
- Belt and braces: each scheduled job first checks `TZ=Europe/Athens date +%H` against its
  expected hour range and exits 0 with a log line otherwise (protects against a parser that
  ignores `timezone`). actionlint may not know the key yet: pin a version that does, or skip that
  rule.
- Collisions checked: MVPS skips 03:00-06:00; history release timer 05:30/11:30 Athens; stats
  03:40/07:40 UTC; thess-stops 03:30 UTC; citybus-stops 03:45 UTC. Promotion at 01:37 + 10 min
  host pickup + up to 10 min citybus readiness ends by ~02:00. No overlap.

## Hotfix lane

Same workflow, same checks; differences only:

- dispatch with explicit `sha` (merged to `main`, CI green) and a `reason` (one line, kept in
  `promoted.json` history);
- short smoke set (backend: Athens only; web: Athens + Thessaloniki);
- runs any time; hosts deploy it at their next timer run (≤ 10 min);
- after it, the next nightly promotes `main` HEAD as usual (it contains the hotfix).

What it never skips: CI on that SHA, the image digest, the runner smoke, the contract check, the
post-deploy verification, the host health check and `last-good` rollback.

## Slices

Each slice: one PR (rt: issue + PR + review, Άγγελος merges), small, reversible. Workers get the
slice text, the files and the verify command. Tests first.

| # | Repo | What | Files | Tests / verify | Risk | Needs yes |
|---|---|---|---|---|---|---|
| 1 | rt | `/v1/status` adds `rev` (git SHA) and `contract_rev`; citybus `/<code>/v1/status` too | `Dockerfile` (`ARG REVISION`, `-ldflags -X main.revision=`; `.git` is in `.dockerignore`), `image.yml` (`build-args: REVISION=${{ github.sha }}`), status handlers in `internal/server`, `internal/citybus/api.go`, `docs/openapi/openapi.yaml`, `CHANGELOG.md` | handler tests: fields present; `docs_drift_test` green; after deploy `curl /v1/status` shows the SHA | low (additive field, contract rev 24) | merge (normal backend rule) |
| 2 | rt | `promote.yml` **dry run**: candidate, CI/image checks, digest, runner smoke, writes `promoted.json` as a workflow artifact only | `.github/workflows/promote.yml`, `scripts/promote/` (candidate + checks as a script with tests) | script tests with fake `gh` output; 2 nights of dry runs, artifact content correct | low (hosts ignore it) | merge |
| 3 | rt | publish `promoted.json` to release `promote`; confirm job polls public `rev` (bounded 60 min, ntfy on timeout); `NTFY_TOPIC` secret | `promote.yml`, repo secret | dispatch once by hand; release asset updated; confirm job stays off (input flag) until slices 4/5 switch the hosts | low | merge + adding the secret |
| 4 | rt | oracle-vm scripts read `promoted.json`: digest pull, window expiry, `last-good`, `failed` memo, status file, rollback, thess `"ok":true` | `deploy/oracle/atrt-deploy-common.sh`, `atrt-thess-deploy`, `atrt-citybus-deploy` (after #187) | shell tests with fake `docker`/`curl` on `PATH` (cases: new digest ok; health fail → last-good; failed memo blocks retry; expired window skips; busy host → `skipped` status); then `DRY_RUN=1` on the host for one night | medium (live host) | merge + install on oracle-vm |
| 5 | rt | same changes for MVPS `atrt-deploy` as a patch in `deploy/mvps/atrt-deploy` (copy of the host script, now versioned) | `deploy/mvps/atrt-deploy` | same shell tests; Alexandros reviews and installs | medium | Alexandros installs |
| 6 | web | `deploy.yml` → build + test on push, **no deploy**; `promote.yml` (lanes nightly/hotfix/rollback) in **dry run**: contract check, production build, `version.json`, `seo-report.json`, city report compare, Playwright smoke, artifact kept 90 d | `.github/workflows/promote.yml`, `scripts/seo.ts` (write report), `scripts/promote/` (+tests), `e2e/smoke.spec.ts`, `package.json`, `src/lib/config.ts` (`MIN_CONTRACT_REV`) | unit tests for report compare and contract rule; dry run 2 nights green; a forced-bad run (city removed) goes red | low while dry run (deploy.yml still deploys) | — (web rule: main after tests) but the switch in 7a needs yes |
| 7a | web | switch: `deploy.yml` stops deploying; `promote.yml` deploys + verifies `version.json` and assets; records `promote-web` | `deploy.yml`, `promote.yml` | first live night watched; `/version.json` = promoted SHA | medium (production) | **yes, Άγγελος** |
| 7b | web | data lane: `thess-stops.yml`, `citybus-stops.yml`, stats rebuild dispatch `promote.yml` lane `data` (promoted SHA + data allowlist) | 3 workflows | dispatch once; deployed `version.json` sha unchanged, data file updated | low | yes (depends on D3) |
| 7c | web | `rollback.yml` (kept artifact → Pages) | `.github/workflows/rollback.yml` | dry run to a Cloudflare preview branch first, then one real rollback to the current SHA (no-op content) | medium | yes |
| 8 | web | `promote-check.yml` + `ops/promote-check/` (reuses `ops/transit-health/rules.ts`) | new files, `tests/promote-check.test.ts` | fixture tests per check; `--simulate` a failure; one dispatched run green | low | — |
| 9 | mini PC | `promote-relay` user timer + script | `~/scripts/promote-relay/` (not in a repo with the thread id) | simulated failure run id → message lands in the origin thread once, not twice | low | yes (installs a timer) |
| 10 | web | «what changed» modal: **mockup only** (impeccable skill), then approval, then build | mockup in `docs/`; later `src/lib/releases.ts`, a modal component | — | — | **yes on the mockup** |

### Slice 10 detail (later)

- `promote.yml` (web) publishes a GitHub Release `web-<date>` after a successful promotion, with
  Greek notes taken from `Release-note:` trailers of the commits since the last promoted SHA. No
  trailer, no release, no modal.
- `releases.ts`: skip `prerelease` (today it includes them); the build embeds the release tag
  (`VITE_RELEASE_TAG`); the modal shows releases newer than `localStorage` `seen` and not newer
  than the bundled tag; it writes `seen` when shown. Optional image-before/image-after in the
  body.

## Decisions needed from Άγγελος

- **D1** Approve this plan and the slice order (1 → 9 now; 10 later).
- **D2** Who may dispatch `hotfix`/`rollback`: repo writers (you, Alexandros), or only you?
- **D3** Data and stats pages: lane `data` during the day (fresh data, promoted code, same gate),
  or nightly only (stats pages up to a day older)? Recommended: lane `data`.
- **D4** Slice 5 touches Alexandros's host: ask him, or wait until he moves Athens deploys himself?
- **D5** No cheap model in v1 (reason above): agree?

## Not verified

- `serve` with OASA unreachable answers `/v2/lines` from the release snapshot (slice 2 spike).
- `download-artifact` across runs + `upload-pages-artifact` for rollback (slice 7c).
- Behaviour of `timezone` at the fall-back repeated hour (our crons avoid it).
- The MVPS script was read as user `angelos` without docker access: running containers and
  `atrt:previous` on that host were not inspected.
- Real runtime of a citybus start on oracle-vm after #187 (estimate 2-5 min, PR #186).

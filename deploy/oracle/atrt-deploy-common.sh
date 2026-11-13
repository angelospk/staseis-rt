# Shared by atrt-thess-deploy and atrt-citybus-deploy on oracle-vm (#185). Sourced, not run.
# One deploy at a time on the host, never on a busy host, and a time limit on the pull.
# Env: ATRT_MIN_AVAIL_MB (250), ATRT_MAX_LOAD (4), DRY_RUN=1 (log what a deploy would do; no container is touched,
# but the pull still updates the local image).
IMG=ghcr.io/angelospk/athens-transit-rt:latest
LOCK=/run/lock/atrt-deploy.lock

# deploy_lock: wait up to 10 min for the other deploy (its health check can take that long).
deploy_lock() {
  exec 9>"$LOCK"
  if ! flock -w 600 9; then
    echo "skip: another atrt deploy holds $LOCK"
    exit 0
  fi
}

# host_check: a start on a host in swap thrash made the maps go dark for 20-30 min (2026-11-13).
# Skip; the timer tries again in 10 min.
host_check() {
  local avail load
  avail=$(awk '/^MemAvailable:/{print int($2/1024)}' /proc/meminfo)
  load=$(cut -d' ' -f1 /proc/loadavg)
  if [ "$avail" -lt "${ATRT_MIN_AVAIL_MB:-250}" ] || awk -v l="$load" -v m="${ATRT_MAX_LOAD:-4}" 'BEGIN{exit !(l>m)}'; then
    echo "skip: host busy (MemAvailable ${avail} MB, load1 $load); the next timer run tries again"
    exit 0
  fi
}

# pull: a pull hung for 44 min on 2026-11-13 (ghcr i/o timeout).
pull() {
  if ! timeout 300 docker pull -q "$IMG" >/dev/null; then
    echo "docker pull failed or took over 300 s" >&2
    exit 1
  fi
}

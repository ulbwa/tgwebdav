#!/usr/bin/env bash
#
# scripts/benchmark.sh — reproducible, WAL-aware performance benchmark for tgwebdav.
#
# WHAT IT MEASURES
#   tgwebdav stores file BYTES in Telegram and only METADATA in Postgres. A PUT is
#   write-ahead-buffered: it lands in Postgres (`wal_chunks`), the file is durable
#   and readable immediately, and a background packer asynchronously uploads ~19 MiB
#   blobs to Telegram. So the latency a client sees on a PUT is NOT the time-to-durable
#   in Telegram — they have very different ceilings. This script measures each phase
#   distinctly, mirroring the methodology behind the table in the README:
#     - PUT -> WAL ingest  (local, Postgres-bound)         MB/s
#     - buffered read       (immediate GET, served from WAL) MB/s
#     - pack -> Telegram    (poll DB until node `stored`)   time / MB/s
#     - cold read           (cleared cache, GET, sha256 verified) MB/s
#     - warm read           (GET again, disk LRU cache)     MB/s
#     - blob count per file
#     - small-file request rate (parallel PUT->WAL & warm GET) with p50/p95/p99
#     - storage efficiency (Telegram bytes vs logical bytes; Postgres metadata/MiB)
#   It prints clean Markdown tables to stdout. The README already holds a sample table;
#   this script lets ANYONE reproduce the methodology with their own bots/channels.
#
# REQUIREMENTS
#   - docker   (a throwaway postgres:17-alpine container is spun up and torn down)
#   - go       (the benchmark builds the tgwebdav binary from this checkout)
#   - curl     (drives the WebDAV and Management API endpoints)
#   - bash (3.2+, the macOS default works) plus standard tools: awk, sort, head,
#     wc, shasum/sha256sum (the script also uses curl, openssl/od for randomness)
#   You do NOT need a local Postgres or psql client — psql runs inside the container
#   via `docker exec`.
#
#   Telegram is the storage backend, so the pack->Telegram and cold-read numbers are
#   bounded by the Telegram Bot API, not by this machine. You must bring YOUR OWN
#   Telegram bots and channels (see below). Nothing here ships secrets.
#
# REQUIRED ENV (provided by the runner at runtime — never committed, never printed)
#   BENCH_BOT_TOKENS    Comma-separated Telegram Bot API tokens, e.g.
#                         BENCH_BOT_TOKENS="123:AAA...,456:BBB..."
#   BENCH_CHANNEL_IDS   Comma-separated BARE channel ids (no -100 prefix), e.g.
#                         BENCH_CHANNEL_IDS="1234567890,9876543210"
#   Every bot MUST be an administrator of every channel you list (the server checks
#   membership when a channel is added, and marks it `available` only if a bot can
#   use it). Bots/channels are added at RUNTIME through the Management API — the
#   server no longer reads them from the environment.
#
#   Convenience: if BENCH_BOT_TOKENS / BENCH_CHANNEL_IDS are unset, the script will
#   try to source TGWEBDAV_BOT_TOKENS / TGWEBDAV_CHANNEL_IDS from a `.env` in the
#   repo root. These values are loaded into shell variables only and are NEVER echoed.
#
# OPTIONAL ENV (sane defaults)
#   BENCH_PG_PORT       Host port for the throwaway Postgres        (default 5599)
#   BENCH_WEBDAV_PORT   WebDAV listen port                          (default 18080)
#   BENCH_MGMT_PORT     Management API listen port                  (default 18081)
#   BENCH_FILE_SIZES    Space-separated sizes for the throughput run
#                                                (default "4K 256K 1M 5M 20M 100M")
#   BENCH_CONCURRENCY   Space-separated concurrency levels for the small-file run
#                                                                  (default "8 16 32")
#   BENCH_SMALL_OPS     Operations per concurrency level            (default 400)
#   BENCH_PG_IMAGE      Postgres image                       (default postgres:17-alpine)
#   BENCH_PACK_TIMEOUT  Seconds to wait for a file to pack to Telegram (default 600)
#   BENCH_WAL_IDLE_MS   WAL idle-flush timeout used during the run, in ms (default 2000;
#                       production default is 60000 — lowered here so the small-file
#                       pack phase measures upload speed, not the idle wait)
#
# EXAMPLE (full matrix)
#   BENCH_BOT_TOKENS="123:AAA,456:BBB" BENCH_CHANNEL_IDS="111,222" ./scripts/benchmark.sh
#
# EXAMPLE (quick smoke run — reduced matrix)
#   BENCH_FILE_SIZES="4K 1M 20M" BENCH_CONCURRENCY="8" BENCH_SMALL_OPS=50 \
#     BENCH_PG_PORT=5599 ./scripts/benchmark.sh
#
# SECURITY
#   No tokens or keys are written to disk or printed. The Postgres password, the
#   generated SECRET_KEY and the admin password live in shell variables for the
#   lifetime of the process and are wiped at exit. The cleanup trap kills the server,
#   stops/removes the container, and deletes all temp files — even on Ctrl-C or error.

set -euo pipefail

# ----------------------------------------------------------------------------------
# Constants and configuration
# ----------------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PG_CONTAINER="tgwebdav-bench-pg"
PG_USER="tgwebdav"
PG_DB="tgwebdav"

# A throwaway, in-process-only Postgres password (never the runner's real one).
PG_PASSWORD="bench$(date +%s)$$"

BENCH_PG_PORT="${BENCH_PG_PORT:-5599}"
BENCH_WEBDAV_PORT="${BENCH_WEBDAV_PORT:-18080}"
BENCH_MGMT_PORT="${BENCH_MGMT_PORT:-18081}"
BENCH_FILE_SIZES="${BENCH_FILE_SIZES:-4K 256K 1M 5M 20M 100M}"
BENCH_CONCURRENCY="${BENCH_CONCURRENCY:-8 16 32}"
BENCH_SMALL_OPS="${BENCH_SMALL_OPS:-400}"
BENCH_PG_IMAGE="${BENCH_PG_IMAGE:-postgres:17-alpine}"
BENCH_PACK_TIMEOUT="${BENCH_PACK_TIMEOUT:-600}"
BENCH_WAL_IDLE_MS="${BENCH_WAL_IDLE_MS:-2000}"

# Generated, kept only in shell vars — never echoed.
SECRET_KEY="$(openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
ADMIN_USER="admin"
ADMIN_PASS="$(openssl rand -hex 16 2>/dev/null || head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"

WEBDAV_URL="http://127.0.0.1:${BENCH_WEBDAV_PORT}"
MGMT_URL="http://127.0.0.1:${BENCH_MGMT_PORT}"
PG_DSN="postgres://${PG_USER}:${PG_PASSWORD}@127.0.0.1:${BENCH_PG_PORT}/${PG_DB}?sslmode=disable"

# Populated during setup.
WORK_DIR=""
BIN=""
CACHE_DIR=""
DATA_DIR=""
SERVER_LOG=""
SERVER_PID=""

# Choose a sha256 tool (macOS ships shasum, Linux ships sha256sum).
if command -v sha256sum >/dev/null 2>&1; then
  SHA256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  SHA256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  echo "ERROR: need sha256sum or shasum" >&2
  exit 1
fi

# ----------------------------------------------------------------------------------
# Logging helpers (everything goes to stderr so stdout stays clean Markdown).
# ----------------------------------------------------------------------------------

log()     { printf '%s\n' "$*" >&2; }
section() { printf '\n=== %s ===\n' "$*" >&2; }
die()     { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# ----------------------------------------------------------------------------------
# Cleanup — runs on EXIT (normal, error, or Ctrl-C). Leaves no leaked process,
# container or temp file, and wipes the secrets held in memory.
# ----------------------------------------------------------------------------------

cleanup() {
  local code=$?
  trap - EXIT INT TERM
  section "Cleanup"

  if [[ -n "${SERVER_PID}" ]] && kill -0 "${SERVER_PID}" 2>/dev/null; then
    log "stopping server (pid ${SERVER_PID})"
    kill "${SERVER_PID}" 2>/dev/null || true
    for ((_i=0;_i<20;_i++)); do
      kill -0 "${SERVER_PID}" 2>/dev/null || break
      sleep 0.25
    done
    kill -9 "${SERVER_PID}" 2>/dev/null || true
  fi

  if docker ps -aq -f "name=^${PG_CONTAINER}$" | grep -q .; then
    log "removing Postgres container ${PG_CONTAINER}"
    docker rm -f "${PG_CONTAINER}" >/dev/null 2>&1 || true
  fi

  if [[ -n "${WORK_DIR}" && -d "${WORK_DIR}" ]]; then
    log "removing temp dir"
    rm -rf "${WORK_DIR}" 2>/dev/null || true
  fi

  # Wipe secrets from the environment of any child we might spawn during cleanup.
  unset SECRET_KEY ADMIN_PASS PG_PASSWORD BENCH_BOT_TOKENS BENCH_CHANNEL_IDS \
        TGWEBDAV_BOT_TOKENS TGWEBDAV_CHANNEL_IDS PG_DSN 2>/dev/null || true

  log "done (exit ${code})"
  exit "${code}"
}
trap cleanup EXIT INT TERM

# ----------------------------------------------------------------------------------
# Requirements & config validation
# ----------------------------------------------------------------------------------

check_requirements() {
  section "Checking requirements"
  local missing=0
  for tool in docker go curl awk sort dd; do
    if ! command -v "${tool}" >/dev/null 2>&1; then
      log "MISSING: ${tool}"
      missing=1
    fi
  done
  [[ ${missing} -eq 0 ]] || die "install the missing tools above and re-run"

  if ! docker info >/dev/null 2>&1; then
    die "the Docker daemon is not running (start Docker Desktop / dockerd)"
  fi
  log "docker, go, curl present; docker daemon up"
}

# Read TGWEBDAV_BOT_TOKENS / TGWEBDAV_CHANNEL_IDS from .env WITHOUT printing them,
# only when the BENCH_* equivalents are unset. Values go into shell vars only.
source_secrets_from_dotenv() {
  local env_file="${REPO_ROOT}/.env"
  [[ -f "${env_file}" ]] || return 0
  if [[ -z "${BENCH_BOT_TOKENS:-}" ]]; then
    local v
    v="$(grep -E '^[[:space:]]*(export[[:space:]]+)?TGWEBDAV_BOT_TOKENS=' "${env_file}" | tail -n1 | sed -E 's/^[^=]*=//; s/^"(.*)"$/\1/; s/^'"'"'(.*)'"'"'$/\1/')" || true
    [[ -n "${v}" ]] && BENCH_BOT_TOKENS="${v}"
  fi
  if [[ -z "${BENCH_CHANNEL_IDS:-}" ]]; then
    local c
    c="$(grep -E '^[[:space:]]*(export[[:space:]]+)?TGWEBDAV_CHANNEL_IDS=' "${env_file}" | tail -n1 | sed -E 's/^[^=]*=//; s/^"(.*)"$/\1/; s/^'"'"'(.*)'"'"'$/\1/')" || true
    [[ -n "${c}" ]] && BENCH_CHANNEL_IDS="${c}"
  fi
}

validate_secrets() {
  source_secrets_from_dotenv
  if [[ -z "${BENCH_BOT_TOKENS:-}" || -z "${BENCH_CHANNEL_IDS:-}" ]]; then
    cat >&2 <<'EOF'

ERROR: this benchmark needs YOUR OWN Telegram bots and channels.

Set both of these (comma-separated), then re-run:
  BENCH_BOT_TOKENS    one or more Telegram Bot API tokens
  BENCH_CHANNEL_IDS   one or more BARE channel ids (no -100 prefix)

  export BENCH_BOT_TOKENS="123456:AAA...,789012:BBB..."
  export BENCH_CHANNEL_IDS="1234567890,9876543210"

Every bot MUST be an administrator of every channel you list — the server
checks membership when a channel is added and only marks it `available`
if at least one bot can use it. Nothing is printed or stored; tokens stay
in process memory and are wiped at exit.

(Convenience: TGWEBDAV_BOT_TOKENS / TGWEBDAV_CHANNEL_IDS in the repo .env
are sourced automatically if the BENCH_* vars are unset.)
EOF
    exit 1
  fi
  # Count without revealing values.
  local nbots nchans
  nbots="$(printf '%s' "${BENCH_BOT_TOKENS}" | awk -F',' '{print NF}')"
  nchans="$(printf '%s' "${BENCH_CHANNEL_IDS}" | awk -F',' '{print NF}')"
  log "configured ${nbots} bot token(s) and ${nchans} channel id(s) [values redacted]"
  BOT_COUNT="${nbots}"
}

# ----------------------------------------------------------------------------------
# Small helpers
# ----------------------------------------------------------------------------------

# Pick a nanosecond-clock strategy ONCE (the per-op loop calls now_ns hot, so we
# must not fork python/perl every call). GNU date supports %N; macOS /bin/date does
# not, so fall back to python3/perl as the clock, or whole-second granularity.
_TIME_MODE="sec"
if [[ "$(date +%N 2>/dev/null)" =~ ^[0-9]+$ ]]; then
  _TIME_MODE="date"
elif command -v python3 >/dev/null 2>&1; then
  _TIME_MODE="python"
elif command -v perl >/dev/null 2>&1; then
  _TIME_MODE="perl"
fi

# now_ns: high-resolution timestamp in nanoseconds.
now_ns() {
  case "${_TIME_MODE}" in
    date)   date +%s%N ;;
    python) python3 -c 'import time;print(int(time.time()*1e9))' ;;
    perl)   perl -MTime::HiRes=time -e 'printf("%d", time()*1e9)' ;;
    *)      echo $(( $(date +%s) * 1000000000 )) ;;
  esac
}

# bytes_for SIZE: convert a human size (4K, 256K, 1M, 100M) to a byte count.
bytes_for() {
  local s="$1" num unit
  num="${s%[KkMmGg]}"
  unit="${s: -1}"
  case "${unit}" in
    K|k) echo $(( num * 1024 )) ;;
    M|m) echo $(( num * 1024 * 1024 )) ;;
    G|g) echo $(( num * 1024 * 1024 * 1024 )) ;;
    *)   echo "${s}" ;;
  esac
}

# gen_file PATH BYTES : write exactly BYTES of random data to PATH. Uses `head -c`,
#   which yields an exact byte count on both macOS and Linux (no `truncate` needed).
gen_file() {
  local path="$1" bytes="$2"
  head -c "${bytes}" /dev/urandom > "${path}"
}

# mbps BYTES SECONDS -> MB/s (decimal MB = 1e6), printed with 2 decimals.
mbps() {
  awk -v b="$1" -v s="$2" 'BEGIN{ if (s<=0){print "n/a"} else {printf "%.2f", (b/1000000.0)/s} }'
}

# psql_scalar SQL -> single value from inside the container.
psql_scalar() {
  docker exec -e PGPASSWORD="${PG_PASSWORD}" "${PG_CONTAINER}" \
    psql -qtAX -U "${PG_USER}" -d "${PG_DB}" -c "$1" 2>/dev/null | tr -d '[:space:]'
}

# curl_admin ARGS... : curl with admin Basic auth.
curl_admin() { curl -fsS -u "${ADMIN_USER}:${ADMIN_PASS}" "$@"; }
# curl_dav ARGS... : curl against the WebDAV server with admin Basic auth.
curl_dav()   { curl -fsS -u "${ADMIN_USER}:${ADMIN_PASS}" "$@"; }

# percentile FILE P : pick the P-th percentile (0-100) of a newline list of numbers.
percentile() {
  local file="$1" p="$2"
  sort -n "${file}" | awk -v p="${p}" '
    {a[NR]=$1}
    END{
      if (NR==0){print "n/a"; exit}
      idx=int((p/100.0)*NR + 0.5); if (idx<1) idx=1; if (idx>NR) idx=NR;
      print a[idx]
    }'
}

# ----------------------------------------------------------------------------------
# Setup: Postgres container, build, migrate, start server, add bots/channels
# ----------------------------------------------------------------------------------

setup_workdir() {
  WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/tgwebdav-bench.XXXXXX")"
  BIN="${WORK_DIR}/tgwebdav"
  CACHE_DIR="${WORK_DIR}/cache"
  DATA_DIR="${WORK_DIR}/data"
  SERVER_LOG="${WORK_DIR}/server.log"
  mkdir -p "${CACHE_DIR}" "${DATA_DIR}"
}

start_postgres() {
  section "Starting throwaway Postgres (${BENCH_PG_IMAGE}) on :${BENCH_PG_PORT}"
  # Remove a stale container from a previous aborted run.
  docker rm -f "${PG_CONTAINER}" >/dev/null 2>&1 || true
  docker run -d --name "${PG_CONTAINER}" \
    -e POSTGRES_USER="${PG_USER}" \
    -e POSTGRES_PASSWORD="${PG_PASSWORD}" \
    -e POSTGRES_DB="${PG_DB}" \
    -p "127.0.0.1:${BENCH_PG_PORT}:5432" \
    "${BENCH_PG_IMAGE}" \
    postgres -c max_connections=200 >/dev/null

  log "waiting for pg_isready..."
  local ok=0
  for ((_i=0;_i<60;_i++)); do
    if docker exec "${PG_CONTAINER}" pg_isready -U "${PG_USER}" -d "${PG_DB}" >/dev/null 2>&1; then
      ok=1; break
    fi
    sleep 0.5
  done
  [[ ${ok} -eq 1 ]] || die "Postgres did not become ready"
  log "Postgres ready"
}

build_binary() {
  section "Building tgwebdav binary"
  ( cd "${REPO_ROOT}" && go build -o "${BIN}" ./cmd/tgwebdav )
  log "built ${BIN##*/}"
}

run_migrations() {
  section "Running migrations"
  "${BIN}" migrate --dsn "${PG_DSN}" >>"${SERVER_LOG}" 2>&1
  log "migrations applied"
}

# start_server: launch the server in the background and wait for /healthz.
start_server() {
  "${BIN}" server \
    --dsn "${PG_DSN}" \
    --webdav-addr "127.0.0.1:${BENCH_WEBDAV_PORT}" \
    --mgmt-addr "127.0.0.1:${BENCH_MGMT_PORT}" \
    --cache-dir "${CACHE_DIR}" \
    --cache-size "1GiB" \
    --first-user "${ADMIN_USER}:${ADMIN_PASS}" \
    --secret-key "${SECRET_KEY}" \
    --log-level "warn" \
    >>"${SERVER_LOG}" 2>&1 &
  SERVER_PID=$!

  local ok=0
  for ((_i=0;_i<120;_i++)); do
    if ! kill -0 "${SERVER_PID}" 2>/dev/null; then
      log "--- server log tail ---"; tail -n 50 "${SERVER_LOG}" >&2 || true
      die "server exited during startup (see log above)"
    fi
    if curl -fsS "${MGMT_URL}/healthz" >/dev/null 2>&1; then ok=1; break; fi
    sleep 0.5
  done
  [[ ${ok} -eq 1 ]] || { tail -n 50 "${SERVER_LOG}" >&2 || true; die "server /healthz never came up"; }
}

# port_in_use PORT : true if something is listening on 127.0.0.1:PORT. Uses curl as
#   a dependency-free probe (a connection refused means free; any response/timeout
#   while connecting means in use). lsof is used when available for a cleaner check.
port_in_use() {
  local port="$1"
  if command -v lsof >/dev/null 2>&1; then
    lsof -nP -iTCP:"${port}" -sTCP:LISTEN >/dev/null 2>&1
  else
    # If a TCP connect succeeds, the port is in use.
    curl -s -o /dev/null --max-time 1 "http://127.0.0.1:${port}/" 2>/dev/null
  fi
}

# wait_ports_free : block until both listen ports are released (or a short timeout).
#   The server's graceful shutdown can take a few seconds while the packer flushes;
#   restarting before the OS frees the sockets would make the new server fail to bind.
wait_ports_free() {
  for ((_i=0;_i<60;_i++)); do
    if ! port_in_use "${BENCH_MGMT_PORT}" && ! port_in_use "${BENCH_WEBDAV_PORT}"; then
      return 0
    fi
    sleep 0.5
  done
  log "  (warning) listen ports still appear busy after waiting; continuing anyway"
}

stop_server() {
  if [[ -n "${SERVER_PID}" ]] && kill -0 "${SERVER_PID}" 2>/dev/null; then
    kill "${SERVER_PID}" 2>/dev/null || true
    # Graceful shutdown timeout in the server is 30s; wait up to ~30s before SIGKILL.
    for ((_i=0;_i<120;_i++)); do
      kill -0 "${SERVER_PID}" 2>/dev/null || break
      sleep 0.25
    done
    kill -9 "${SERVER_PID}" 2>/dev/null || true
    # Reap the SIGKILLed child so it does not linger as a zombie.
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
  SERVER_PID=""
  # Ensure the listen sockets are actually released before any restart.
  wait_ports_free
}

add_bots_and_channels() {
  section "Starting server and registering bots/channels via Management API"
  start_server
  log "server up; /healthz ok"

  # 1) Add bots first, so the channel membership check below can see them.
  #    Body shape from api/openapi/management.yaml (AddBot: {token}).
  #    NOTE: scope IFS to the read ONLY (a function-wide `local IFS=,` would break
  #    later `$(seq ...)` word-splitting in start_server during the restart below).
  local added_bots=0
  local _tokens=()
  IFS=',' read -ra _tokens <<< "${BENCH_BOT_TOKENS}"
  for tok in "${_tokens[@]}"; do
    tok="$(printf '%s' "${tok}" | awk '{$1=$1};1')"  # trim
    [[ -n "${tok}" ]] || continue
    if curl_admin -X POST "${MGMT_URL}/api/v1/bots" \
         -H 'Content-Type: application/json' \
         --data-binary "$(printf '{"token":"%s"}' "${tok}")" >/dev/null 2>>"${SERVER_LOG}"; then
      added_bots=$((added_bots+1))
    else
      die "failed to add a bot (token redacted) — check it is valid; see ${SERVER_LOG##*/}"
    fi
  done
  log "added ${added_bots} bot(s) [tokens redacted]"

  # 2) Add channels by bare id (AddChannel: {bare_id}). The server checks bot
  #    membership synchronously and marks the channel available if a bot can use it.
  local added_chans=0
  local _chans=()
  IFS=',' read -ra _chans <<< "${BENCH_CHANNEL_IDS}"
  for cid in "${_chans[@]}"; do
    cid="$(printf '%s' "${cid}" | awk '{$1=$1};1')"
    [[ -n "${cid}" ]] || continue
    if curl_admin -X POST "${MGMT_URL}/api/v1/channels" \
         -H 'Content-Type: application/json' \
         --data-binary "$(printf '{"bare_id":%s}' "${cid}")" >/dev/null 2>>"${SERVER_LOG}"; then
      added_chans=$((added_chans+1))
    else
      die "failed to add channel ${cid} — is every bot an admin of it? see ${SERVER_LOG##*/}"
    fi
  done
  log "added ${added_chans} channel(s)"

  # 3) Verify at least one channel is `available` (a bot is a member).
  local channels_json avail_count
  channels_json="$(curl_admin "${MGMT_URL}/api/v1/channels")"
  avail_count="$(printf '%s' "${channels_json}" | grep -o '"available":true' | wc -l | tr -d ' ')"
  [[ "${avail_count}" -ge 1 ]] || die "no channel became available — bots must be admins of the channels"
  log "${avail_count} channel(s) report available"

  # 4) Restart the server ONCE so the packer's upload-worker pool scales to the
  #    number of enabled bots (parallelism is fixed at server start; adding bots
  #    afterwards does not change it until a restart — see README Limitations).
  log "restarting server so packer scales to ${BOT_COUNT} bot(s)"
  stop_server
  start_server
  log "server restarted; ready to benchmark"

  # 5) Lower the WAL idle-flush timeout for the benchmark. In production the
  #    default is 60 s so a trickle of small files accumulates into one ~19 MiB
  #    blob (conserving each bot's Telegram request budget). For measurement we
  #    want the small-file "pack" phase to reflect upload speed, not a 60 s wait,
  #    so we shrink it via the Management API (settings live in the DB).
  if curl_admin -X PUT "${MGMT_URL}/api/v1/settings" \
      -H 'Content-Type: application/json' \
      -d "{\"wal_idle_timeout_ms\": ${BENCH_WAL_IDLE_MS}}" >/dev/null 2>&1; then
    log "wal_idle_timeout_ms set to ${BENCH_WAL_IDLE_MS} for benchmarking (production default 60000)"
  else
    log "warning: could not lower wal_idle_timeout_ms; small-file pack timings will include the idle wait"
  fi
}

# ----------------------------------------------------------------------------------
# Phase 1: throughput by file size
# ----------------------------------------------------------------------------------

# wait_for_pack PATH : poll the DB until the node for PATH is `stored` (state=2)
#   and it has zero live wal_chunks rows. Returns 0 on success, 1 on timeout.
#   NodeStateStored=2; matched by path suffix because nodes.path is the per-user path.
wait_for_pack() {
  local path="$1"
  local deadline=$(( $(date +%s) + BENCH_PACK_TIMEOUT ))
  while :; do
    # state of the node; '' if not found yet
    local state walrows
    state="$(psql_scalar "SELECT state FROM nodes WHERE path LIKE '%${path}' ORDER BY modified_at DESC LIMIT 1;")"
    walrows="$(psql_scalar "SELECT count(*) FROM wal_chunks w JOIN nodes n ON n.id=w.node_id WHERE n.path LIKE '%${path}';")"
    if [[ "${state}" == "2" && "${walrows}" == "0" ]]; then
      return 0
    fi
    if [[ "$(date +%s)" -ge "${deadline}" ]]; then
      log "  pack timeout for ${path} (state=${state:-none}, wal_rows=${walrows:-?})"
      return 1
    fi
    sleep 1
  done
}

# blob_count_for PATH : number of distinct blobs referenced by the node's extents.
blob_count_for() {
  local path="$1"
  psql_scalar "SELECT count(DISTINCT e.blob_id) FROM extents e JOIN nodes n ON n.id=e.node_id WHERE n.path LIKE '%${path}';"
}

# clear_cache: wipe the disk LRU cache so the next GET is a true cold read.
clear_cache() { rm -rf "${CACHE_DIR:?}/"* 2>/dev/null || true; }

run_throughput() {
  section "Phase 1: throughput by file size"
  THROUGHPUT_ROWS="${WORK_DIR}/throughput.tsv"
  : > "${THROUGHPUT_ROWS}"

  local idx=0
  for size in ${BENCH_FILE_SIZES}; do
    idx=$((idx+1))
    local bytes; bytes="$(bytes_for "${size}")"
    local remote="/bench/size_${idx}_${size}.bin"
    local src="${DATA_DIR}/src_${idx}.bin"
    local dst="${DATA_DIR}/dst_${idx}.bin"

    log "[${size}] generating ${bytes} bytes of random data"
    gen_file "${src}" "${bytes}"
    local src_bytes; src_bytes="$(wc -c < "${src}" | tr -d ' ')"
    local src_sha;   src_sha="$(SHA256 "${src}")"

    # Ensure the parent collection exists (idempotent).
    curl_dav -X MKCOL "${WEBDAV_URL}/bench/" >/dev/null 2>&1 || true

    # --- PUT -> WAL ingest ---
    local t0 t1 put_s
    t0="$(now_ns)"
    curl_dav -T "${src}" "${WEBDAV_URL}${remote}" >/dev/null
    t1="$(now_ns)"
    put_s="$(awk -v a="${t0}" -v b="${t1}" 'BEGIN{printf "%.6f",(b-a)/1e9}')"
    local put_mbps; put_mbps="$(mbps "${src_bytes}" "${put_s}")"

    # --- buffered read (immediate GET, served from WAL) ---
    t0="$(now_ns)"
    curl_dav -o "${dst}" "${WEBDAV_URL}${remote}" >/dev/null
    t1="$(now_ns)"
    local buf_s; buf_s="$(awk -v a="${t0}" -v b="${t1}" 'BEGIN{printf "%.6f",(b-a)/1e9}')"
    local buf_mbps; buf_mbps="$(mbps "${src_bytes}" "${buf_s}")"

    # --- pack -> Telegram ---
    local pack_t0 pack_t1 pack_s pack_mbps pack_note
    pack_t0="$(now_ns)"
    if wait_for_pack "${remote}"; then
      pack_t1="$(now_ns)"
      pack_s="$(awk -v a="${pack_t0}" -v b="${pack_t1}" 'BEGIN{printf "%.3f",(b-a)/1e9}')"
      pack_mbps="$(mbps "${src_bytes}" "${pack_s}")"
      pack_note="${pack_s}s / ${pack_mbps} MB/s"
    else
      pack_note="TIMEOUT"
      pack_mbps="n/a"
    fi
    local blobs; blobs="$(blob_count_for "${remote}")"

    # --- cold read (clear cache, GET, verify sha256) ---
    clear_cache
    t0="$(now_ns)"
    curl_dav -o "${dst}" "${WEBDAV_URL}${remote}" >/dev/null
    t1="$(now_ns)"
    local cold_s; cold_s="$(awk -v a="${t0}" -v b="${t1}" 'BEGIN{printf "%.6f",(b-a)/1e9}')"
    local cold_mbps; cold_mbps="$(mbps "${src_bytes}" "${cold_s}")"
    local dst_sha; dst_sha="$(SHA256 "${dst}")"
    local verify="OK"
    if [[ "${dst_sha}" != "${src_sha}" ]]; then
      verify="MISMATCH"
      log "  !! sha256 MISMATCH on cold read of ${size} (src=${src_sha} dst=${dst_sha})"
    else
      log "  sha256 verified on cold read (${size})"
    fi

    # --- warm read (GET again, served from disk LRU cache) ---
    t0="$(now_ns)"
    curl_dav -o "${dst}" "${WEBDAV_URL}${remote}" >/dev/null
    t1="$(now_ns)"
    local warm_s; warm_s="$(awk -v a="${t0}" -v b="${t1}" 'BEGIN{printf "%.6f",(b-a)/1e9}')"
    local warm_mbps; warm_mbps="$(mbps "${src_bytes}" "${warm_s}")"

    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
      "${size}" "${put_mbps}" "${buf_mbps}" "${pack_note}" "${cold_mbps}" "${warm_mbps}" "${blobs}" "${verify}" \
      >> "${THROUGHPUT_ROWS}"

    log "[${size}] put=${put_mbps} buf=${buf_mbps} pack=${pack_note} cold=${cold_mbps} warm=${warm_mbps} blobs=${blobs} ${verify}"

    rm -f "${src}" "${dst}" 2>/dev/null || true
  done
}

# ----------------------------------------------------------------------------------
# Phase 2: small-file request rate by concurrency
# ----------------------------------------------------------------------------------

# run_parallel_ops LABEL TOTAL CONC WORKER_FN : run TOTAL invocations of WORKER_FN
#   across CONC parallel workers; each worker appends per-op millisecond timings to
#   its own file. Echoes "ops_per_s p50 p95 p99 errors".
run_parallel_ops() {
  local label="$1" total="$2" conc="$3" worker_fn="$4"
  local per=$(( (total + conc - 1) / conc ))
  local timings="${WORK_DIR}/timings_$$.txt"
  local errflag="${WORK_DIR}/err_$$.flag"
  : > "${timings}"; rm -f "${errflag}"

  local batch_t0 batch_t1
  batch_t0="$(now_ns)"
  local w
  for (( w=0; w<conc; w++ )); do
    (
      local i tt0 tt1 ms
      for (( i=0; i<per; i++ )); do
        tt0="$(now_ns)"
        if "${worker_fn}" "${w}" "${i}"; then
          tt1="$(now_ns)"
          ms="$(awk -v a="${tt0}" -v b="${tt1}" 'BEGIN{printf "%.3f",(b-a)/1e6}')"
          printf '%s\n' "${ms}" >> "${timings}"
        else
          : > "${errflag}"
        fi
      done
    ) &
  done
  wait
  batch_t1="$(now_ns)"

  local count; count="$(wc -l < "${timings}" | tr -d ' ')"
  local elapsed; elapsed="$(awk -v a="${batch_t0}" -v b="${batch_t1}" 'BEGIN{printf "%.6f",(b-a)/1e9}')"
  local ops; ops="$(awk -v c="${count}" -v s="${elapsed}" 'BEGIN{ if(s<=0){print "n/a"}else{printf "%.0f", c/s} }')"
  local p50 p95 p99
  p50="$(percentile "${timings}" 50)"
  p95="$(percentile "${timings}" 95)"
  p99="$(percentile "${timings}" 99)"
  local errors="0"; [[ -f "${errflag}" ]] && errors="some"

  rm -f "${timings}" "${errflag}" 2>/dev/null || true
  printf '%s|%s|%s|%s|%s\n' "${ops}" "${p50}" "${p95}" "${p99}" "${errors}"
}

# Worker: PUT a fresh 4 KiB file into the WAL. Unique path per op.
_put_small_file=""
worker_put_small() {
  local w="$1" i="$2"
  curl_dav -T "${_put_small_file}" "${WEBDAV_URL}/bench/small/p_${w}_${i}.bin" >/dev/null 2>&1
}

# Worker: warm GET of a pre-cached 256 KiB file.
worker_get_warm() {
  curl_dav -o /dev/null "${WEBDAV_URL}/bench/warm.bin" >/dev/null 2>&1
}

run_concurrency() {
  section "Phase 2: small-file request rate by concurrency"
  CONC_ROWS="${WORK_DIR}/concurrency.tsv"
  : > "${CONC_ROWS}"

  curl_dav -X MKCOL "${WEBDAV_URL}/bench/" >/dev/null 2>&1 || true
  curl_dav -X MKCOL "${WEBDAV_URL}/bench/small/" >/dev/null 2>&1 || true

  # 4 KiB sample for the PUT test.
  _put_small_file="${DATA_DIR}/small4k.bin"
  gen_file "${_put_small_file}" 4096

  # 256 KiB warm file: PUT once, then GET once to populate the cache.
  local warm="${DATA_DIR}/warm256k.bin"
  gen_file "${warm}" 262144
  curl_dav -T "${warm}" "${WEBDAV_URL}/bench/warm.bin" >/dev/null 2>&1
  # Pack + warm it so the GET is served from the disk LRU cache.
  wait_for_pack "/bench/warm.bin" || log "  (warm file did not pack in time; GET may serve from WAL)"
  curl_dav -o /dev/null "${WEBDAV_URL}/bench/warm.bin" >/dev/null 2>&1

  local c res
  for c in ${BENCH_CONCURRENCY}; do
    log "[PUT 4KiB->WAL] concurrency=${c} ops=${BENCH_SMALL_OPS}"
    res="$(run_parallel_ops "put" "${BENCH_SMALL_OPS}" "${c}" worker_put_small)"
    printf 'PUT\t%s\t%s\n' "${c}" "${res}" >> "${CONC_ROWS}"
    log "  -> ${res}"

    log "[GET 256KiB warm] concurrency=${c} ops=${BENCH_SMALL_OPS}"
    res="$(run_parallel_ops "get" "${BENCH_SMALL_OPS}" "${c}" worker_get_warm)"
    printf 'GET\t%s\t%s\n' "${c}" "${res}" >> "${CONC_ROWS}"
    log "  -> ${res}"
  done

  rm -f "${_put_small_file}" "${warm}" 2>/dev/null || true
}

# ----------------------------------------------------------------------------------
# Phase 3: storage efficiency (after everything is packed)
# ----------------------------------------------------------------------------------

run_storage() {
  section "Phase 3: storage efficiency"

  # Pack everything still buffered: poll until no node is non-stored and the WAL
  # is empty (bounded by BENCH_PACK_TIMEOUT).
  local wal_before
  wal_before="$(psql_scalar "SELECT count(*) FROM wal_chunks;")"
  log "wal_chunks rows before final drain: ${wal_before:-?}"

  local deadline=$(( $(date +%s) + BENCH_PACK_TIMEOUT ))
  while :; do
    local pending wal_now
    pending="$(psql_scalar "SELECT count(*) FROM nodes WHERE is_dir=false AND state<>2;")"
    wal_now="$(psql_scalar "SELECT count(*) FROM wal_chunks;")"
    [[ "${pending}" == "0" && "${wal_now}" == "0" ]] && break
    if [[ "$(date +%s)" -ge "${deadline}" ]]; then
      log "  storage drain timeout (pending=${pending:-?}, wal=${wal_now:-?})"
      break
    fi
    sleep 1
  done

  local wal_after wal_bytes meta_bytes blob_bytes blob_count file_count logical_bytes
  wal_after="$(psql_scalar "SELECT count(*) FROM wal_chunks;")"
  wal_bytes="$(psql_scalar "SELECT COALESCE(sum(octet_length(data)),0) FROM wal_chunks;")"
  meta_bytes="$(psql_scalar "SELECT pg_total_relation_size('nodes') + pg_total_relation_size('extents') + pg_total_relation_size('blobs') + pg_total_relation_size('blob_bot_files');")"
  blob_bytes="$(psql_scalar "SELECT COALESCE(sum(size),0) FROM blobs;")"
  blob_count="$(psql_scalar "SELECT count(*) FROM blobs;")"
  file_count="$(psql_scalar "SELECT count(*) FROM nodes WHERE is_dir=false;")"
  logical_bytes="$(psql_scalar "SELECT COALESCE(sum(size),0) FROM nodes WHERE is_dir=false;")"

  STORAGE_WAL_BEFORE="${wal_before:-0}"
  STORAGE_WAL_AFTER="${wal_after:-0}"
  STORAGE_WAL_BYTES="${wal_bytes:-0}"
  STORAGE_META_BYTES="${meta_bytes:-0}"
  STORAGE_BLOB_BYTES="${blob_bytes:-0}"
  STORAGE_BLOB_COUNT="${blob_count:-0}"
  STORAGE_FILE_COUNT="${file_count:-0}"
  STORAGE_LOGICAL_BYTES="${logical_bytes:-0}"

  log "wal rows after: ${STORAGE_WAL_AFTER} | telegram bytes: ${STORAGE_BLOB_BYTES} across ${STORAGE_BLOB_COUNT} blobs | metadata: ${STORAGE_META_BYTES} bytes"
}

# ----------------------------------------------------------------------------------
# Output: Markdown tables to stdout
# ----------------------------------------------------------------------------------

print_results() {
  section "Results (Markdown on stdout)"

  echo "## tgwebdav benchmark results"
  echo
  echo "_Setup: throwaway Postgres (${BENCH_PG_IMAGE}) in Docker on :${BENCH_PG_PORT};" \
       "WebDAV :${BENCH_WEBDAV_PORT}, Management :${BENCH_MGMT_PORT}; ${BOT_COUNT} bot(s);" \
       "$(go version | awk '{print $3}'). Numbers only; tokens/keys redacted._"
  echo

  echo "### Throughput by file size"
  echo
  echo "| File size | PUT → WAL (MB/s) | Buffered read (MB/s) | Pack → Telegram | Cold read (MB/s) | Warm read (MB/s) | Blobs | sha256 |"
  echo "|-----------|-----------------:|---------------------:|-----------------|-----------------:|-----------------:|------:|:------:|"
  if [[ -f "${THROUGHPUT_ROWS}" ]]; then
    while IFS=$'\t' read -r size put buf pack cold warm blobs verify; do
      printf '| %s | %s | %s | %s | %s | %s | %s | %s |\n' \
        "${size}" "${put}" "${buf}" "${pack}" "${cold}" "${warm}" "${blobs}" "${verify}"
    done < "${THROUGHPUT_ROWS}"
  fi
  echo

  echo "### Small-file request rate by concurrency"
  echo
  echo "| Operation | Concurrency | Throughput (ops/s) | p50 (ms) | p95 (ms) | p99 (ms) | Errors |"
  echo "|-----------|------------:|-------------------:|---------:|---------:|---------:|:------:|"
  if [[ -f "${CONC_ROWS}" ]]; then
    while IFS=$'\t' read -r op conc rest; do
      IFS='|' read -r ops p50 p95 p99 errors <<< "${rest}"
      local label
      case "${op}" in
        PUT) label="Small-file PUT (4 KiB → WAL)" ;;
        GET) label="Warm GET (256 KiB, cached)" ;;
        *)   label="${op}" ;;
      esac
      printf '| %s | %s | %s | %s | %s | %s | %s |\n' \
        "${label}" "${conc}" "${ops}" "${p50}" "${p95}" "${p99}" "${errors}"
    done < "${CONC_ROWS}"
  fi
  echo

  echo "### Storage efficiency"
  echo
  local ratio per_mib logical_gib blob_gib meta_mib
  # Telegram bytes ÷ logical bytes — ~1.0000 means no permanent duplication.
  ratio="$(awk -v b="${STORAGE_BLOB_BYTES}" -v l="${STORAGE_LOGICAL_BYTES}" \
    'BEGIN{ if(l<=0){print "n/a"}else{printf "%.4f", b/l} }')"
  # Postgres metadata bytes per MiB of logical stored file.
  per_mib="$(awk -v m="${STORAGE_META_BYTES}" -v l="${STORAGE_LOGICAL_BYTES}" \
    'BEGIN{ if(l<=0){print "n/a"}else{printf "%.2f", m / (l/1048576.0) } }')"
  logical_gib="$(awk -v l="${STORAGE_LOGICAL_BYTES}" 'BEGIN{printf "%.3f", l/1073741824.0}')"
  blob_gib="$(awk -v b="${STORAGE_BLOB_BYTES}" 'BEGIN{printf "%.3f", b/1073741824.0}')"
  meta_mib="$(awk -v m="${STORAGE_META_BYTES}" 'BEGIN{printf "%.3f", m/1048576.0}')"

  echo "| Metric | Value |"
  echo "|--------|-------|"
  printf '| Logical data stored | %s bytes (%s GiB), %s files |\n' \
    "${STORAGE_LOGICAL_BYTES}" "${logical_gib}" "${STORAGE_FILE_COUNT}"
  printf '| Bytes living in Telegram (Σ blob sizes) | %s bytes (%s GiB) across %s blobs |\n' \
    "${STORAGE_BLOB_BYTES}" "${blob_gib}" "${STORAGE_BLOB_COUNT}"
  printf '| Telegram bytes ÷ logical bytes | %s× |\n' "${ratio}"
  printf '| `wal_chunks` rows before final drain → after | %s → %s |\n' \
    "${STORAGE_WAL_BEFORE}" "${STORAGE_WAL_AFTER}"
  printf '| `wal_chunks` live bytes after packing | %s |\n' "${STORAGE_WAL_BYTES}"
  printf '| Postgres metadata (nodes+extents+blobs+blob_bot_files, incl. indexes/toast) | %s bytes (%s MiB) |\n' \
    "${STORAGE_META_BYTES}" "${meta_mib}"
  printf '| Postgres metadata per MiB stored | %s bytes/MiB |\n' "${per_mib}"
  echo

  log "stdout above is the Markdown report."
}

# ----------------------------------------------------------------------------------
# Main
# ----------------------------------------------------------------------------------

main() {
  check_requirements
  validate_secrets
  setup_workdir
  start_postgres
  build_binary
  run_migrations
  add_bots_and_channels
  run_throughput
  run_concurrency
  run_storage
  print_results
}

main "$@"

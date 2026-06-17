# tgwebdav

**A WebDAV server backed by Telegram channels.** Files are written to a
PostgreSQL write-ahead log, packed by a background worker into immutable blobs
under 20 MiB, and uploaded to Telegram channels through the official Bot API.
Reads assemble files from those blobs on demand, with a disk cache in front.

A single binary runs two servers over one PostgreSQL database:

1. **WebDAV** (`golang.org/x/net/webdav`) — a per-user, isolated namespace.
2. **Management API** — an OpenAPI-generated admin REST API for users, bots,
   channels, runtime settings, statistics and audit events.

> ⚠️ Telegram is not designed as a filesystem. Channel messages can be evicted
> after ~1M messages and deletions are irreversible. tgwebdav handles these
> cases gracefully (availability flags, cascade rules) but is best treated as an
> experiment / cold archive, not authoritative storage.

## Features

- **WAL-buffered writes** — a `PUT` is durable and immediately readable the
  moment it lands in Postgres; Telegram upload happens asynchronously.
- **Blobs + extents** — every file (large or small) is described by extents that
  map byte ranges onto shared, immutable blobs. Large files are split across
  several blobs; many small files are packed into one.
- **Per-user isolation** — each user sees only their own namespace. Data in
  Telegram is shared; isolation is enforced entirely at the access/DB layer.
- **Admin impersonation** — an administrator can act in any user's namespace via
  the Basic username `admin/target`.
- **Multi-bot, multi-channel** — uploads/downloads are spread across bots;
  membership is verified per (bot, channel); rate limits (`retry_after`) and
  channel eviction are tracked, and availability is recomputed automatically.
- **Cross-bot recovery** — a blob can be re-fetched by any member bot via
  `forwardMessage`, so losing one bot does not lose readability.
- **Quotas, bandwidth and rate limits** per user.
- **Disk LRU cache** of whole blobs, so adjacent reads stay local.
- **Efficient MOVE/COPY** — MOVE is a metadata rename; COPY shares the
  underlying blobs (reference-counted) with no re-upload.
- **Graceful shutdown** that drains requests and flushes the WAL.

## Requirements

- Go 1.26+
- PostgreSQL 14+ (17 recommended; a `docker-compose.yml` is provided)
- One or more Telegram bots that are **administrators** of one or more channels

## Quick start

```sh
# 1. Start PostgreSQL
docker compose up -d

# 2. Configure (copy and edit)
cp .env.example .env
#   set TGWEBDAV_DSN, TGWEBDAV_SECRET_KEY, TGWEBDAV_FIRST_USER,
#   TGWEBDAV_BOT_TOKENS and TGWEBDAV_CHANNEL_IDS

# 3. Run (loads .env automatically)
go run ./cmd/tgwebdav
# or: go build -o tgwebdav ./cmd/tgwebdav && ./tgwebdav
```

On first boot tgwebdav runs migrations, seeds the configured channels and bots
(verifying which bot is a member of which channel against the live Bot API), and
creates the first administrator from `TGWEBDAV_FIRST_USER` if the users table is
empty.

### Connect a WebDAV client

Any standard WebDAV client works (rclone, macOS Finder, Windows Explorer,
Cyberduck, GNOME Files):

```sh
# rclone
rclone config create tg webdav url=http://localhost:8080 vendor=other \
  user=admin pass=$(rclone obscure 'your-password')
rclone copy ./localdir tg:remote/
rclone ls tg:
```

- macOS Finder: *Go → Connect to Server → `http://localhost:8080`*
- Windows: *Map network drive → `http://localhost:8080`*

## Configuration

Only bootstrap settings are passed via flags/env; everything tunable at runtime
(blob size, WAL idle timeout, max file size, eviction threshold) lives in the
database and is edited through the Management API. The CLI is a
[cobra](https://github.com/spf13/cobra) command and environment variables are
bound with [viper](https://github.com/spf13/viper) (prefix `TGWEBDAV_`);
precedence is flag > environment variable > `.env` file > default.

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--dsn` | `TGWEBDAV_DSN` | — (required) | PostgreSQL DSN |
| `--webdav-addr` | `TGWEBDAV_WEBDAV_ADDR` | `:8080` | WebDAV listen address |
| `--mgmt-addr` | `TGWEBDAV_MGMT_ADDR` | `:8081` | Management API listen address |
| `--cache-dir` | `TGWEBDAV_CACHE_DIR` | user cache dir | Disk blob cache directory |
| `--cache-size` | `TGWEBDAV_CACHE_SIZE` | `1GiB` | Disk cache size (e.g. `512MiB`, `2GiB`) |
| `--first-user` | `TGWEBDAV_FIRST_USER` | — | Bootstrap admin `login:password` |
| `--log-level` | `TGWEBDAV_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| — | `TGWEBDAV_SECRET_KEY` | — | Secret deriving the AES-256-GCM key that encrypts bot tokens at rest (required when bots are configured) |
| — | `TGWEBDAV_BOT_TOKENS` | — | Comma-separated bot tokens to seed |
| — | `TGWEBDAV_CHANNEL_IDS` | — | Comma-separated bare channel ids to seed (the `-100` prefix is applied internally) |

## Management API

Admin-only REST API under `/api/v1` (HTTP Basic for an admin, or a Bearer token
owned by an admin); `GET /healthz` is public, and the OpenAPI document is served
at `/openapi.yaml`.

```sh
# create a user with a 1 GiB quota
curl -u admin:pw -X POST http://localhost:8081/api/v1/users \
  -H 'Content-Type: application/json' \
  -d '{"login":"alice","password":"s3cret","quota_bytes":1073741824}'

curl -u admin:pw http://localhost:8081/api/v1/bots       # list bots
curl -u admin:pw http://localhost:8081/api/v1/channels   # list channels
curl -u admin:pw http://localhost:8081/api/v1/settings   # runtime settings
curl -u admin:pw http://localhost:8081/api/v1/stats      # time-series metrics
curl -u admin:pw http://localhost:8081/api/v1/events     # audit log
```

## How it works

```
WebDAV PUT ──► node (writing) ──► wal_chunks ──► node (buffered, readable)
                                                      │
                          packer worker (≤19 MiB) ────┤  sendDocument → blob + extents
                                                      ▼
                                            node (stored), WAL rows removed

WebDAV GET ──► node ──► stored?  yes ─► extents ─► [disk cache | Telegram download] ─► assemble
                         └─ buffered ─► read bytes straight from wal_chunks
```

- **Write**: `PUT` creates/replaces a node and appends content to `wal_chunks`
  (≤1 MiB rows). On `Close` the size/hash/etag are recorded and the node becomes
  `buffered` — already readable. A background packer leases buffered nodes
  (`FOR UPDATE SKIP LOCKED`), splits large files into ≤19 MiB blobs and packs
  small files together, and uploads those blobs **in parallel across the bots**
  (one upload worker per bot). All of a file's blobs go to **one channel** (so
  losing a channel loses whole files rather than corrupting many). A node is
  marked `stored` and its WAL rows deleted only once every one of its blobs has
  landed — extents are written and refcounts bumped in that final, lease-guarded
  transaction, so a crash or lost lease just re-packs with no duplicate extents.
- **Read**: stored files are assembled from their extents; each needed blob is
  served from the disk cache or downloaded from Telegram (preferring a bot with
  a cached `file_id`, else recovering a fresh one via `forwardMessage`). A GET
  runs a **sliding-window read-ahead** that downloads the blobs just ahead of the
  read cursor in parallel (bounded to cache capacity), so sequential reads do not
  stall on the next blob. A definitively missing message marks the blob
  permanently unavailable and cascade-deletes files that referenced only it.
- **Delete/Move/Copy**: DELETE removes nodes and decrements blob refcounts
  (messages are never auto-deleted — they may be shared); MOVE rewrites paths;
  COPY duplicates extents and bumps refcounts, sharing blobs without re-upload.

## Performance

Measured end-to-end on an Apple Silicon laptop with PostgreSQL in Docker and the
four real bots/channels (32 concurrent clients unless noted). Telegram-bound
figures depend entirely on the Bot API and your network.

**WebDAV request throughput** (local, DB-bound operations):

| Operation | Throughput | Requests/min | Latency |
|-----------|-----------:|-------------:|--------:|
| `GET` cached file | ~4,800 req/s | **~287,000 rpm** | 0.21 ms |
| `PUT` new small file (→ WAL) | ~1,500 req/s | **~91,000 rpm** | 0.66 ms |
| `PROPFIND` (small dir) | ~1,300 req/s | **~79,000 rpm** | 0.76 ms |
| `PUT` overwrite stored file | ~240 req/s | ~14,000 rpm | 4.2 ms |
| `PROPFIND` over 2,000 entries | — | — | ~1.5 s |

**Data throughput**:

| Path | Throughput |
|------|-----------:|
| Write ingest into WAL (`PUT` 50 MiB) | ~87 MiB/s |
| Read from warm disk cache | ~230 MiB/s |
| Upload to Telegram (packer, 200 MiB, 4 bots in parallel) | ~9.8 MiB/s |
| Cold read from Telegram (sequential blob fetch) | ~0.8 MiB/s |

A large file is split into 19 MiB blobs uploaded concurrently across all bots
(≈ one bot's rate × bot count) and round-trips through real Telegram with a
verified byte-for-byte SHA-256 match. Local operations are fast because writes
hit Postgres, not Telegram; the Telegram-bound figures reflect the Bot API's own
per-bot upload/download limits, which the parallel packer multiplies by spreading
work across bots.

> Note: per-request HTTP Basic auth runs argon2id, which is deliberately slow; a
> short-lived in-memory verification cache (30 s, keyed by the stored hash) keeps
> repeated requests from the same client cheap.

## Development

```sh
go build ./...
go test ./...                                   # unit tests
TGWEBDAV_TEST_DSN='postgres://tgwebdav:tgwebdav@localhost:5433/tgwebdav?sslmode=disable' \
  go test ./...                                 # + Postgres integration tests
```

The codebase is organized hexagonally: `internal/domain` holds entities and all
port interfaces; every other package implements a port and depends only on the
domain contracts.

```
cmd/tgwebdav/              entrypoint (cobra command), wiring, graceful shutdown
api/openapi.yaml           Management API OpenAPI 3 spec (embedded + served)
db/migrations/             embedded dbmate migrations (*.sql) + slog-logging runner
internal/domain/           entities, errors, repository & service ports
internal/config/           cobra flags + viper env binding (+ .env loader)
internal/storage/postgres/ GORM repositories + transactional unit-of-work
internal/telegram/         Bot API client (per-bot pacing, typed errors)
internal/cache/            disk LRU blob cache
internal/blob/             read path: bot selection, recovery, cascade
internal/wal/              packer (parallel multi-bot uploads, lease-guarded finalize)
internal/webdavfs/         webdav.FileSystem over the store
internal/auth/             argon2id, basic/bearer, impersonation
internal/limits/           per-user quota / bandwidth / rate limiting
internal/services/         bot, channel and settings services
internal/management/       generated server + handlers (spec lives in api/)
internal/stats/            counters + gauges → time series
internal/server/           WebDAV HTTP server
```

## License

[Apache License 2.0](LICENSE).

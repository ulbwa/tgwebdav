# tgwebdav

**A WebDAV server that stores file bytes in Telegram channels and keeps only metadata in PostgreSQL.**

A single binary serves a per-user, isolated WebDAV namespace and an administrative Management API over one PostgreSQL database, packing file content into Telegram channel messages (blobs) through the official Bot API. Writes land in a Postgres write-ahead buffer and become durable and readable immediately; a background worker asynchronously packs them into immutable, SHA-256-verified blobs and uploads those to Telegram, after which the bytes live only in Telegram and Postgres retains a few hundred bytes of metadata per file. A local disk LRU cache fronts reads.

> ⚠️ **Experimental — do not trust it as authoritative storage.** Telegram is not a filesystem. A channel evicts its oldest messages once it grows past roughly one million messages, deletions are irreversible, and bot/channel access can be revoked out from under you. tgwebdav degrades gracefully (availability flags, integrity verification, cross-bot recovery, cascade rules) but is best treated as an **experiment or a cold archive**, never as primary or authoritative storage. Keep an independent copy of anything you cannot afford to lose.

---

## Features

- **WAL-buffered durable writes** — a `PUT` is committed to a Postgres write-ahead buffer and is immediately durable and readable; the Telegram upload happens asynchronously in the background, so the client never waits on the network or Telegram.
- **Blobs + extents** — every file is described by *extents* mapping byte ranges onto shared, immutable *blobs* (each ~19 MiB). Large files span several blobs; many small files share one blob.
- **Per-user isolation** — each user sees only their own namespace; isolation is enforced entirely at the access / database layer.
- **Admin impersonation** — an administrator can act inside any user's namespace via the Basic username form `admin/target`.
- **Multi-bot, multi-channel** — uploads and downloads are spread across multiple bots and channels for throughput and resilience.
- **Cross-bot recovery** — a blob is reachable as long as *any* member bot can still serve it; a stale `file_id` is re-minted by forwarding the original channel message with another bot.
- **Per-blob SHA-256 integrity verification** — every blob has a stored `content_hash`; bytes downloaded from Telegram are SHA-256-verified before being served or cached, and corrupt or substituted bytes are rejected (fail-closed), never returned.
- **Quotas, bandwidth and rate limits** — per-user storage quota, read-bandwidth cap, and request rate cap (each `0` = unlimited).
- **Disk LRU cache** — a bounded local cache holds warm blobs for fast repeat reads.
- **Efficient MOVE/COPY** — `COPY` shares immutable blobs by reference count instead of re-uploading bytes.
- **Blob GC / reaper** — a background maintenance worker collects unreferenced blobs and reconciles channel/bot availability.
- **Graceful shutdown** — listeners stop accepting, in-flight requests drain, and the packer performs a final flush before exit.

---

## Architecture

tgwebdav runs **two HTTP servers** from one binary over one PostgreSQL database:

1. **WebDAV server** (`golang.org/x/net/webdav`) — the per-user file namespace.
2. **Management API server** — an OpenAPI-generated admin REST API for users, tokens, bots, channels, runtime settings, statistics and audit events.

### The write path (WAL model)

File **bytes live in Telegram**; **metadata lives in Postgres**. Writes are write-ahead-buffered:

```
            PUT bytes
               │
               ▼
   ┌────────────────────────┐  request returns here  (Postgres speed)
   │  wal_chunks (Postgres)  │ ─ node marked "buffered": durable + readable
   └───────────┬────────────┘
               │  background packer (async)
               ▼
   ┌────────────────────────┐
   │  group into ~19 MiB     │  upload each blob to a Telegram channel,
   │  blobs, SHA-256 each     │  spread across enabled bots in parallel
   └───────────┬────────────┘
               │  record blob + per-bot file_id + extents
               ▼
   node marked "stored" ──► WAL rows deleted; bytes now only in Telegram,
                            Postgres keeps ~hundreds of bytes of metadata
```

- A `PUT` does **not** upload to Telegram inline. The bytes go into the Postgres write-ahead buffer (`wal_chunks`), the file is marked *buffered*, and the request completes at Postgres speed. The file is durable and readable from the WAL immediately.
- A background **packer** groups buffered data into ~19 MiB blobs (small files batched together, large files split), computes each blob's SHA-256, uploads it to a Telegram channel spread across the enabled bots, records the resulting per-bot `file_id`s and the file's extents, marks the node *stored*, and **deletes the WAL rows**.
- **Batching to conserve the Telegram request budget:** the packer fills a blob to ~19 MiB before uploading; a partially-filled blob is only flushed after `wal_idle_timeout_ms` of no new writes (**default 60 s**). The idle timer resets on every write, so under sustained load blobs upload as soon as they reach ~19 MiB, and a trickle of tiny files accumulates into one blob instead of one Telegram upload per file (each bot has a limited request rate). Buffered files remain durable and readable from Postgres the whole time; tune `wal_idle_timeout_ms` via `PUT /api/v1/settings`.
- **Reads** are served from the WAL (before packing), from the local disk LRU cache (warm), or by downloading + SHA-256-verifying the blobs from Telegram (cold).

### Integrity verification

Each blob stores a SHA-256 `content_hash`. After **every** fresh Telegram download (including cache misses and recovered `file_id`s), the bytes are hashed and compared (constant-time) against the stored hash. On a mismatch the reader **fails closed**: it logs a `blob_corrupt` event and returns "blob unavailable" rather than serving or caching corrupt or substituted bytes.

### Cross-bot recovery

A blob records a `file_id` per bot. If a cached `file_id` goes stale (e.g. the bot that fetched it lost it), the reader rotates to other member bots and, when needed, **forwards the blob's original channel message with another bot** to mint a fresh `file_id`. A blob is only treated as permanently gone when forward-recovery itself proves the underlying message no longer exists.

### Data model (Postgres)

The init migration creates: `users`, `api_tokens`, `bots` (encrypted token + SHA-256 fingerprint, enabled flag, availability), `channels` (Telegram chat id, message counter, eviction threshold, availability), `bot_channel` (per-pair membership), `blobs` (channel, message id/seq, size, `content_hash`, state, refcount), `blob_bot_files` (per-bot `file_id` for each blob), `nodes` (the file tree: per-user path, dir flag, size, etag, content type, state, packer lease), `extents` (byte-range → blob mapping), `wal_chunks` (the write-ahead buffer), `events` (audit log), `stat_samples` (time-series metrics), and `settings` (a single-row table of runtime-tunable values).

### Background workers

The packer, the disk cache evictor, the stats recorder, the rate limiter, and a maintenance worker (blob reaper / GC, bot & channel availability reconciliation) all run as tracked goroutines and are drained on graceful shutdown.

---

## Requirements

- **Go 1.26+** (the module targets `go 1.26.2`) — to build from source.
- **PostgreSQL 14+** (17 recommended; the dev compose file and tests use Postgres 17).
- **At least one Telegram bot** that is an **administrator of at least one channel**. Bots are added at runtime via the Management API, not via configuration.
- **Docker** — optional, for the bundled Postgres compose file and for `task test` (testcontainers).

---

## Quick start

### 1. Start PostgreSQL

The bundled compose file runs Postgres 17 and maps it to host port **5433**:

```sh
docker compose up -d postgres
```

This exposes `postgres://tgwebdav:tgwebdav@localhost:5433/tgwebdav?sslmode=disable`.

### 2. Build

```sh
go build -o tgwebdav ./cmd/tgwebdav
# or cross-compile every supported platform into ./dist:
make build
```

### 3. Run migrations

Migrations are embedded in the binary. They run automatically on `server` boot, but you can also apply them explicitly:

```sh
./tgwebdav migrate --dsn 'postgres://tgwebdav:tgwebdav@localhost:5433/tgwebdav?sslmode=disable'
```

### 4. Start the server

Bootstrap the first administrator at the same time (created only when the users table is empty) and set a high-entropy secret key (required to add bots — see below):

```sh
export TGWEBDAV_DSN='postgres://tgwebdav:tgwebdav@localhost:5433/tgwebdav?sslmode=disable'
export TGWEBDAV_FIRST_USER='admin:change-me-please'
export TGWEBDAV_SECRET_KEY="$(openssl rand -hex 32)"

./tgwebdav server
# WebDAV listens on :8080, Management API on :8081 (defaults)
```

Running the binary with no subcommand is equivalent to `server`.

### 5. Add bots and channels (Management API only)

> **Bots and channels are NOT configured via environment variables or flags.** They are managed exclusively at runtime through the Management API.

All `/api/v1` calls require admin auth (HTTP Basic with an admin user, or an admin Bearer token). Using the bootstrapped admin's Basic credentials:

```sh
AUTH='-u admin:change-me-please'
MGMT='http://localhost:8081'

# Add a bot by its Telegram Bot API token (the token is encrypted at rest using SECRET_KEY).
curl $AUTH -X POST "$MGMT/api/v1/bots" \
  -H 'Content-Type: application/json' \
  -d '{"token":"123456:ABC-DEF..."}'

# Add a channel by its BARE numeric id (the -100 supergroup prefix is applied internally).
# The bot(s) must already be administrators of this channel.
curl $AUTH -X POST "$MGMT/api/v1/channels" \
  -H 'Content-Type: application/json' \
  -d '{"bare_id":1234567890}'
```

The server periodically re-checks which bots are members of which channels and marks a channel *available* once at least one enabled bot can use it.

---

## Configuration

Every setting is available **both** as a CLI flag and as a `TGWEBDAV_*` environment variable. They are interchangeable — using one does not preclude the other. Precedence is **flag > environment variable > default**. An optional `.env` file (default `.env`, override with `--env-file`) is loaded first and never overrides variables already set in the environment.

| Setting | Flag | Env var | Default | Commands |
|---|---|---|---|---|
| PostgreSQL DSN (required) | `--dsn` | `TGWEBDAV_DSN` | — | `server`, `migrate` |
| Log level (`debug\|info\|warn\|error`) | `--log-level` | `TGWEBDAV_LOG_LEVEL` | `info` | `server`, `migrate` (global) |
| `.env` file to load | `--env-file` | — | `.env` | `server`, `migrate` (global) |
| WebDAV listen address | `--webdav-addr` | `TGWEBDAV_WEBDAV_ADDR` | `:8080` | `server` |
| Management API listen address | `--mgmt-addr` | `TGWEBDAV_MGMT_ADDR` | `:8081` | `server` |
| Disk blob cache directory | `--cache-dir` | `TGWEBDAV_CACHE_DIR` | OS user cache dir `/tgwebdav` | `server` |
| Disk blob cache size | `--cache-size` | `TGWEBDAV_CACHE_SIZE` | `1GiB` | `server` |
| Bootstrap admin (`login:password`) | `--first-user` | `TGWEBDAV_FIRST_USER` | — (none) | `server` |
| Secret key (encrypts bot tokens) | `--secret-key` | `TGWEBDAV_SECRET_KEY` | — (none) | `server` |

Notes:

- `--cache-size` accepts human sizes: binary (`512MiB`, `2GiB`, `1TiB`) and decimal (`512mb`, `2gb`) suffixes, or a bare byte count.
- `--first-user` creates the admin **only when no users exist yet**; it is ignored otherwise. The value must be `login:password`.
- `--secret-key` is **required to add bots**: its SHA-256 derives an AES-256-GCM key that encrypts bot tokens at rest. Without it the server still starts but bot creation fails. **Use a high-entropy value** (e.g. `openssl rand -hex 32`); changing it later makes existing stored bot tokens undecryptable.
- **Bots and channels are managed exclusively via the Management API (RPC), never via configuration.**

Runtime-tunable settings (working blob size, WAL idle-flush timeout, max file size, default channel eviction threshold) live in the database and are edited via `GET/PUT /api/v1/settings`, not via flags or env. See [`.env.example`](.env.example) for an annotated starting point.

---

## Management API

- **Base path:** `/api/v1`
- **Auth:** every `/api/v1` endpoint requires an administrator — HTTP **Basic** with an `is_admin` user's credentials, **or** a **Bearer** token belonging to an `is_admin` user. A request that authenticates but is not an admin gets `403`; missing/invalid auth gets `401`.
- **Public endpoints (no auth):**
  - `GET /healthz` — liveness probe, returns `{"status":"ok"}`.
  - `GET /openapi.yaml` — the full embedded OpenAPI 3.0 spec (the authoritative reference for request/response shapes and status codes).

| Method & path | Description | Body / notes |
|---|---|---|
| `GET /api/v1/users` | List users | — |
| `POST /api/v1/users` | Create a user | `{login, password, is_admin?, quota_bytes?, bandwidth_bps?, rate_per_min?}` |
| `GET /api/v1/users/{userId}` | Get a user | — |
| `DELETE /api/v1/users/{userId}` | Delete a user | — |
| `PUT /api/v1/users/{userId}/password` | Set a user's password | `{password}` |
| `GET /api/v1/users/{userId}/tokens` | List a user's API tokens (hashes never returned) | — |
| `POST /api/v1/users/{userId}/tokens` | Create an API token | `{name}` → plaintext `token` returned **once** |
| `DELETE /api/v1/users/{userId}/tokens/{tokenId}` | Revoke a token | — |
| `GET /api/v1/bots` | List bots | — |
| `POST /api/v1/bots` | Add a bot | `{token}` (Telegram Bot API token) |
| `DELETE /api/v1/bots/{botId}` | Remove a bot | — |
| `PUT /api/v1/bots/{botId}/enabled` | Enable/disable a bot | `{enabled}` |
| `GET /api/v1/channels` | List channels | — |
| `POST /api/v1/channels` | Add a channel | `{bare_id}` (bare numeric id; `-100` prefix applied internally) |
| `DELETE /api/v1/channels/{channelId}` | Decommission a channel | — |
| `PUT /api/v1/channels/{channelId}/eviction` | Set eviction threshold | `{threshold}` (≥ 0) |
| `GET /api/v1/settings` | Get runtime settings | — |
| `PUT /api/v1/settings` | Update runtime settings | any of `{blob_max_size, wal_idle_timeout_ms, max_file_size, default_eviction_threshold}` |
| `GET /api/v1/stats` | Query a time-series metric | query: `metric` (required), `label?`, `from?`, `to?` (RFC 3339) |
| `GET /api/v1/events` | List audit/log events | query: `kind?`, `limit?` (1–1000, default 100), `offset?` |

A created token's plaintext `token` is returned exactly once on creation and is never recoverable afterwards.

---

## WebDAV usage

- **Endpoint:** the WebDAV server (default `:8080`), rooted at `/`.
- **Auth:** HTTP **Basic**.
  - `username = user` — operate inside that user's own namespace.
  - `username = admin/target` — **admin impersonation**: the part before the slash is an administrator's login (whose password is supplied and who must have the admin flag); the part after is the target user whose namespace is served. A valid non-admin attempting this gets `403`.
- **Supported methods:** `OPTIONS`, `PROPFIND`, `GET` (incl. `Range`), `PUT`, `DELETE`, `MKCOL`, `MOVE`, `COPY`, `LOCK`, `UNLOCK`.
- A `PUT` that would exceed the user's storage quota is rejected with `507 Insufficient Storage` before bytes stream. `COPY` shares blobs by reference instead of re-uploading.

### Examples

Upload and download with `curl`:

```sh
curl -u alice:secret -T ./photo.jpg http://localhost:8080/photos/photo.jpg
curl -u alice:secret -o photo.jpg     http://localhost:8080/photos/photo.jpg
# Range request:
curl -u alice:secret -H 'Range: bytes=0-1023' http://localhost:8080/photos/photo.jpg
```

Mount with [rclone](https://rclone.org/webdav/):

```sh
rclone config create tg webdav \
  url http://localhost:8080 vendor other \
  user alice pass "$(rclone obscure secret)"
rclone copy ./localdir tg:backup
```

Admin acting in Alice's namespace (note the quoted `admin/alice` username):

```sh
curl -u 'admin/change-me-please:adminpass' -T ./x.bin http://localhost:8080/x.bin
```

macOS Finder: **Go → Connect to Server…**, `http://host:8080`, then the Basic credentials.

---

## Security

- **Run behind a TLS-terminating reverse proxy.** The server speaks **plain HTTP**; without TLS in front, Basic credentials and Bearer tokens travel in cleartext. Terminate TLS at nginx/Caddy/Traefik and forward to the WebDAV and Management ports.
- **`SECRET_KEY` must be high-entropy.** It is SHA-256-derived into an AES-256-GCM key that encrypts every bot token at rest. Generate it with `openssl rand -hex 32` and treat it like a master key — losing or rotating it renders stored bot tokens undecryptable.
- **Passwords** are hashed with **argon2id** (PHC format). Authentication runs a constant decoy hash on the unknown-user path to neutralise username-enumeration timing, and caches a successful argon2id verification briefly (invalidated by any password change) so WebDAV's per-request Basic resends stay cheap.
- **Bearer tokens** are stored only as their **SHA-256** hash; the plaintext is shown once at creation and never again.
- **Strict per-user isolation** — namespaces are scoped per user at the database layer; Telegram-side bytes are shared but never cross-accessible through the API.
- **Per-blob SHA-256 integrity** — corrupt or substituted bytes from Telegram are detected and rejected (fail-closed); they are never served or cached.

---

# Performance

tgwebdav stores file **bytes in Telegram** and keeps only **metadata in Postgres**. Understanding the numbers requires understanding the write path, which is **WAL-buffered**:

> A `PUT` does **not** upload to Telegram inline. The bytes are written into a Postgres **write-ahead buffer** (`wal_chunks`), the file is marked *buffered*, and the client's request completes — at Postgres speed, not Telegram speed. The file is **immediately durable and readable** from the WAL. A background **packer** then asynchronously groups buffered data into ~19 MiB blobs, uploads each blob to a Telegram channel (spread across multiple bots in parallel), records the resulting `file_id`s, and **deletes the WAL rows**. Once packed, the file is *stored* and its bytes live only in Telegram; Postgres retains a few hundred bytes of metadata per file. Reads are served from the WAL (before packing), from a local disk LRU cache (warm), or by downloading + SHA-256-verifying the blobs from Telegram (cold).

This means **write latency the client sees ≠ time-to-durable-in-Telegram**, and the two have very different throughput ceilings. The tables below measure each phase distinctly. All figures are framed against a **~100 Mbit/s (≈12.5 MB/s)** link.

> **Reproduce these yourself.** The tables below were measured on the setup described in [Test setup](#test-setup). Anyone can reproduce the methodology with [`scripts/benchmark.sh`](scripts/benchmark.sh) — a self-contained, Docker-based benchmark (it spins a throwaway Postgres, builds the binary, registers bots/channels via the Management API, and prints these same Markdown tables). Bring your own Telegram bots/channels via `BENCH_BOT_TOKENS` / `BENCH_CHANNEL_IDS` (every bot must be an admin of every channel). See the script header for all options. Your numbers will differ — pack-to-Telegram and cold-read throughput are bounded by the Telegram Bot API and your bot count, not by the machine.

### Throughput by file size

Representative figures (median of repeated runs). PUT/WAL, buffered, and warm reads are local (Postgres / disk) and are bounded by the machine, not the network. Pack-to-Telegram and cold reads are bounded by the **Telegram Bot API**.

| File size | PUT → WAL ingest | Buffered read (WAL) | Pack → Telegram | Cold read (from Telegram) | Warm read (disk cache) | Blobs |
|-----------|-----------------:|--------------------:|----------------:|--------------------------:|-----------------------:|------:|
| 4 KiB    | ~0.2 MB/s (≈18 ms/op) | ~1 MB/s | (idle-flush bound)¹ | ~220 ms/op (latency-bound) | ~1 MB/s | 1 (shared) |
| 256 KiB  | ~13 MB/s | ~50 MB/s | (idle-flush bound)¹ | ~340 ms/op | ~57 MB/s | 1 (shared) |
| 1 MiB    | ~45 MB/s | ~100 MB/s | (idle-flush bound)¹ | ~0.2–1.2 MB/s | ~220 MB/s | 1 (shared) |
| 5 MiB    | ~113 MB/s | ~160 MB/s | (idle-flush bound)¹ | ~0.7–3.0 MB/s | ~660 MB/s | 1 (shared) |
| 20 MiB   | ~139 MB/s | ~190 MB/s | **~6.2 MB/s** | ~2.2 MB/s | ~950 MB/s | 2 |
| 100 MiB  | ~133 MB/s | ~175 MB/s | **~5–8 MB/s** | ~2.5–3.1 MB/s | ~1.3 GB/s | 6 |

¹ Files ≤ ~19 MiB are batched into shared blobs and only sealed after the WAL idle timeout (`wal_idle_timeout_ms`, default 60 s in production — the benchmark lowers it so this phase measures upload speed, not the flush delay). Their per-file "pack time" therefore reflects the flush delay; true upload throughput is shown by multi-blob files and the sustained test below.

**Sustained pack-to-Telegram (packer kept busy):** 500 MiB (5 × 100 MiB) written into the WAL in **2.2 s (≈230 MB/s ingest)**, then fully uploaded to Telegram in **63 s → 7.9 MB/s**, using all 4 bots in parallel (91 blobs).

**Parallel-bot effect (single 100 MiB file):** 1 bot → 20.7 s (**4.8 MB/s**); 4 bots → 12.8–14.6 s (**~7–8 MB/s**). More bots raise aggregate upload throughput, with diminishing returns on a single small-blob-count file.

### Small-file request rate / concurrency

Single client, localhost; ops/sec and latency percentiles at three concurrency levels (zero errors at all levels).

| Operation | Concurrency | Throughput | p50 | p95 | p99 |
|-----------|------------:|-----------:|----:|----:|----:|
| Small-file `PUT` (4 KiB → WAL) | 8  | 1,144 ops/s (≈69 k/min)  | 6.9 ms | 8.3 ms | 8.7 ms |
| Small-file `PUT` (4 KiB → WAL) | 16 | 1,656 ops/s (≈99 k/min)  | 9.5 ms | 11 ms  | 14 ms |
| Small-file `PUT` (4 KiB → WAL) | 32 | 2,173 ops/s (≈130 k/min) | 13 ms  | 20 ms  | 36 ms |
| Warm `GET` (256 KiB, cached)   | 8  | 4,023 ops/s (≈241 k/min) | 1.9 ms | 2.6 ms | 2.9 ms |
| Warm `GET` (256 KiB, cached)   | 16 | 4,922 ops/s (≈295 k/min) | 3.2 ms | 4.2 ms | 4.9 ms |
| Warm `GET` (256 KiB, cached)   | 32 | 5,538 ops/s (≈332 k/min) | 5.3 ms | 7.3 ms | 8.6 ms |
| `PROPFIND` Depth:1 (300 entries) | 8  | 25 ops/s | 319 ms | 331 ms | 338 ms |
| `PROPFIND` Depth:1 (300 entries) | 16 | 32 ops/s | 495 ms | 511 ms | 514 ms |
| `PROPFIND` Depth:1 (300 entries) | 32 | 38 ops/s | 750 ms | 1,157 ms | 1,303 ms |

`PROPFIND` cost scales with directory size (full child listing + XML serialization) and degrades under concurrency; keep directories modest or use shallow listings for large trees.

### Storage efficiency

After packing all test data and polling until the WAL was empty (all nodes *stored*, `wal_chunks` rows = 0):

| Metric | Value |
|--------|-------|
| Logical data stored | **1.51 GiB** (3,158 files) |
| Bytes living in Telegram (Σ blob sizes) | **1.51 GiB** across **104 blobs** (avg 14.9 MiB/blob) |
| Telegram bytes ÷ logical bytes | **1.0000×** — no permanent duplication |
| `wal_chunks` live rows / bytes, after packing | **0 rows / 0 bytes** (peaks at the full file size mid-pack, then drains to zero) |
| **Postgres metadata total** (`nodes`+`extents`+`blobs`+`blob_bot_files`, incl. indexes/toast) | **3.61 MiB** |
| **Postgres bytes per MiB of stored file** | **≈2.4 KiB/MiB (0.23%)** |
| Metadata per file | ≈1.2 KB/file |

The metadata-per-MiB figure here is a **worst case**: 3,109 of the 3,158 files were 4 KiB, so node rows dominate; with realistically sized files the percentage is far lower. The result is unambiguous: **heavy bytes live in Telegram, Postgres holds only metadata, and the WAL is a transient buffer that returns to zero after packing.**

### Bottleneck summary (vs the ~12.5 MB/s link)

- **Writes (client-visible):** WAL ingest reaches ~130–230 MB/s — **far above** the 12.5 MB/s link. A PUT is never link- or Telegram-bound; it returns at Postgres speed.
- **Durability (background pack):** ~5 MB/s per bot, ~8 MB/s aggregate with 4 bots — **below** the link. **The Telegram Bot API per-bot upload rate is the true write bottleneck**, not the 100 Mbit link. Adding bots raises this ceiling (restart required — see Limitations).
- **Cold reads:** ~0.2–3 MB/s, latency-dominated for small files (~220–350 ms round-trip per blob) — **below** the link; Telegram download (per bot, with parallel prefetch for multi-blob files) is the bottleneck.
- **Warm reads:** up to ~1.3 GB/s from local disk cache — the link, not the server, is the limit; once cached, content streams at full line rate.

### Test setup

Apple M1 Pro (10 cores, 16 GiB RAM), macOS; Go 1.26. Postgres 17 (alpine) in Docker, exposed on `localhost:5433`, pool max 50 conns. tgwebdav single binary; WebDAV on `127.0.0.1:18080`, Management API on `127.0.0.1:18081`; 1 GiB disk LRU blob cache. Storage backend: real Telegram with **4 bots** across **2 channels**; `blob_max_size` ≈ 19 MiB; `wal_idle_timeout_ms` lowered for the small-file pack measurement (production default is **60000**). Throughput measured against a **~100 Mbit/s (≈12.5 MB/s)** reference link. SHA-256 verified on all reads.

---

## Limitations & caveats

- **Experimental.** Telegram is not a filesystem. A channel evicts its oldest messages once it exceeds roughly **one million messages**, and deletions are **irreversible**. Treat tgwebdav as an experiment or cold archive, not authoritative storage.
- **`MaxFileSize` is currently not enforced.** The setting exists (and a 413 mapping is wired up), but no write path rejects on it today; per-file size is effectively bounded by the user's storage quota.
- **Upload parallelism is fixed at server start.** The packer's upload concurrency defaults to the number of **enabled bots** at the moment `server` starts (clamped to 1–16). **Adding bots does not increase upload concurrency until you restart the server.**
- **`PROPFIND` cost grows with directory size** (full child listing + XML serialization) and degrades under concurrency. Keep directories modest for large trees.
- **Whole blobs are held in memory during transfer.** A blob (≤ ~19 MiB) is buffered in memory while uploading or downloading; memory pressure scales with concurrent transfers, not with total file size.

---

## Development

The project uses [Task](https://taskfile.dev) for common workflows:

```sh
task generate     # run all code generators: go generate (go-enum), sqlc, oapi-codegen
task test         # go test ./... -race -count=1  (uses testcontainers; needs Docker)
task lint         # golangci-lint via go run
task migrate:up   # apply pending migrations   (DATABASE_URL from env, dbmate)
task migrate:down # roll back the last migration
task migrate:new -- <name>   # scaffold a new migration
task schema:dump  # spin up throwaway Postgres, migrate, dump db/schema.sql, tear down
```

`make build` cross-compiles the binary into `./dist` for linux (amd64/arm64/arm), darwin (amd64/arm64), windows (amd64/arm64) and freebsd/amd64; `make run` runs `server`; `make clean` removes `dist/`.

### Architecture & tooling

- **Layered:** `handler → service → repository / client`. Handlers depend only on services, the model, the generated Management interface and the embedded OpenAPI spec — never on repositories or clients.
- **Data:** `sqlc` generates type-safe queries; `pgx/v5` (pgxpool) is the driver; `dbmate` runs the embedded migrations (also auto-applied on `server` boot).
- **HTTP:** `chi/v5` middleware for request-id, structured slog logging and panic recovery; `golang.org/x/net/webdav` for the WebDAV core.
- **CLI:** `cobra` commands with `viper` for flag/env resolution.
- **Codegen:** `oapi-codegen` (Management server interface from `api/openapi/management.yaml`), `sqlc`, and `go-enum` for typed enums.

---

## License

Licensed under the **Apache License, Version 2.0**. See [LICENSE](LICENSE).

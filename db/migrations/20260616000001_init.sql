-- migrate:up

CREATE TABLE users (
    id            uuid PRIMARY KEY,
    login         text NOT NULL UNIQUE,
    password_hash text NOT NULL,
    is_admin      boolean NOT NULL DEFAULT false,
    quota_bytes   bigint NOT NULL DEFAULT 0,
    bandwidth_bps bigint NOT NULL DEFAULT 0,
    rate_per_min  integer NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_tokens (
    id           uuid PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash   text NOT NULL UNIQUE,
    name         text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz
);
CREATE INDEX idx_api_tokens_user ON api_tokens(user_id);

CREATE TABLE bots (
    id                uuid PRIMARY KEY,
    username          text NOT NULL DEFAULT '',
    token_sha         text NOT NULL UNIQUE,
    token_enc         bytea NOT NULL,
    enabled           boolean NOT NULL DEFAULT true,
    unavailable_until timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE channels (
    id                 uuid PRIMARY KEY,
    tg_chat_id         bigint NOT NULL UNIQUE,
    title              text NOT NULL DEFAULT '',
    message_counter    bigint NOT NULL DEFAULT 0,
    eviction_threshold bigint NOT NULL DEFAULT 900000,
    available          boolean NOT NULL DEFAULT true,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE bot_channel (
    bot_id     uuid NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    channel_id uuid NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    member     boolean NOT NULL DEFAULT false,
    checked_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (bot_id, channel_id)
);
CREATE INDEX idx_bot_channel_channel ON bot_channel(channel_id);

CREATE TABLE blobs (
    id           uuid PRIMARY KEY,
    channel_id   uuid NOT NULL REFERENCES channels(id) ON DELETE RESTRICT,
    message_id   bigint NOT NULL,
    message_seq  bigint NOT NULL DEFAULT 0,
    size         bigint NOT NULL DEFAULT 0,
    content_hash bytea NOT NULL,
    state        integer NOT NULL,
    refcount     bigint NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    sealed_at    timestamptz
);
CREATE INDEX idx_blobs_channel ON blobs(channel_id);
CREATE INDEX idx_blobs_state ON blobs(state);
CREATE INDEX idx_blobs_collectable ON blobs(refcount) WHERE refcount <= 0;
CREATE INDEX idx_blobs_channel_seq ON blobs(channel_id, message_seq);

CREATE TABLE blob_bot_files (
    blob_id        uuid NOT NULL REFERENCES blobs(id) ON DELETE CASCADE,
    bot_id         uuid NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    file_id        text NOT NULL,
    file_unique_id text NOT NULL DEFAULT '',
    fetched_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (blob_id, bot_id)
);

CREATE TABLE nodes (
    id                 uuid PRIMARY KEY,
    user_id            uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_id          uuid REFERENCES nodes(id) ON DELETE CASCADE,
    name               text NOT NULL,
    path               text NOT NULL,
    is_dir             boolean NOT NULL,
    size               bigint NOT NULL DEFAULT 0,
    content_hash       text NOT NULL DEFAULT '',
    etag               text NOT NULL DEFAULT '',
    content_type       text NOT NULL DEFAULT '',
    state              integer NOT NULL DEFAULT 2,
    packer_lease_owner text NOT NULL DEFAULT '',
    packer_lease_until timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    modified_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, path)
);
CREATE INDEX idx_nodes_user_parent ON nodes(user_id, parent_id);
CREATE INDEX idx_nodes_user ON nodes(user_id);
CREATE INDEX idx_nodes_pack ON nodes(packer_lease_until) WHERE state = 1;

CREATE TABLE extents (
    id          uuid PRIMARY KEY,
    node_id     uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    seq         bigint NOT NULL,
    file_offset bigint NOT NULL,
    length      bigint NOT NULL,
    blob_id     uuid NOT NULL REFERENCES blobs(id) ON DELETE RESTRICT,
    blob_offset bigint NOT NULL
);
CREATE INDEX idx_extents_node ON extents(node_id, seq);
CREATE INDEX idx_extents_blob ON extents(blob_id);

CREATE TABLE wal_chunks (
    id         uuid PRIMARY KEY,
    node_id    uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    seq        bigint NOT NULL,
    data       bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (node_id, seq)
);
CREATE INDEX idx_wal_node_seq ON wal_chunks(node_id, seq);

CREATE TABLE events (
    id      uuid PRIMARY KEY,
    ts      timestamptz NOT NULL DEFAULT now(),
    kind    text NOT NULL,
    message text NOT NULL DEFAULT '',
    ref     text NOT NULL DEFAULT ''
);
CREATE INDEX idx_events_ts ON events(ts DESC);
CREATE INDEX idx_events_kind ON events(kind);

CREATE TABLE stat_samples (
    id     uuid PRIMARY KEY,
    ts     timestamptz NOT NULL DEFAULT now(),
    metric text NOT NULL,
    label  text NOT NULL DEFAULT '',
    value  double precision NOT NULL
);
CREATE INDEX idx_stat_samples_metric ON stat_samples(metric, label, ts DESC);

CREATE TABLE settings (
    id                         integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    blob_max_size              bigint NOT NULL,
    wal_idle_timeout_ms        bigint NOT NULL,
    max_file_size              bigint NOT NULL DEFAULT 0,
    default_eviction_threshold bigint NOT NULL DEFAULT 900000,
    updated_at                 timestamptz NOT NULL DEFAULT now()
);
-- No seed row: the single settings row is created on first PUT /api/v1/settings.
-- Until then GetSettings returns the built-in defaults (model.DefaultSettings),
-- so code is the single source of truth and the seed can never drift from it.

-- migrate:down

DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS stat_samples;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS wal_chunks;
DROP TABLE IF EXISTS extents;
DROP TABLE IF EXISTS nodes;
DROP TABLE IF EXISTS blob_bot_files;
DROP TABLE IF EXISTS blobs;
DROP TABLE IF EXISTS bot_channel;
DROP TABLE IF EXISTS channels;
DROP TABLE IF EXISTS bots;
DROP TABLE IF EXISTS api_tokens;
DROP TABLE IF EXISTS users;

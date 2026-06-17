-- name: GetSettings :one
SELECT * FROM settings WHERE id = 1;

-- name: UpsertSettings :exec
INSERT INTO settings (
    id, blob_max_size, wal_idle_timeout_ms, max_file_size, default_eviction_threshold, updated_at
) VALUES (
    1, $1, $2, $3, $4, $5
)
ON CONFLICT (id) DO UPDATE
SET blob_max_size = excluded.blob_max_size,
    wal_idle_timeout_ms = excluded.wal_idle_timeout_ms,
    max_file_size = excluded.max_file_size,
    default_eviction_threshold = excluded.default_eviction_threshold,
    updated_at = excluded.updated_at;

-- name: UpsertBlobBotFile :exec
INSERT INTO blob_bot_files (
    blob_id, bot_id, file_id, file_unique_id, fetched_at
) VALUES (
    $1, $2, $3, $4, $5
)
ON CONFLICT (blob_id, bot_id) DO UPDATE
SET file_id = excluded.file_id,
    file_unique_id = excluded.file_unique_id,
    fetched_at = excluded.fetched_at;

-- name: GetBlobBotFile :one
SELECT * FROM blob_bot_files WHERE blob_id = $1 AND bot_id = $2;

-- name: ListBlobBotFilesByBlob :many
SELECT * FROM blob_bot_files WHERE blob_id = $1;

-- name: DeleteBlobBotFilesByBlob :exec
DELETE FROM blob_bot_files WHERE blob_id = $1;

-- name: DeleteBlobBotFilesByBot :exec
DELETE FROM blob_bot_files WHERE bot_id = $1;

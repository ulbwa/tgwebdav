-- name: CreateBlob :exec
INSERT INTO blobs (
    id, channel_id, message_id, message_seq, size, state, refcount, created_at, sealed_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
);

-- name: GetBlobByID :one
SELECT * FROM blobs WHERE id = $1;

-- name: UpdateBlob :execrows
UPDATE blobs
SET channel_id = $2,
    message_id = $3,
    message_seq = $4,
    size = $5,
    state = $6,
    refcount = $7,
    sealed_at = $8
WHERE id = $1;

-- name: SetBlobState :execrows
UPDATE blobs SET state = $2 WHERE id = $1;

-- name: AddBlobRefcount :one
UPDATE blobs
SET refcount = refcount + $2
WHERE id = $1
RETURNING refcount;

-- name: ListBlobsByChannel :many
SELECT * FROM blobs WHERE channel_id = $1 ORDER BY message_seq;

-- name: ListBlobsByState :many
SELECT * FROM blobs WHERE state = $1 ORDER BY created_at;

-- name: ListCollectableBlobs :many
SELECT * FROM blobs
WHERE state = $1
  AND refcount <= 0
  AND created_at < now() - interval '10 minutes'
ORDER BY created_at
LIMIT $2;

-- name: MarkChannelBlobsUnavailable :exec
UPDATE blobs
SET state = $2
WHERE channel_id = $1 AND state <> $3;

-- name: MarkChannelBlobsAvailable :exec
UPDATE blobs
SET state = $2
WHERE channel_id = $1 AND state = $3;

-- name: EvictBlobsOlderThan :execrows
UPDATE blobs
SET state = $4
WHERE channel_id = $1 AND message_seq < $2 AND state <> $3;

-- name: DeleteBlob :execrows
DELETE FROM blobs WHERE id = $1;

-- name: CountBlobs :one
SELECT count(*) FROM blobs;

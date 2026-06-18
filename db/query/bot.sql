-- name: CreateBot :exec
INSERT INTO bots (
    id, username, token_sha, token_enc, enabled, unavailable_until, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
);

-- name: UpdateBot :execrows
UPDATE bots
SET username = $2,
    token_sha = $3,
    token_enc = $4,
    enabled = $5,
    unavailable_until = $6
WHERE id = $1;

-- name: DeleteBot :execrows
DELETE FROM bots WHERE id = $1;

-- name: GetBotByID :one
SELECT * FROM bots WHERE id = $1;

-- name: ListBotsByIDs :many
-- Batch load bots by id (one round-trip instead of GetByID per member bot on the
-- blob read candidate path).
SELECT * FROM bots WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: GetBotByUsername :one
SELECT * FROM bots WHERE username = $1;

-- name: ListBots :many
SELECT * FROM bots ORDER BY created_at;

-- name: SetBotUnavailableUntil :execrows
UPDATE bots SET unavailable_until = $2 WHERE id = $1;

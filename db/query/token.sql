-- name: CreateAPIToken :exec
INSERT INTO api_tokens (
    id, user_id, token_hash, name, created_at, last_used_at
) VALUES (
    $1, $2, $3, $4, $5, $6
);

-- name: GetAPITokenByHash :one
SELECT * FROM api_tokens WHERE token_hash = $1;

-- name: ListAPITokensByUser :many
SELECT * FROM api_tokens WHERE user_id = $1 ORDER BY created_at DESC;

-- name: DeleteAPIToken :execrows
DELETE FROM api_tokens WHERE id = $1;

-- name: TouchAPITokenLastUsed :execrows
UPDATE api_tokens SET last_used_at = $2 WHERE id = $1;

-- name: CreateUser :exec
INSERT INTO users (
    id, login, password_hash, is_admin, quota_bytes, bandwidth_bps, rate_per_min, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
);

-- name: UpdateUser :execrows
UPDATE users
SET login = $2,
    password_hash = $3,
    is_admin = $4,
    quota_bytes = $5,
    bandwidth_bps = $6,
    rate_per_min = $7
WHERE id = $1;

-- name: DeleteUser :execrows
DELETE FROM users WHERE id = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByLogin :one
SELECT * FROM users WHERE login = $1;

-- name: ListUsers :many
SELECT * FROM users ORDER BY created_at;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: LogEvent :exec
INSERT INTO events (
    id, ts, kind, message, ref
) VALUES (
    $1, $2, $3, $4, $5
);

-- name: CountEvents :one
SELECT count(*) FROM events
WHERE (sqlc.arg(kind)::text = '' OR kind = sqlc.arg(kind)::text);

-- name: ListEvents :many
SELECT * FROM events
WHERE (sqlc.arg(kind)::text = '' OR kind = sqlc.arg(kind)::text)
ORDER BY ts DESC
LIMIT sqlc.narg(row_limit)::int
OFFSET sqlc.narg(row_offset)::int;

-- name: CreateChannel :exec
INSERT INTO channels (
    id, tg_chat_id, title, message_counter, eviction_threshold, available, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
);

-- name: UpdateChannel :execrows
UPDATE channels
SET tg_chat_id = $2,
    title = $3,
    message_counter = $4,
    eviction_threshold = $5,
    available = $6
WHERE id = $1;

-- name: DeleteChannel :execrows
DELETE FROM channels WHERE id = $1;

-- name: GetChannelByID :one
SELECT * FROM channels WHERE id = $1;

-- name: GetChannelByChatID :one
SELECT * FROM channels WHERE tg_chat_id = $1;

-- name: ListChannels :many
SELECT * FROM channels ORDER BY created_at;

-- name: IncrementChannelCounter :one
UPDATE channels
SET message_counter = message_counter + $2
WHERE id = $1
RETURNING message_counter;

-- name: SetChannelAvailable :execrows
UPDATE channels SET available = $2 WHERE id = $1;

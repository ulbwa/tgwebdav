-- name: UpsertBotChannel :exec
INSERT INTO bot_channel (
    bot_id, channel_id, member, checked_at
) VALUES (
    $1, $2, $3, $4
)
ON CONFLICT (bot_id, channel_id) DO UPDATE
SET member = excluded.member,
    checked_at = excluded.checked_at;

-- name: GetBotChannel :one
SELECT * FROM bot_channel WHERE bot_id = $1 AND channel_id = $2;

-- name: ListBotChannelsByChannel :many
SELECT * FROM bot_channel WHERE channel_id = $1;

-- name: ListBotChannelsByBot :many
SELECT * FROM bot_channel WHERE bot_id = $1;

-- name: DeleteBotChannelsByBot :exec
DELETE FROM bot_channel WHERE bot_id = $1;

-- name: DeleteBotChannelsByChannel :exec
DELETE FROM bot_channel WHERE channel_id = $1;

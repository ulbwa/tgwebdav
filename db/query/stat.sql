-- name: RecordStat :exec
INSERT INTO stat_samples (
    id, ts, metric, label, value
) VALUES (
    $1, $2, $3, $4, $5
);

-- name: QueryStats :many
SELECT * FROM stat_samples
WHERE metric = $1 AND label = $2 AND ts >= $3 AND ts <= $4
ORDER BY ts;

-- name: LatestStat :one
SELECT * FROM stat_samples
WHERE metric = $1 AND label = $2
ORDER BY ts DESC
LIMIT 1;

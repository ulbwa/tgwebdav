-- name: AppendWALChunk :exec
INSERT INTO wal_chunks (
    id, node_id, seq, data, created_at
) VALUES (
    $1, $2, $3, $4, $5
);

-- name: ListWALChunksByNode :many
SELECT * FROM wal_chunks WHERE node_id = $1 ORDER BY seq;

-- name: ListWALChunksByNodeSeqRange :many
SELECT * FROM wal_chunks
WHERE node_id = $1 AND seq >= $2 AND seq <= $3
ORDER BY seq;

-- name: WALSizeByNode :one
SELECT COALESCE(SUM(octet_length(data)), 0)::bigint FROM wal_chunks
WHERE node_id = $1;

-- name: DeleteWALChunksByNode :exec
DELETE FROM wal_chunks WHERE node_id = $1;

-- name: CreateExtents :copyfrom
INSERT INTO extents (
    id, node_id, seq, file_offset, length, blob_id, blob_offset
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
);

-- name: ListExtentsByNode :many
SELECT * FROM extents WHERE node_id = $1 ORDER BY seq;

-- name: DeleteExtentsByNode :exec
DELETE FROM extents WHERE node_id = $1;

-- name: ListBlobIDsByNode :many
SELECT DISTINCT blob_id FROM extents WHERE node_id = $1;

-- name: CopyExtentsForNode :exec
INSERT INTO extents (id, node_id, seq, file_offset, length, blob_id, blob_offset)
SELECT gen_random_uuid(), $1, src.seq, src.file_offset, src.length, src.blob_id, src.blob_offset
FROM extents src
WHERE src.node_id = $2;

-- name: ListNodesSolelyOnBlob :many
SELECT node_id
FROM extents
GROUP BY node_id
HAVING bool_and(blob_id = $1);

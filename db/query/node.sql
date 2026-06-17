-- name: CreateNode :exec
INSERT INTO nodes (
    id, user_id, parent_id, name, path, is_dir, size,
    content_hash, etag, content_type, state, created_at, modified_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10, $11, $12, $13
);

-- name: UpdateNode :execrows
UPDATE nodes
SET parent_id = $2,
    name = $3,
    path = $4,
    is_dir = $5,
    size = $6,
    content_hash = $7,
    etag = $8,
    content_type = $9,
    state = $10,
    modified_at = $11
WHERE id = $1;

-- name: DeleteNode :execrows
DELETE FROM nodes WHERE id = $1;

-- name: GetNodeByID :one
SELECT * FROM nodes WHERE id = $1;

-- name: GetNodeByPath :one
SELECT * FROM nodes WHERE user_id = $1 AND path = $2;

-- name: ListChildren :many
SELECT * FROM nodes
WHERE user_id = $1 AND parent_id = $2
ORDER BY name;

-- name: ListSubtree :many
SELECT * FROM nodes
WHERE user_id = $1 AND (path = $2 OR path LIKE $3 ESCAPE '\')
ORDER BY path;

-- name: CountChildren :one
SELECT count(*) FROM nodes WHERE parent_id = $1;

-- name: SumSizeByUser :one
SELECT COALESCE(SUM(size), 0)::bigint FROM nodes
WHERE user_id = $1 AND is_dir = false;

-- name: ClaimBufferedForPacking :many
UPDATE nodes
SET packer_lease_owner = $1, packer_lease_until = $2
WHERE id IN (
    SELECT id FROM nodes
    WHERE state = 1
      AND (packer_lease_until IS NULL OR packer_lease_until < now())
    ORDER BY modified_at
    LIMIT $3
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: ReleaseLease :execrows
UPDATE nodes
SET packer_lease_owner = '', packer_lease_until = NULL
WHERE id = $1;

-- name: MarkStoredIfOwner :one
UPDATE nodes
SET state = 2, packer_lease_owner = '', packer_lease_until = NULL
WHERE id = $1 AND state = 1 AND packer_lease_owner = $2
RETURNING id;

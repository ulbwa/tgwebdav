package repository

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// insertNodeForWAL inserts a buffered file node so WAL chunks have a valid FK
// parent, returning its id.
func insertNodeForWAL(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	userID := insertUser(t, pool)
	noderepo := NewNodeRepository(pool)
	n := &model.Node{
		UserID: userID,
		Name:   "walfile",
		Path:   "/walfile-" + uuid.NewString(),
		IsDir:  false,
		State:  model.NodeStateWriting,
	}
	if err := noderepo.Create(context.Background(), n); err != nil {
		t.Fatalf("insert node for wal: %v", err)
	}
	return n.ID
}

func TestWALRepository_AppendAndEachChunk_SeqOrder(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewWALRepository(pool)
	ctx := context.Background()
	nodeID := insertNodeForWAL(t, pool)

	// Append out of seq order to prove EachChunk returns them ordered by seq.
	want := map[int64][]byte{
		0: []byte("hello "),
		1: []byte("world"),
		2: []byte("!"),
	}
	for _, seq := range []int64{2, 0, 1} {
		c := &model.WALChunk{NodeID: nodeID, Seq: seq, Data: want[seq]}
		if err := repo.AppendChunk(ctx, c); err != nil {
			t.Fatalf("append seq %d: %v", seq, err)
		}
		if c.ID == uuid.Nil {
			t.Fatalf("append seq %d: id not assigned", seq)
		}
		if c.CreatedAt.IsZero() {
			t.Fatalf("append seq %d: created_at not set", seq)
		}
	}

	var gotSeqs []int64
	var assembled []byte
	err := repo.EachChunk(ctx, nodeID, func(c model.WALChunk) error {
		gotSeqs = append(gotSeqs, c.Seq)
		assembled = append(assembled, c.Data...)
		return nil
	})
	if err != nil {
		t.Fatalf("each chunk: %v", err)
	}
	if len(gotSeqs) != 3 || gotSeqs[0] != 0 || gotSeqs[1] != 1 || gotSeqs[2] != 2 {
		t.Fatalf("seq order = %v, want [0 1 2]", gotSeqs)
	}
	if string(assembled) != "hello world!" {
		t.Fatalf("assembled = %q, want %q", assembled, "hello world!")
	}
}

func TestWALRepository_EachChunk_FnErrorStops(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewWALRepository(pool)
	ctx := context.Background()
	nodeID := insertNodeForWAL(t, pool)

	for seq := int64(0); seq < 3; seq++ {
		if err := repo.AppendChunk(ctx, &model.WALChunk{NodeID: nodeID, Seq: seq, Data: []byte{byte(seq)}}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	sentinel := errors.New("stop")
	calls := 0
	err := repo.EachChunk(ctx, nodeID, func(c model.WALChunk) error {
		calls++
		if c.Seq == 1 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("each chunk err = %v, want sentinel", err)
	}
	if calls != 2 {
		t.Fatalf("fn called %d times, want 2 (stop at seq 1)", calls)
	}
}

func TestWALRepository_AppendChunk_DuplicateSeq(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewWALRepository(pool)
	ctx := context.Background()
	nodeID := insertNodeForWAL(t, pool)

	if err := repo.AppendChunk(ctx, &model.WALChunk{NodeID: nodeID, Seq: 0, Data: []byte("a")}); err != nil {
		t.Fatalf("first append: %v", err)
	}
	err := repo.AppendChunk(ctx, &model.WALChunk{NodeID: nodeID, Seq: 0, Data: []byte("b")})
	if !errors.Is(err, model.ErrAlreadyExists) {
		t.Fatalf("duplicate seq err = %v, want ErrAlreadyExists", err)
	}
}

func TestWALRepository_ReadRange(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewWALRepository(pool)
	ctx := context.Background()
	nodeID := insertNodeForWAL(t, pool)

	// Production always writes fixed model.WALChunkSize chunks (only the final
	// chunk is smaller), and ReadRange relies on that seq->offset invariant to
	// fetch only the chunks a window touches. So the fixture mirrors it: three
	// full 1 MiB chunks plus a smaller tail chunk. Each chunk is filled with a
	// distinct byte so any boundary mistake shows up as wrong content.
	sz := model.WALChunkSize
	fill := func(b byte, n int64) []byte {
		out := make([]byte, n)
		for i := range out {
			out[i] = b
		}
		return out
	}
	chunks := [][]byte{
		fill('A', sz),   // seq 0: bytes [0, sz)
		fill('B', sz),   // seq 1: bytes [sz, 2*sz)
		fill('C', sz),   // seq 2: bytes [2*sz, 3*sz)
		fill('D', sz/2), // seq 3: bytes [3*sz, 3*sz+sz/2)  (smaller final chunk)
	}
	for seq, data := range chunks {
		if err := repo.AppendChunk(ctx, &model.WALChunk{NodeID: nodeID, Seq: int64(seq), Data: data}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	var full []byte
	for _, c := range chunks {
		full = append(full, c...)
	}
	total := int64(len(full)) // 3*sz + sz/2

	// expect returns full[offset:min(offset+length, total)] (clamped, empty when
	// the range is degenerate), so each case's want is derived from the fixture.
	expect := func(offset, length int64) []byte {
		if length <= 0 || offset < 0 || offset >= total {
			return []byte{}
		}
		end := offset + length
		if end > total {
			end = total
		}
		return full[offset:end]
	}

	tests := []struct {
		name           string
		offset, length int64
	}{
		{"whole file", 0, total},
		{"within first chunk", 1, 3},
		{"spanning two chunks", sz - 2, 4},          // crosses seq0/seq1 boundary
		{"spanning three chunks", sz - 1, 2*sz + 2}, // seq0 -> seq2
		{"middle window", sz + 100, 200},            // entirely inside seq1
		{"tail past end clamps", total - 2, 100},    // reads last 2 bytes of seq3
		{"exactly at boundary", sz, sz},             // exactly all of seq1
		{"zero length", 5, 0},
		{"offset beyond end", total + 100, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := repo.ReadRange(ctx, nodeID, tc.offset, tc.length)
			if err != nil {
				t.Fatalf("read range: %v", err)
			}
			want := expect(tc.offset, tc.length)
			if !bytes.Equal(got, want) {
				t.Fatalf("ReadRange(%d,%d) len = %d, want %d", tc.offset, tc.length, len(got), len(want))
			}
		})
	}
}

func TestWALRepository_SizeByNode(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewWALRepository(pool)
	ctx := context.Background()
	nodeID := insertNodeForWAL(t, pool)

	// No chunks yet -> 0 (COALESCE).
	if size, err := repo.SizeByNode(ctx, nodeID); err != nil || size != 0 {
		t.Fatalf("empty size = (%d, %v), want (0, nil)", size, err)
	}

	total := int64(0)
	for seq, data := range [][]byte{[]byte("12345"), []byte("678"), []byte("90")} {
		total += int64(len(data))
		if err := repo.AppendChunk(ctx, &model.WALChunk{NodeID: nodeID, Seq: int64(seq), Data: data}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	size, err := repo.SizeByNode(ctx, nodeID)
	if err != nil {
		t.Fatalf("size by node: %v", err)
	}
	if size != total {
		t.Fatalf("size = %d, want %d", size, total)
	}
}

func TestWALRepository_DeleteByNode(t *testing.T) {
	pool := setupTestPool(t)
	repo := NewWALRepository(pool)
	ctx := context.Background()
	nodeID := insertNodeForWAL(t, pool)
	otherNode := insertNodeForWAL(t, pool)

	for seq := int64(0); seq < 3; seq++ {
		if err := repo.AppendChunk(ctx, &model.WALChunk{NodeID: nodeID, Seq: seq, Data: []byte{byte(seq)}}); err != nil {
			t.Fatalf("append node: %v", err)
		}
	}
	// Chunk on another node must survive the delete.
	if err := repo.AppendChunk(ctx, &model.WALChunk{NodeID: otherNode, Seq: 0, Data: []byte("keep")}); err != nil {
		t.Fatalf("append other: %v", err)
	}

	if err := repo.DeleteByNode(ctx, nodeID); err != nil {
		t.Fatalf("delete by node: %v", err)
	}
	if size, err := repo.SizeByNode(ctx, nodeID); err != nil || size != 0 {
		t.Fatalf("size after delete = (%d, %v), want (0, nil)", size, err)
	}
	count := 0
	if err := repo.EachChunk(ctx, nodeID, func(model.WALChunk) error { count++; return nil }); err != nil {
		t.Fatalf("each after delete: %v", err)
	}
	if count != 0 {
		t.Fatalf("chunks remaining after delete = %d, want 0", count)
	}
	// DeleteByNode is idempotent: deleting again is not an error.
	if err := repo.DeleteByNode(ctx, nodeID); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	// Other node's chunk is untouched.
	if size, err := repo.SizeByNode(ctx, otherNode); err != nil || size != 4 {
		t.Fatalf("other node size = (%d, %v), want (4, nil)", size, err)
	}
}

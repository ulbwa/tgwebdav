package webdavfs

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/domain"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"":                  "/",
		"/":                 "/",
		"/foo/":             "/foo",
		"foo/bar":           "/foo/bar",
		"/foo/../bar":       "/bar",
		"/../etc/passwd":    "/etc/passwd", // cannot escape root
		"/../../../../x":    "/x",
		"/a//b":             "/a/b",
		"/foo/./bar":        "/foo/bar",
		"/foo/bar/../../qux": "/qux",
	}
	for in, want := range cases {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPathHasPrefix(t *testing.T) {
	cases := []struct {
		child, parent string
		want          bool
	}{
		{"/a/b", "/a", true},
		{"/a", "/a", true},
		{"/ab", "/a", false},
		{"/a/b/c", "/a/b", true},
		{"/x", "/a", false},
	}
	for _, c := range cases {
		if got := pathHasPrefix(c.child, c.parent); got != c.want {
			t.Errorf("pathHasPrefix(%q,%q) = %v, want %v", c.child, c.parent, got, c.want)
		}
	}
}

type fakeBlobReader struct{ data map[uuid.UUID][]byte }

func (f fakeBlobReader) ReadBlob(_ context.Context, id uuid.UUID) ([]byte, error) {
	b, ok := f.data[id]
	if !ok {
		return nil, domain.ErrBlobUnavailable
	}
	return b, nil
}

func TestExtentReaderAssembly(t *testing.T) {
	blobA, blobB := uuid.New(), uuid.New()
	fs := &FileSystem{blobs: fakeBlobReader{data: map[uuid.UUID][]byte{
		blobA: bytes.Repeat([]byte("A"), 10),
		blobB: bytes.Repeat([]byte("B"), 10),
	}}}
	// File of 15 bytes: 5 from blobA[3:8], 10 from blobB[0:10].
	extents := []domain.Extent{
		{FileOffset: 0, Length: 5, BlobID: blobA, BlobOffset: 3},
		{FileOffset: 5, Length: 10, BlobID: blobB, BlobOffset: 0},
	}
	const size = 15
	want := append(bytes.Repeat([]byte("A"), 5), bytes.Repeat([]byte("B"), 10)...)

	for _, start := range []int64{0, 3, 5, 14} {
		r := &extentReader{fs: fs, ctx: context.Background(), extents: extents, pos: start, size: size}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("start=%d ReadAll: %v", start, err)
		}
		if !bytes.Equal(got, want[start:]) {
			t.Errorf("start=%d got %q, want %q", start, got, want[start:])
		}
	}
}

func TestExtentReaderUnavailableBlob(t *testing.T) {
	fs := &FileSystem{blobs: fakeBlobReader{data: map[uuid.UUID][]byte{}}}
	r := &extentReader{
		fs:      fs,
		ctx:     context.Background(),
		extents: []domain.Extent{{FileOffset: 0, Length: 4, BlobID: uuid.New(), BlobOffset: 0}},
		pos:     0,
		size:    4,
	}
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("expected error reading from an unavailable blob")
	}
}

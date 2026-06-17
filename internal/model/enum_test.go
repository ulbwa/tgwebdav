package model_test

import (
	"database/sql/driver"
	"testing"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// TestNodeStateOrdering verifies that the int32 values match the documented ordering.
func TestNodeStateOrdering(t *testing.T) {
	if int32(model.NodeStateWriting) != 0 {
		t.Errorf("NodeStateWriting: want 0, got %d", int32(model.NodeStateWriting))
	}
	if int32(model.NodeStateBuffered) != 1 {
		t.Errorf("NodeStateBuffered: want 1, got %d", int32(model.NodeStateBuffered))
	}
	if int32(model.NodeStateStored) != 2 {
		t.Errorf("NodeStateStored: want 2, got %d", int32(model.NodeStateStored))
	}
}

// TestBlobStateOrdering verifies that the int32 values match the documented ordering.
func TestBlobStateOrdering(t *testing.T) {
	cases := []struct {
		state model.BlobState
		want  int32
	}{
		{model.BlobStateOpen, 0},
		{model.BlobStateSealed, 1},
		{model.BlobStateUploading, 2},
		{model.BlobStateStored, 3},
		{model.BlobStateUnavailable, 4},
		{model.BlobStatePermUnavailable, 5},
	}
	for _, c := range cases {
		if int32(c.state) != c.want {
			t.Errorf("BlobState %s: want %d, got %d", c.state, c.want, int32(c.state))
		}
	}
}

// TestNodeStateRoundTrip verifies ParseNodeState / String round-trip.
func TestNodeStateRoundTrip(t *testing.T) {
	for _, s := range model.NodeStateValues() {
		str := s.String()
		got, err := model.ParseNodeState(str)
		if err != nil {
			t.Errorf("ParseNodeState(%q) error: %v", str, err)
			continue
		}
		if got != s {
			t.Errorf("ParseNodeState(%q) = %v, want %v", str, got, s)
		}
	}
}

// TestBlobStateRoundTrip verifies ParseBlobState / String round-trip.
func TestBlobStateRoundTrip(t *testing.T) {
	for _, s := range model.BlobStateValues() {
		str := s.String()
		got, err := model.ParseBlobState(str)
		if err != nil {
			t.Errorf("ParseBlobState(%q) error: %v", str, err)
			continue
		}
		if got != s {
			t.Errorf("ParseBlobState(%q) = %v, want %v", str, got, s)
		}
	}
}

// TestBlobStateSQLRoundTrip verifies Value()/Scan(...) round-trip through
// database/sql/driver.
func TestBlobStateSQLRoundTrip(t *testing.T) {
	for _, s := range model.BlobStateValues() {
		val, err := s.Value()
		if err != nil {
			t.Errorf("BlobState(%v).Value() error: %v", s, err)
			continue
		}
		// Ensure Value() returns a valid driver.Value (string).
		if _, ok := val.(string); !ok {
			t.Errorf("BlobState(%v).Value() returned %T, want string", s, val)
			continue
		}

		var got model.BlobState
		if err := got.Scan(val); err != nil {
			t.Errorf("BlobState.Scan(%v) error: %v", val, err)
			continue
		}
		if got != s {
			t.Errorf("BlobState.Scan(%v) = %v, want %v", val, got, s)
		}
	}
}

// TestBlobStateScanInt64 verifies that Scan(int64) works (e.g. from Postgres integer column).
func TestBlobStateScanInt64(t *testing.T) {
	cases := []struct {
		val  driver.Value
		want model.BlobState
	}{
		{int64(0), model.BlobStateOpen},
		{int64(3), model.BlobStateStored},
		{int64(5), model.BlobStatePermUnavailable},
	}
	for _, c := range cases {
		var got model.BlobState
		if err := got.Scan(c.val); err != nil {
			t.Errorf("BlobState.Scan(%v) error: %v", c.val, err)
			continue
		}
		if got != c.want {
			t.Errorf("BlobState.Scan(%v) = %v, want %v", c.val, got, c.want)
		}
	}
}

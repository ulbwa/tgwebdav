package model_test

import (
	"database/sql/driver"
	"errors"
	"strings"
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

// TestParseBlobStateInvalid verifies the error branch of ParseBlobState (an
// unknown name) wraps the sentinel ErrInvalidBlobState.
func TestParseBlobStateInvalid(t *testing.T) {
	_, err := model.ParseBlobState("nope")
	if !errors.Is(err, model.ErrInvalidBlobState) {
		t.Fatalf("ParseBlobState(nope) err = %v, want ErrInvalidBlobState", err)
	}
}

// TestParseNodeStateInvalid verifies the error branch of ParseNodeState.
func TestParseNodeStateInvalid(t *testing.T) {
	_, err := model.ParseNodeState("nope")
	if !errors.Is(err, model.ErrInvalidNodeState) {
		t.Fatalf("ParseNodeState(nope) err = %v, want ErrInvalidNodeState", err)
	}
}

// TestBlobStateIsValid covers both branches of BlobState.IsValid.
func TestBlobStateIsValid(t *testing.T) {
	for _, s := range model.BlobStateValues() {
		if !s.IsValid() {
			t.Errorf("BlobState %v should be valid", s)
		}
	}
	if model.BlobState(99).IsValid() {
		t.Error("BlobState(99) should not be valid")
	}
}

// TestNodeStateIsValid covers both branches of NodeState.IsValid.
func TestNodeStateIsValid(t *testing.T) {
	for _, s := range model.NodeStateValues() {
		if !s.IsValid() {
			t.Errorf("NodeState %v should be valid", s)
		}
	}
	if model.NodeState(99).IsValid() {
		t.Error("NodeState(99) should not be valid")
	}
}

// TestBlobStateStringFallback covers the unknown-value branch of String, which
// returns the BlobState(%d) placeholder rather than panicking.
func TestBlobStateStringFallback(t *testing.T) {
	if got := model.BlobState(42).String(); !strings.Contains(got, "42") {
		t.Errorf("BlobState(42).String() = %q, want it to contain 42", got)
	}
	if got := model.NodeState(42).String(); !strings.Contains(got, "42") {
		t.Errorf("NodeState(42).String() = %q, want it to contain 42", got)
	}
}

// TestBlobStateScanNilAndBytes covers the nil and []byte scan branches.
func TestBlobStateScanNilAndBytes(t *testing.T) {
	got := model.BlobStatePermUnavailable
	if err := got.Scan(nil); err != nil {
		t.Fatalf("Scan(nil) err = %v", err)
	}
	if got != model.BlobStateOpen { // BlobState(0)
		t.Errorf("Scan(nil) = %v, want BlobStateOpen (zero value)", got)
	}

	var fromBytes model.BlobState
	if err := fromBytes.Scan([]byte("stored")); err != nil {
		t.Fatalf("Scan([]byte) err = %v", err)
	}
	if fromBytes != model.BlobStateStored {
		t.Errorf("Scan([]byte(stored)) = %v, want BlobStateStored", fromBytes)
	}

	// Scanning an invalid string surfaces the parse error.
	var bad model.BlobState
	if err := bad.Scan("bogus"); !errors.Is(err, model.ErrInvalidBlobState) {
		t.Errorf("Scan(bogus) err = %v, want ErrInvalidBlobState", err)
	}
}

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// fakeEventListRepo is an in-memory eventRepo that applies the same kind filter,
// newest-first ordering and limit/offset paging as the real repository, so the
// service's pass-through can be exercised end to end. (The Log-side fake lives
// in service_test.go as fakeEventRepo.)
type fakeEventListRepo struct {
	events  []model.Event // newest-first
	listErr error

	// capture of the last List call args.
	gotKind   string
	gotLimit  int
	gotOffset int
}

func (f *fakeEventListRepo) List(_ context.Context, kind string, limit, offset int) ([]model.Event, int64, error) {
	f.gotKind = kind
	f.gotLimit = limit
	f.gotOffset = offset
	if f.listErr != nil {
		return nil, 0, f.listErr
	}

	var filtered []model.Event
	for _, e := range f.events {
		if kind == "" || e.Kind == kind {
			filtered = append(filtered, e)
		}
	}
	total := int64(len(filtered))

	if offset > 0 {
		if offset >= len(filtered) {
			filtered = nil
		} else {
			filtered = filtered[offset:]
		}
	}
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered, total, nil
}

var _ eventRepo = (*fakeEventListRepo)(nil)

func ev(kind, msg string) model.Event {
	return model.Event{ID: uuid.New(), TS: time.Now(), Kind: kind, Message: msg}
}

func TestEventListNoFilterReturnsAllWithTotal(t *testing.T) {
	repo := &fakeEventListRepo{events: []model.Event{
		ev(model.EventBotDisabled, "a"),
		ev(model.EventChannelEvicted, "b"),
		ev(model.EventBotDisabled, "c"),
	}}
	svc := NewEventService(repo)

	got, total, err := svc.List(context.Background(), "", 0, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 3 {
		t.Errorf("total: got %d, want 3", total)
	}
	if len(got) != 3 {
		t.Errorf("len: got %d, want 3", len(got))
	}
}

func TestEventListKindFilter(t *testing.T) {
	repo := &fakeEventListRepo{events: []model.Event{
		ev(model.EventBotDisabled, "a"),
		ev(model.EventChannelEvicted, "b"),
		ev(model.EventBotDisabled, "c"),
	}}
	svc := NewEventService(repo)

	got, total, err := svc.List(context.Background(), model.EventBotDisabled, 0, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 2 {
		t.Errorf("total: got %d, want 2 (filtered count)", total)
	}
	if len(got) != 2 {
		t.Fatalf("len: got %d, want 2", len(got))
	}
	for _, e := range got {
		if e.Kind != model.EventBotDisabled {
			t.Errorf("filter leaked kind %q", e.Kind)
		}
	}
	if repo.gotKind != model.EventBotDisabled {
		t.Errorf("kind not forwarded to repo: got %q", repo.gotKind)
	}
}

func TestEventListPaging(t *testing.T) {
	var events []model.Event
	for i := 0; i < 5; i++ {
		events = append(events, ev(model.EventBotDisabled, ""))
	}
	repo := &fakeEventListRepo{events: events}
	svc := NewEventService(repo)

	// limit 2, offset 1 -> 2 of 5, total still 5.
	got, total, err := svc.List(context.Background(), "", 2, 1)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 5 {
		t.Errorf("total: got %d, want 5", total)
	}
	if len(got) != 2 {
		t.Errorf("len: got %d, want 2", len(got))
	}
	if repo.gotLimit != 2 || repo.gotOffset != 1 {
		t.Errorf("paging not forwarded: limit=%d offset=%d", repo.gotLimit, repo.gotOffset)
	}
}

func TestEventListPropagatesError(t *testing.T) {
	repo := &fakeEventListRepo{listErr: errors.New("boom")}
	svc := NewEventService(repo)
	if _, _, err := svc.List(context.Background(), "", 0, 0); err == nil {
		t.Fatal("List: expected error, got nil")
	}
}

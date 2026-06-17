package service

import (
	"context"
	"fmt"

	"github.com/ulbwa/tgwebdav/internal/model"
)

// eventRepo is the repository surface EventService needs. The real
// *repository.EventRepository satisfies this structurally. Only List is
// required: the Management API exposes events read-only (event writes go
// through the eventLogger interface used by the bot/channel/GC services).
type eventRepo interface {
	List(ctx context.Context, kind string, limit, offset int) ([]model.Event, int64, error)
}

// EventService exposes the audit/log event stream to the Management API.
type EventService struct {
	events eventRepo
}

// NewEventService wires an EventService from the event repository.
func NewEventService(events eventRepo) *EventService {
	return &EventService{events: events}
}

// List returns events newest-first, optionally filtered by kind, along with the
// total count for that filter (for pagination). A zero or negative limit means
// no limit; a negative offset means no offset (these conventions match the
// underlying repository).
func (s *EventService) List(ctx context.Context, kind string, limit, offset int) ([]model.Event, int64, error) {
	events, total, err := s.events.List(ctx, kind, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list events: %w", err)
	}
	return events, total, nil
}

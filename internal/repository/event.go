package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/samber/lo"
	"github.com/ulbwa/tgwebdav/internal/database"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository/sqlc"
)

// EventRepository persists audit/log events against a pgx pool.
type EventRepository struct {
	pool *pgxpool.Pool
}

// NewEventRepository returns an EventRepository backed by pool.
func NewEventRepository(pool *pgxpool.Pool) *EventRepository {
	return &EventRepository{pool: pool}
}

// Log records a new audit/log event with the current timestamp. The id and
// ts are generated in Go (matching the old GORM behavior).
func (r *EventRepository) Log(ctx context.Context, kind, message, ref string) error {
	db := database.FromContext(ctx, r.pool)
	err := sqlc.New(db).LogEvent(ctx, sqlc.LogEventParams{
		ID:      uuid.New(),
		Ts:      pgtype.Timestamptz{Time: time.Now(), Valid: true},
		Kind:    kind,
		Message: message,
		Ref:     ref,
	})
	return translateError(err)
}

// List returns events newest-first, optionally filtered by kind, along with the
// total count for that filter (for pagination). limit <= 0 means no limit;
// offset <= 0 means no offset.
func (r *EventRepository) List(ctx context.Context, kind string, limit, offset int) ([]model.Event, int64, error) {
	db := database.FromContext(ctx, r.pool)
	q := sqlc.New(db)

	total, err := q.CountEvents(ctx, kind)
	if err != nil {
		return nil, 0, translateError(err)
	}

	var rowLimit, rowOffset *int32
	if limit > 0 {
		v := int32(limit)
		rowLimit = &v
	}
	if offset > 0 {
		v := int32(offset)
		rowOffset = &v
	}

	rows, err := q.ListEvents(ctx, sqlc.ListEventsParams{
		Kind:      kind,
		RowOffset: rowOffset,
		RowLimit:  rowLimit,
	})
	if err != nil {
		return nil, 0, translateError(err)
	}

	out := lo.Map(rows, func(row sqlc.Event, _ int) model.Event { return mapEvent(row) })
	return out, total, nil
}

// mapEvent converts a sqlc.Event row to a model.Event.
func mapEvent(row sqlc.Event) model.Event {
	return model.Event{
		ID:      row.ID,
		TS:      row.Ts.Time,
		Kind:    row.Kind,
		Message: row.Message,
		Ref:     row.Ref,
	}
}

package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/domain"
	"gorm.io/gorm"
)

// eventRepo implements domain.EventRepository.
type eventRepo struct{ base *gorm.DB }

// Log records a new audit/log event with the current timestamp.
func (r *eventRepo) Log(ctx context.Context, kind, message, ref string) error {
	m := &eventModel{
		ID:      uuid.New(),
		TS:      time.Now(),
		Kind:    kind,
		Message: message,
		Ref:     ref,
	}
	if err := txFromCtx(ctx, r.base).Create(m).Error; err != nil {
		return fmt.Errorf("log event: %w", translateError(err))
	}
	return nil
}

// List returns events newest-first, optionally filtered by kind, along with the
// total count for that filter (for pagination).
func (r *eventRepo) List(ctx context.Context, kind string, limit, offset int) ([]domain.Event, int64, error) {
	db := txFromCtx(ctx, r.base)
	q := db.Model(&eventModel{})
	if kind != "" {
		q = q.Where("kind = ?", kind)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count events: %w", translateError(err))
	}

	q = q.Order("ts DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if offset > 0 {
		q = q.Offset(offset)
	}
	var ms []eventModel
	if err := q.Find(&ms).Error; err != nil {
		return nil, 0, fmt.Errorf("list events: %w", translateError(err))
	}
	out := make([]domain.Event, len(ms))
	for i := range ms {
		out[i] = ms[i].toDomain()
	}
	return out, total, nil
}

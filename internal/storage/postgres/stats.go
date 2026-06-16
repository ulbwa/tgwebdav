package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/ulbwa/tgwebdav/internal/domain"
	"gorm.io/gorm"
)

// statRepo implements domain.StatRepository.
type statRepo struct{ base *gorm.DB }

// Record persists a single time-series sample at the current time.
func (r *statRepo) Record(ctx context.Context, metric, label string, value float64) error {
	m := &statSampleModel{
		ID:     uuid.New(),
		TS:     time.Now(),
		Metric: metric,
		Label:  label,
		Value:  value,
	}
	if err := txFromCtx(ctx, r.base).Create(m).Error; err != nil {
		return fmt.Errorf("record stat: %w", translateError(err))
	}
	return nil
}

// Query returns samples for a metric/label in the [from, to] window, oldest
// first.
func (r *statRepo) Query(ctx context.Context, metric, label string, from, to time.Time) ([]domain.StatSample, error) {
	var ms []statSampleModel
	if err := txFromCtx(ctx, r.base).
		Where("metric = ? AND label = ? AND ts >= ? AND ts <= ?", metric, label, from, to).
		Order("ts").
		Find(&ms).Error; err != nil {
		return nil, fmt.Errorf("query stats: %w", translateError(err))
	}
	out := make([]domain.StatSample, len(ms))
	for i := range ms {
		out[i] = *ms[i].toDomain()
	}
	return out, nil
}

// Latest returns the most recent sample for a metric/label, or
// domain.ErrNotFound when none exists.
func (r *statRepo) Latest(ctx context.Context, metric, label string) (*domain.StatSample, error) {
	var m statSampleModel
	if err := txFromCtx(ctx, r.base).
		Where("metric = ? AND label = ?", metric, label).
		Order("ts DESC").
		First(&m).Error; err != nil {
		return nil, fmt.Errorf("latest stat: %w", translateError(err))
	}
	return m.toDomain(), nil
}

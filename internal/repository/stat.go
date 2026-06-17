package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ulbwa/tgwebdav/internal/database"
	"github.com/ulbwa/tgwebdav/internal/model"
	"github.com/ulbwa/tgwebdav/internal/repository/sqlc"
)

// StatRepository persists time-series stat samples against a pgx pool.
type StatRepository struct {
	pool *pgxpool.Pool
}

// NewStatRepository returns a StatRepository backed by pool.
func NewStatRepository(pool *pgxpool.Pool) *StatRepository {
	return &StatRepository{pool: pool}
}

// Record persists a single time-series sample at the current time. The id and
// ts are generated in Go (matching the old GORM behavior).
func (r *StatRepository) Record(ctx context.Context, metric, label string, value float64) error {
	db := database.FromContext(ctx, r.pool)
	err := sqlc.New(db).RecordStat(ctx, sqlc.RecordStatParams{
		ID:     uuid.New(),
		Ts:     pgtype.Timestamptz{Time: time.Now(), Valid: true},
		Metric: metric,
		Label:  label,
		Value:  value,
	})
	return translateError(err)
}

// Query returns samples for a metric/label in the closed [from, to] window,
// ordered oldest-first.
func (r *StatRepository) Query(ctx context.Context, metric, label string, from, to time.Time) ([]model.StatSample, error) {
	db := database.FromContext(ctx, r.pool)
	rows, err := sqlc.New(db).QueryStats(ctx, sqlc.QueryStatsParams{
		Metric: metric,
		Label:  label,
		Ts:     pgtype.Timestamptz{Time: from, Valid: true},
		Ts_2:   pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]model.StatSample, len(rows))
	for i, row := range rows {
		out[i] = mapStat(row)
	}
	return out, nil
}

// Latest returns the most recent sample for a metric/label, or ErrNotFound
// when none exists.
func (r *StatRepository) Latest(ctx context.Context, metric, label string) (*model.StatSample, error) {
	db := database.FromContext(ctx, r.pool)
	row, err := sqlc.New(db).LatestStat(ctx, sqlc.LatestStatParams{
		Metric: metric,
		Label:  label,
	})
	if err != nil {
		return nil, translateError(err)
	}
	s := mapStat(row)
	return &s, nil
}

// mapStat converts a sqlc.StatSample row to a model.StatSample.
func mapStat(row sqlc.StatSample) model.StatSample {
	return model.StatSample{
		ID:     row.ID,
		TS:     row.Ts.Time,
		Metric: row.Metric,
		Label:  row.Label,
		Value:  row.Value,
	}
}

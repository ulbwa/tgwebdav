package repository

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// uuidToPtr converts a nullable pgtype.UUID into a *uuid.UUID, returning nil when
// the column is NULL.
func uuidToPtr(v pgtype.UUID) *uuid.UUID {
	if !v.Valid {
		return nil
	}
	id := uuid.UUID(v.Bytes)
	return &id
}

// ptrToUUID converts a *uuid.UUID into a pgtype.UUID, producing a NULL value when
// p is nil.
func ptrToUUID(p *uuid.UUID) pgtype.UUID {
	if p == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *p, Valid: true}
}

// timeToPtr converts a nullable pgtype.Timestamptz into a *time.Time, returning
// nil when the column is NULL.
func timeToPtr(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}

// ptrToTime converts a *time.Time into a pgtype.Timestamptz, producing a NULL
// value when p is nil.
func ptrToTime(p *time.Time) pgtype.Timestamptz {
	if p == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *p, Valid: true}
}

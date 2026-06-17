package model

import (
	"time"

	"github.com/google/uuid"
)

// APIToken is a hashed, revocable bearer token for the Management API.
type APIToken struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	TokenHash  string // sha-256 hex of the presented token
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

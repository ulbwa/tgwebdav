package model

import (
	"time"

	"github.com/google/uuid"
)

// User is an authenticated principal with an isolated WebDAV namespace.
type User struct {
	ID           uuid.UUID
	Login        string
	PasswordHash string // argon2id encoded hash
	IsAdmin      bool
	QuotaBytes   int64 // 0 means unlimited
	BandwidthBPS int64 // 0 means unlimited (bytes/sec for reads)
	RatePerMin   int   // 0 means unlimited (requests/min)
	CreatedAt    time.Time
}

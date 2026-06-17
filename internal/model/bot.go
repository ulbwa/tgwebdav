package model

import (
	"time"

	"github.com/google/uuid"
)

// Bot is a Telegram bot used to upload/download blobs. Token is the decrypted
// API token; the postgres layer encrypts it at rest with TGWEBDAV_SECRET_KEY.
type Bot struct {
	ID               uuid.UUID
	Username         string
	Token            string
	Enabled          bool
	UnavailableUntil *time.Time // set from Telegram retry_after
	CreatedAt        time.Time
}

// Available reports whether the bot may be used at instant now.
func (b Bot) Available(now time.Time) bool {
	if !b.Enabled {
		return false
	}
	if b.UnavailableUntil != nil && now.Before(*b.UnavailableUntil) {
		return false
	}
	return true
}

package model

import (
	"time"

	"github.com/google/uuid"
)

// BotChannel records whether a bot is a member/admin of a channel.
type BotChannel struct {
	BotID     uuid.UUID
	ChannelID uuid.UUID
	Member    bool
	CheckedAt time.Time
}

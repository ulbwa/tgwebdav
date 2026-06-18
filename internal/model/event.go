package model

import (
	"time"

	"github.com/google/uuid"
)

// Event is an audit/log record surfaced through the Management API.
type Event struct {
	ID      uuid.UUID
	TS      time.Time
	Kind    string
	Message string
	Ref     string
}

// Common event kinds.
const (
	EventBlobUnavailable = "blob_unavailable"
	EventBlobPermDeleted = "blob_perm_deleted"
	EventBlobCorrupt     = "blob_corrupt"
	EventBlobReaped      = "blob_reaped"
	EventCascadeDelete   = "cascade_delete"
	EventBotDisabled     = "bot_disabled"
	EventBotUnavailable  = "bot_unavailable"
	EventChannelEvicted  = "channel_evicted"
	EventChannelDisabled = "channel_disabled"
	EventUploadFailed    = "upload_failed"
)

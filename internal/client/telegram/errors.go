package telegram

import (
	"errors"
	"fmt"
	"time"
)

// RateLimitError reports a 429 with the server-provided retry delay.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited, retry after %s", e.RetryAfter)
}

// Sentinel errors raised by the Telegram client. Callers map them to recovery
// behavior via errors.Is. Error identity is part of the package's public
// contract.
var (
	// ErrMessageNotFound means the message/file is gone (definitive); triggers
	// blob perm-unavailable + cascade delete.
	ErrMessageNotFound = errors.New("message or file not found")
	// ErrForbidden means the bot lacks access (kicked/not admin).
	ErrForbidden = errors.New("forbidden")
)

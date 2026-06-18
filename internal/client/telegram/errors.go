package telegram

import (
	"errors"
	"fmt"
	"time"
)

// RateLimitError reports a Telegram 429 with the server-provided retry delay.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("telegram rate limited, retry after %s", e.RetryAfter)
}

// Sentinel errors raised by the Telegram client. Callers map them to recovery
// behavior via errors.Is. Error identity is part of the package's public
// contract.
var (
	// ErrTelegramNotFound means the message/file is gone (definitive); triggers
	// blob perm-unavailable + cascade delete.
	ErrTelegramNotFound = errors.New("telegram: message or file not found")
	// ErrTelegramForbidden means the bot lacks access (kicked/not admin).
	ErrTelegramForbidden = errors.New("telegram: forbidden")
)

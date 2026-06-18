package service

import "errors"

// Sentinel errors raised by the service layer. Callers (e.g. the HTTP handler
// and the auth middleware) map them to protocol responses via errors.Is.
// Error identity is part of the package's public contract, so other layers may
// import this package solely to errors.Is-check these sentinels.
var (
	// ErrUnauthorized means authentication failed or is missing.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrForbidden means the principal is authenticated but not allowed.
	ErrForbidden = errors.New("forbidden")
	// ErrNoBot means no enabled bot has access to the required channel.
	ErrNoBot = errors.New("no available bot")
	// ErrBlobUnavailable means the requested bytes live in an unreadable blob.
	ErrBlobUnavailable = errors.New("blob unavailable")
)

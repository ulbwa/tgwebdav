package model

import "errors"

// Sentinel errors. Repositories and services return these (optionally wrapped
// with %w) so callers can map them to protocol responses without depending on
// storage-specific error types.
var (
	// ErrNotFound is returned when an entity does not exist.
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists is returned on a unique-constraint conflict.
	ErrAlreadyExists = errors.New("already exists")
	// ErrConflict signals a concurrent-modification / state conflict.
	ErrConflict = errors.New("conflict")
	// ErrUnauthorized means authentication failed or is missing.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrForbidden means the principal is authenticated but not allowed.
	ErrForbidden = errors.New("forbidden")
	// ErrQuotaExceeded means the write would exceed the user's storage quota.
	ErrQuotaExceeded = errors.New("quota exceeded")
	// ErrFileTooLarge means the file exceeds the configured maximum size.
	ErrFileTooLarge = errors.New("file too large")
	// ErrRateLimited means the user exceeded their request rate limit.
	ErrRateLimited = errors.New("rate limited")
	// ErrBlobUnavailable means the requested bytes live in an unreadable blob.
	ErrBlobUnavailable = errors.New("blob unavailable")
	// ErrNoBot means no enabled bot has access to the required channel.
	ErrNoBot = errors.New("no available bot")
	// ErrNotDir / ErrIsDir are filesystem shape errors.
	ErrNotDir = errors.New("not a directory")
	ErrIsDir  = errors.New("is a directory")
	// ErrNotEmpty is returned when removing a non-empty directory illegally.
	ErrNotEmpty = errors.New("directory not empty")
	// ErrInvalidPath is returned for malformed paths.
	ErrInvalidPath = errors.New("invalid path")
)

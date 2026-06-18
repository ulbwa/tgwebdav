package http

import "errors"

// Sentinel errors that make up the handler's HTTP-mapping vocabulary. These have
// no raise site in the service/repository layers; they exist so statusForError
// can map them to a stable HTTP status code. Error identity is part of the
// package's public contract.
var (
	// ErrConflict signals a concurrent-modification / state conflict.
	ErrConflict = errors.New("conflict")
	// ErrFileTooLarge means the file exceeds the configured maximum size.
	ErrFileTooLarge = errors.New("file too large")
	// ErrRateLimited means the user exceeded their request rate limit.
	ErrRateLimited = errors.New("rate limited")
	// ErrInvalidPath is returned for malformed paths.
	ErrInvalidPath = errors.New("invalid path")
	// ErrNotDir / ErrIsDir are filesystem shape errors.
	ErrNotDir = errors.New("not a directory")
	ErrIsDir  = errors.New("is a directory")
	// ErrNotEmpty is returned when removing a non-empty directory illegally.
	ErrNotEmpty = errors.New("directory not empty")
)

package model

import (
	"time"

	"github.com/google/uuid"
)

// NodeState describes the lifecycle of a filesystem node's content.
// ENUM(writing, buffered, stored)
//
//go:generate go-enum --values
type NodeState int32

// Node is a filesystem entry (file or directory) in a user's namespace.
type Node struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	ParentID    *uuid.UUID
	Name        string
	Path        string // normalized, slash-rooted, no trailing slash (root = "/")
	IsDir       bool
	Size        int64
	ContentHash string // sha-256 hex of content (files only)
	ETag        string
	ContentType string
	State       NodeState
	CreatedAt   time.Time
	ModifiedAt  time.Time
}

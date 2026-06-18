package webdavfs

import "errors"

// ErrQuotaExceeded means the write would exceed the user's storage quota. It is
// raised by filesystem operations (Stat/OpenFile/Copy quota checks) and mapped
// to an HTTP status by the handler via errors.Is. Error identity is part of the
// package's public contract.
var ErrQuotaExceeded = errors.New("quota exceeded")

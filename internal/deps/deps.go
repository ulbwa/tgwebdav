// Package deps blank-imports every external dependency the implementation
// packages rely on, so `go mod tidy` keeps them in go.mod while packages are
// developed in parallel. It is removed once the real code imports them.
package deps

import (
	_ "github.com/oapi-codegen/runtime"
	_ "github.com/oapi-codegen/runtime/types"
	_ "golang.org/x/crypto/argon2"
	_ "golang.org/x/net/webdav"
	_ "golang.org/x/time/rate"
	_ "gorm.io/driver/postgres"
	_ "gorm.io/gorm"
)

// Package openapi embeds the Management API's OpenAPI 3 document so the server
// can serve it without depending on a heavyweight spec loader. The spec itself
// lives at api/openapi.yaml (the conventional location for API definitions); the
// generated server code is produced from it into internal/management/api.
package openapi

import _ "embed"

// Spec is the raw OpenAPI 3 document for the Management API.
//
//go:embed openapi.yaml
var Spec []byte

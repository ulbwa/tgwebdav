// Package openapi embeds the Management API OpenAPI document so the server can
// serve it at GET /openapi.yaml without a heavyweight spec loader.
package openapi

import _ "embed"

//go:embed management.yaml
var Spec []byte

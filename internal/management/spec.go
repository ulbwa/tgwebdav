package management

import _ "embed"

// rawSpec is the OpenAPI 3 document describing the Management API. It is
// embedded so the server can serve it at GET /openapi.yaml without depending on
// the (heavyweight) kin-openapi loader.
//
//go:embed openapi.yaml
var rawSpec []byte

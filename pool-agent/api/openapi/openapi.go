// Package openapi exposes pool-agent-owned OpenAPI contracts.
package openapi

import _ "embed"

// PoolYAML is the canonical pool-local sandbox operations API contract.
//
//go:embed pool.yaml
var PoolYAML []byte

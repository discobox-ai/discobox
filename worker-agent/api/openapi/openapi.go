// Package openapi exposes worker-agent-owned OpenAPI contracts.
package openapi

import _ "embed"

// SandboxJSON is the canonical worker-local sandbox operations API contract.
//
//go:embed sandbox.json
var SandboxJSON []byte

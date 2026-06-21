// Package openapi exposes worker-agent-owned OpenAPI contracts.
package openapi

import _ "embed"

// WorkerYAML is the canonical worker-local sandbox operations API contract.
//
//go:embed worker.yaml
var WorkerYAML []byte

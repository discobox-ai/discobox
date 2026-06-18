// Package openapi exposes canonical OpenAPI contracts checked into the root module.
package openapi

import _ "embed"

// ServerYAML is the canonical Server REST API OpenAPI document in YAML form.
//
//go:embed server.yaml
var ServerYAML []byte

// SandboxYAML is the sandbox-agent REST API seed in YAML form.
//
//go:embed sandbox.yaml
var SandboxYAML []byte

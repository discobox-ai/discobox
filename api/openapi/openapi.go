// Package openapi exposes canonical OpenAPI contracts checked into the root module.
package openapi

import _ "embed"

// ServerJSON is the canonical Server REST API OpenAPI document in JSON form.
//
//go:embed server.json
var ServerJSON []byte

// ServerYAML is the canonical Server REST API OpenAPI document in YAML form.
//
//go:embed server.yaml
var ServerYAML []byte

// SandboxYAML is the canonical Sandbox REST API OpenAPI document in YAML form.
//
//go:embed sandbox.yaml
var SandboxYAML []byte

package bridge

// The wire's JSON Schema is generated from the types in wire.go, and the
// desktop app's Rust types from the schema (ADR 26-11-07-289 §3).
//go:generate go run ./genwire -schema ./wire.schema.json -rust ../../../../desktop/bridge/src/wire.rs

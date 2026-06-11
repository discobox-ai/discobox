// Package orchestration defines a durable, typed work queue primitive.
//
// The package intentionally knows nothing about application job types. Callers
// define payload structs in their own packages, enqueue those payloads, and
// register executors with whatever service dependencies each executor needs.
package orchestration

// Package sandbox defines the runtime provider boundary for managed sandboxes.
package sandbox

import (
	"errors"
	"fmt"
)

var (
	// ErrNotFound indicates the runtime sandbox does not exist.
	ErrNotFound = errors.New("sandbox not found")

	// ErrAlreadyExists indicates a runtime sandbox already exists.
	ErrAlreadyExists = errors.New("sandbox already exists")

	// ErrNotRunning indicates the runtime sandbox is not running.
	ErrNotRunning = errors.New("sandbox not running")

	// ErrAlreadyRunning indicates the runtime sandbox is already running.
	ErrAlreadyRunning = errors.New("sandbox already running")

	// ErrNoSandboxCapacity indicates no provider capacity is available for sandbox placement.
	ErrNoSandboxCapacity = errors.New("no sandbox capacity")

	// ErrProviderResourcesUnsupported indicates the provider does not support
	// provider resource inspection or updates.
	ErrProviderResourcesUnsupported = errors.New("provider resources not supported")

	// ErrProjectInspectionUnsupported indicates the provider does not support
	// project inspection shell access.
	ErrProjectInspectionUnsupported = errors.New("project inspection not supported by provider")

	// ErrProjectCacheUnsupported indicates the provider does not support project
	// cache clearing.
	ErrProjectCacheUnsupported = errors.New("project cache clearing not supported by provider")
)

// WorkerFailure reports that a worker-backed provider has no capacity because
// its workers FAILED, not because they are still coming up. It carries the
// worker's recorded error so the cause (a missing image, an unreachable daemon)
// reaches the sandbox instead of a bare capacity error. It unwraps to
// ErrNoSandboxCapacity: callers classifying capacity exhaustion still match.
type WorkerFailure struct {
	WorkerID string
	Message  string
}

func (e *WorkerFailure) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("worker %s failed", e.WorkerID)
	}
	return fmt.Sprintf("worker %s failed: %s", e.WorkerID, e.Message)
}

func (e *WorkerFailure) Unwrap() error { return ErrNoSandboxCapacity }

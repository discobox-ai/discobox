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

	// ErrPoolLogsUnsupported indicates the backend hosting a pool keeps no log
	// of its own that the driver can read. It is a runtime condition, not a
	// missing implementation: the same driver answers on one host and declines
	// on another, so the wrapping error carries the reason.
	ErrPoolLogsUnsupported = errors.New("pool host logs are not available from this backend")
)

// PoolFailure reports that a pool has no capacity because its runtime FAILED,
// not because it is still coming up. It carries the pool's recorded error so
// the cause (a missing image, an unreachable daemon) reaches the sandbox
// instead of a bare capacity error. It unwraps to ErrNoSandboxCapacity:
// callers classifying capacity exhaustion still match.
type PoolFailure struct {
	PoolID  string
	Message string
}

func (e *PoolFailure) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("pool %s failed", e.PoolID)
	}
	return fmt.Sprintf("pool %s failed: %s", e.PoolID, e.Message)
}

func (e *PoolFailure) Unwrap() error { return ErrNoSandboxCapacity }

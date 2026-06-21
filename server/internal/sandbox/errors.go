// Package sandbox defines the runtime provider boundary for managed sandboxes.
package sandbox

import "errors"

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

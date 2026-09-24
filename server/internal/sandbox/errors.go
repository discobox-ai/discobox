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

	// ErrArchived indicates the sandbox exists as retained data with no
	// runtime, and will not get one until it is unarchived (ADR 0022 §5).
	//
	// It is distinct from ErrAlreadyExists even though the pool agent reports
	// both as 409: a create that answers "already exists" has succeeded as far
	// as the caller is concerned, while one that answers "archived" has done
	// nothing and will keep doing nothing. Collapsing the two let a refused
	// create settle as a converged, healthy sandbox with no container.
	ErrArchived = errors.New("sandbox is archived; unarchive it to use it")

	// ErrNotRunning indicates the runtime sandbox is not running.
	ErrNotRunning = errors.New("sandbox not running")

	// ErrAlreadyRunning indicates the runtime sandbox is already running.
	ErrAlreadyRunning = errors.New("sandbox already running")

	// ErrPoolNotReachable indicates the pool hosting a sandbox is not taking
	// traffic yet: its host is being replaced or restarted, or its agent has not
	// passed its healthcheck. It is a condition, not an answer. The pool agent
	// comes up and reports ready on its own, so a caller that can wait (the
	// attach wait, ADR 0039) waits it out, and the wrapping error carries the
	// reason for when the wait runs out.
	ErrPoolNotReachable = errors.New("pool agent is not reachable")

	// ErrImageUnavailable indicates the pool cannot obtain the image a sandbox
	// is pinned to: the pinned image is not on the pool and its reference no
	// longer names it, or no registry will hand the pool that reference. It is
	// an answer about the pin, not a failed attempt, so retrying does not help
	// and an upgrade that re-pins the sandbox does. See ImageUnavailableError.
	ErrImageUnavailable = errors.New("sandbox image is not available")

	// ErrPoolAgentUnsupported indicates the pool's agent does not have the
	// operation asked of it: it answered with a route-level 404 rather than one
	// of its own errors, which is what an agent that predates the operation
	// does. The pool is moved onto the current agent when it is reconciled.
	ErrPoolAgentUnsupported = errors.New("the pool agent does not support this operation")

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

	// ErrGuestImageBuildUnsupported indicates the backend hosting a pool boots
	// no guest image of its own, so there is nothing for it to build. Like
	// ErrPoolLogsUnsupported it is a settled answer about this backend rather
	// than an unfinished implementation, and it is the answer for every backend
	// whose pool host is a machine somebody else provisioned.
	ErrGuestImageBuildUnsupported = errors.New("this backend boots no guest image of its own to build")
)

// PoolFailure reports a failed pool startup or an expired readiness wait.
// It carries the pool's concrete error so
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

// ImageUnavailableError is ErrImageUnavailable with the pool agent's account of
// which image and why, which is what a person reading the failure needs. It
// matches ErrImageUnavailable, so callers classify it without the message.
type ImageUnavailableError struct {
	Message string
}

func (e *ImageUnavailableError) Error() string {
	if e.Message == "" {
		return ErrImageUnavailable.Error()
	}
	return e.Message
}

func (e *ImageUnavailableError) Is(target error) bool { return target == ErrImageUnavailable }

// ProviderUnavailableError is a provider that cannot run on this host at all,
// found when a first start checks the provider it would install as the default
// (ADR 0148 §2). Reason is one of the health package's Reason constants, which
// is what lets a client that launched the server say why without reading prose.
type ProviderUnavailableError struct {
	Provider string
	Reason   string
	Err      error
}

func (e *ProviderUnavailableError) Error() string {
	return fmt.Sprintf("the %s provider cannot run on this host: %v", e.Provider, e.Err)
}

func (e *ProviderUnavailableError) Unwrap() error { return e.Err }

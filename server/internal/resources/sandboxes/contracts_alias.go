package sandboxes

import (
	contract "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
)

type Provider = contract.Provider
type ProviderManager = contract.ProviderManager
type CreateOptions = contract.CreateOptions
type UpdateOptions = contract.UpdateOptions
type SandboxRef = contract.SandboxRef
type Sandbox = contract.Sandbox
type Status = contract.Status
type AssignedPort = contract.AssignedPort
type ImageRef = contract.ImageRef
type ResolvedHarnessConfig = contract.ResolvedHarnessConfig
type ProviderStatus = contract.ProviderStatus
type ProviderConfigField = contract.ProviderConfigField
type ProviderDefinition = contract.ProviderDefinition
type StateEvent = contract.StateEvent
type HTTPClientLease = transport.HTTPClientLease
type PoolRuntime = contract.PoolRuntime
type PoolManager = contract.PoolManager

var ErrNotFound = contract.ErrNotFound
var ErrAlreadyExists = contract.ErrAlreadyExists
var ErrNotRunning = contract.ErrNotRunning
var ErrAlreadyRunning = contract.ErrAlreadyRunning
var StatusCreated = contract.StatusCreated
var NewHTTPClientLease = transport.NewHTTPClientLease
var NewProviderManager = contract.NewProviderManager

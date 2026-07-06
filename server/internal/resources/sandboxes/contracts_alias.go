package sandboxes

import (
	contract "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
)

type Provider = contract.Provider
type ProviderManager = contract.ProviderManager
type CreateOptions = contract.CreateOptions
type SandboxRef = contract.SandboxRef
type Sandbox = contract.Sandbox
type Status = contract.Status
type AssignedPort = contract.AssignedPort
type ImageRef = contract.ImageRef
type ResolvedAgentConfig = contract.ResolvedAgentConfig
type AgentConfig = contract.AgentConfig
type ProviderStatus = contract.ProviderStatus
type ProviderConfigField = contract.ProviderConfigField
type ProviderDefinition = contract.ProviderDefinition
type StateEvent = contract.StateEvent
type HTTPClientLease = transport.HTTPClientLease
type WorkerRuntimeReconciler = contract.WorkerRuntimeReconciler
type ImageEvent = contract.ImageEvent
type ImageStatus = contract.ImageStatus
type ImageProvider = contract.ImageProvider
type RemoveConfig = contract.RemoveConfig
type RemoveOption = contract.RemoveOption

var ErrNotFound = contract.ErrNotFound
var ErrAlreadyExists = contract.ErrAlreadyExists
var ErrNotRunning = contract.ErrNotRunning
var ErrAlreadyRunning = contract.ErrAlreadyRunning
var RemoveVolumes = contract.RemoveVolumes
var ImageStatusFailed = contract.ImageStatusFailed
var StatusCreated = contract.StatusCreated
var NewHTTPClientLease = transport.NewHTTPClientLease
var NewProviderManager = contract.NewProviderManager

package sandbox

import (
	"context"
	"net/http"
	"time"
)

// ImageRef names a sandbox base image.
type ImageRef struct {
	Name string `json:"name"`
}

const DefaultSandboxImageName = "discobox-sandbox-agent:local"

// ImageStatus describes provider image availability.
type ImageStatus string

const (
	ImageStatusUnknown   ImageStatus = "unknown"
	ImageStatusMissing   ImageStatus = "missing"
	ImageStatusPulling   ImageStatus = "pulling"
	ImageStatusAvailable ImageStatus = "available"
	ImageStatusFailed    ImageStatus = "failed"
)

// ImageProgress describes a point-in-time image pull progress update.
type ImageProgress struct {
	Message      string   `json:"message,omitempty"`
	CurrentBytes int64    `json:"currentBytes,omitempty"`
	TotalBytes   int64    `json:"totalBytes,omitempty"`
	Percent      *float64 `json:"percent,omitempty"`
}

// ImageInfo describes provider knowledge about an image.
type ImageInfo struct {
	Ref       ImageRef       `json:"ref"`
	ID        string         `json:"id,omitempty"`
	Status    ImageStatus    `json:"status"`
	Progress  *ImageProgress `json:"progress,omitempty"`
	Error     string         `json:"error,omitempty"`
	UpdatedAt time.Time      `json:"updatedAt"`
}

// ImageEvent reports a long-running image operation update.
type ImageEvent struct {
	Ref      ImageRef       `json:"ref"`
	Status   ImageStatus    `json:"status"`
	Progress *ImageProgress `json:"progress,omitempty"`
	Error    string         `json:"error,omitempty"`
	Time     time.Time      `json:"time"`
}

// ImageProvider is the user-facing image-management capability expected from
// production providers. Implementations may pull locally, inside a project VM,
// or through a remote backend.
type ImageProvider interface {
	DefaultImage(ctx context.Context) (ImageRef, error)
	ImageExists(ctx context.Context, ref ImageRef) (bool, error)
	GetImage(ctx context.Context, ref ImageRef) (*ImageInfo, error)
	PullImage(ctx context.Context, ref ImageRef) (<-chan ImageEvent, error)
}

// CurrentImageIDProvider can report the immutable ID of an image.
type CurrentImageIDProvider interface {
	CurrentImageID(ctx context.Context, ref ImageRef) (string, error)
}

// CleanupUnusedImagesProvider can remove unused provider images.
type CleanupUnusedImagesProvider interface {
	CleanupUnusedImages(ctx context.Context) error
}

// LocalityProvider reports whether sandboxes run on the same host.
type LocalityProvider interface {
	IsLocal() bool
}

// ProviderResourceInfo describes effective provider resources for a project.
type ProviderResourceInfo struct {
	Provider   string `json:"provider"`
	CPUCount   int    `json:"cpuCount"`
	MemoryMB   int    `json:"memoryMB"`
	DataDiskGB int    `json:"dataDiskGB"`
}

// UpdateProviderResourcesRequest describes provider resource changes.
type UpdateProviderResourcesRequest struct {
	MemoryMB   *int `json:"memoryMB,omitempty"`
	DataDiskGB *int `json:"dataDiskGB,omitempty"`
}

// ProviderResourceManager is an optional resource-management capability.
type ProviderResourceManager interface {
	GetProviderResourceInfo(ctx context.Context, projectID string) (*ProviderResourceInfo, error)
	ApplyProviderResourceUpdate(ctx context.Context, projectID string, req UpdateProviderResourcesRequest) error
}

// ProjectInspectionInfo describes provider inspection access.
type ProjectInspectionInfo struct {
	Provider      string `json:"provider"`
	Available     bool   `json:"available"`
	ContainerName string `json:"containerName"`
	Scope         string `json:"scope"`
}

// ProjectInspectionManager is an optional inspection-shell capability.
type ProjectInspectionManager interface {
	GetProjectInspectionInfo(ctx context.Context, projectID string) (*ProjectInspectionInfo, error)
	AttachProjectInspection(ctx context.Context, projectID string, opts AttachOptions) (PTY, error)
}

// ProjectCacheManager is an optional cache-management capability.
type ProjectCacheManager interface {
	ClearCache(ctx context.Context, projectID string) error
}

// DockerProxyProvider is an optional debug proxy capability.
type DockerProxyProvider interface {
	DockerTransport(projectID string) (http.RoundTripper, error)
}

// ProviderConfigField describes one provider instance configuration field.
type ProviderConfigField struct {
	Key                string `json:"key"`
	Label              string `json:"label"`
	Type               string `json:"type"`
	Description        string `json:"description,omitempty"`
	Placeholder        string `json:"placeholder,omitempty"`
	Required           bool   `json:"required,omitempty"`
	Advanced           bool   `json:"advanced,omitempty"`
	CredentialProvider string `json:"credentialProvider,omitempty"`
	CredentialAuthType string `json:"credentialAuthType,omitempty"`
}

// ProviderDefinition describes a registered provider driver.
type ProviderDefinition struct {
	Name         string                `json:"name,omitempty"`
	Icon         string                `json:"icon,omitempty"`
	Description  string                `json:"description,omitempty"`
	ConfigFields []ProviderConfigField `json:"configFields,omitempty"`
}

// DefinitionProvider reports provider metadata.
type DefinitionProvider interface {
	Definition() ProviderDefinition
}

// ProviderStatus describes runtime provider availability and capabilities.
type ProviderStatus struct {
	Available          bool   `json:"available"`
	State              string `json:"state"`
	Message            string `json:"message,omitempty"`
	SupportsResources  bool   `json:"supportsResources"`
	SupportsInspection bool   `json:"supportsInspection"`
	SupportsClearCache bool   `json:"supportsClearCache"`
	SupportsImages     bool   `json:"supportsImages"`
	Details            any    `json:"details,omitempty"`
}

// StatusProvider reports provider status.
type StatusProvider interface {
	Status() ProviderStatus
}

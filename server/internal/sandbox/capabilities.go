package sandbox

import (
	"context"
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

// ProviderStatus describes runtime provider availability.
type ProviderStatus struct {
	Available bool   `json:"available"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
	Details   any    `json:"details,omitempty"`
}

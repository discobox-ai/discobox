package dockerworker

import (
	"os"
	"strings"
)

// DefaultWorkerImage is the default worker-agent container image launched by
// the engine on every backend.
const DefaultWorkerImage = "ghcr.io/obot-platform/discobox-systemd:latest"

// WorkerImageEnv globally overrides the default worker-agent image, primarily
// for local development against freshly built images.
const WorkerImageEnv = "DISCOBOX_DOCKER_WORKER_IMAGE"

// EffectiveWorkerImage resolves the worker-agent image from provider
// configuration, the global override, or the static default.
func EffectiveWorkerImage(image string) string {
	if image = strings.TrimSpace(image); image != "" {
		return image
	}
	if value := strings.TrimSpace(os.Getenv(WorkerImageEnv)); value != "" {
		return value
	}
	return DefaultWorkerImage
}

// WorkerImageSource reports where the effective worker image came from.
func WorkerImageSource(image string) string {
	if strings.TrimSpace(image) != "" {
		return "provider"
	}
	if strings.TrimSpace(os.Getenv(WorkerImageEnv)) != "" {
		return "global"
	}
	return "default"
}

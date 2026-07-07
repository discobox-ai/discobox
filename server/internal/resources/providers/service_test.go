package providers

import (
	"encoding/json"
	"testing"
)

func TestDockerProviderStatusDetailsReportsProviderImageSource(t *testing.T) {
	t.Setenv("DISCOBOX_DOCKER_WORKER_IMAGE", "worker:global")

	data, err := dockerProviderStatusDetails(json.RawMessage(`{"image":"worker:provider"}`))
	if err != nil {
		t.Fatalf("docker provider status details: %v", err)
	}

	var details struct {
		Image struct {
			Configured string `json:"configured"`
			Effective  string `json:"effective"`
			Source     string `json:"source"`
		} `json:"image"`
	}
	if err := json.Unmarshal(data, &details); err != nil {
		t.Fatalf("decode details: %v", err)
	}
	if details.Image.Configured != "worker:provider" || details.Image.Effective != "worker:provider" || details.Image.Source != "provider" {
		t.Fatalf("image details = %#v", details.Image)
	}
}

func TestDockerProviderStatusDetailsReportsGlobalImageSource(t *testing.T) {
	t.Setenv("DISCOBOX_DOCKER_WORKER_IMAGE", "worker:global")

	data, err := dockerProviderStatusDetails(nil)
	if err != nil {
		t.Fatalf("docker provider status details: %v", err)
	}

	var details struct {
		Image struct {
			Configured string `json:"configured"`
			Effective  string `json:"effective"`
			Source     string `json:"source"`
		} `json:"image"`
	}
	if err := json.Unmarshal(data, &details); err != nil {
		t.Fatalf("decode details: %v", err)
	}
	if details.Image.Configured != "" || details.Image.Effective != "worker:global" || details.Image.Source != "global" {
		t.Fatalf("image details = %#v", details.Image)
	}
}

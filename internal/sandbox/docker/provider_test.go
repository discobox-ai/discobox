package docker

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"

	"github.com/obot-platform/disco2/internal/sandbox"
)

func TestContainerNameSanitizesSandboxID(t *testing.T) {
	got := containerName("sandbox/id with spaces")
	if got != "disco2-sandbox-sandbox-id-with-spaces" {
		t.Fatalf("container name = %q", got)
	}
}

func TestDecodePullProgress(t *testing.T) {
	event := decodePullProgress(imageRef("example:latest"), []byte(`{"status":"Downloading","progressDetail":{"current":25,"total":100}}`))
	if event.Progress == nil {
		t.Fatal("expected progress")
	}
	if event.Progress.Message != "Downloading" || event.Progress.CurrentBytes != 25 || event.Progress.TotalBytes != 100 {
		t.Fatalf("progress = %#v", event.Progress)
	}
	if event.Progress.Percent == nil || *event.Progress.Percent != 25 {
		t.Fatalf("percent = %v, want 25", event.Progress.Percent)
	}
}

func TestDecodePullProgressFailure(t *testing.T) {
	event := decodePullProgress(imageRef("example:latest"), []byte(`{"error":"denied"}`))
	if event.Status != "failed" || event.Error != "denied" {
		t.Fatalf("event = %#v", event)
	}
}

func TestAssignedPorts(t *testing.T) {
	port, ok := network.PortFrom(3002, network.TCP)
	if !ok {
		t.Fatal("failed to build port")
	}
	ports := assignedPorts(network.PortMap{
		port: []network.PortBinding{{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "49153"}},
	})
	if len(ports) != 1 {
		t.Fatalf("ports = %#v", ports)
	}
	if ports[0].HostIP != "127.0.0.1" || ports[0].HostPort != 49153 || ports[0].ContainerPort != 3002 || ports[0].Protocol != "tcp" {
		t.Fatalf("assigned port = %#v", ports[0])
	}
}

func TestStateEventFromDockerEvent(t *testing.T) {
	event, ok := stateEventFromDockerEvent(events.Message{
		Action: events.ActionStart,
		Actor:  events.Actor{Attributes: map[string]string{labelSandboxID: "sandbox-1"}},
	})
	if !ok {
		t.Fatal("expected event")
	}
	if event.SandboxID != "sandbox-1" || event.Status != sandbox.StatusRunning {
		t.Fatalf("event = %#v", event)
	}
}

func TestStateEventFromDockerEventIgnoresUnmanagedEvent(t *testing.T) {
	_, ok := stateEventFromDockerEvent(events.Message{Action: events.ActionStart})
	if ok {
		t.Fatal("expected unmanaged event to be ignored")
	}
}

func TestResolveWorkspaceMountSource(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveWorkspaceMountSource(dir)
	if err != nil {
		t.Fatalf("resolve workspace mount source: %v", err)
	}
	if got != dir {
		t.Fatalf("workspace source = %q, want %q", got, dir)
	}

	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := resolveWorkspaceMountSource(file); err == nil {
		t.Fatal("expected file path to fail")
	}
}

func imageRef(name string) sandbox.ImageRef {
	return sandbox.ImageRef{Name: name}
}

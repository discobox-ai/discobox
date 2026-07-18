package digitalocean

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
)

func encodeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func newTestDriver(t *testing.T, handler http.Handler, mutate func(*DriverConfig)) *Driver {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := DriverConfig{
		Token:      "token-1",
		APIBaseURL: server.URL,
		Region:     "sfo3",
		Size:       "s-2vcpu-4gb",
		Image:      "ubuntu-24-04-x64",
		SSHKeys:    []string{"key-1"},
		Tags:       []string{"custom"},
		AgentPort:  3002,
		HTTPClient: server.Client(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	driver, err := NewDriver(cfg)
	if err != nil {
		t.Fatalf("new driver: %v", err)
	}
	return driver
}

func activeDroplet(id int64, ip string) droplet {
	return droplet{
		ID:     id,
		Name:   "discobox-vm-worker-1",
		Status: "active",
		Networks: dropletNetworks{V4: []dropletNetwork{
			{IPAddress: "10.0.0.5", Type: "private"},
			{IPAddress: ip, Type: "public"},
		}},
	}
}

func TestEnsureVMCreatesDropletWithDockerCloudInitAndPoolTag(t *testing.T) {
	var createReq createDropletRequest
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/droplets":
			if got := r.URL.Query().Get("tag_name"); got != "discobox-pool-pool-1" {
				t.Fatalf("tag lookup = %q", got)
			}
			encodeJSON(t, w, dropletsResponse{})
		case r.Method == http.MethodPost && r.URL.Path == "/v2/droplets":
			if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
				t.Fatalf("authorization = %q", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&createReq); err != nil {
				t.Fatalf("decode create request: %v", err)
			}
			encodeJSON(t, w, dropletResponse{Droplet: droplet{ID: 42, Status: "new"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	driver := newTestDriver(t, handler, nil)

	info, err := driver.EnsureVM(context.Background(), "pool-1", dockerworker.VMSpec{Name: "discobox-vm-worker-1"})
	if err != nil {
		t.Fatalf("ensure vm: %v", err)
	}
	if info.ID != "42" || info.Status != sandbox.StatusCreated {
		t.Fatalf("vm info = %#v", info)
	}
	if createReq.Region != "sfo3" || createReq.Size != "s-2vcpu-4gb" || createReq.Image != "ubuntu-24-04-x64" {
		t.Fatalf("create request = %#v", createReq)
	}
	if !strings.Contains(createReq.UserData, "docker.io") || !strings.Contains(createReq.UserData, "#cloud-config") {
		t.Fatalf("user data = %q, want docker install cloud-init", createReq.UserData)
	}
	if strings.Contains(createReq.UserData, "discobox-pool-agent") {
		t.Fatalf("user data = %q, worker agent is launched by the engine, not cloud-init", createReq.UserData)
	}
	wantTags := map[string]bool{"custom": true, "discobox": true, "discobox-pool-pool-1": true}
	for _, tag := range createReq.Tags {
		delete(wantTags, tag)
	}
	if len(wantTags) != 0 {
		t.Fatalf("create tags = %#v, missing %#v", createReq.Tags, wantTags)
	}
}

func TestEnsureVMReturnsExistingDroplet(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v2/droplets" {
			encodeJSON(t, w, dropletsResponse{Droplets: []droplet{activeDroplet(7, "203.0.113.5")}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	})
	driver := newTestDriver(t, handler, nil)

	info, err := driver.EnsureVM(context.Background(), "pool-1", dockerworker.VMSpec{})
	if err != nil {
		t.Fatalf("ensure vm: %v", err)
	}
	if info.ID != "7" || info.Status != sandbox.StatusRunning || info.Address != "203.0.113.5" {
		t.Fatalf("vm info = %#v", info)
	}
}

func TestInspectVMReportsNotFound(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		encodeJSON(t, w, dropletsResponse{})
	})
	driver := newTestDriver(t, handler, nil)

	if _, err := driver.InspectVM(context.Background(), "pool-1"); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("inspect err = %v, want ErrNotFound", err)
	}
}

func TestDeleteVMDeletesDropletByWorkerTag(t *testing.T) {
	deleted := ""
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/droplets":
			encodeJSON(t, w, dropletsResponse{Droplets: []droplet{activeDroplet(7, "203.0.113.5")}})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v2/droplets/"):
			deleted = strings.TrimPrefix(r.URL.Path, "/v2/droplets/")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	driver := newTestDriver(t, handler, nil)

	if err := driver.DeleteVM(context.Background(), "pool-1"); err != nil {
		t.Fatalf("delete vm: %v", err)
	}
	if deleted != "7" {
		t.Fatalf("deleted droplet = %q, want 7", deleted)
	}
}

func TestDeleteVMSucceedsWhenDropletIsGone(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		encodeJSON(t, w, dropletsResponse{})
	})
	driver := newTestDriver(t, handler, nil)

	if err := driver.DeleteVM(context.Background(), "pool-1"); err != nil {
		t.Fatalf("delete missing vm: %v", err)
	}
}

func TestAcquirePoolAgentClientUsesPublicIPAndAgentPort(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		encodeJSON(t, w, dropletsResponse{Droplets: []droplet{activeDroplet(7, "203.0.113.5")}})
	})
	driver := newTestDriver(t, handler, nil)

	lease, err := driver.AcquirePoolAgentClient(context.Background(), "pool-1")
	if err != nil {
		t.Fatalf("acquire worker agent client: %v", err)
	}
	defer lease.Release()
	if lease.BaseURL != "http://203.0.113.5:3002" {
		t.Fatalf("lease base URL = %q", lease.BaseURL)
	}
}

func TestAcquireDockerClientRequiresSSHKey(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		encodeJSON(t, w, dropletsResponse{Droplets: []droplet{activeDroplet(7, "203.0.113.5")}})
	})
	driver := newTestDriver(t, handler, nil)

	_, err := driver.AcquireDockerClient(context.Background(), "pool-1")
	if err == nil || !strings.Contains(err.Error(), "sshPrivateKey") {
		t.Fatalf("acquire docker client err = %v, want missing ssh key error", err)
	}
}

func TestNewDriverRejectsInvalidSSHKey(t *testing.T) {
	_, err := NewDriver(DriverConfig{Token: "token-1", SSHPrivateKey: "not-a-key"})
	if err == nil || !strings.Contains(err.Error(), "ssh private key") {
		t.Fatalf("new driver err = %v, want ssh key parse error", err)
	}
}

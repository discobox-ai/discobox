package digitalocean_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/obot-platform/discobox/providers/sandbox/vm"
	"github.com/obot-platform/discobox/providers/sandbox/vm/digitalocean"
	sandbox "github.com/obot-platform/discobox/sandboxprovider"
	workerbootstrap "github.com/obot-platform/discobox/workerbootstrap"
)

func TestCreateVMSendsDropletRequestWithCloudInitAndTags(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/droplets" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer token-1" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		writeDroplet(t, w, "new")
	}))
	defer server.Close()

	driver, err := digitalocean.NewDriver(digitalocean.Config{
		Token:      "token-1",
		APIBaseURL: server.URL,
		Region:     "sfo3",
		Size:       "s-2vcpu-2gb",
		Image:      "ubuntu-24-04-x64",
		SSHKeys:    []string{"12345"},
		Tags:       []string{"existing"},
		Monitoring: true,
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("new driver: %v", err)
	}

	boot := vm.BuildBootConfig(vm.BootInput{
		Ref: sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"},
		WorkerBootstrap: workerbootstrap.Bootstrap{
			WorkerID: "worker-1",
			Token:    "bootstrap-token",
		},
		ControlPlaneURL: "https://control.example",
		AgentPort:       3002,
	})
	inst, err := driver.CreateVM(context.Background(), vm.InstanceSpec{
		Ref:   sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"},
		Name:  "discobox-project-1-sandbox-1",
		Image: "custom-image",
		Boot:  boot,
	})
	if err != nil {
		t.Fatalf("create vm: %v", err)
	}
	if inst.ID != "42" {
		t.Fatalf("instance id = %q", inst.ID)
	}
	if got["name"] != "discobox-project-1-sandbox-1" {
		t.Fatalf("name = %#v", got["name"])
	}
	if got["region"] != "sfo3" || got["size"] != "s-2vcpu-2gb" || got["image"] != "custom-image" {
		t.Fatalf("unexpected region/size/image body: %#v", got)
	}
	if got["monitoring"] != true {
		t.Fatalf("monitoring = %#v", got["monitoring"])
	}
	if got["user_data"] == "" {
		t.Fatalf("expected cloud-init user_data")
	}
	wantTags := []any{"existing", "discobox", "discobox-project-project-1", "discobox-sandbox-sandbox-1"}
	if !reflect.DeepEqual(got["tags"], wantTags) {
		t.Fatalf("tags = %#v", got["tags"])
	}
}

func TestLifecycleActionsAndInspect(t *testing.T) {
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token-1" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/droplets/42/actions":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode action: %v", err)
			}
			actions = append(actions, body["type"])
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"action":{"id":1}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/droplets/42":
			writeDroplet(t, w, "active")
		case r.Method == http.MethodDelete && r.URL.Path == "/v2/droplets/42":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	driver, err := digitalocean.NewDriver(digitalocean.Config{Token: "token-1", APIBaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("new driver: %v", err)
	}
	started, err := driver.StartVM(context.Background(), "42")
	if err != nil {
		t.Fatalf("start vm: %v", err)
	}
	if started.Status != sandbox.StatusRunning || started.AgentHost != "203.0.113.10" {
		t.Fatalf("started instance = %#v", started)
	}
	if _, err := driver.StopVM(context.Background(), "42", time.Second); err != nil {
		t.Fatalf("stop vm: %v", err)
	}
	if err := driver.DeleteVM(context.Background(), "42", true); err != nil {
		t.Fatalf("delete vm: %v", err)
	}
	want := []string{"power_on", "shutdown"}
	if !reflect.DeepEqual(actions, want) {
		t.Fatalf("actions = %#v", actions)
	}
}

func TestNewProviderWrapsDigitalOceanDriver(t *testing.T) {
	provider, err := digitalocean.NewProvider(digitalocean.Config{
		Token:      "token-1",
		APIBaseURL: "https://api.example.invalid",
	}, vm.Config{})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	definition := provider.Definition()
	if definition.Name != "DigitalOcean" {
		t.Fatalf("provider name = %q", definition.Name)
	}
}

func writeDroplet(t *testing.T, w http.ResponseWriter, status string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"droplet": map[string]any{
			"id":         42,
			"name":       "discobox-project-1-sandbox-1",
			"status":     status,
			"created_at": "2026-06-10T00:00:00Z",
			"size_slug":  "s-2vcpu-2gb",
			"region": map[string]any{
				"slug": "sfo3",
			},
			"image": map[string]any{
				"slug": "ubuntu-24-04-x64",
			},
			"networks": map[string]any{
				"v4": []map[string]any{{"ip_address": "10.0.0.5", "type": "private"}, {"ip_address": "203.0.113.10", "type": "public"}},
			},
		},
	}); err != nil {
		t.Fatalf("encode droplet: %v", err)
	}
}

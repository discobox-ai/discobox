package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiclientgen "github.com/obot-platform/disco2/internal/apiclient/gen"
)

func TestProviderCreateHelpDoesNotHitAPI(t *testing.T) {
	hit := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hit = true
	}))
	defer server.Close()

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--server", server.URL, "provider", "create", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute help: %v", err)
	}
	if hit {
		t.Fatalf("plain provider create --help hit API server")
	}
	if !strings.Contains(out.String(), "disco2 provider catalog") || !strings.Contains(out.String(), "--help=PROVIDER") {
		t.Fatalf("help output = %q, want provider catalog and --help=PROVIDER hints", out.String())
	}
}

func TestProviderCreateHelpProviderLoadsDynamicFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/providers/catalog" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"providers": []map[string]any{{
			"id":        "example",
			"name":      "Example",
			"available": true,
			"builtIn":   true,
			"capabilities": map[string]any{
				"available":          true,
				"details":            map[string]any{},
				"state":              "ready",
				"supportsClearCache": false,
				"supportsImages":     true,
				"supportsInspection": false,
				"supportsResources":  false,
			},
			"configFields": []map[string]any{{
				"key":         "controlPlaneUrl",
				"label":       "Control Plane URL",
				"type":        "string",
				"required":    true,
				"placeholder": "https://example.test",
			}, {
				"key":      "poolSize",
				"label":    "Pool Size",
				"type":     "number",
				"advanced": true,
			}},
		}}})
	}))
	defer server.Close()

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--server", server.URL, "provider", "create", "--help=example"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute provider help: %v", err)
	}
	if !strings.Contains(out.String(), "--control-plane-url") || !strings.Contains(out.String(), "--pool-size") {
		t.Fatalf("provider help output = %q, want dynamic flags", out.String())
	}
}

func TestDynamicProviderCreateBodyUsesCatalogFields(t *testing.T) {
	provider := apiclientgen.SandboxProviderCatalogItem{ID: "example", Name: "Example"}
	provider.ConfigFields.SetTo([]apiclientgen.ProviderConfigField{{
		Key:      "controlPlaneUrl",
		Label:    "Control Plane URL",
		Type:     "string",
		Required: apiclientgen.NewOptBool(true),
	}, {
		Key:   "poolSize",
		Label: "Pool Size",
		Type:  "number",
	}, {
		Key:   "systemd",
		Label: "Systemd",
		Type:  "boolean",
	}})
	cmd := NewRootCommand()
	createCmd, _, err := cmd.Find([]string{"provider", "create"})
	if err != nil {
		t.Fatalf("find create command: %v", err)
	}
	opts, err := parseDynamicProviderCreateArgs(createCmd, []string{"--type", "example", "--name", "local", "--control-plane-url", "http://localhost:8080", "--pool-size", "2", "--systemd"}, provider)
	if err != nil {
		t.Fatalf("parse dynamic args: %v", err)
	}
	body, err := dynamicProviderCreateBody(opts, provider)
	if err != nil {
		t.Fatalf("dynamic body: %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal(body.GetConfig(), &config); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if config["controlPlaneUrl"] != "http://localhost:8080" || config["poolSize"].(float64) != 2 || config["systemd"] != true {
		t.Fatalf("config = %#v", config)
	}
}

func TestProviderCreateCommandSendsDynamicConfig(t *testing.T) {
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/providers/catalog":
			_ = json.NewEncoder(w).Encode(map[string]any{"providers": []map[string]any{{
				"id":        "example",
				"name":      "Example",
				"available": true,
				"builtIn":   true,
				"capabilities": map[string]any{
					"available":          true,
					"details":            map[string]any{},
					"state":              "ready",
					"supportsClearCache": false,
					"supportsImages":     true,
					"supportsInspection": false,
					"supportsResources":  false,
				},
				"configFields": []map[string]any{{"key": "controlPlaneUrl", "label": "Control Plane URL", "type": "string", "required": true}, {"key": "poolSize", "label": "Pool Size", "type": "number"}},
			}}})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/projects/") && strings.HasSuffix(r.URL.Path, "/providers"):
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Errorf("decode request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        "00000000-0000-0000-0000-000000000010",
				"projectId": "00000000-0000-0000-0000-000000000002",
				"name":      "local",
				"type":      "example",
				"config":    posted["config"],
				"builtIn":   false,
				"disabled":  false,
				"createdAt": now,
				"updatedAt": now,
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--server", server.URL, "provider", "create", "--type", "example", "--name", "local", "--control-plane-url", "http://localhost:8080", "--pool-size", "2"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute create: %v", err)
	}
	config, ok := posted["config"].(map[string]any)
	if !ok {
		t.Fatalf("posted config = %#v", posted["config"])
	}
	if config["controlPlaneUrl"] != "http://localhost:8080" || config["poolSize"].(float64) != 2 {
		t.Fatalf("posted = %#v", posted)
	}
}

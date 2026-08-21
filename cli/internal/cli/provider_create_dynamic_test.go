package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-faster/jx"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
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
	cmd.SetArgs([]string{"--server", server.URL, "admin", "provider", "create", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute help: %v", err)
	}
	if hit {
		t.Fatalf("plain provider create --help hit API server")
	}
	if !strings.Contains(out.String(), "discobox admin provider catalog") || !strings.Contains(out.String(), "--help=PROVIDER") {
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
			"id":   "example",
			"name": "Example",
			"capabilities": map[string]any{
				"available": true,
				"details":   map[string]any{},
				"state":     "ready",
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
	cmd.SetArgs([]string{"--server", server.URL, "admin", "provider", "create", "--help=example"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute provider help: %v", err)
	}
	if !strings.Contains(out.String(), "--control-plane-url") || !strings.Contains(out.String(), "--pool-size") {
		t.Fatalf("provider help output = %q, want dynamic flags", out.String())
	}
}

func TestProviderUpdateHelpDoesNotHitAPI(t *testing.T) {
	hit := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hit = true
	}))
	defer server.Close()

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--server", server.URL, "admin", "provider", "update", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute help: %v", err)
	}
	if hit {
		t.Fatalf("plain provider update --help hit API server")
	}
	if !strings.Contains(out.String(), "discobox admin provider catalog") || !strings.Contains(out.String(), "--help=PROVIDER") {
		t.Fatalf("help output = %q, want provider catalog and --help=PROVIDER hints", out.String())
	}
}

func TestProviderUpdateHelpProviderLoadsDynamicFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/providers/catalog" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"providers": []map[string]any{{
			"id":   "example",
			"name": "Example",
			"capabilities": map[string]any{
				"available": true,
				"details":   map[string]any{},
				"state":     "ready",
			},
			"configFields": []map[string]any{{
				"key":         "controlPlaneUrl",
				"label":       "Control Plane URL",
				"type":        "string",
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
	cmd.SetArgs([]string{"--server", server.URL, "admin", "provider", "update", "--help=example"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute provider help: %v", err)
	}
	if !strings.Contains(out.String(), "--control-plane-url") || !strings.Contains(out.String(), "--pool-size") {
		t.Fatalf("provider help output = %q, want dynamic flags", out.String())
	}
}

func TestDynamicProviderCreateBodyUsesCatalogFields(t *testing.T) {
	provider := apimodel.SandboxProviderCatalogItem{ID: "example", Name: "Example"}
	provider.ConfigFields.SetTo([]apimodel.ProviderConfigField{{
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
	createCmd, _, err := cmd.Find([]string{"admin", "provider", "create"})
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

func TestDynamicProviderUpdateBodyMergesCatalogFields(t *testing.T) {
	provider := apimodel.SandboxProviderCatalogItem{ID: "example", Name: "Example"}
	provider.ConfigFields.SetTo([]apimodel.ProviderConfigField{{
		Key:   "controlPlaneUrl",
		Label: "Control Plane URL",
		Type:  "string",
	}, {
		Key:   "poolSize",
		Label: "Pool Size",
		Type:  "number",
	}, {
		Key:   "systemd",
		Label: "Systemd",
		Type:  "boolean",
	}})
	current := &apimodel.SandboxProviderInstance{Config: jx.Raw(`{"controlPlaneUrl":"http://old","poolSize":1,"other":"kept"}`)}
	cmd := NewRootCommand()
	updateCmd, _, err := cmd.Find([]string{"admin", "provider", "update"})
	if err != nil {
		t.Fatalf("find update command: %v", err)
	}
	opts, err := parseDynamicProviderUpdateArgs(updateCmd, []string{"provider-1", "--name", "renamed", "--pool-size", "2", "--systemd"}, provider)
	if err != nil {
		t.Fatalf("parse dynamic update args: %v", err)
	}
	body, err := dynamicProviderUpdateBody(opts, provider, current)
	if err != nil {
		t.Fatalf("dynamic update body: %v", err)
	}
	if name, ok := body.GetName().Get(); !ok || name != "renamed" {
		t.Fatalf("name = %q, ok %v", name, ok)
	}
	var config map[string]any
	if err := json.Unmarshal(body.GetConfig(), &config); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if config["controlPlaneUrl"] != "http://old" || config["poolSize"].(float64) != 2 || config["systemd"] != true || config["other"] != "kept" {
		t.Fatalf("config = %#v", config)
	}
}

func TestDynamicProviderCreateAllowsMissingName(t *testing.T) {
	provider := apimodel.SandboxProviderCatalogItem{ID: "example", Name: "Example"}
	cmd := NewRootCommand()
	createCmd, _, err := cmd.Find([]string{"admin", "provider", "create"})
	if err != nil {
		t.Fatalf("find create command: %v", err)
	}
	opts, err := parseDynamicProviderCreateArgs(createCmd, []string{"--type", "example"}, provider)
	if err != nil {
		t.Fatalf("parse dynamic args without name: %v", err)
	}
	body, err := dynamicProviderCreateBody(opts, provider)
	if err != nil {
		t.Fatalf("dynamic body: %v", err)
	}
	if body.Name != "" {
		t.Fatalf("name = %q, want empty", body.Name)
	}
}

func TestProviderUpdateCommandSendsDynamicConfig(t *testing.T) {
	const providerID = "prov_0000000000000010"
	var patched map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		now := time.Now().UTC().Format(time.RFC3339Nano)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/default/providers/"+providerID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        providerID,
				"projectId": "default",
				"name":      "local",
				"type":      "example",
				"config":    map[string]any{"controlPlaneUrl": "http://old", "poolSize": 1, "other": "kept"},
				"disabled":  false,
				"createdAt": now,
				"updatedAt": now,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/providers/catalog":
			_ = json.NewEncoder(w).Encode(map[string]any{"providers": []map[string]any{{
				"id":   "example",
				"name": "Example",
				"capabilities": map[string]any{
					"available": true,
					"details":   map[string]any{},
					"state":     "ready",
				},
				"configFields": []map[string]any{{"key": "controlPlaneUrl", "label": "Control Plane URL", "type": "string"}, {"key": "poolSize", "label": "Pool Size", "type": "number"}},
			}}})
		case r.Method == http.MethodPatch && r.URL.Path == "/projects/default/providers/"+providerID:
			if err := json.NewDecoder(r.Body).Decode(&patched); err != nil {
				t.Errorf("decode request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        providerID,
				"projectId": "default",
				"name":      patched["name"],
				"type":      "example",
				"config":    patched["config"],
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
	cmd.SetArgs([]string{"--server", server.URL, "admin", "provider", "update", providerID, "--name", "renamed", "--pool-size", "2"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute update: %v", err)
	}
	config, ok := patched["config"].(map[string]any)
	if !ok {
		t.Fatalf("patched config = %#v", patched["config"])
	}
	if patched["name"] != "renamed" || config["controlPlaneUrl"] != "http://old" || config["poolSize"].(float64) != 2 || config["other"] != "kept" {
		t.Fatalf("patched = %#v", patched)
	}
}

func TestProviderCreateCommandSendsDynamicConfig(t *testing.T) {
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/providers/catalog":
			_ = json.NewEncoder(w).Encode(map[string]any{"providers": []map[string]any{{
				"id":   "example",
				"name": "Example",
				"capabilities": map[string]any{
					"available": true,
					"details":   map[string]any{},
					"state":     "ready",
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
				"id":        "prov_0000000000000010",
				"projectId": "prj_default",
				"name":      "local",
				"type":      "example",
				"config":    posted["config"],
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
	cmd.SetArgs([]string{"--server", server.URL, "admin", "provider", "create", "--type", "example", "--name", "local", "--control-plane-url", "http://localhost:8080", "--pool-size", "2"})
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

func TestProviderCreateCommandConsumesDebugGlobalFlag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/providers/catalog":
			_ = json.NewEncoder(w).Encode(map[string]any{"providers": []map[string]any{{
				"id":   "example",
				"name": "Example",
				"capabilities": map[string]any{
					"available": true,
					"details":   map[string]any{},
					"state":     "ready",
				},
			}}})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/projects/") && strings.HasSuffix(r.URL.Path, "/providers"):
			now := time.Now().UTC().Format(time.RFC3339Nano)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        "prov_0000000000000010",
				"projectId": "prj_default",
				"name":      "local",
				"type":      "example",
				"config":    map[string]any{},
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
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", server.URL, "admin", "provider", "create", "--debug", "--type", "example", "--name", "local"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute create: %v", err)
	}
	debugOutput := errOut.String()
	if !strings.Contains(debugOutput, "> GET "+server.URL+"/providers/catalog") {
		t.Fatalf("debug output = %q, want catalog request", debugOutput)
	}
	if !strings.Contains(debugOutput, "> POST "+server.URL+"/projects/default/providers") {
		t.Fatalf("debug output = %q, want provider create request", debugOutput)
	}
}

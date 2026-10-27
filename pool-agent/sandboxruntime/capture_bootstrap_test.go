package sandboxruntime

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
)

func TestCaptureBootstrapReachesEffectiveHarnessAcrossPoolWire(t *testing.T) {
	//nolint:gosec // Public OAuth client metadata is a test fixture, not a credential.
	definition := harnesscapture.Definition{Version: 1, Watch: []string{"~/.settings.json", "~/.auth.json"}, ExportCommand: []string{"/usr/bin/export"}, HandoffCommand: []string{"/usr/bin/handoff"}, Providers: []harnesscapture.Provider{{ID: "provider", EnvName: "API_TOKEN", Types: []string{"oauth"}, Host: "api.example.com", OAuth: &harnesscapture.OAuth{TokenURL: "https://auth.example.com/token", ClientID: "registered-client", Encoding: "form"}, Routes: []harnesscapture.Route{{Host: "api.example.com", Path: "/v1", Use: "Call model"}}}}, Settings: []harnesscapture.Setting{{Path: "~/.settings.json", Format: "json", Exclude: []string{"apiKey", "env"}}}}
	definition.CredentialFiles = []string{"~/.auth.json"}
	definition.CredentialSettings = []harnesscapture.Setting{{Path: "~/.auth.json", Format: "json", Fields: map[string]string{"token": "secret"}}}
	baseline := harnesscapture.Baseline{HarnessConfigID: "harness-1", ConfigRevision: 17, ManifestDigest: "sha256:manifest", SecretRevisions: map[string]string{"API_TOKEN": "secret-id:epoch"}, Files: []harnesscapture.File{{Path: "~/.settings.json", Content: `{"model":"configured"}`}}}
	// Serialize through the actual independently generated pool API shapes.
	wire, marshalErr := json.Marshal(map[string]any{"sandboxId": "sandbox-1", "config": map[string]any{}, "resolvedHarnessConfig": map[string]any{"id": "harness-1", "name": "Harness", "saveBack": definition, "captureBaseline": baseline, "files": []harnesscapture.File{{Path: "~/.settings.json", Content: `{"model":"image"}`}}, "configuredFiles": baseline.Files}})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	var req workerapimodel.PoolSandboxCreateRequest
	if err := json.Unmarshal(wire, &req); err != nil {
		t.Fatal(err)
	}
	project := &sandboxconfig.ProjectLayer{FilesAdd: []sandboxconfig.File{{Path: "~/.project.json", Content: `{"model":"project"}`}}}
	doc, err := buildSandboxDocument(linuxPaths, "project-1", "sandbox-1", "pool-1", "key", "", "image", proxyagent.PoolEndpoints(""), &req, nil, project)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc.Image.SaveBack, &definition) || !reflect.DeepEqual(doc.Runtime.CaptureBaseline, &baseline) {
		t.Fatal("pool conversion dropped registered metadata or provenance")
	}
	encoded, err := marshalSandboxDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	var effective sandboxconfig.Config
	if err := json.Unmarshal(encoded, &effective); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(effective.Harness.SaveBack, &definition) {
		t.Fatal("provider contract did not reach agent")
	}
	got := effective.Harness.CaptureBaseline
	if got.ConfigRevision != 17 || got.SecretRevisions["API_TOKEN"] != "secret-id:epoch" || got.Files[0].Content != `{"model":"configured"}` {
		t.Fatal("delivered baseline lost config-owned provenance")
	}
	if !reflect.DeepEqual(got.ProjectFiles, []string{"~/.project.json"}) {
		t.Fatalf("project-owned paths not protected: %#v", got.ProjectFiles)
	}
	i := slices.IndexFunc(effective.Files, func(f sandboxconfig.File) bool { return f.Path == "~/.project.json" })
	if i < 0 || effective.Files[i].Content != `{"model":"project"}` {
		t.Fatal("project addition did not remain effective")
	}
	if len(baseline.ProjectFiles) != 0 {
		t.Fatal("Effective mutated immutable input baseline")
	}
}
func TestCaptureBootstrapRejectsInvalidDefinition(t *testing.T) {
	req := &workerapimodel.PoolSandboxCreateRequest{ResolvedHarnessConfig: workerclient.NewOptResolvedHarnessConfig(workerapimodel.ResolvedHarnessConfig{SaveBack: workerclient.NewOptHarnessConfigSaveBack(workerclient.HarnessConfigSaveBack{Version: 99})})}
	if _, err := buildSandboxDocument(linuxPaths, "project-1", "sandbox-1", "pool-1", "key", "", "image", proxyagent.PoolEndpoints(""), req, nil, nil); err == nil {
		t.Fatal("invalid exporter definition delivered")
	}
}

func TestCaptureBootstrapRequiresMatchingProvenance(t *testing.T) {
	for _, baseline := range []any{nil, map[string]any{"harnessConfigId": "other", "configRevision": 1, "manifestDigest": "sha256:pinned"}} {
		wire, marshalErr := json.Marshal(map[string]any{"sandboxId": "sandbox-1", "config": map[string]any{}, "resolvedHarnessConfig": map[string]any{"id": "harness-1", "name": "Harness", "saveBack": harnesscapture.Definition{Version: 1, Watch: []string{"~/.settings"}, ExportCommand: []string{"/bin/export"}, HandoffCommand: []string{"/bin/handoff"}}, "captureBaseline": baseline}})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if baseline == nil {
			var object map[string]any
			if err := json.Unmarshal(wire, &object); err != nil {
				t.Fatal(err)
			}
			delete(object["resolvedHarnessConfig"].(map[string]any), "captureBaseline")
			wire, marshalErr = json.Marshal(object)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
		}
		var req workerapimodel.PoolSandboxCreateRequest
		if err := json.Unmarshal(wire, &req); err != nil {
			t.Fatal(err)
		}
		if _, err := buildSandboxDocument(linuxPaths, "project-1", "sandbox-1", "pool-1", "key", "", "image", proxyagent.PoolEndpoints(""), &req, nil, nil); err == nil {
			t.Fatal("mismatched provenance delivered")
		}
	}
}

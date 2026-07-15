package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeConfigureControlPlane fakes just enough of the control-plane REST API
// and the sandbox-agent exec/terminal attach protocol to drive
// `discobox harnesses enable` through a full configure sandbox lifecycle:
// create the ephemeral sandbox, resolve/attach its primary terminal, read
// back /run/discobox/harness-configure.json via a "cat" exec, then create the
// real HarnessConfig plus its secrets/bindings. It intentionally does not run a
// real sandbox-agent or Docker; it only exercises the CLI-side orchestration
// in cli/internal/cli/harness.go (runHarnessConfigure, applyHarnessConfigureSecrets).
type fakeConfigureControlPlane struct {
	mu sync.Mutex

	projectID string

	// definition is the raw HarnessDefinition JSON object served under
	// /harness-definitions.
	definition map[string]any
	// existingHarnessConfigs seeds ListHarnessConfigs so `enable` doesn't treat
	// this as the project's first harness config (which would also call
	// SetDefaultHarnessConfig, an endpoint this fake doesn't implement).
	existingHarnessConfigs []map[string]any

	primaryExitCode int64
	primaryStatus   string // "exited" or "failed"

	configureOutput []byte // raw bytes the "cat" exec returns as output

	failCreateSecret bool

	sandboxSeq int
	execSeq    int
	execKinds  map[string]string // execID -> "primary" | "cat"

	events                  []string
	createdHarnessConfigs   []map[string]any
	createdSecrets          []map[string]any
	boundSecrets            []boundSecret
	deletedHarnessConfigIDs []string
	deletedSandboxIDs       []string
}

type boundSecret struct {
	harnessConfigID string
	envName         string
	secretID        string
}

func newFakeConfigureControlPlane() *fakeConfigureControlPlane {
	return &fakeConfigureControlPlane{
		projectID:       "project-1",
		primaryExitCode: 0,
		primaryStatus:   "exited",
		execKinds:       map[string]string{"exec-primary": "primary"},
	}
}

func (fc *fakeConfigureControlPlane) logEvent(event string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.events = append(fc.events, event)
}

func (fc *fakeConfigureControlPlane) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /harness-definitions", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBody(w, http.StatusOK, map[string]any{
			"harnessDefinitions": []any{fc.definition},
		})
	})

	mux.HandleFunc("GET /projects/{project}/harness-configs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBody(w, http.StatusOK, map[string]any{"harnessConfigs": fc.existingHarnessConfigs})
	})

	mux.HandleFunc("POST /projects/{project}/harness-configs", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fc.mu.Lock()
		fc.createdHarnessConfigs = append(fc.createdHarnessConfigs, body)
		fc.mu.Unlock()
		fc.logEvent("create-harness-config")
		harness := map[string]any{
			"id":              "hnc-real",
			"projectId":       fc.projectID,
			"slug":            "configure-test",
			"name":            "Configure Test",
			"runCommand":      []string{"true"},
			"installCommand":  body["installCommand"],
			"relaunchCommand": body["relaunchCommand"],
			"files":           body["files"],
			"secrets":         body["secrets"],
			"createdAt":       testTimeRFC3339,
			"updatedAt":       testTimeRFC3339,
		}
		writeJSONBody(w, http.StatusOK, harness)
	})

	mux.HandleFunc("DELETE /projects/{project}/harness-configs/{id}", func(w http.ResponseWriter, r *http.Request) {
		fc.mu.Lock()
		fc.deletedHarnessConfigIDs = append(fc.deletedHarnessConfigIDs, r.PathValue("id"))
		fc.mu.Unlock()
		fc.logEvent("delete-harness-config")
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("PUT /projects/{project}/harness-configs/{id}/default", func(w http.ResponseWriter, r *http.Request) {
		fc.logEvent("set-default-harness-config")
		writeJSONBody(w, http.StatusOK, map[string]any{
			"id":                     fc.projectID,
			"ownerUserId":            "user-1",
			"name":                   "Project",
			"slug":                   "project-1",
			"default":                true,
			"defaultHarnessConfigId": r.PathValue("id"),
			"createdAt":              testTimeRFC3339,
			"updatedAt":              testTimeRFC3339,
		})
	})

	mux.HandleFunc("POST /projects/{project}/secrets", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fc.mu.Lock()
		fc.createdSecrets = append(fc.createdSecrets, body)
		fail := fc.failCreateSecret
		fc.mu.Unlock()
		fc.logEvent("create-secret")
		if fail {
			writeJSONBody(w, http.StatusInternalServerError, map[string]any{"title": "boom", "status": http.StatusInternalServerError})
			return
		}
		writeJSONBody(w, http.StatusOK, map[string]any{
			"id":                     "secret-1",
			"projectId":              fc.projectID,
			"name":                   body["name"],
			"type":                   body["type"],
			"defaultGrantTTLSeconds": 0,
			"createdAt":              testTimeRFC3339,
			"updatedAt":              testTimeRFC3339,
		})
	})

	mux.HandleFunc("PUT /projects/{project}/harness-configs/{id}/secret-bindings/{envName}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		secretID, _ := body["secretId"].(string)
		fc.mu.Lock()
		fc.boundSecrets = append(fc.boundSecrets, boundSecret{
			harnessConfigID: r.PathValue("id"),
			envName:         r.PathValue("envName"),
			secretID:        secretID,
		})
		fc.mu.Unlock()
		fc.logEvent("bind-secret")
		writeJSONBody(w, http.StatusOK, map[string]any{
			"id":              "binding-1",
			"projectId":       fc.projectID,
			"harnessConfigId": r.PathValue("id"),
			"envName":         r.PathValue("envName"),
			"secretId":        secretID,
			"createdAt":       testTimeRFC3339,
			"updatedAt":       testTimeRFC3339,
		})
	})

	mux.HandleFunc("POST /projects/{project}/sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		fc.mu.Lock()
		fc.sandboxSeq++
		id := fmt.Sprintf("sbx-configure-%d", fc.sandboxSeq)
		fc.mu.Unlock()
		fc.logEvent("create-sandbox")
		writeJSONBody(w, http.StatusAccepted, fc.sandboxJSON(id))
	})

	mux.HandleFunc("GET /projects/{project}/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusOK, fc.sandboxJSON(r.PathValue("id")))
	})

	mux.HandleFunc("DELETE /projects/{project}/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		fc.mu.Lock()
		fc.deletedSandboxIDs = append(fc.deletedSandboxIDs, r.PathValue("id"))
		fc.mu.Unlock()
		fc.logEvent("delete-sandbox")
		w.WriteHeader(http.StatusAccepted)
	})

	// Raw sandbox-agent exec endpoints (bypass the generated client; see
	// (*App).execJSON), all rooted at /api/projects/{p}/sandboxes/{s}/execs.
	mux.HandleFunc("GET /api/projects/{project}/sandboxes/{sandbox}/execs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBody(w, http.StatusOK, map[string]any{
			"execs": []any{fc.execRecord("exec-primary", "primary", true)},
		})
	})

	mux.HandleFunc("POST /api/projects/{project}/sandboxes/{sandbox}/execs", func(w http.ResponseWriter, _ *http.Request) {
		fc.mu.Lock()
		fc.execSeq++
		id := fmt.Sprintf("exec-cat-%d", fc.execSeq)
		fc.execKinds[id] = "cat"
		fc.mu.Unlock()
		fc.logEvent("create-cat-exec")
		writeJSONBody(w, http.StatusCreated, map[string]any{"exec": fc.execRecord(id, "", false)})
	})

	mux.HandleFunc("POST /api/projects/{project}/sandboxes/{sandbox}/execs/{exec}/start", func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusOK, map[string]any{"exec": fc.execRecordFor(r.PathValue("exec"))})
	})

	mux.HandleFunc("GET /api/projects/{project}/sandboxes/{sandbox}/execs/{exec}", func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, http.StatusOK, fc.execRecordFor(r.PathValue("exec")))
	})

	mux.HandleFunc("GET /api/projects/{project}/sandboxes/{sandbox}/execs/{exec}/attach", func(w http.ResponseWriter, r *http.Request) {
		execID := r.PathValue("exec")
		fc.mu.Lock()
		kind := fc.execKinds[execID]
		fc.mu.Unlock()
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		netConn := websocket.NetConn(r.Context(), conn, websocket.MessageBinary)
		defer netConn.Close()
		switch kind {
		case "primary":
			fc.logEvent("attach-primary")
			payload, _ := json.Marshal(attachExitPayload{Status: fc.primaryStatus, ExitCode: int64Ptr(fc.primaryExitCode)})
			_ = writeTerminalFrame(netConn, attachFrameExit, payload)
		case "cat":
			fc.logEvent("attach-cat")
			_ = writeTerminalFrame(netConn, attachFrameOutput, fc.configureOutput)
			payload, _ := json.Marshal(attachExitPayload{Status: "exited", ExitCode: int64Ptr(0)})
			_ = writeTerminalFrame(netConn, attachFrameExit, payload)
		}
		// Give the client a moment to read before the deferred close tears
		// down the connection.
		time.Sleep(20 * time.Millisecond)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func (fc *fakeConfigureControlPlane) sandboxJSON(id string) map[string]any {
	return map[string]any{
		"id":              id,
		"projectId":       fc.projectID,
		"createdByUserId": "user-1",
		"config": map[string]any{
			"name":         "configure-" + id,
			"image":        "",
			"cpuVcpus":     0,
			"memoryBytes":  0,
			"storageBytes": 0,
		},
		"runtime": map[string]any{
			"phase":               "running",
			"desiredState":        "running",
			"lastOperationStatus": "success",
			"generation":          1,
			"observedGeneration":  1,
			"restartGeneration":   0,
			"restartedGeneration": 0,
		},
		"createdAt": testTimeRFC3339,
		"updatedAt": testTimeRFC3339,
	}
}

func (fc *fakeConfigureControlPlane) execRecord(id, harnessID string, primary bool) map[string]any {
	rec := map[string]any{
		"id":        id,
		"status":    "running",
		"command":   []string{"sh"},
		"workdir":   "",
		"createdAt": testTimeRFC3339,
	}
	if harnessID != "" {
		rec["harnessId"] = harnessID
		rec["primary"] = primary
	}
	return rec
}

// execRecordFor returns the exec record for id reflecting how its attach
// would have concluded: the seeded primary terminal reflects
// primaryStatus/primaryExitCode, and any "cat" exec always reports a clean
// exit, matching what the attach handler streamed.
func (fc *fakeConfigureControlPlane) execRecordFor(id string) map[string]any {
	fc.mu.Lock()
	kind := fc.execKinds[id]
	status := fc.primaryStatus
	exitCode := fc.primaryExitCode
	fc.mu.Unlock()

	harnessID := ""
	primary := false
	if kind == "primary" {
		harnessID = "inline"
		primary = true
	} else {
		status = "exited"
		exitCode = 0
	}
	rec := fc.execRecord(id, harnessID, primary)
	rec["status"] = status
	rec["exitCode"] = exitCode
	return rec
}

func writeJSONBody(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		panic(err)
	}
}

func int64Ptr(v int64) *int64 { return &v }

const testTimeRFC3339 = "2026-07-10T00:00:00Z"

func configureTestDefinition() map[string]any {
	return map[string]any{
		"id":         "configure-test",
		"name":       "configure-test",
		"runCommand": []string{"true"},
		"configure": map[string]any{
			"harnessConfig": map[string]any{
				"runCommand": []string{"sh", "configure.sh"},
			},
		},
	}
}

func runEnableCommand(t *testing.T, server *httptest.Server, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(""))
	fullArgs := append([]string{"--server", server.URL, "--project", "project-1"}, args...)
	cmd.SetArgs(fullArgs)
	err := cmd.Execute()
	return &out, err
}

func TestHarnessEnableRunsConfigureHappyPath(t *testing.T) {
	fc := newFakeConfigureControlPlane()
	fc.definition = configureTestDefinition()
	fc.existingHarnessConfigs = []map[string]any{
		{"id": "hnc-other", "projectId": "project-1", "slug": "other", "name": "Other", "runCommand": []string{"true"}, "createdAt": testTimeRFC3339, "updatedAt": testTimeRFC3339},
	}
	configureOutput, err := json.Marshal(map[string]any{
		"secrets": []any{
			map[string]any{
				"envName": "ANTHROPIC_API_KEY",
				"name":    "Anthropic API key",
				"type":    "bearer",
				"value":   map[string]any{"token": "sk-test-token"},
			},
		},
		"files": []any{
			map[string]any{"path": ".claude.json", "content": `{"theme":"dark"}`},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fc.configureOutput = configureOutput

	server := fc.server(t)
	out, err := runEnableCommand(t, server, "harnesses", "enable", "configure-test")
	if err != nil {
		t.Fatalf("harnesses enable: %v\noutput: %s", err, out.String())
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()

	if len(fc.createdHarnessConfigs) != 1 {
		t.Fatalf("createdHarnessConfigs = %d, want 1", len(fc.createdHarnessConfigs))
	}
	if len(fc.createdSecrets) != 1 || fc.createdSecrets[0]["name"] != "Anthropic API key" {
		t.Fatalf("createdSecrets = %#v, want one Anthropic API key secret", fc.createdSecrets)
	}
	if len(fc.boundSecrets) != 1 || fc.boundSecrets[0].envName != "ANTHROPIC_API_KEY" || fc.boundSecrets[0].harnessConfigID != "hnc-real" {
		t.Fatalf("boundSecrets = %#v, want ANTHROPIC_API_KEY bound to hnc-real", fc.boundSecrets)
	}
	if len(fc.deletedSandboxIDs) != 1 {
		t.Fatalf("deletedSandboxIDs = %#v, want the ephemeral configure sandbox deleted", fc.deletedSandboxIDs)
	}
	if len(fc.deletedHarnessConfigIDs) != 0 {
		t.Fatalf("deletedHarnessConfigIDs = %#v, want no rollback on success", fc.deletedHarnessConfigIDs)
	}

	// The real HarnessConfig must only be created after the configure sandbox's
	// primary terminal ran and its output was read back.
	createSandboxIdx := indexOf(fc.events, "create-sandbox")
	attachPrimaryIdx := indexOf(fc.events, "attach-primary")
	createHarnessConfigIdx := indexOf(fc.events, "create-harness-config")
	if createSandboxIdx < 0 || attachPrimaryIdx < 0 || createHarnessConfigIdx < 0 {
		t.Fatalf("events = %#v, missing expected steps", fc.events)
	}
	if createSandboxIdx >= attachPrimaryIdx || attachPrimaryIdx >= createHarnessConfigIdx {
		t.Fatalf("events = %#v, want create-sandbox < attach-primary < create-harness-config", fc.events)
	}

	files := fc.createdHarnessConfigs[0]["files"]
	filesJSON, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(filesJSON), ".claude.json") {
		t.Fatalf("created harness config files = %s, want .claude.json from configure output", filesJSON)
	}
}

func TestHarnessEnableConfigureNonZeroExitAbortsBeforeCreatingHarnessConfig(t *testing.T) {
	fc := newFakeConfigureControlPlane()
	fc.definition = configureTestDefinition()
	fc.primaryExitCode = 1
	fc.configureOutput = []byte(`{"secrets":[],"files":[]}`)

	server := fc.server(t)
	_, err := runEnableCommand(t, server, "harnesses", "enable", "configure-test")
	if err == nil {
		t.Fatal("harnesses enable = nil error, want failure on non-zero configure exit")
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.createdHarnessConfigs) != 0 {
		t.Fatalf("createdHarnessConfigs = %#v, want none created when configure fails", fc.createdHarnessConfigs)
	}
	if len(fc.deletedSandboxIDs) != 1 {
		t.Fatalf("deletedSandboxIDs = %#v, want the ephemeral sandbox cleaned up even on failure", fc.deletedSandboxIDs)
	}
}

func TestHarnessEnableConfigureSecretFailureRollsBackHarnessConfig(t *testing.T) {
	fc := newFakeConfigureControlPlane()
	fc.definition = configureTestDefinition()
	fc.failCreateSecret = true
	configureOutput, err := json.Marshal(map[string]any{
		"secrets": []any{
			map[string]any{"envName": "ANTHROPIC_API_KEY", "name": "key", "type": "bearer", "value": map[string]any{"token": "x"}},
		},
		"files": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	fc.configureOutput = configureOutput

	server := fc.server(t)
	_, err = runEnableCommand(t, server, "harnesses", "enable", "configure-test")
	if err == nil {
		t.Fatal("harnesses enable = nil error, want failure when secret creation fails")
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.createdHarnessConfigs) != 1 {
		t.Fatalf("createdHarnessConfigs = %d, want the config created before the failed secret apply", len(fc.createdHarnessConfigs))
	}
	if len(fc.deletedHarnessConfigIDs) != 1 || fc.deletedHarnessConfigIDs[0] != "hnc-real" {
		t.Fatalf("deletedHarnessConfigIDs = %#v, want hnc-real rolled back", fc.deletedHarnessConfigIDs)
	}
}

func TestHarnessEnableNoConfigureFlagSkipsConfigureSandbox(t *testing.T) {
	fc := newFakeConfigureControlPlane()
	fc.definition = configureTestDefinition()

	server := fc.server(t)
	_, err := runEnableCommand(t, server, "harnesses", "enable", "--no-configure", "configure-test")
	if err != nil {
		t.Fatalf("harnesses enable --no-configure: %v", err)
	}

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if indexOf(fc.events, "create-sandbox") >= 0 {
		t.Fatalf("events = %#v, want no configure sandbox created with --no-configure", fc.events)
	}
	if len(fc.createdHarnessConfigs) != 1 {
		t.Fatalf("createdHarnessConfigs = %d, want 1", len(fc.createdHarnessConfigs))
	}
}

func indexOf(values []string, target string) int {
	for i, v := range values {
		if v == target {
			return i
		}
	}
	return -1
}

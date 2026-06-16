package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/server/internal/api"
)

func TestCreateAgentConfig(t *testing.T) {
	h := newTestAPI(t).h

	config := createAgentConfig(t, h, "Codex")
	if config.ID == "" {
		t.Fatal("expected agent config ID")
	}
	if config.Name != "Codex" {
		t.Fatalf("agent config name = %q, want Codex", config.Name)
	}
	if config.RunCommand != "codex exec" {
		t.Fatalf("agent config run command = %q, want codex exec", config.RunCommand)
	}

	resp := h.Get(projectURL() + "/agent-configs")
	if resp.Code != http.StatusOK {
		t.Fatalf("list agent configs status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var list api.ListAgentConfigsBody
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode agent configs: %v", err)
	}
	if len(list.AgentConfigs) != 1 || list.AgentConfigs[0].ID != config.ID {
		t.Fatalf("agent configs = %#v, want created config", list.AgentConfigs)
	}
}

func TestAgentConfigDefinitions(t *testing.T) {
	h := newTestAPI(t).h

	resp := h.Get("/agent-config-definitions")
	if resp.Code != http.StatusOK {
		t.Fatalf("list agent config definitions status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var list api.ListAgentConfigDefinitionsBody
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode agent config definitions: %v", err)
	}
	if len(list.AgentConfigDefinitions) == 0 {
		t.Fatal("expected agent config definitions")
	}
	var codex *model.AgentConfigDefinition
	for i := range list.AgentConfigDefinitions {
		if list.AgentConfigDefinitions[i].ID == "codex" {
			codex = &list.AgentConfigDefinitions[i]
			break
		}
	}
	if codex == nil {
		t.Fatalf("agent config definitions = %#v, want codex", list.AgentConfigDefinitions)
	}
	if codex.Name != "Codex" || codex.RunCommand == "" {
		t.Fatalf("codex definition = %#v, want populated definition", codex)
	}

	resp = h.Get("/agent-config-definitions/codex")
	if resp.Code != http.StatusOK {
		t.Fatalf("get agent config definition status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var got model.AgentConfigDefinition
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode agent config definition: %v", err)
	}
	if got.ID != "codex" || got.Name != "Codex" {
		t.Fatalf("agent config definition = %#v, want codex", got)
	}

	resp = h.Get("/agent-config-definitions/missing")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("missing agent config definition status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

func TestCreateAgentConfigFromDefinition(t *testing.T) {
	h := newTestAPI(t).h

	resp := h.Post(projectURL()+"/agent-configs", map[string]any{
		"definitionId": "codex",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("create agent config from definition status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var config model.AgentConfig
	if err := json.Unmarshal(resp.Body.Bytes(), &config); err != nil {
		t.Fatalf("decode agent config: %v", err)
	}
	if config.ID == "" {
		t.Fatal("expected agent config ID")
	}
	if config.Name != "Codex" {
		t.Fatalf("agent config name = %q, want Codex", config.Name)
	}
	if config.InstallCommand != "npm install -g @openai/codex" {
		t.Fatalf("agent config install command = %q, want codex install", config.InstallCommand)
	}
	if config.RunCommand != "codex exec" {
		t.Fatalf("agent config run command = %q, want codex exec", config.RunCommand)
	}
	if len(config.Capabilities) == 0 {
		t.Fatal("expected capabilities from definition")
	}

	resp = h.Post(projectURL()+"/agent-configs", map[string]any{
		"definitionId": "missing",
	})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("create agent config from missing definition status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

func TestCreateSandboxWithAgentConfigByName(t *testing.T) {
	h := newTestAPI(t).h
	config := createAgentConfig(t, h, "Codex")

	resp := h.Post(projectURL()+"/sandboxes", map[string]any{
		"name":                     "agent sandbox",
		"agentName":                "Codex",
		"agentModel":               "gpt-5.1-codex-max",
		"agentModelServiceTier":    "priority",
		"agentModelReasoningLevel": "high",
		"prompt":                   "implement the change",
		"sourceUrl":                "file:///tmp/repo",
		"sourceRef":                "main",
		"sourceRefType":            "branch",
		"sourceDirectory":          "/workspace/repo",
		"workingDirectory":         "/workspace/repo",
		"sourceCodeReferences": map[string]any{
			"lib": map[string]any{
				"url":       "https://example.com/lib.git",
				"ref":       "abc123",
				"refType":   "commit",
				"directory": "/workspace/lib",
			},
		},
		"userUid": 1000,
		"userGid": 1000,
	})
	if resp.Code != http.StatusAccepted {
		t.Fatalf("create sandbox status = %d, body = %s", resp.Code, resp.Body.String())
	}
	sandbox := decodeSandbox(t, resp.Body.Bytes())
	if sandbox.AgentConfigID == nil || *sandbox.AgentConfigID != config.ID {
		t.Fatalf("agentConfigId = %v, want %q", sandbox.AgentConfigID, config.ID)
	}
	if sandbox.AgentConfig == nil || sandbox.AgentConfig.Name != config.Name {
		t.Fatalf("agentConfig = %#v, want %q", sandbox.AgentConfig, config.Name)
	}
	if sandbox.AgentModel == nil || *sandbox.AgentModel != "gpt-5.1-codex-max" {
		t.Fatalf("agentModel = %v, want gpt-5.1-codex-max", sandbox.AgentModel)
	}
	if sandbox.AgentModelServiceTier == nil || *sandbox.AgentModelServiceTier != "priority" {
		t.Fatalf("agentModelServiceTier = %v, want priority", sandbox.AgentModelServiceTier)
	}
	if sandbox.AgentModelReasoningLevel == nil || *sandbox.AgentModelReasoningLevel != "high" {
		t.Fatalf("agentModelReasoningLevel = %v, want high", sandbox.AgentModelReasoningLevel)
	}
	if sandbox.Prompt == nil || *sandbox.Prompt != "implement the change" {
		t.Fatalf("prompt = %v, want implement the change", sandbox.Prompt)
	}
	if sandbox.SourceRefType == nil || *sandbox.SourceRefType != "branch" {
		t.Fatalf("sourceRefType = %v, want branch", sandbox.SourceRefType)
	}
	if sandbox.SourceDirectory == nil || *sandbox.SourceDirectory != "/workspace/repo" {
		t.Fatalf("sourceDirectory = %v, want /workspace/repo", sandbox.SourceDirectory)
	}
	if sandbox.UserUID == nil || *sandbox.UserUID != 1000 || sandbox.UserGID == nil || *sandbox.UserGID != 1000 {
		t.Fatalf("user uid/gid = %v/%v, want 1000/1000", sandbox.UserUID, sandbox.UserGID)
	}
	lib, ok := sandbox.SourceCodeReferences["lib"]
	if !ok {
		t.Fatal("expected lib sourceCodeReference")
	}
	if lib.Directory != "/workspace/lib" {
		t.Fatalf("nested source directory = %q, want /workspace/lib", lib.Directory)
	}
}

func createAgentConfig(t *testing.T, h humatest.TestAPI, name string) model.AgentConfig {
	t.Helper()
	resp := h.Post(projectURL()+"/agent-configs", map[string]any{
		"name":           name,
		"installCommand": "npm install -g @openai/codex",
		"runCommand":     "codex exec",
		"capabilities": map[string]any{
			"tools": []string{"shell", "edit"},
		},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("create agent config status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var config model.AgentConfig
	if err := json.Unmarshal(resp.Body.Bytes(), &config); err != nil {
		t.Fatalf("decode agent config: %v", err)
	}
	return config
}

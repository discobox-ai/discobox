package model_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	rootopenapi "github.com/discobox-ai/discobox/api/openapi"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// The stream's payload is no generated type: ogen emits only schemas a JSON
// body references, and the stream's body is text/event-stream. So the
// documented schemas are pinned to what the model writes here, field for
// field (ADR 26-10-30-868 §3).
func TestSandboxEventMatchesItsSchema(t *testing.T) {
	event := model.SandboxEvent{
		SandboxProjection: model.SandboxProjection{
			SandboxID: "sbx", ProjectID: "prj", Name: "alpha",
			DesiredState: model.DesiredStatePresent, State: "ready", RuntimeState: model.SandboxRuntimeStateRunning,
			Agent:       &model.SandboxEventAgent{State: "blocked", Kind: "permission"},
			ErrorReason: model.FailureReasonImageUnavailable,
		},
		At: time.Now(),
	}
	for name, value := range map[string]any{"SandboxEvent": event, "SandboxEventAgent": event.Agent} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		written := make([]string, 0, len(fields))
		for field := range fields {
			written = append(written, field)
		}
		documented := schemaProperties(t, name)
		slices.Sort(written)
		slices.Sort(documented)
		if !slices.Equal(written, documented) {
			t.Errorf("%s: the model writes %v, server.yaml documents %v", name, written, documented)
		}
	}
}

// A field the schema requires is one the model always writes, empty or not.
func TestSandboxEventWritesEveryRequiredField(t *testing.T) {
	encoded, err := json.Marshal(model.SandboxEvent{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, required := range schemaRequired(t, "SandboxEvent") {
		if _, ok := fields[required]; !ok {
			t.Errorf("SandboxEvent requires %q, which an empty event does not write", required)
		}
	}
}

type openAPISchema struct {
	Properties map[string]any `yaml:"properties"`
	Required   []string       `yaml:"required"`
}

func serverSchema(t *testing.T, name string) openAPISchema {
	t.Helper()
	var document struct {
		Components struct {
			Schemas map[string]openAPISchema `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(rootopenapi.ServerYAML, &document); err != nil {
		t.Fatal(err)
	}
	schema, ok := document.Components.Schemas[name]
	if !ok {
		t.Fatalf("server.yaml has no schema %s", name)
	}
	return schema
}

func schemaProperties(t *testing.T, name string) []string {
	t.Helper()
	properties := []string{}
	for property := range serverSchema(t, name).Properties {
		properties = append(properties, property)
	}
	return properties
}

func schemaRequired(t *testing.T, name string) []string {
	t.Helper()
	return serverSchema(t, name).Required
}

// The agent is the newest primary terminal's program, read only while the
// sandbox runs; a payload that does not decode is no agent, not a failure.
func TestSandboxProjectionReadsTheNewestPrimaryProgram(t *testing.T) {
	status := model.RawJSON(`{"sessions":[
		{"terminalId":"old","primary":true,"startedAt":"2026-10-30T10:00:00Z","programStatus":{"state":"done","reportedAt":"2026-10-30T10:00:00Z"}},
		{"terminalId":"new","primary":true,"startedAt":"2026-10-30T11:00:00Z","programStatus":{"state":"blocked","kind":"question","reportedAt":"2026-10-30T11:00:00Z"}},
		{"terminalId":"side","primary":false,"startedAt":"2026-10-30T12:00:00Z","programStatus":{"state":"working","reportedAt":"2026-10-30T12:00:00Z"}}
	]}`)
	sandbox := model.Sandbox{RuntimeState: model.SandboxRuntimeStateRunning, AgentStatus: status}
	if agent := sandbox.EventProjection().Agent; agent == nil || agent.State != "blocked" || agent.Kind != "question" {
		t.Fatalf("agent = %+v, want the newest primary's", agent)
	}
	sandbox.RuntimeState = model.SandboxRuntimeStateStopped
	if agent := sandbox.EventProjection().Agent; agent != nil {
		t.Fatalf("stopped agent = %+v, want none", agent)
	}
	// A newer primary that has reported nothing, or a state the protocol
	// does not name, leaves the older one standing, as a listing reads it.
	sandbox.RuntimeState = model.SandboxRuntimeStateRunning
	sandbox.AgentStatus = model.RawJSON(`{"sessions":[
		{"terminalId":"old","primary":true,"startedAt":"2026-10-30T10:00:00Z","programStatus":{"state":"blocked","kind":"mystery","reportedAt":"2026-10-30T10:00:00Z"}},
		{"terminalId":"new","primary":true,"startedAt":"2026-10-30T11:00:00Z"},
		{"terminalId":"newer","primary":true,"startedAt":"2026-10-30T12:00:00Z","programStatus":{"state":"thinking","reportedAt":"2026-10-30T12:00:00Z"}}
	]}`)
	if agent := sandbox.EventProjection().Agent; agent == nil || agent.State != "blocked" || agent.Kind != "" {
		t.Fatalf("agent = %+v, want the older primary, blocked, its unknown kind dropped", agent)
	}
	sandbox.AgentStatus = model.RawJSON(`{"sessions":[{"primary":true,"startedAt":"2026-10-30T11:00:00Z"}]}`)
	if agent := sandbox.EventProjection().Agent; agent != nil {
		t.Fatalf("agent with no program status = %+v, want none", agent)
	}
	sandbox.AgentStatus = model.RawJSON(`{"sessions":"not a list"}`)
	if agent := sandbox.EventProjection().Agent; agent != nil {
		t.Fatalf("undecodable agent = %+v, want none", agent)
	}
}

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"

	"github.com/discobox-ai/discobox/sandbox-agent/intake"
	"github.com/discobox-ai/discobox/sandbox-agent/sourceconverge"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

// GetSandboxRuntimeConfig answers with the runtime-config document the sandbox
// last applied (ADR 0126 §3).
func (h *handler) GetSandboxRuntimeConfig(_ context.Context, _ sandboxapi.GetSandboxRuntimeConfigParams) (*sandboxapi.SandboxRuntimeConfig, error) {
	if h.runtimeConfig == nil {
		return nil, errRuntimeConfigUnavailable
	}
	applied, ok := h.runtimeConfig.Applied()
	if !ok {
		return nil, statusError{status: http.StatusNotFound, message: "no runtime config has been applied"}
	}
	return runtimeConfigToWire(intake.WithoutClientKey(applied))
}

// PutSandboxRuntimeConfig applies a delivered document and answers with the
// one the sandbox now holds, which is older than the delivery only when the
// delivery was itself out of date. Neither route answers with the client key:
// it is delivered, never read back.
func (h *handler) PutSandboxRuntimeConfig(_ context.Context, req *sandboxapi.SandboxRuntimeConfig, _ sandboxapi.PutSandboxRuntimeConfigParams) (*sandboxapi.SandboxRuntimeConfig, error) {
	if h.runtimeConfig == nil {
		return nil, errRuntimeConfigUnavailable
	}
	doc, err := runtimeConfigFromWire(req)
	if err != nil {
		return nil, statusError{status: http.StatusBadRequest, message: err.Error()}
	}
	held, err := h.runtimeConfig.Apply(doc)
	switch {
	case errors.Is(err, intake.ErrInvalid):
		return nil, statusError{status: http.StatusUnprocessableEntity, message: err.Error()}
	case errors.Is(err, intake.ErrConflict):
		return nil, statusError{status: http.StatusConflict, message: err.Error()}
	case err != nil:
		return nil, err
	}
	// The sources converge on what the sandbox now holds, which is this
	// document unless it was out of date; converging on the held one again is
	// a no-op for every source already materialized.
	if h.sourceConverger != nil {
		h.sourceConverger.Converge(held)
	}
	return runtimeConfigToWire(intake.WithoutClientKey(held))
}

// GetSandboxSourceProjectLayer answers with a materialized source's project
// layer, which the pool reads to settle the sandbox's spec before it marks the
// source delivered (ADR 0055, ADR 0126 §4).
func (h *handler) GetSandboxSourceProjectLayer(ctx context.Context, params sandboxapi.GetSandboxSourceProjectLayerParams) (*sandboxapi.SandboxSourceProjectLayer, error) {
	if h.sourceConverger == nil {
		return nil, errRuntimeConfigUnavailable
	}
	layer, err := h.sourceConverger.ProjectLayer(ctx, params.Slug)
	switch {
	case errors.Is(err, sourceconverge.ErrUnknownSource):
		return nil, statusError{status: http.StatusNotFound, message: err.Error()}
	case errors.Is(err, sourceconverge.ErrNotMaterialized):
		return nil, statusError{status: http.StatusConflict, message: err.Error()}
	case errors.Is(err, sourceconverge.ErrInvalidProjectLayer):
		return nil, statusError{status: http.StatusUnprocessableEntity, message: err.Error()}
	case err != nil:
		return nil, err
	}
	out := &sandboxapi.SandboxSourceProjectLayer{Slug: layer.Slug, Commit: layer.Commit}
	if layer.Layer != nil {
		var object sandboxapi.SandboxSourceProjectLayerProjectLayer
		if err := object.UnmarshalJSON(layer.Layer); err != nil {
			return nil, statusError{status: http.StatusUnprocessableEntity, message: err.Error()}
		}
		out.ProjectLayer = sandboxapi.NewOptSandboxSourceProjectLayerProjectLayer(object)
	}
	return out, nil
}

// sandboxAgentSourceStates is the converger's per-source report on the wire.
func sandboxAgentSourceStates(states []sourceconverge.SourceState) []sandboxapi.SandboxAgentSourceState {
	if len(states) == 0 {
		return nil
	}
	out := make([]sandboxapi.SandboxAgentSourceState, 0, len(states))
	for _, state := range states {
		wire := sandboxapi.SandboxAgentSourceState{
			Slug:      state.Slug,
			State:     sandboxapi.SandboxAgentSourceStateState(state.State),
			Revision:  state.Revision,
			UpdatedAt: state.UpdatedAt,
		}
		if state.Commit != "" {
			wire.Commit = sandboxapi.NewOptString(state.Commit)
		}
		if state.Error != "" {
			wire.Error = sandboxapi.NewOptString(state.Error)
		}
		out = append(out, wire)
	}
	return out
}

var errRuntimeConfigUnavailable = statusError{status: http.StatusServiceUnavailable, message: "the runtime-config intake is not available in this sandbox"}

// runtimeConfigFromWire and runtimeConfigToWire move a document between the
// generated schema and sandboxconfig.RuntimeConfig, the type the pool and the
// intake share. The two describe one wire shape, so the conversion is through
// that shape rather than field by field: a field added to one and not the
// other fails the round-trip test instead of being silently dropped here.
func runtimeConfigFromWire(in *sandboxapi.SandboxRuntimeConfig) (sandboxconfig.RuntimeConfig, error) {
	var out sandboxconfig.RuntimeConfig
	if in == nil {
		return out, errors.New("a runtime config document is required")
	}
	data, err := in.MarshalJSON()
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(data, &out)
	return out, err
}

func runtimeConfigToWire(in sandboxconfig.RuntimeConfig) (*sandboxapi.SandboxRuntimeConfig, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out sandboxapi.SandboxRuntimeConfig
	if err := out.UnmarshalJSON(data); err != nil {
		return nil, err
	}
	return &out, nil
}

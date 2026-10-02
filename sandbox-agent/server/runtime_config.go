package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"

	"github.com/discobox-ai/discobox/sandbox-agent/intake"
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
	return runtimeConfigToWire(intake.WithoutClientKey(held))
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

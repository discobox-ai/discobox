package handlers

import (
	"context"
	"net/http"

	serverapi "github.com/discobox-ai/discobox/api/gen"
)

// Sandbox exec, service, and harness-terminal operations are served inside the
// sandbox and reverse-proxied by the control plane, so the generated
// control-plane handler only needs not-implemented stubs to satisfy the
// interface.

func (h *Handler) AttachSandboxExec(context.Context, serverapi.AttachSandboxExecParams) (serverapi.AttachSandboxExecRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) AttachSandboxExecOnce(context.Context, serverapi.AttachSandboxExecOnceReq, serverapi.AttachSandboxExecOnceParams) (serverapi.AttachSandboxExecOnceRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) CreateSandboxExec(context.Context, *serverapi.CreateSandboxExecRequest, serverapi.CreateSandboxExecParams) (serverapi.CreateSandboxExecRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) DeleteSandboxExec(context.Context, serverapi.DeleteSandboxExecParams) (serverapi.DeleteSandboxExecRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) GetSandboxExec(context.Context, serverapi.GetSandboxExecParams) (serverapi.GetSandboxExecRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) GetSandboxExecResources(context.Context, serverapi.GetSandboxExecResourcesParams) (serverapi.GetSandboxExecResourcesRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

// GetSandboxAgentStatus is deliberately left unproxied for now (ADR 0030's
// stated non-goal): the sandbox-agent status endpoint is reached only by
// pool-agent's periodic poll, not by an on-demand user request through the
// control plane.
func (h *Handler) GetSandboxAgentStatus(context.Context, serverapi.GetSandboxAgentStatusParams) (serverapi.GetSandboxAgentStatusRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) ListHarnessHooks(context.Context, serverapi.ListHarnessHooksParams) (serverapi.ListHarnessHooksRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) ListSandboxExecEvents(context.Context, serverapi.ListSandboxExecEventsParams) (serverapi.ListSandboxExecEventsRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) ListSandboxExecLogs(context.Context, serverapi.ListSandboxExecLogsParams) (serverapi.ListSandboxExecLogsRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) ListSandboxExecResourceHistory(context.Context, serverapi.ListSandboxExecResourceHistoryParams) (serverapi.ListSandboxExecResourceHistoryRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) ListSandboxExecs(context.Context, serverapi.ListSandboxExecsParams) (serverapi.ListSandboxExecsRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) ListSandboxServices(context.Context, serverapi.ListSandboxServicesParams) (serverapi.ListSandboxServicesRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) GetSandboxService(context.Context, serverapi.GetSandboxServiceParams) (serverapi.GetSandboxServiceRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) ListSandboxServiceLogs(context.Context, serverapi.ListSandboxServiceLogsParams) (serverapi.ListSandboxServiceLogsRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) StartSandboxService(context.Context, serverapi.StartSandboxServiceParams) (serverapi.StartSandboxServiceRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) StopSandboxService(context.Context, serverapi.StopSandboxServiceParams) (serverapi.StopSandboxServiceRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) RestartSandboxService(context.Context, serverapi.RestartSandboxServiceParams) (serverapi.RestartSandboxServiceRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) StartSandboxExec(context.Context, serverapi.StartSandboxExecParams) (serverapi.StartSandboxExecRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) StreamSandboxExecResources(context.Context, serverapi.StreamSandboxExecResourcesParams) (serverapi.StreamSandboxExecResourcesRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func sandboxAgentRuntimeNotImplemented() *serverapi.ErrorResponseStatusCode {
	return &serverapi.ErrorResponseStatusCode{
		StatusCode: http.StatusNotImplemented,
		Response: serverapi.ErrorResponse{
			Error: "sandbox agent runtime operations are handled by the reverse proxy",
		},
	}
}

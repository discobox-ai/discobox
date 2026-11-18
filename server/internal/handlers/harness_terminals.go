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

func (h *Handler) GetSandboxExecScreen(context.Context, serverapi.GetSandboxExecScreenParams) (serverapi.GetSandboxExecScreenRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) GetSandboxExecResources(context.Context, serverapi.GetSandboxExecResourcesParams) (serverapi.GetSandboxExecResourcesRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) ListHarnessHooks(context.Context, serverapi.ListHarnessHooksParams) (serverapi.ListHarnessHooksRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) ListExecEvents(context.Context, serverapi.ListExecEventsParams) (serverapi.ListExecEventsRes, error) {
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

func (h *Handler) ListSandboxTools(context.Context, serverapi.ListSandboxToolsParams) (serverapi.ListSandboxToolsRes, error) {
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

// ReprovisionSandbox is forwarded to the sandbox's agent by the reverse proxy
// (ADR 26-11-11-560 §5), which answers it.
func (h *Handler) ReprovisionSandbox(context.Context, serverapi.ReprovisionSandboxParams) (serverapi.ReprovisionSandboxRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) SendSandboxExecInput(context.Context, *serverapi.SandboxExecInputBody, serverapi.SendSandboxExecInputParams) (serverapi.SendSandboxExecInputRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) StartSandboxExec(context.Context, serverapi.StartSandboxExecParams) (serverapi.StartSandboxExecRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) StreamSandboxExecResources(context.Context, serverapi.StreamSandboxExecResourcesParams) (serverapi.StreamSandboxExecResourcesRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) WaitSandboxExec(context.Context, *serverapi.SandboxExecWaitBody, serverapi.WaitSandboxExecParams) (serverapi.WaitSandboxExecRes, error) {
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

// The files API (ADR 26-10-22-499) is the sandbox agent's, forwarded by the
// reverse proxy like the exec routes, so none of it is served here.

func (h *Handler) StatSandboxFile(context.Context, serverapi.StatSandboxFileParams) (serverapi.StatSandboxFileRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) ListSandboxFiles(context.Context, serverapi.ListSandboxFilesParams) (serverapi.ListSandboxFilesRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) ReadSandboxFile(context.Context, serverapi.ReadSandboxFileParams) (serverapi.ReadSandboxFileRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) WriteSandboxFile(context.Context, serverapi.WriteSandboxFileReq, serverapi.WriteSandboxFileParams) (serverapi.WriteSandboxFileRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) MakeSandboxDirectory(context.Context, *serverapi.SandboxFileMkdirRequest, serverapi.MakeSandboxDirectoryParams) (serverapi.MakeSandboxDirectoryRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) RemoveSandboxFile(context.Context, *serverapi.SandboxFileRemoveRequest, serverapi.RemoveSandboxFileParams) (serverapi.RemoveSandboxFileRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) MoveSandboxFile(context.Context, *serverapi.SandboxFileTransferRequest, serverapi.MoveSandboxFileParams) (serverapi.MoveSandboxFileRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) CopySandboxFile(context.Context, *serverapi.SandboxFileTransferRequest, serverapi.CopySandboxFileParams) (serverapi.CopySandboxFileRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) ExportSandboxFiles(context.Context, serverapi.ExportSandboxFilesParams) (serverapi.ExportSandboxFilesRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}
func (h *Handler) ImportSandboxFiles(context.Context, serverapi.ImportSandboxFilesReq, serverapi.ImportSandboxFilesParams) (serverapi.ImportSandboxFilesRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}
func (h *Handler) SetSandboxFileMetadata(context.Context, *serverapi.SandboxFileMetadataRequest, serverapi.SetSandboxFileMetadataParams) (serverapi.SetSandboxFileMetadataRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

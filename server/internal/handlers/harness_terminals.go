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

// GetSandboxAgentStatus is deliberately left unproxied (ADR 0030's
// stated non-goal): the sandbox-agent status endpoint is reached only by
// pool-agent's periodic poll, not by an on-demand user request through the
// control plane.
func (h *Handler) GetSandboxAgentStatus(context.Context, serverapi.GetSandboxAgentStatusParams) (serverapi.GetSandboxAgentStatusRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

// GetSandboxAgentInventory is left unproxied like the status: the inventory
// is read only by the pool agent's poll (ADR 26-10-11-657 §1), and what a
// client reads of it comes back through the list routes it answers.
func (h *Handler) GetSandboxAgentInventory(context.Context, serverapi.GetSandboxAgentInventoryParams) (serverapi.GetSandboxAgentInventoryRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

// UpdateSandboxAgentMeta is the sandbox agent's half of a meta write. The
// control plane's own is UpdateSandboxMeta, which calls it and records the
// answer (ADR 0136); this route is neither served nor proxied here.
func (h *Handler) UpdateSandboxAgentMeta(context.Context, *serverapi.UpdateSandboxMetaBody, serverapi.UpdateSandboxAgentMetaParams) (serverapi.UpdateSandboxAgentMetaRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

// GetSandboxRuntimeConfig and PutSandboxRuntimeConfig are the sandbox
// agent's runtime-config intake, which its pool drives (ADR 0126 §3). The
// control plane neither serves nor proxies them: what a sandbox is told is the
// pool's to say.
func (h *Handler) GetSandboxRuntimeConfig(context.Context, serverapi.GetSandboxRuntimeConfigParams) (serverapi.GetSandboxRuntimeConfigRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

func (h *Handler) PutSandboxRuntimeConfig(context.Context, *serverapi.SandboxRuntimeConfig, serverapi.PutSandboxRuntimeConfigParams) (serverapi.PutSandboxRuntimeConfigRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

// GetSandboxSourceProjectLayer is the read the pool settles a sandbox's spec
// with once the sandbox has materialized a source (ADR 0126 §4). Like the
// intake beside it, it is the pool's route, neither served nor proxied here.
func (h *Handler) GetSandboxSourceProjectLayer(context.Context, serverapi.GetSandboxSourceProjectLayerParams) (serverapi.GetSandboxSourceProjectLayerRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

// GetSandboxSourceStates is what the pool waits on while it settles a create
// (ADR 0126 §4). Like the intake beside it, it is the pool's route, neither
// served nor proxied here.
func (h *Handler) GetSandboxSourceStates(context.Context, serverapi.GetSandboxSourceStatesParams) (serverapi.GetSandboxSourceStatesRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

// ConfirmSandboxAutostop is what a pool asks the sandbox under its power lock
// before it powers it down (ADR 26-10-16-337 §5). Like the intake beside it,
// it is the pool's route, neither served nor proxied here.
func (h *Handler) ConfirmSandboxAutostop(context.Context, *serverapi.SandboxAutostopConfirmRequest, serverapi.ConfirmSandboxAutostopParams) (serverapi.ConfirmSandboxAutostopRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

// ResumeSandboxAutostop is what a pool tells the sandbox once it has thawed it
// from a suspend (ADR 26-10-16-420 §4). Like the confirm beside it, it is the
// pool's route, neither served nor proxied here.
func (h *Handler) ResumeSandboxAutostop(context.Context, serverapi.ResumeSandboxAutostopParams) (serverapi.ResumeSandboxAutostopRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}

// ClearSandboxCache is what a machine pool asks the sandbox to empty its own
// caches with (ADR 26-10-23-551 §3). Like the autostop routes beside it, it
// is the pool's route, neither served nor proxied here.
func (h *Handler) ClearSandboxCache(context.Context, serverapi.ClearSandboxCacheParams) (serverapi.ClearSandboxCacheRes, error) {
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

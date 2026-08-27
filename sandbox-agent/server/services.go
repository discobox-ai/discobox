package server

import (
	"context"
	"errors"
	"net/http"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"

	"github.com/discobox-ai/discobox/sandbox-agent/services"
)

// Service routes are thin: the services layer owns discovery and the exec
// layer owns everything that runs, so these handlers translate and nothing
// else. There is no create or delete — a service exists because a file in the
// repository declares it, and the API cannot declare one.

func (h *handler) ListSandboxServices(context.Context, sandboxapi.ListSandboxServicesParams) (*sandboxapi.SandboxServicesResponse, error) {
	if h.services == nil {
		return nil, servicesUnavailable()
	}
	items, err := h.services.List()
	if err != nil {
		return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	response := sandboxapi.SandboxServicesResponse{Services: make([]sandboxapi.SandboxService, 0, len(items))}
	for _, item := range items {
		response.Services = append(response.Services, sandboxService(item))
	}
	return &response, nil
}

func (h *handler) GetSandboxService(_ context.Context, params sandboxapi.GetSandboxServiceParams) (*sandboxapi.SandboxService, error) {
	if h.services == nil {
		return nil, servicesUnavailable()
	}
	service, err := h.services.Get(params.ServiceId)
	if err != nil {
		return nil, serviceError(err)
	}
	out := sandboxService(service)
	return &out, nil
}

func (h *handler) StartSandboxService(ctx context.Context, params sandboxapi.StartSandboxServiceParams) (*sandboxapi.SandboxService, error) {
	return h.actOnService(ctx, params.ServiceId, h.startService)
}

func (h *handler) StopSandboxService(ctx context.Context, params sandboxapi.StopSandboxServiceParams) (*sandboxapi.SandboxService, error) {
	return h.actOnService(ctx, params.ServiceId, h.stopService)
}

func (h *handler) RestartSandboxService(ctx context.Context, params sandboxapi.RestartSandboxServiceParams) (*sandboxapi.SandboxService, error) {
	return h.actOnService(ctx, params.ServiceId, h.restartService)
}

func (h *handler) ListSandboxServiceLogs(ctx context.Context, params sandboxapi.ListSandboxServiceLogsParams) (*sandboxapi.SandboxExecLogsResponse, error) {
	if h.services == nil {
		return nil, servicesUnavailable()
	}
	entries, err := h.services.Logs(ctx, params.ServiceId)
	if err != nil {
		return nil, serviceError(err)
	}
	response := sandboxapi.SandboxExecLogsResponse{Entries: make([]sandboxapi.SandboxExecLogEntry, 0, len(entries))}
	for _, entry := range entries {
		response.Entries = append(response.Entries, sandboxExecLogEntry(entry))
	}
	return &response, nil
}

// The three lifecycle verbs differ only in which method they call, so the
// shared shape — availability, translation, error mapping — is written once.
func (h *handler) startService(ctx context.Context, id string) (services.Service, error) {
	return h.services.Start(ctx, id)
}

func (h *handler) stopService(ctx context.Context, id string) (services.Service, error) {
	return h.services.Stop(ctx, id)
}

func (h *handler) restartService(ctx context.Context, id string) (services.Service, error) {
	return h.services.Restart(ctx, id)
}

func (h *handler) actOnService(ctx context.Context, id string, act func(context.Context, string) (services.Service, error)) (*sandboxapi.SandboxService, error) {
	if h.services == nil {
		return nil, servicesUnavailable()
	}
	service, err := act(ctx, id)
	if err != nil {
		return nil, serviceError(err)
	}
	out := sandboxService(service)
	return &out, nil
}

// servicesUnavailable is what a sandbox whose service root could not be
// resolved answers. It is 501 rather than 404: services are a capability this
// sandbox does not have right now, not a listing that happens to be empty,
// and an empty listing would report "no services declared" for a sandbox whose
// declarations were never looked at.
func servicesUnavailable() error {
	return statusError{status: http.StatusNotImplemented, message: "sandbox services are not available in this sandbox"}
}

func serviceError(err error) error {
	if errors.Is(err, services.ErrNotFound) {
		return statusError{status: http.StatusNotFound, message: "sandbox service not found"}
	}
	return statusError{status: http.StatusInternalServerError, message: err.Error()}
}

func sandboxService(in services.Service) sandboxapi.SandboxService {
	out := sandboxapi.SandboxService{
		ID:     in.ID,
		Name:   in.Name,
		Status: sandboxapi.SandboxServiceStatus(in.Status),
	}
	if in.Description != "" {
		out.Description = sandboxapi.NewOptString(in.Description)
	}
	if in.Path != "" {
		out.Path = sandboxapi.NewOptString(in.Path)
	}
	if in.FileName != "" {
		out.FileName = sandboxapi.NewOptString(in.FileName)
	}
	for _, port := range in.Ports {
		out.Ports = append(out.Ports, int64(port))
	}
	if in.Problem != "" {
		out.Problem = sandboxapi.NewOptString(in.Problem)
	}
	if in.ExecID != "" {
		out.ExecId = sandboxapi.NewOptString(in.ExecID)
	}
	if in.PID != 0 {
		out.Pid = sandboxapi.NewOptInt64(in.PID)
	}
	if in.ExitCode != nil {
		out.ExitCode = sandboxapi.NewOptInt64(*in.ExitCode)
	}
	if in.StartedAt != nil {
		out.StartedAt = sandboxapi.NewOptDateTime(*in.StartedAt)
	}
	if in.ExitedAt != nil {
		out.ExitedAt = sandboxapi.NewOptDateTime(*in.ExitedAt)
	}
	if in.Error != "" {
		out.Error = sandboxapi.NewOptString(in.Error)
	}
	return out
}

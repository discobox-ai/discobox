package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	hookapigen "github.com/obot-platform/discobox/hooks/api/gen"
	"github.com/obot-platform/discobox/hooks/api/model"
	"github.com/obot-platform/discobox/hooks/manager"
	"github.com/obot-platform/discobox/hooks/service"
)

type generatedHandler struct {
	manager *manager.Manager
	wait    func(context.Context, time.Duration) (model.WaitResponse, error)
}

func (h *generatedHandler) HooksPing(ctx context.Context) (*hookapigen.PingResponse, error) {
	return convertToGenerated[hookapigen.PingResponse](h.manager.Ping(ctx))
}

func (h *generatedHandler) HooksStatus(ctx context.Context) (*hookapigen.StatusResponse, error) {
	status, err := h.manager.Status(ctx)
	if err != nil {
		return nil, err
	}
	return convertToGenerated[hookapigen.StatusResponse](status)
}

func (h *generatedHandler) HooksWait(ctx context.Context, params hookapigen.HooksWaitParams) (*hookapigen.WaitResponse, error) {
	if h.wait == nil {
		return nil, fmt.Errorf("wait handler is unavailable")
	}
	timeout := 10 * time.Minute
	if seconds, ok := params.TimeoutSeconds.Get(); ok {
		timeout = time.Duration(seconds) * time.Second
	}
	resp, err := h.wait(ctx, timeout)
	if err != nil {
		return nil, err
	}
	return convertToGenerated[hookapigen.WaitResponse](resp)
}

func (h *generatedHandler) HooksList(ctx context.Context) (*hookapigen.HooksResponse, error) {
	hooksList, err := h.manager.ListHooks(ctx)
	if err != nil {
		return nil, err
	}
	return convertToGenerated[hookapigen.HooksResponse](model.HooksResponse{Hooks: hooksList})
}

func (h *generatedHandler) HooksListEvents(ctx context.Context, params hookapigen.HooksListEventsParams) (*hookapigen.EventsResponse, error) {
	events, err := h.manager.ListEvents(ctx, model.EventListRequest{
		HookID: optString(params.HookID),
		Limit:  optIntDefault(params.Limit, 100),
	})
	if err != nil {
		return nil, err
	}
	return convertToGenerated[hookapigen.EventsResponse](model.EventsResponse{Events: events})
}

func (h *generatedHandler) HooksListRuns(ctx context.Context, params hookapigen.HooksListRunsParams) (*hookapigen.RunsResponse, error) {
	runs, err := h.manager.ListRuns(ctx, model.RunListRequest{
		HookID: optString(params.HookID),
		Limit:  optIntDefault(params.Limit, 50),
	})
	if err != nil {
		return nil, err
	}
	return convertToGenerated[hookapigen.RunsResponse](model.RunsResponse{Runs: runs})
}

func (h *generatedHandler) HooksListChanges(ctx context.Context, params hookapigen.HooksListChangesParams) (*hookapigen.ChangesResponse, error) {
	changes, err := h.manager.ListObservedChanges(ctx, model.ListRequest{Limit: optIntDefault(params.Limit, 50)})
	if err != nil {
		return nil, err
	}
	return convertToGenerated[hookapigen.ChangesResponse](model.ChangesResponse{Changes: changes})
}

func (h *generatedHandler) HooksListSnapshots(ctx context.Context, params hookapigen.HooksListSnapshotsParams) (*hookapigen.SnapshotsResponse, error) {
	snapshots, err := h.manager.ListWorkspaceSnapshots(ctx, model.ListRequest{Limit: optIntDefault(params.Limit, 20)})
	if err != nil {
		return nil, err
	}
	return convertToGenerated[hookapigen.SnapshotsResponse](model.SnapshotsResponse{Snapshots: snapshots})
}

func (h *generatedHandler) HooksListQueue(ctx context.Context, params hookapigen.HooksListQueueParams) (*hookapigen.QueueResponse, error) {
	queue, err := h.manager.ListQueue(ctx, model.ListRequest{Limit: optIntDefault(params.Limit, 50)})
	if err != nil {
		return nil, err
	}
	return convertToGenerated[hookapigen.QueueResponse](model.QueueResponse{Queue: queue})
}

func (h *generatedHandler) HooksSetExecution(ctx context.Context, req *hookapigen.ExecutionPatchRequest) (*hookapigen.ExecutionResponse, error) {
	resp, err := h.manager.SetGlobalExecution(ctx, model.ExecutionPatchRequest{Paused: req.Paused})
	if err != nil {
		return nil, err
	}
	return convertToGenerated[hookapigen.ExecutionResponse](resp)
}

func (h *generatedHandler) HooksShutdown(ctx context.Context) (*hookapigen.ShutdownResponse, error) {
	return convertToGenerated[hookapigen.ShutdownResponse](h.manager.Shutdown(ctx))
}

func (h *generatedHandler) HooksSetHookExecution(ctx context.Context, req *hookapigen.ExecutionPatchRequest, params hookapigen.HooksSetHookExecutionParams) (hookapigen.HooksSetHookExecutionRes, error) {
	resp, err := h.manager.SetHookExecution(ctx, params.HookId, model.ExecutionPatchRequest{Paused: req.Paused})
	if errors.Is(err, service.ErrNotFound) {
		return notFoundResponse(err), nil
	}
	if err != nil {
		return nil, err
	}
	return convertToGenerated[hookapigen.ExecutionResponse](resp)
}

func (h *generatedHandler) HooksRunHook(ctx context.Context, req hookapigen.OptRunRequest, params hookapigen.HooksRunHookParams) (hookapigen.HooksRunHookRes, error) {
	body := model.RunRequest{}
	if value, ok := req.Get(); ok {
		body.Force = value.Force.Or(false)
		body.Phase = value.Phase.Or("")
	}
	resp, err := h.manager.RunHook(ctx, params.HookId, body)
	if errors.Is(err, service.ErrNotFound) {
		return notFoundResponse(err), nil
	}
	if err != nil {
		return nil, err
	}
	return convertToGenerated[hookapigen.RunResponse](resp)
}

func (h *generatedHandler) HooksOutput(ctx context.Context, params hookapigen.HooksOutputParams) (hookapigen.HooksOutputRes, error) {
	resp, err := h.manager.Output(ctx, params.HookId)
	if errors.Is(err, service.ErrNotFound) {
		return notFoundResponse(err), nil
	}
	if err != nil {
		return nil, err
	}
	return convertToGenerated[hookapigen.OutputResponse](resp)
}

func (h *generatedHandler) HooksStreamEvents(ctx context.Context, params hookapigen.HooksStreamEventsParams) (hookapigen.HooksStreamEventsOK, error) {
	// The daemon intercepts GET /events/stream before generated routing so it can
	// preserve explicit SSE flush behavior. This method only satisfies the
	// generated handler interface for non-routed fallback paths.
	return hookapigen.HooksStreamEventsOK{Data: strings.NewReader("")}, nil
}

func notFoundResponse(err error) *hookapigen.ErrorResponse {
	msg := "not found"
	if err != nil {
		msg = err.Error()
	}
	return &hookapigen.ErrorResponse{Error: hookapigen.NewOptString(msg)}
}

func optString(value hookapigen.OptString) string {
	if v, ok := value.Get(); ok {
		return v
	}
	return ""
}

func optIntDefault(value hookapigen.OptInt, fallback int) int {
	if v, ok := value.Get(); ok {
		return v
	}
	return fallback
}

func convertToGenerated[T any](value any) (*T, error) {
	var out T
	if err := convertGeneratedValue(value, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func convertGeneratedValue(value, out any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("convert daemon API response: %w", err)
	}
	return nil
}

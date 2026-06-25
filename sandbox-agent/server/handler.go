package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-faster/jx"
	sandboxapi "github.com/obot-platform/discobox/api/sandboxgen"
	"github.com/obot-platform/discobox/sandbox-agent/resources"
	"github.com/obot-platform/discobox/sandbox-agent/store"
	"github.com/obot-platform/discobox/sandbox-agent/terminal"
)

type handler struct {
	identity          Identity
	terminals         *terminal.Manager
	store             terminalStore
	resourceCollector resources.Collector
	resourceInterval  time.Duration
	resourceRetention int
}

type terminalStore interface {
	ListEvents(context.Context, string, int) ([]store.Event, error)
	RecordResourceSample(context.Context, store.ResourceSample, int) (store.ResourceSample, error)
	ListResourceSamples(context.Context, string, int) ([]store.ResourceSample, error)
}

func (h *handler) AttachAgentTerminal(ctx context.Context, params sandboxapi.AttachAgentTerminalParams) (*sandboxapi.AttachAgentTerminalSwitchingProtocols, error) {
	return nil, statusError{status: http.StatusNotImplemented, message: "agent terminal attach is not implemented by generated handler"}
}

func (h *handler) attachHTTP(w http.ResponseWriter, r *http.Request, terminalID string) {
	if err := h.terminals.Attach(r.Context(), w, terminalID); err != nil {
		if errors.Is(err, terminal.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, sandboxapi.ErrorResponse{Error: "agent terminal not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, sandboxapi.ErrorResponse{Error: err.Error()})
		return
	}
}

func (h *handler) CreateAgentTerminal(ctx context.Context, req *sandboxapi.CreateAgentTerminalRequest, _ sandboxapi.CreateAgentTerminalParams) (sandboxapi.CreateAgentTerminalRes, error) {
	if req == nil {
		req = &sandboxapi.CreateAgentTerminalRequest{}
	}
	created, err := h.terminals.Create(ctx, terminal.CreateRequest{
		AgentID:  req.AgentId.Or(""),
		Args:     append([]string{}, req.Args...),
		Workdir:  req.Workdir.Or(""),
		Env:      stringMap(req.Env.Or(nil)),
		Metadata: stringMap(req.Metadata.Or(nil)),
		Rows:     uint16(req.Rows.Or(0)),
		Cols:     uint16(req.Cols.Or(0)),
	})
	if err != nil {
		if created.ID != "" && created.Status == terminal.StatusFailed {
			return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
		}
		return nil, statusError{status: http.StatusBadRequest, message: err.Error()}
	}
	return &sandboxapi.CreateAgentTerminalResponse{Terminal: agentTerminal(created)}, nil
}

func (h *handler) DeleteAgentTerminal(ctx context.Context, params sandboxapi.DeleteAgentTerminalParams) error {
	if err := h.terminals.Delete(ctx, params.TerminalId); err != nil {
		if errors.Is(err, terminal.ErrNotFound) {
			return statusError{status: http.StatusNotFound, message: "agent terminal not found"}
		}
		return statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	return nil
}

func (h *handler) GetAgentTerminalResources(ctx context.Context, params sandboxapi.GetAgentTerminalResourcesParams) (*sandboxapi.ResourceSnapshot, error) {
	sample, err := h.collectResourceSample(ctx, params.TerminalId)
	if err != nil {
		if errors.Is(err, terminal.ErrNotFound) {
			return nil, statusError{status: http.StatusNotFound, message: "agent terminal not found"}
		}
		return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	return resourceSnapshot(sample), nil
}

func (h *handler) ListAgentTerminalEvents(ctx context.Context, params sandboxapi.ListAgentTerminalEventsParams) (*sandboxapi.AgentTerminalEventsResponse, error) {
	if _, ok := h.terminals.Get(params.TerminalId); !ok {
		return nil, statusError{status: http.StatusNotFound, message: "agent terminal not found"}
	}
	events, err := h.listEvents(ctx, params.TerminalId, params.Limit.Or(100))
	if err != nil {
		return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	response := sandboxapi.AgentTerminalEventsResponse{Events: make([]sandboxapi.AgentTerminalEvent, 0, len(events))}
	for _, event := range events {
		response.Events = append(response.Events, agentTerminalEvent(event))
	}
	return &response, nil
}

func (h *handler) ListAgentTerminalLogs(ctx context.Context, params sandboxapi.ListAgentTerminalLogsParams) (*sandboxapi.AgentTerminalLogsResponse, error) {
	entries, err := h.terminals.Logs(ctx, params.TerminalId)
	if err != nil {
		if errors.Is(err, terminal.ErrNotFound) {
			return nil, statusError{status: http.StatusNotFound, message: "agent terminal not found"}
		}
		return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	response := sandboxapi.AgentTerminalLogsResponse{Entries: make([]sandboxapi.AgentTerminalLogEntry, 0, len(entries))}
	for _, entry := range entries {
		response.Entries = append(response.Entries, agentTerminalLogEntry(entry))
	}
	return &response, nil
}

func (h *handler) ListAgentTerminalResourceHistory(ctx context.Context, params sandboxapi.ListAgentTerminalResourceHistoryParams) (*sandboxapi.ResourceHistoryResponse, error) {
	if _, ok := h.terminals.Get(params.TerminalId); !ok {
		return nil, statusError{status: http.StatusNotFound, message: "agent terminal not found"}
	}
	samples, err := h.listResourceSamples(ctx, params.TerminalId, params.Limit.Or(100))
	if err != nil {
		return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	response := sandboxapi.ResourceHistoryResponse{Snapshots: make([]sandboxapi.ResourceSnapshot, 0, len(samples))}
	for _, sample := range samples {
		response.Snapshots = append(response.Snapshots, *resourceSnapshot(sample))
	}
	return &response, nil
}

func (h *handler) ListAgentTerminals(context.Context, sandboxapi.ListAgentTerminalsParams) (*sandboxapi.AgentTerminalsResponse, error) {
	terminals := h.terminals.List()
	response := sandboxapi.AgentTerminalsResponse{
		Terminals: make([]sandboxapi.AgentTerminal, 0, len(terminals)),
	}
	for _, item := range terminals {
		response.Terminals = append(response.Terminals, agentTerminal(item))
	}
	return &response, nil
}

func (h *handler) StreamAgentTerminalResources(ctx context.Context, params sandboxapi.StreamAgentTerminalResourcesParams) (sandboxapi.StreamAgentTerminalResourcesOK, error) {
	if _, ok := h.terminals.Get(params.TerminalId); !ok {
		return sandboxapi.StreamAgentTerminalResourcesOK{}, statusError{status: http.StatusNotFound, message: "agent terminal not found"}
	}
	reader, writer := io.Pipe()
	go h.writeResourceStream(ctx, writer, params.TerminalId)
	return sandboxapi.StreamAgentTerminalResourcesOK{Data: reader}, nil
}

func (h *handler) NewError(_ context.Context, err error) *sandboxapi.ErrorResponseStatusCode {
	status := http.StatusInternalServerError
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) {
		status = statusErr.StatusCode()
	}
	return errorStatus(status, err.Error())
}

func agentTerminal(in terminal.Terminal) sandboxapi.AgentTerminal {
	out := sandboxapi.AgentTerminal{
		ID:        in.ID,
		Status:    sandboxapi.AgentTerminalStatus(in.Status),
		Command:   append([]string{}, in.Command...),
		Workdir:   in.Workdir,
		CreatedAt: in.CreatedAt,
	}
	if in.AgentID != "" {
		out.AgentId = sandboxapi.NewOptString(in.AgentID)
	}
	if in.Unit != "" {
		out.Unit = sandboxapi.NewOptString(in.Unit)
	}
	if in.PID != 0 {
		out.Pid = sandboxapi.NewOptInt64(in.PID)
	}
	if in.ExitCode != nil {
		out.ExitCode = sandboxapi.NewOptInt64(*in.ExitCode)
	}
	if in.Error != "" {
		out.Error = sandboxapi.NewOptString(in.Error)
	}
	if in.StartedAt != nil {
		out.StartedAt = sandboxapi.NewOptDateTime(*in.StartedAt)
	}
	if in.ExitedAt != nil {
		out.ExitedAt = sandboxapi.NewOptDateTime(*in.ExitedAt)
	}
	if len(in.Metadata) > 0 {
		out.Metadata = sandboxapi.NewOptAgentTerminalMetadata(sandboxapi.AgentTerminalMetadata(stringMap(in.Metadata)))
	}
	return out
}

func agentTerminalLogEntry(in terminal.LogEntry) sandboxapi.AgentTerminalLogEntry {
	return sandboxapi.AgentTerminalLogEntry{
		Timestamp: in.Timestamp,
		Stream:    sandboxapi.AgentTerminalLogEntryStream(in.Stream),
		Data:      append([]byte{}, in.Data...),
	}
}

func agentTerminalEvent(in store.Event) sandboxapi.AgentTerminalEvent {
	out := sandboxapi.AgentTerminalEvent{
		ID:        in.ID,
		Type:      in.Type,
		CreatedAt: in.CreatedAt,
	}
	if in.TerminalID != "" {
		out.TerminalId = sandboxapi.NewOptString(in.TerminalID)
	}
	if in.Message != "" {
		out.Message = sandboxapi.NewOptString(in.Message)
	}
	if len(in.Details) > 0 {
		raw, err := json.Marshal(in.Details)
		if err == nil && json.Valid(raw) {
			out.Details = jx.Raw(raw)
		}
	}
	return out
}

func resourceSnapshot(in store.ResourceSample) *sandboxapi.ResourceSnapshot {
	data := in.Data
	if len(data) == 0 || !json.Valid(data) {
		data = json.RawMessage(`{}`)
	}
	return &sandboxapi.ResourceSnapshot{
		TerminalId: in.TerminalID,
		SampledAt:  in.SampledAt,
		Source:     in.Source,
		Data:       jx.Raw(data),
	}
}

func (h *handler) collectResourceSample(ctx context.Context, terminalID string) (store.ResourceSample, error) {
	term, ok := h.terminals.Get(terminalID)
	if !ok {
		return store.ResourceSample{}, terminal.ErrNotFound
	}
	collector := h.resourceCollector
	defaultCollector := resources.NewCollector()
	if collector.ProcRoot == "" {
		collector.ProcRoot = defaultCollector.ProcRoot
	}
	if collector.CgroupRoot == "" {
		collector.CgroupRoot = defaultCollector.CgroupRoot
	}
	sample, err := collector.Collect(ctx, term)
	if err != nil {
		return store.ResourceSample{}, err
	}
	if h.store == nil {
		return sample, nil
	}
	return h.store.RecordResourceSample(ctx, sample, h.resourceRetention)
}

func (h *handler) listEvents(ctx context.Context, terminalID string, limit int) ([]store.Event, error) {
	if h.store == nil {
		return nil, nil
	}
	return h.store.ListEvents(ctx, terminalID, limit)
}

func (h *handler) listResourceSamples(ctx context.Context, terminalID string, limit int) ([]store.ResourceSample, error) {
	if h.store == nil {
		return nil, nil
	}
	return h.store.ListResourceSamples(ctx, terminalID, limit)
}

func (h *handler) writeResourceStream(ctx context.Context, writer *io.PipeWriter, terminalID string) {
	defer writer.Close()
	history, err := h.listResourceSamples(ctx, terminalID, 100)
	if err != nil {
		_ = writer.CloseWithError(err)
		return
	}
	for _, sample := range history {
		if err := writeSSE(writer, "resource", resourceSnapshot(sample)); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
	}
	interval := h.resourceInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sample, err := h.collectResourceSample(ctx, terminalID)
			if err != nil {
				_ = writeSSE(writer, "error", map[string]string{"error": err.Error()})
				return
			}
			if err := writeSSE(writer, "resource", resourceSnapshot(sample)); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
		}
	}
}

func writeSSE(writer io.Writer, event string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event, data)
	return err
}

func stringMap[M ~map[string]string](in M) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

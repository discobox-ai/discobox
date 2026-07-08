package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-faster/jx"
	sandboxapi "github.com/obot-platform/discobox/api/sandboxgen"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
	"github.com/obot-platform/discobox/sandbox-agent/resources"
	"github.com/obot-platform/discobox/sandbox-agent/store"
	"github.com/obot-platform/discobox/sandbox-agent/terminal"
)

type handler struct {
	identity          Identity
	terminals         *terminal.Manager
	execs             *execs.Manager
	store             terminalStore
	resourceCollector resources.Collector
	resourceInterval  time.Duration
	resourceRetention int
}

type terminalStore interface {
	ListEvents(context.Context, string, int) ([]store.Event, error)
	RecordResourceSample(context.Context, store.ResourceSample, int) (store.ResourceSample, error)
	ListResourceSamples(context.Context, string, int) ([]store.ResourceSample, error)
	ListAgentHooks(context.Context, string, int) ([]store.AgentHookRecord, error)
}

func (h *handler) AttachAgentTerminal(ctx context.Context, params sandboxapi.AttachAgentTerminalParams) (*sandboxapi.AttachAgentTerminalSwitchingProtocols, error) {
	return nil, statusError{status: http.StatusNotImplemented, message: "agent terminal attach is not implemented by generated handler"}
}

func (h *handler) AttachSandboxExec(ctx context.Context, params sandboxapi.AttachSandboxExecParams) (*sandboxapi.AttachSandboxExecSwitchingProtocols, error) {
	return nil, statusError{status: http.StatusNotImplemented, message: "sandbox exec attach is not implemented by generated handler"}
}

func (h *handler) attachHTTP(w http.ResponseWriter, r *http.Request, terminalID string) {
	replay, _ := strconv.ParseBool(r.URL.Query().Get("replay"))
	if err := h.terminals.Attach(r.Context(), w, terminalID, replay); err != nil {
		if errors.Is(err, terminal.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, sandboxapi.ErrorResponse{Error: "agent terminal not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, sandboxapi.ErrorResponse{Error: err.Error()})
		return
	}
}

func (h *handler) startTerminalHTTP(w http.ResponseWriter, r *http.Request, terminalID string) {
	started, err := h.terminals.Start(r.Context(), terminalID)
	if err != nil {
		if errors.Is(err, terminal.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, sandboxapi.ErrorResponse{Error: "agent terminal not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, sandboxapi.ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, agentTerminal(started))
}

func (h *handler) attachExecHTTP(w http.ResponseWriter, r *http.Request, execID string) {
	if err := h.execs.Attach(r.Context(), w, r, execID); err != nil {
		if errors.Is(err, execs.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, sandboxapi.ErrorResponse{Error: "sandbox exec not found"})
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

func (h *handler) StartAgentTerminal(ctx context.Context, params sandboxapi.StartAgentTerminalParams) (*sandboxapi.AgentTerminal, error) {
	started, err := h.terminals.Start(ctx, params.TerminalId)
	if err != nil {
		if errors.Is(err, terminal.ErrNotFound) {
			return nil, statusError{status: http.StatusNotFound, message: "agent terminal not found"}
		}
		return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	out := agentTerminal(started)
	return &out, nil
}

func (h *handler) CreateSandboxExec(ctx context.Context, req *sandboxapi.CreateSandboxExecRequest, _ sandboxapi.CreateSandboxExecParams) (*sandboxapi.CreateSandboxExecResponse, error) {
	if req == nil {
		req = &sandboxapi.CreateSandboxExecRequest{}
	}
	created, err := h.execs.Create(ctx, execs.CreateRequest{
		Command:  append([]string{}, req.Command...),
		Workdir:  req.Workdir.Or(""),
		Env:      stringMap(req.Env.Or(nil)),
		User:     execUserFromAPI(req.User),
		TTY:      req.Tty.Or(false),
		Rows:     uint16(req.Rows.Or(0)),
		Cols:     uint16(req.Cols.Or(0)),
		Metadata: stringMap(req.Metadata.Or(nil)),
	})
	if err != nil {
		if created.ID != "" && created.Status == execs.StatusFailed {
			return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
		}
		return nil, statusError{status: http.StatusBadRequest, message: err.Error()}
	}
	return &sandboxapi.CreateSandboxExecResponse{Exec: sandboxExec(created)}, nil
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

func (h *handler) GetSandboxExec(ctx context.Context, params sandboxapi.GetSandboxExecParams) (*sandboxapi.SandboxExec, error) {
	exec, ok := h.execs.Get(params.ExecId)
	if !ok {
		return nil, statusError{status: http.StatusNotFound, message: "sandbox exec not found"}
	}
	out := sandboxExec(exec)
	return &out, nil
}

func (h *handler) startExecHTTP(w http.ResponseWriter, r *http.Request, execID string) {
	exec, err := h.execs.Start(r.Context(), execID)
	if err != nil {
		if errors.Is(err, execs.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, sandboxapi.ErrorResponse{Error: "sandbox exec not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, sandboxapi.ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, sandboxExec(exec))
}

func (h *handler) StartSandboxExec(ctx context.Context, params sandboxapi.StartSandboxExecParams) (*sandboxapi.SandboxExec, error) {
	exec, err := h.execs.Start(ctx, params.ExecId)
	if err != nil {
		if errors.Is(err, execs.ErrNotFound) {
			return nil, statusError{status: http.StatusNotFound, message: "sandbox exec not found"}
		}
		return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	out := sandboxExec(exec)
	return &out, nil
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

func (h *handler) ListAgentHooks(ctx context.Context, params sandboxapi.ListAgentHooksParams) (*sandboxapi.AgentHookLogsResponse, error) {
	if h.store == nil {
		return &sandboxapi.AgentHookLogsResponse{}, nil
	}
	records, err := h.store.ListAgentHooks(ctx, params.TerminalId.Or(""), params.Limit.Or(100))
	if err != nil {
		return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	response := sandboxapi.AgentHookLogsResponse{Hooks: make([]sandboxapi.AgentHookLog, 0, len(records))}
	for _, record := range records {
		response.Hooks = append(response.Hooks, agentHookLog(record))
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

func (h *handler) ListSandboxExecLogs(ctx context.Context, params sandboxapi.ListSandboxExecLogsParams) (*sandboxapi.SandboxExecLogsResponse, error) {
	entries, err := h.execs.Logs(ctx, params.ExecId)
	if err != nil {
		if errors.Is(err, execs.ErrNotFound) {
			return nil, statusError{status: http.StatusNotFound, message: "sandbox exec not found"}
		}
		return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	response := sandboxapi.SandboxExecLogsResponse{Entries: make([]sandboxapi.SandboxExecLogEntry, 0, len(entries))}
	for _, entry := range entries {
		response.Entries = append(response.Entries, sandboxExecLogEntry(entry))
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

func (h *handler) ListSandboxExecs(context.Context, sandboxapi.ListSandboxExecsParams) (*sandboxapi.SandboxExecsResponse, error) {
	execs := h.execs.List()
	response := sandboxapi.SandboxExecsResponse{
		Execs: make([]sandboxapi.SandboxExec, 0, len(execs)),
	}
	for _, item := range execs {
		response.Execs = append(response.Execs, sandboxExec(item))
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
	if in.Primary {
		out.Primary = sandboxapi.NewOptBool(true)
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

func sandboxExec(in execs.Exec) sandboxapi.SandboxExec {
	out := sandboxapi.SandboxExec{
		ID:        in.ID,
		Status:    sandboxapi.SandboxExecStatus(in.Status),
		Command:   append([]string{}, in.Command...),
		Workdir:   in.Workdir,
		Tty:       in.TTY,
		CreatedAt: in.CreatedAt,
	}
	if len(in.Env) > 0 {
		out.Env = sandboxapi.NewOptSandboxExecEnv(sandboxapi.SandboxExecEnv(stringMap(in.Env)))
	}
	if user := execUserToAPI(in.User); user != nil {
		out.User = sandboxapi.NewOptSandboxUser(*user)
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
		out.Metadata = sandboxapi.NewOptSandboxExecMetadata(sandboxapi.SandboxExecMetadata(stringMap(in.Metadata)))
	}
	return out
}

func execUserFromAPI(opt sandboxapi.OptSandboxUser) *execs.User {
	in, ok := opt.Get()
	if !ok {
		return nil
	}
	user := &execs.User{
		Name:          strings.TrimSpace(in.Name.Or("")),
		HomeDirectory: strings.TrimSpace(in.HomeDirectory.Or("")),
	}
	if uid, ok := in.UID.Get(); ok {
		user.UID = int64ValuePtr(uid)
	}
	if gid, ok := in.Gid.Get(); ok {
		user.GID = int64ValuePtr(gid)
	}
	return user
}

func int64ValuePtr(value int64) *int64 {
	return &value
}

func execUserToAPI(in *execs.User) *sandboxapi.SandboxUser {
	if in == nil {
		return nil
	}
	out := sandboxapi.SandboxUser{}
	if strings.TrimSpace(in.Name) != "" {
		out.Name = sandboxapi.NewOptString(strings.TrimSpace(in.Name))
	}
	if strings.TrimSpace(in.HomeDirectory) != "" {
		out.HomeDirectory = sandboxapi.NewOptString(strings.TrimSpace(in.HomeDirectory))
	}
	if in.UID != nil {
		out.UID = sandboxapi.NewOptInt64(*in.UID)
	}
	if in.GID != nil {
		out.Gid = sandboxapi.NewOptInt64(*in.GID)
	}
	if !out.Name.Set && !out.HomeDirectory.Set && !out.UID.Set && !out.Gid.Set {
		return nil
	}
	return &out
}

func sandboxExecLogEntry(in execs.LogEntry) sandboxapi.SandboxExecLogEntry {
	return sandboxapi.SandboxExecLogEntry{
		Timestamp: in.Timestamp,
		Stream:    sandboxapi.SandboxExecLogEntryStream(in.Stream),
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

func agentHookLog(in store.AgentHookRecord) sandboxapi.AgentHookLog {
	payload := in.Payload
	if len(payload) == 0 || !json.Valid(payload) {
		payload = json.RawMessage(`{}`)
	}
	out := sandboxapi.AgentHookLog{
		ID:        in.ID,
		Provider:  in.Provider,
		Event:     in.Event,
		Payload:   jx.Raw(payload),
		CreatedAt: in.CreatedAt,
	}
	if in.TerminalID != "" {
		out.TerminalId = sandboxapi.NewOptString(in.TerminalID)
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

func optInt64Ptr(in sandboxapi.OptInt64) *int64 {
	value, ok := in.Get()
	if !ok {
		return nil
	}
	return &value
}

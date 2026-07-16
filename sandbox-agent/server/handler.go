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
	execs             *execs.Manager
	terminals         *terminal.Service
	store             terminalStore
	resourceCollector resources.Collector
	resourceInterval  time.Duration
	resourceRetention int
}

type terminalStore interface {
	ListEvents(context.Context, string, int) ([]store.Event, error)
	RecordResourceSample(context.Context, store.ResourceSample, int) (store.ResourceSample, error)
	ListResourceSamples(context.Context, string, int) ([]store.ResourceSample, error)
	ListHarnessHooks(context.Context, string, int) ([]store.HarnessHookRecord, error)
}

func (h *handler) AttachSandboxExec(context.Context, sandboxapi.AttachSandboxExecParams) (*sandboxapi.AttachSandboxExecSwitchingProtocols, error) {
	return nil, statusError{status: http.StatusNotImplemented, message: "sandbox exec attach is not implemented by generated handler"}
}

// resolveExecID maps the virtual primary exec id to the sandbox's current
// primary terminal, relaunching it when it has stopped; every other id passes
// through unchanged. Use it for attach/start, where resuming a stopped primary
// is the goal.
func (h *handler) resolveExecID(ctx context.Context, execID string) (string, error) {
	if execID != terminal.PrimaryExecID {
		return execID, nil
	}
	exec, err := h.terminals.ResolvePrimary(ctx)
	if err != nil {
		return "", err
	}
	return exec.ID, nil
}

// resolveExecIDReadOnly maps the virtual primary exec id to the current primary
// terminal without relaunching it. Use it for status reads so a client's attach
// done-check observes a real exit instead of triggering a resume.
func (h *handler) resolveExecIDReadOnly(execID string) (string, error) {
	if execID != terminal.PrimaryExecID {
		return execID, nil
	}
	exec, ok := h.terminals.CurrentPrimary()
	if !ok {
		return "", execs.ErrNotFound
	}
	return exec.ID, nil
}

// attachExecHTTP proxies a websocket attach to the exec shim. replay=true (used
// by terminal clients) replays the shim's buffered output on connect.
func (h *handler) attachExecHTTP(w http.ResponseWriter, r *http.Request, execID string) {
	replay, _ := strconv.ParseBool(r.URL.Query().Get("replay"))
	execID, err := h.resolveExecID(r.Context(), execID)
	if err != nil {
		writeExecResolveError(w, err)
		return
	}
	if err := h.execs.Attach(r.Context(), w, r, execID, replay); err != nil {
		if errors.Is(err, execs.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, sandboxapi.ErrorResponse{Error: "sandbox exec not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, sandboxapi.ErrorResponse{Error: err.Error()})
		return
	}
}

// writeExecResolveError renders a resolve failure as 404 for a missing primary
// or 500 otherwise.
func writeExecResolveError(w http.ResponseWriter, err error) {
	if errors.Is(err, execs.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, sandboxapi.ErrorResponse{Error: "sandbox exec not found"})
		return
	}
	writeJSON(w, http.StatusInternalServerError, sandboxapi.ErrorResponse{Error: err.Error()})
}

func (h *handler) startExecHTTP(w http.ResponseWriter, r *http.Request, execID string) {
	execID, err := h.resolveExecID(r.Context(), execID)
	if err != nil {
		writeExecResolveError(w, err)
		return
	}
	exec, err := h.execs.Start(r.Context(), execID)
	if err != nil {
		if errors.Is(err, execs.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, sandboxapi.ErrorResponse{Error: "sandbox exec not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, sandboxapi.ErrorResponse{Error: err.Error()})
		return
	}
	out := h.sandboxExec(exec)
	writeJSON(w, http.StatusOK, &out)
}

// CreateSandboxExec creates a plain exec (command) or, when harnessId is set, an
// harness terminal (an exec created in harness mode via the terminal layer).
func (h *handler) CreateSandboxExec(ctx context.Context, req *sandboxapi.CreateSandboxExecRequest, _ sandboxapi.CreateSandboxExecParams) (*sandboxapi.CreateSandboxExecResponse, error) {
	if req == nil {
		req = &sandboxapi.CreateSandboxExecRequest{}
	}
	harnessID := strings.TrimSpace(req.HarnessId.Or(""))
	if harnessID != "" || len(req.Command) == 0 {
		created, err := h.terminals.Create(ctx, terminal.CreateRequest{
			HarnessID: harnessID,
			Args:      append([]string{}, req.Args...),
			Workdir:   req.Workdir.Or(""),
			Env:       stringMap(req.Env.Or(nil)),
			Metadata:  stringMap(req.Metadata.Or(nil)),
			Rows:      uint16(req.Rows.Or(0)),
			Cols:      uint16(req.Cols.Or(0)),
		})
		if err != nil {
			if created.ID != "" && created.Status == execs.StatusFailed {
				return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
			}
			return nil, statusError{status: http.StatusBadRequest, message: err.Error()}
		}
		return &sandboxapi.CreateSandboxExecResponse{Exec: h.sandboxExec(created)}, nil
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
	return &sandboxapi.CreateSandboxExecResponse{Exec: h.sandboxExec(created)}, nil
}

func (h *handler) DeleteSandboxExec(ctx context.Context, params sandboxapi.DeleteSandboxExecParams) error {
	if err := h.execs.Delete(ctx, params.ExecId); err != nil {
		if errors.Is(err, execs.ErrNotFound) {
			return statusError{status: http.StatusNotFound, message: "sandbox exec not found"}
		}
		return statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	return nil
}

func (h *handler) GetSandboxExec(_ context.Context, params sandboxapi.GetSandboxExecParams) (*sandboxapi.SandboxExec, error) {
	execID, err := h.resolveExecIDReadOnly(params.ExecId)
	if err != nil {
		return nil, statusError{status: http.StatusNotFound, message: "sandbox exec not found"}
	}
	exec, ok := h.execs.Get(execID)
	if !ok {
		return nil, statusError{status: http.StatusNotFound, message: "sandbox exec not found"}
	}
	out := h.sandboxExec(exec)
	return &out, nil
}

func (h *handler) StartSandboxExec(ctx context.Context, params sandboxapi.StartSandboxExecParams) (*sandboxapi.SandboxExec, error) {
	exec, err := h.execs.Start(ctx, params.ExecId)
	if err != nil {
		if errors.Is(err, execs.ErrNotFound) {
			return nil, statusError{status: http.StatusNotFound, message: "sandbox exec not found"}
		}
		return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	out := h.sandboxExec(exec)
	return &out, nil
}

func (h *handler) ListSandboxExecs(context.Context, sandboxapi.ListSandboxExecsParams) (*sandboxapi.SandboxExecsResponse, error) {
	items := h.execs.List()
	response := sandboxapi.SandboxExecsResponse{
		Execs: make([]sandboxapi.SandboxExec, 0, len(items)),
	}
	for _, item := range items {
		response.Execs = append(response.Execs, h.sandboxExec(item))
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

func (h *handler) ListSandboxExecEvents(ctx context.Context, params sandboxapi.ListSandboxExecEventsParams) (*sandboxapi.SandboxExecEventsResponse, error) {
	if _, ok := h.execs.Get(params.ExecId); !ok {
		return nil, statusError{status: http.StatusNotFound, message: "sandbox exec not found"}
	}
	events, err := h.listEvents(ctx, params.ExecId, params.Limit.Or(100))
	if err != nil {
		return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	response := sandboxapi.SandboxExecEventsResponse{Events: make([]sandboxapi.SandboxExecEvent, 0, len(events))}
	for _, event := range events {
		response.Events = append(response.Events, sandboxExecEvent(event))
	}
	return &response, nil
}

func (h *handler) GetSandboxExecResources(ctx context.Context, params sandboxapi.GetSandboxExecResourcesParams) (*sandboxapi.ResourceSnapshot, error) {
	sample, err := h.collectResourceSample(ctx, params.ExecId)
	if err != nil {
		if errors.Is(err, execs.ErrNotFound) {
			return nil, statusError{status: http.StatusNotFound, message: "sandbox exec not found"}
		}
		return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	return resourceSnapshot(sample), nil
}

func (h *handler) ListSandboxExecResourceHistory(ctx context.Context, params sandboxapi.ListSandboxExecResourceHistoryParams) (*sandboxapi.ResourceHistoryResponse, error) {
	if _, ok := h.execs.Get(params.ExecId); !ok {
		return nil, statusError{status: http.StatusNotFound, message: "sandbox exec not found"}
	}
	samples, err := h.listResourceSamples(ctx, params.ExecId, params.Limit.Or(100))
	if err != nil {
		return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	response := sandboxapi.ResourceHistoryResponse{Snapshots: make([]sandboxapi.ResourceSnapshot, 0, len(samples))}
	for _, sample := range samples {
		response.Snapshots = append(response.Snapshots, *resourceSnapshot(sample))
	}
	return &response, nil
}

func (h *handler) StreamSandboxExecResources(ctx context.Context, params sandboxapi.StreamSandboxExecResourcesParams) (sandboxapi.StreamSandboxExecResourcesOK, error) {
	if _, ok := h.execs.Get(params.ExecId); !ok {
		return sandboxapi.StreamSandboxExecResourcesOK{}, statusError{status: http.StatusNotFound, message: "sandbox exec not found"}
	}
	reader, writer := io.Pipe()
	go h.writeResourceStream(ctx, writer, params.ExecId)
	return sandboxapi.StreamSandboxExecResourcesOK{Data: reader}, nil
}

func (h *handler) ListHarnessHooks(ctx context.Context, params sandboxapi.ListHarnessHooksParams) (*sandboxapi.HarnessHookLogsResponse, error) {
	if h.store == nil {
		return &sandboxapi.HarnessHookLogsResponse{}, nil
	}
	records, err := h.store.ListHarnessHooks(ctx, params.TerminalId.Or(""), params.Limit.Or(100))
	if err != nil {
		return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	response := sandboxapi.HarnessHookLogsResponse{Hooks: make([]sandboxapi.HarnessHookLog, 0, len(records))}
	for _, record := range records {
		response.Hooks = append(response.Hooks, harnessHookLog(record))
	}
	return &response, nil
}

func (h *handler) NewError(_ context.Context, err error) *sandboxapi.ErrorResponseStatusCode {
	status := http.StatusInternalServerError
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) {
		status = statusErr.StatusCode()
	}
	return errorStatus(status, err.Error())
}

func (h *handler) sandboxExec(in execs.Exec) sandboxapi.SandboxExec {
	// A terminal-mode exec whose harness setup is still running is projected as
	// the "installing" phase, overriding its underlying "starting" status. install
	// is a terminal-layer step, so it stays out of the generic execs.Status enum.
	status := sandboxapi.SandboxExecStatus(in.Status)
	if h.terminals != nil && h.terminals.IsInstalling(in.ID) {
		status = sandboxapi.SandboxExecStatusInstalling
	}
	out := sandboxapi.SandboxExec{
		ID:        in.ID,
		Status:    status,
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
	if harnessID := terminal.HarnessID(in); harnessID != "" {
		out.HarnessId = sandboxapi.NewOptString(harnessID)
	}
	if terminal.IsPrimary(in) {
		out.Primary = sandboxapi.NewOptBool(true)
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

func sandboxExecEvent(in store.Event) sandboxapi.SandboxExecEvent {
	out := sandboxapi.SandboxExecEvent{
		ID:        in.ID,
		Type:      in.Type,
		CreatedAt: in.CreatedAt,
	}
	if in.TerminalID != "" {
		out.ExecId = sandboxapi.NewOptString(in.TerminalID)
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

func harnessHookLog(in store.HarnessHookRecord) sandboxapi.HarnessHookLog {
	payload := in.Payload
	if len(payload) == 0 || !json.Valid(payload) {
		payload = json.RawMessage(`{}`)
	}
	out := sandboxapi.HarnessHookLog{
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

func (h *handler) collectResourceSample(ctx context.Context, execID string) (store.ResourceSample, error) {
	exec, ok := h.execs.Get(execID)
	if !ok {
		return store.ResourceSample{}, execs.ErrNotFound
	}
	collector := h.resourceCollector
	defaultCollector := resources.NewCollector()
	if collector.ProcRoot == "" {
		collector.ProcRoot = defaultCollector.ProcRoot
	}
	if collector.CgroupRoot == "" {
		collector.CgroupRoot = defaultCollector.CgroupRoot
	}
	sample, err := collector.Collect(ctx, exec)
	if err != nil {
		return store.ResourceSample{}, err
	}
	if h.store == nil {
		return sample, nil
	}
	return h.store.RecordResourceSample(ctx, sample, h.resourceRetention)
}

func (h *handler) listEvents(ctx context.Context, execID string, limit int) ([]store.Event, error) {
	if h.store == nil {
		return nil, nil
	}
	return h.store.ListEvents(ctx, execID, limit)
}

func (h *handler) listResourceSamples(ctx context.Context, execID string, limit int) ([]store.ResourceSample, error) {
	if h.store == nil {
		return nil, nil
	}
	return h.store.ListResourceSamples(ctx, execID, limit)
}

func (h *handler) writeResourceStream(ctx context.Context, writer *io.PipeWriter, execID string) {
	defer writer.Close()
	history, err := h.listResourceSamples(ctx, execID, 100)
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
			sample, err := h.collectResourceSample(ctx, execID)
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

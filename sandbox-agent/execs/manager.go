package execs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/obot-platform/discobox/sandbox-agent/shimproxy"
)

type Status string

const (
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusExited   Status = "exited"
	StatusFailed   Status = "failed"
	StatusLost     Status = "lost"
)

type Exec struct {
	ID          string            `json:"id"`
	Status      Status            `json:"status"`
	Command     []string          `json:"command"`
	Workdir     string            `json:"workdir"`
	Env         map[string]string `json:"env,omitempty"`
	UID         *int64            `json:"uid,omitempty"`
	GID         *int64            `json:"gid,omitempty"`
	TTY         bool              `json:"tty"`
	Unit        string            `json:"unit,omitempty"`
	PID         int64             `json:"pid,omitempty"`
	ExitCode    *int64            `json:"exitCode,omitempty"`
	Error       string            `json:"error,omitempty"`
	CreatedAt   time.Time         `json:"createdAt"`
	StartedAt   *time.Time        `json:"startedAt,omitempty"`
	ExitedAt    *time.Time        `json:"exitedAt,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	SocketPath  string            `json:"socketPath,omitempty"`
	RuntimePath string            `json:"runtimePath,omitempty"`
}

type CreateRequest struct {
	Command  []string
	Workdir  string
	Env      map[string]string
	UID      *int64
	GID      *int64
	TTY      bool
	Rows     uint16
	Cols     uint16
	Metadata map[string]string
}

type UnitManager interface {
	Start(context.Context, StartRequest) (StartResult, error)
	Status(context.Context, string) (UnitStatus, error)
	List(context.Context) ([]UnitStatus, error)
}

type StartRequest struct {
	ID          string
	Unit        string
	Command     []string
	Workdir     string
	Env         map[string]string
	UID         *int64
	GID         *int64
	TTY         bool
	SocketPath  string
	RuntimePath string
	LogDir      string
	Rows        uint16
	Cols        uint16
}

type StartResult struct {
	Unit string
	PID  int64
}

type UnitStatus struct {
	Unit      string
	Active    bool
	Status    Status
	PID       int64
	ExitCode  *int64
	Error     string
	StartedAt *time.Time
	ExitedAt  *time.Time
}

type AuditRecorder interface {
	RecordExecEvent(context.Context, string, string, string, map[string]any) error
	ObserveExec(context.Context, Exec) error
}

type Manager struct {
	workingRoot string
	runtimeDir  string
	logDir      string
	env         map[string]string
	units       UnitManager
	audit       AuditRecorder
}

type ManagerConfig struct {
	WorkingRoot string
	RuntimeDir  string
	Env         map[string]string
	Units       UnitManager
	Audit       AuditRecorder
}

func NewManager(workingRoot, runtimeDir string, units UnitManager, audit AuditRecorder) (*Manager, error) {
	return NewManagerWithConfig(ManagerConfig{
		WorkingRoot: workingRoot,
		RuntimeDir:  runtimeDir,
		Units:       units,
		Audit:       audit,
	})
}

func NewManagerWithConfig(cfg ManagerConfig) (*Manager, error) {
	workingRoot := cfg.WorkingRoot
	runtimeDir := cfg.RuntimeDir
	units := cfg.Units
	if strings.TrimSpace(workingRoot) == "" {
		return nil, errors.New("working root is required")
	}
	if strings.TrimSpace(runtimeDir) == "" {
		runtimeDir = "/run/discobox/execs"
	}
	if units == nil {
		units = SystemdRunner{}
	}
	runtimeDir = filepath.Clean(runtimeDir)
	return &Manager{
		workingRoot: filepath.Clean(workingRoot),
		runtimeDir:  runtimeDir,
		logDir:      filepath.Join(runtimeDir, "logs"),
		env:         cloneMap(cfg.Env),
		units:       units,
		audit:       cfg.Audit,
	}, nil
}

func mergeEnv(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(override))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range override {
		out[key] = value
	}
	return out
}

func (m *Manager) Create(ctx context.Context, req CreateRequest) (Exec, error) {
	if len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "" {
		return Exec{}, errors.New("exec command is required")
	}
	workdir, err := m.resolveWorkdir(req.Workdir)
	if err != nil {
		return Exec{}, err
	}
	env := mergeEnv(m.env, req.Env)
	id, err := newID()
	if err != nil {
		return Exec{}, err
	}
	unit := "discobox-exec-" + id
	socketPath := m.socketPath(id)
	runtimePath := m.runtimePath(id)
	now := time.Now().UTC()
	exec := Exec{
		ID:          id,
		Status:      StatusStarting,
		Command:     append([]string{}, req.Command...),
		Workdir:     workdir,
		Env:         cloneMap(env),
		UID:         cloneInt64(req.UID),
		GID:         cloneInt64(req.GID),
		TTY:         req.TTY,
		Unit:        unit,
		CreatedAt:   now,
		Metadata:    cloneMap(req.Metadata),
		SocketPath:  socketPath,
		RuntimePath: runtimePath,
	}
	if err := writeRuntime(runtimePath, exec); err != nil {
		return Exec{}, err
	}
	_ = m.recordEvent(ctx, id, "exec.created", "exec created", map[string]any{
		"unit":    unit,
		"workdir": workdir,
		"command": exec.Command,
		"tty":     req.TTY,
		"uid":     req.UID,
		"gid":     req.GID,
	})
	result, err := m.units.Start(ctx, StartRequest{
		ID:          id,
		Unit:        unit,
		Command:     exec.Command,
		Workdir:     workdir,
		Env:         cloneMap(env),
		UID:         cloneInt64(req.UID),
		GID:         cloneInt64(req.GID),
		TTY:         req.TTY,
		SocketPath:  socketPath,
		RuntimePath: runtimePath,
		LogDir:      m.logDir,
		Rows:        req.Rows,
		Cols:        req.Cols,
	})
	current := exec
	if err != nil {
		current.Status = StatusFailed
		current.Error = err.Error()
		exitedAt := time.Now().UTC()
		current.ExitedAt = &exitedAt
		_ = writeRuntime(runtimePath, current)
		_ = m.observe(ctx, current)
		_ = m.recordEvent(ctx, id, "exec.start.failed", "exec start failed", map[string]any{"error": err.Error()})
		return current, err
	}
	if result.Unit != "" {
		current.Unit = result.Unit
	}
	_ = writeRuntime(runtimePath, current)
	_ = m.observe(ctx, current)
	_ = m.recordEvent(ctx, id, "exec.prepared", "exec prepared", map[string]any{"unit": current.Unit})
	return current, nil
}

func (m *Manager) List() []Exec {
	execs := m.runtimeExecs(context.Background())
	if units, err := m.units.List(context.Background()); err == nil {
		byUnit := map[string]UnitStatus{}
		for _, unit := range units {
			byUnit[unit.Unit] = unit
		}
		for i := range execs {
			if unit, ok := byUnit[execs[i].Unit]; ok {
				execs[i] = applyUnitStatus(execs[i], unit)
			}
			execs[i] = m.refreshExec(context.Background(), execs[i])
		}
	} else {
		for i := range execs {
			execs[i] = m.refreshExec(context.Background(), execs[i])
		}
	}
	sort.Slice(execs, func(i, j int) bool {
		return execs[i].CreatedAt.Before(execs[j].CreatedAt)
	})
	return execs
}

func (m *Manager) Reconcile(ctx context.Context) error {
	for _, exec := range m.runtimeExecs(ctx) {
		_ = m.refreshExec(ctx, exec)
	}
	return nil
}

func (m *Manager) Get(id string) (Exec, bool) {
	exec, ok := m.readRuntime(id)
	if !ok {
		return Exec{}, false
	}
	exec = m.refreshExec(context.Background(), exec)
	return cloneExec(exec), true
}

func (m *Manager) Logs(ctx context.Context, id string) ([]LogEntry, error) {
	if _, ok := m.Get(id); !ok {
		return nil, ErrNotFound
	}
	return ReadLogs(ctx, m.logDir, id)
}

func (m *Manager) Start(ctx context.Context, id string) (Exec, error) {
	exec, ok := m.readRuntime(id)
	if !ok {
		return Exec{}, ErrNotFound
	}
	if exec.Status != StatusStarting {
		return cloneExec(exec), nil
	}
	started, err := shimproxy.StartJSON[Exec](ctx, exec.SocketPath)
	if err != nil {
		exec.Status = StatusFailed
		exec.Error = err.Error()
		exitedAt := time.Now().UTC()
		exec.ExitedAt = &exitedAt
		_ = writeRuntime(exec.RuntimePath, exec)
		_ = m.observe(ctx, exec)
		_ = m.recordEvent(ctx, id, "exec.start.failed", "exec start failed", map[string]any{"error": err.Error()})
		return cloneExec(exec), err
	}
	current := mergeExecStatus(exec, started)
	_ = writeRuntime(current.RuntimePath, current)
	_ = m.observe(ctx, current)
	_ = m.recordEvent(ctx, id, "exec.started", "exec started", map[string]any{"unit": current.Unit, "pid": current.PID})
	return cloneExec(current), nil
}

func (m *Manager) Attach(ctx context.Context, w http.ResponseWriter, r *http.Request, id string) error {
	exec, ok := m.Get(id)
	if !ok {
		return ErrNotFound
	}
	_ = m.recordEvent(ctx, id, "exec.attach.opened", "exec attach opened", map[string]any{"unit": exec.Unit})
	defer func() {
		_ = m.recordEvent(context.Background(), id, "exec.attach.closed", "exec attach closed", map[string]any{"unit": exec.Unit})
	}()
	return shimproxy.AttachWebSocket(ctx, w, r, exec.SocketPath, "discobox-sandbox-exec")
}

var ErrNotFound = errors.New("sandbox exec not found")

func (m *Manager) resolveWorkdir(requested string) (string, error) {
	if strings.TrimSpace(requested) == "" {
		return m.workingRoot, nil
	}
	if !filepath.IsAbs(requested) {
		requested = filepath.Join(m.workingRoot, requested)
	}
	cleaned := filepath.Clean(requested)
	rel, err := filepath.Rel(m.workingRoot, cleaned)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("workdir %q is outside working root %q", cleaned, m.workingRoot)
	}
	return cleaned, nil
}

func (m *Manager) runtimePath(id string) string {
	return filepath.Join(m.runtimeDir, safeName(id)+".json")
}

func (m *Manager) socketPath(id string) string {
	return filepath.Join(m.runtimeDir, safeName(id)+".sock")
}

func (m *Manager) runtimeExecs(ctx context.Context) []Exec {
	matches, err := filepath.Glob(filepath.Join(m.runtimeDir, "*.json"))
	if err != nil {
		return nil
	}
	out := make([]Exec, 0, len(matches))
	for _, path := range matches {
		exec, err := readRuntime(path)
		if err != nil || exec.ID == "" {
			continue
		}
		if exec.RuntimePath == "" {
			exec.RuntimePath = path
		}
		if exec.SocketPath == "" {
			exec.SocketPath = m.socketPath(exec.ID)
		}
		out = append(out, m.refreshExec(ctx, exec))
	}
	return out
}

func (m *Manager) readRuntime(id string) (Exec, bool) {
	exec, err := readRuntime(m.runtimePath(id))
	return exec, err == nil && exec.ID != ""
}

func (m *Manager) refreshExec(ctx context.Context, exec Exec) Exec {
	if exec.Status == StatusExited || exec.Status == StatusFailed {
		_ = m.observe(ctx, exec)
		return exec
	}
	if unit, err := m.units.Status(ctx, exec.Unit); err == nil {
		exec = applyUnitStatus(exec, unit)
	} else if exec.ExitedAt == nil {
		exec.Status = StatusLost
		exec.Error = "exec unit status is unavailable"
	}
	_ = writeRuntime(exec.RuntimePath, exec)
	_ = m.observe(ctx, exec)
	return exec
}

func applyUnitStatus(exec Exec, unit UnitStatus) Exec {
	if unit.Unit != "" {
		exec.Unit = unit.Unit
	}
	if exec.Status == StatusStarting && exec.StartedAt == nil {
		if unit.Error != "" {
			exec.Error = unit.Error
		}
		return exec
	}
	if unit.PID != 0 {
		exec.PID = unit.PID
	}
	if unit.StartedAt != nil {
		exec.StartedAt = unit.StartedAt
	}
	if unit.ExitedAt != nil {
		exec.ExitedAt = unit.ExitedAt
	}
	if unit.ExitCode != nil {
		exec.ExitCode = unit.ExitCode
	}
	if unit.Error != "" {
		exec.Error = unit.Error
	}
	if unit.Status != "" {
		exec.Status = unit.Status
	}
	return exec
}

func mergeExecStatus(base, status Exec) Exec {
	if status.ID != "" {
		base.ID = status.ID
	}
	if status.Status != "" {
		base.Status = status.Status
	}
	if status.PID != 0 {
		base.PID = status.PID
	}
	if status.ExitCode != nil {
		base.ExitCode = status.ExitCode
	}
	if status.Error != "" {
		base.Error = status.Error
	}
	if status.StartedAt != nil {
		base.StartedAt = status.StartedAt
	}
	if status.ExitedAt != nil {
		base.ExitedAt = status.ExitedAt
	}
	return base
}

func (m *Manager) recordEvent(ctx context.Context, execID, typ, message string, details map[string]any) error {
	if m.audit == nil {
		return nil
	}
	return m.audit.RecordExecEvent(ctx, execID, typ, message, details)
}

func (m *Manager) observe(ctx context.Context, exec Exec) error {
	if m.audit == nil {
		return nil
	}
	return m.audit.ObserveExec(ctx, exec)
}

func writeRuntime(path string, exec Exec) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(exec, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func readRuntime(path string) (Exec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Exec{}, err
	}
	var exec Exec
	if err := json.Unmarshal(data, &exec); err != nil {
		return Exec{}, err
	}
	return exec, nil
}

func newID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return "ex_" + hex.EncodeToString(data[:]), nil
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneInt64(in *int64) *int64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneExec(in Exec) Exec {
	out := in
	out.Command = append([]string{}, in.Command...)
	out.Env = cloneMap(in.Env)
	out.Metadata = cloneMap(in.Metadata)
	out.SocketPath = in.SocketPath
	out.UID = cloneInt64(in.UID)
	out.GID = cloneInt64(in.GID)
	return out
}

func safeName(value string) string {
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return "exec"
	}
	return out
}

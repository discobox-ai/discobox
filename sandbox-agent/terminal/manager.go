package terminal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/obot-platform/discobox/sandbox-agent/config"
)

type Status string

const (
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusExited   Status = "exited"
	StatusFailed   Status = "failed"
	StatusLost     Status = "lost"
)

type Terminal struct {
	ID          string            `json:"id"`
	AgentID     string            `json:"agentId,omitempty"`
	Status      Status            `json:"status"`
	Command     []string          `json:"command"`
	Workdir     string            `json:"workdir"`
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
	AgentID  string
	Args     []string
	Workdir  string
	Env      map[string]string
	Metadata map[string]string
	Rows     uint16
	Cols     uint16
}

type UnitManager interface {
	Start(context.Context, StartRequest) (StartResult, error)
	Stop(context.Context, string) error
	Status(context.Context, string) (UnitStatus, error)
	List(context.Context) ([]UnitStatus, error)
}

type StartRequest struct {
	ID          string
	AgentID     string
	Unit        string
	Command     []string
	Workdir     string
	Env         map[string]string
	SocketPath  string
	RuntimePath string
	LogDir      string
	Rows        uint16
	Cols        uint16
}

type StartResult struct {
	Unit           string
	PID            int64
	SkipStatusWait bool
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
	RecordEvent(context.Context, string, string, string, map[string]any) error
	ObserveTerminal(context.Context, Terminal) error
}

type Manager struct {
	agents      map[string]config.Agent
	defaultID   string
	workingRoot string
	runtimeDir  string
	logDir      string
	units       UnitManager
	audit       AuditRecorder
}

func NewManager(agents []config.Agent, workingRoot string, runtimeDir string, units UnitManager, audit AuditRecorder) (*Manager, error) {
	if strings.TrimSpace(workingRoot) == "" {
		return nil, errors.New("working root is required")
	}
	if strings.TrimSpace(runtimeDir) == "" {
		runtimeDir = "/run/discobox/agent-terminals"
	}
	if units == nil {
		units = SystemdRunner{}
	}
	m := &Manager{
		agents:      map[string]config.Agent{},
		workingRoot: filepath.Clean(workingRoot),
		runtimeDir:  filepath.Clean(runtimeDir),
		logDir:      filepath.Join(filepath.Clean(runtimeDir), "logs"),
		units:       units,
		audit:       audit,
	}
	for _, agent := range agents {
		if _, exists := m.agents[agent.ID]; exists {
			return nil, fmt.Errorf("duplicate agent %q", agent.ID)
		}
		m.agents[agent.ID] = agent
		if m.defaultID == "" || agent.IsDefault {
			m.defaultID = agent.ID
		}
	}
	return m, nil
}

func (m *Manager) Create(ctx context.Context, req CreateRequest) (Terminal, error) {
	agent, agentID, err := m.resolveAgent(req.AgentID)
	if err != nil {
		return Terminal{}, err
	}
	workdir, err := m.resolveWorkdir(req.Workdir)
	if err != nil {
		return Terminal{}, err
	}
	id, err := newID()
	if err != nil {
		return Terminal{}, err
	}
	command := append([]string{}, agent.Command...)
	command = append(command, req.Args...)
	unit := "discobox-agent-terminal-" + id
	socketPath := m.socketPath(id)
	runtimePath := m.runtimePath(id)
	now := time.Now().UTC()
	terminal := Terminal{
		ID:          id,
		AgentID:     agentID,
		Status:      StatusStarting,
		Command:     command,
		Workdir:     workdir,
		Unit:        unit,
		CreatedAt:   now,
		Metadata:    cloneMap(req.Metadata),
		SocketPath:  socketPath,
		RuntimePath: runtimePath,
	}

	if err := writeRuntime(runtimePath, terminal); err != nil {
		return Terminal{}, err
	}
	_ = m.recordEvent(ctx, id, "terminal.created", "terminal created", map[string]any{
		"agentId": agentID,
		"unit":    unit,
		"workdir": workdir,
		"command": command,
	})

	result, err := m.units.Start(ctx, StartRequest{
		ID:          id,
		AgentID:     agentID,
		Unit:        unit,
		Command:     command,
		Workdir:     workdir,
		Env:         cloneMap(req.Env),
		SocketPath:  socketPath,
		RuntimePath: runtimePath,
		LogDir:      m.logDir,
		Rows:        req.Rows,
		Cols:        req.Cols,
	})
	var live Terminal
	if err == nil && !result.SkipStatusWait {
		live, _ = waitForStatus(ctx, socketPath, 5*time.Second)
	}

	current := terminal
	if err != nil {
		current.Status = StatusFailed
		current.Error = err.Error()
		exitedAt := time.Now().UTC()
		current.ExitedAt = &exitedAt
		_ = writeRuntime(runtimePath, current)
		_ = m.observe(ctx, current)
		_ = m.recordEvent(ctx, id, "terminal.start.failed", "terminal start failed", map[string]any{"error": err.Error()})
		return current, err
	}
	startedAt := time.Now().UTC()
	current.Status = StatusRunning
	current.StartedAt = &startedAt
	if result.Unit != "" {
		current.Unit = result.Unit
	}
	current.PID = result.PID
	if live.ID != "" {
		current = mergeStatus(current, live)
	}
	_ = writeRuntime(runtimePath, current)
	_ = m.observe(ctx, current)
	_ = m.recordEvent(ctx, id, "terminal.started", "terminal started", map[string]any{"unit": current.Unit, "pid": current.PID})
	return current, nil
}

func (m *Manager) List() []Terminal {
	return m.list(context.Background())
}

func (m *Manager) Reconcile(ctx context.Context) error {
	_ = m.list(ctx)
	return nil
}

func (m *Manager) list(ctx context.Context) []Terminal {
	terminals := m.runtimeTerminals(ctx)
	if units, err := m.units.List(ctx); err == nil {
		byUnit := map[string]UnitStatus{}
		for _, unit := range units {
			byUnit[unit.Unit] = unit
		}
		for i := range terminals {
			if unit, ok := byUnit[terminals[i].Unit]; ok {
				terminals[i] = applyUnitStatus(terminals[i], unit)
			}
			terminals[i] = m.refreshTerminal(ctx, terminals[i])
		}
	} else {
		for i := range terminals {
			terminals[i] = m.refreshTerminal(ctx, terminals[i])
		}
	}
	sort.Slice(terminals, func(i, j int) bool {
		return terminals[i].CreatedAt.Before(terminals[j].CreatedAt)
	})
	return terminals
}

func (m *Manager) Delete(ctx context.Context, id string) error {
	terminal, ok := m.Get(id)
	if !ok {
		return ErrNotFound
	}
	if err := m.units.Stop(ctx, terminal.Unit); err != nil {
		return err
	}
	_ = m.recordEvent(ctx, id, "terminal.stop.requested", "terminal stop requested", map[string]any{"unit": terminal.Unit})
	_ = os.Remove(terminal.RuntimePath)
	_ = os.Remove(terminal.SocketPath)
	_ = m.recordEvent(ctx, id, "terminal.deleted", "terminal deleted", map[string]any{"unit": terminal.Unit})
	return nil
}

func (m *Manager) Get(id string) (Terminal, bool) {
	terminal, ok := m.readRuntime(id)
	if !ok {
		return Terminal{}, false
	}
	terminal = m.refreshTerminal(context.Background(), terminal)
	return cloneTerminal(terminal), true
}

func (m *Manager) Attach(ctx context.Context, w http.ResponseWriter, id string) error {
	terminal, ok := m.Get(id)
	if !ok {
		return ErrNotFound
	}
	_ = m.recordEvent(ctx, id, "terminal.attach.opened", "terminal attach opened", map[string]any{"unit": terminal.Unit})
	defer func() {
		_ = m.recordEvent(context.Background(), id, "terminal.attach.closed", "terminal attach closed", map[string]any{"unit": terminal.Unit})
	}()
	return proxyAttach(ctx, w, terminal.SocketPath)
}

func (m *Manager) Logs(ctx context.Context, id string) ([]LogEntry, error) {
	if _, ok := m.Get(id); !ok {
		return nil, ErrNotFound
	}
	return ReadLogs(ctx, m.logDir, id)
}

var ErrNotFound = errors.New("agent terminal not found")

func (m *Manager) resolveAgent(requested string) (config.Agent, string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		requested = m.defaultID
	}
	if requested == "" {
		return config.Agent{}, "", errors.New("no agent terminals are configured")
	}
	agent, ok := m.agents[requested]
	if !ok {
		return config.Agent{}, "", fmt.Errorf("agent %q is not configured", requested)
	}
	return agent, requested, nil
}

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

func (m *Manager) socketPath(id string) string {
	return filepath.Join(m.runtimeDir, safeName(id)+".sock")
}

func (m *Manager) runtimePath(id string) string {
	return filepath.Join(m.runtimeDir, safeName(id)+".json")
}

func (m *Manager) runtimeTerminals(ctx context.Context) []Terminal {
	matches, err := filepath.Glob(filepath.Join(m.runtimeDir, "*.json"))
	if err != nil {
		return nil
	}
	out := make([]Terminal, 0, len(matches))
	for _, path := range matches {
		terminal, err := readRuntime(path)
		if err != nil || terminal.ID == "" {
			continue
		}
		if terminal.RuntimePath == "" {
			terminal.RuntimePath = path
		}
		if terminal.SocketPath == "" {
			terminal.SocketPath = m.socketPath(terminal.ID)
		}
		out = append(out, m.refreshTerminal(ctx, terminal))
	}
	return out
}

func (m *Manager) readRuntime(id string) (Terminal, bool) {
	terminal, err := readRuntime(m.runtimePath(id))
	return terminal, err == nil && terminal.ID != ""
}

func (m *Manager) refreshTerminal(ctx context.Context, terminal Terminal) Terminal {
	if unit, err := m.units.Status(ctx, terminal.Unit); err == nil {
		terminal = applyUnitStatus(terminal, unit)
	}
	if live, err := statusFromSocket(ctx, terminal.SocketPath); err == nil {
		terminal = mergeStatus(terminal, live)
	} else if terminal.ExitedAt == nil && terminal.Status != StatusFailed && terminal.Status != StatusExited {
		terminal.Status = StatusLost
		terminal.Error = "agent terminal shim socket is unavailable"
	}
	_ = writeRuntime(terminal.RuntimePath, terminal)
	_ = m.observe(ctx, terminal)
	return terminal
}

func newID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return "agt_" + hex.EncodeToString(data[:]), nil
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

func cloneTerminal(in Terminal) Terminal {
	out := in
	out.Command = append([]string{}, in.Command...)
	out.Metadata = cloneMap(in.Metadata)
	return out
}

func applyUnitStatus(terminal Terminal, unit UnitStatus) Terminal {
	if unit.Unit != "" {
		terminal.Unit = unit.Unit
	}
	if unit.PID != 0 {
		terminal.PID = unit.PID
	}
	if unit.StartedAt != nil {
		terminal.StartedAt = unit.StartedAt
	}
	if unit.ExitedAt != nil {
		terminal.ExitedAt = unit.ExitedAt
	}
	if unit.ExitCode != nil {
		terminal.ExitCode = unit.ExitCode
	}
	if unit.Error != "" {
		terminal.Error = unit.Error
	}
	if unit.Status != "" {
		terminal.Status = unit.Status
	}
	return terminal
}

func (m *Manager) recordEvent(ctx context.Context, terminalID, typ, message string, details map[string]any) error {
	if m.audit == nil {
		return nil
	}
	return m.audit.RecordEvent(ctx, terminalID, typ, message, details)
}

func (m *Manager) observe(ctx context.Context, terminal Terminal) error {
	if m.audit == nil {
		return nil
	}
	return m.audit.ObserveTerminal(ctx, terminal)
}

func writeRuntime(path string, terminal Terminal) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(terminal, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func readRuntime(path string) (Terminal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Terminal{}, err
	}
	var terminal Terminal
	if err := json.Unmarshal(data, &terminal); err != nil {
		return Terminal{}, err
	}
	return terminal, nil
}

func statusFromSocket(ctx context.Context, socketPath string) (Terminal, error) {
	if strings.TrimSpace(socketPath) == "" {
		return Terminal{}, errors.New("socket path is required")
	}
	httpClient := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/status", nil)
	if err != nil {
		return Terminal{}, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return Terminal{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Terminal{}, fmt.Errorf("shim status returned %d", resp.StatusCode)
	}
	var terminal Terminal
	if err := json.NewDecoder(resp.Body).Decode(&terminal); err != nil {
		return Terminal{}, err
	}
	return terminal, nil
}

func waitForStatus(ctx context.Context, socketPath string, timeout time.Duration) (Terminal, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		status, err := statusFromSocket(ctx, socketPath)
		if err == nil {
			return status, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return Terminal{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out waiting for shim status")
	}
	return Terminal{}, lastErr
}

func proxyAttach(ctx context.Context, w http.ResponseWriter, socketPath string) error {
	shimConn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return err
	}
	defer shimConn.Close()
	clientConn, clientRW, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return err
	}
	defer clientConn.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/attach", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "discobox-agent-terminal")
	if err := req.Write(shimConn); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(shimConn, clientRW)
		if conn, ok := shimConn.(*net.UnixConn); ok {
			_ = conn.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, shimConn)
		if conn, ok := clientConn.(*net.UnixConn); ok {
			_ = conn.CloseWrite()
		}
	}()
	wg.Wait()
	return nil
}

func mergeStatus(base, live Terminal) Terminal {
	live.Unit = base.Unit
	live.SocketPath = base.SocketPath
	live.RuntimePath = base.RuntimePath
	if live.CreatedAt.IsZero() {
		live.CreatedAt = base.CreatedAt
	}
	if len(live.Metadata) == 0 {
		live.Metadata = cloneMap(base.Metadata)
	}
	return live
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
		return "agent-terminal"
	}
	return out
}

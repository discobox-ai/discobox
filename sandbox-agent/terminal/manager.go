package terminal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/harness/registry"
	"github.com/obot-platform/discobox/sandbox-agent/config"
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

type Installer interface {
	EnsureInstalled(context.Context, config.Agent, string, map[string]string) error
}

type ManagerConfig struct {
	ResolvedAgentConfig *config.Agent
	AgentConfigs        []config.Agent
	Agents              []config.Agent
	WorkingRoot         string
	RuntimeDir          string
	Env                 map[string]string
	Units               UnitManager
	Installer           Installer
	Audit               AuditRecorder
}

type Manager struct {
	forcedAgent    *config.Agent
	agentConfigs   map[string]config.Agent
	agents         map[string]config.Agent
	defaultID      string
	workingRoot    string
	runtimeDir     string
	logDir         string
	env            map[string]string
	hookSocketPath string
	units          UnitManager
	installer      Installer
	audit          AuditRecorder
}

func NewManager(cfg ManagerConfig) (*Manager, error) {
	workingRoot := cfg.WorkingRoot
	runtimeDir := cfg.RuntimeDir
	units := cfg.Units
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
		agentConfigs: map[string]config.Agent{},
		agents:       map[string]config.Agent{},
		workingRoot:  filepath.Clean(workingRoot),
		runtimeDir:   filepath.Clean(runtimeDir),
		logDir:       filepath.Join(filepath.Clean(runtimeDir), "logs"),
		env:          cloneMap(cfg.Env),
		units:        units,
		installer:    cfg.Installer,
		audit:        cfg.Audit,
	}
	if m.installer == nil {
		m.installer = CompositeInstaller{Installers: []Installer{
			CommandInstaller{},
			HarnessInstaller{},
		}}
	}
	if cfg.ResolvedAgentConfig != nil && strings.TrimSpace(cfg.ResolvedAgentConfig.ID) != "" {
		agent := cloneAgent(*cfg.ResolvedAgentConfig)
		m.forcedAgent = &agent
	}
	for _, agent := range cfg.AgentConfigs {
		if strings.TrimSpace(agent.ID) == "" {
			continue
		}
		if _, exists := m.agentConfigs[agent.ID]; exists {
			return nil, fmt.Errorf("duplicate agent config %q", agent.ID)
		}
		m.agentConfigs[agent.ID] = cloneAgent(agent)
		if m.defaultID == "" || agent.IsDefault {
			m.defaultID = agent.ID
		}
	}
	for _, agent := range cfg.Agents {
		if strings.TrimSpace(agent.ID) == "" {
			continue
		}
		if _, exists := m.agents[agent.ID]; exists {
			return nil, fmt.Errorf("duplicate agent %q", agent.ID)
		}
		m.agents[agent.ID] = cloneAgent(agent)
		if m.defaultID == "" || agent.IsDefault {
			m.defaultID = agent.ID
		}
	}
	return m, nil
}

func (m *Manager) SetHookSocketPath(path string) {
	m.hookSocketPath = strings.TrimSpace(path)
}

func (m *Manager) Create(ctx context.Context, req CreateRequest) (Terminal, error) {
	workdir, err := m.resolveWorkdir(req.Workdir)
	if err != nil {
		return Terminal{}, err
	}
	env := mergeEnv(m.env, req.Env)
	if env == nil {
		env = map[string]string{}
	}
	agent, agentID, err := m.resolveAgent(req.AgentID, workdir)
	if err != nil {
		return Terminal{}, err
	}
	id, err := newID()
	if err != nil {
		return Terminal{}, err
	}
	env["DISCOBOX_TERMINAL_ID"] = id
	if m.hookSocketPath != "" {
		env["DISCOBOX_HOOK_SOCKET"] = m.hookSocketPath
	}
	if err := m.installer.EnsureInstalled(ctx, agent, workdir, env); err != nil {
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
		Env:         env,
		SocketPath:  socketPath,
		RuntimePath: runtimePath,
		LogDir:      m.logDir,
		Rows:        req.Rows,
		Cols:        req.Cols,
	})
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
	if result.Unit != "" {
		current.Unit = result.Unit
	}
	_ = writeRuntime(runtimePath, current)
	_ = m.observe(ctx, current)
	_ = m.recordEvent(ctx, id, "terminal.prepared", "terminal prepared", map[string]any{"unit": current.Unit})
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
	return shimproxy.AttachHTTPUpgrade(ctx, w, terminal.SocketPath, "discobox-agent-terminal")
}

func (m *Manager) Start(ctx context.Context, id string) (Terminal, error) {
	terminal, ok := m.readRuntime(id)
	if !ok {
		return Terminal{}, ErrNotFound
	}
	if terminal.Status != StatusStarting {
		return cloneTerminal(terminal), nil
	}
	started, err := shimproxy.StartJSON[Terminal](ctx, terminal.SocketPath)
	if err != nil {
		terminal.Status = StatusFailed
		terminal.Error = err.Error()
		exitedAt := time.Now().UTC()
		terminal.ExitedAt = &exitedAt
		_ = writeRuntime(terminal.RuntimePath, terminal)
		_ = m.observe(ctx, terminal)
		_ = m.recordEvent(ctx, id, "terminal.start.failed", "terminal start failed", map[string]any{"error": err.Error()})
		return cloneTerminal(terminal), err
	}
	current := mergeStatus(terminal, started)
	_ = writeRuntime(current.RuntimePath, current)
	_ = m.observe(ctx, current)
	_ = m.recordEvent(ctx, id, "terminal.started", "terminal started", map[string]any{"unit": current.Unit, "pid": current.PID})
	return cloneTerminal(current), nil
}

func (m *Manager) Logs(ctx context.Context, id string) ([]LogEntry, error) {
	if _, ok := m.Get(id); !ok {
		return nil, ErrNotFound
	}
	return ReadLogs(ctx, m.logDir, id)
}

var ErrNotFound = errors.New("agent terminal not found")

type CommandInstaller struct{}

var (
	installedCommandsMu sync.Mutex
	installedCommands   = map[string]struct{}{}
)

type CompositeInstaller struct {
	Installers []Installer
}

func (i CompositeInstaller) EnsureInstalled(ctx context.Context, agent config.Agent, workdir string, env map[string]string) error {
	for _, installer := range i.Installers {
		if installer == nil {
			continue
		}
		if err := installer.EnsureInstalled(ctx, agent, workdir, env); err != nil {
			return err
		}
	}
	return nil
}

type HarnessInstaller struct {
	ManagedRoot      string
	PublisherCommand string
}

func (i HarnessInstaller) EnsureInstalled(ctx context.Context, agent config.Agent, workdir string, env map[string]string) error {
	installer := registry.Installer{
		Drivers:          registry.DriverForAgent(harnessAgent(agent)),
		ManagedRoot:      i.ManagedRoot,
		PublisherCommand: i.PublisherCommand,
	}
	return installer.Install(ctx, harness.InstallRequest{
		Agent:            harnessAgent(agent),
		Workdir:          workdir,
		Env:              env,
		ManagedRoot:      i.ManagedRoot,
		PublisherCommand: i.PublisherCommand,
	})
}

func harnessAgent(agent config.Agent) harness.Agent {
	return harness.Agent{
		ID:      agent.ID,
		Name:    agent.Name,
		Command: append([]string{}, agent.Command...),
	}
}

func (CommandInstaller) EnsureInstalled(ctx context.Context, agent config.Agent, workdir string, env map[string]string) error {
	installCommand := strings.TrimSpace(agent.InstallCommand)
	if installCommand == "" {
		return nil
	}
	installedCommandsMu.Lock()
	defer installedCommandsMu.Unlock()
	if _, ok := installedCommands[installCommand]; ok {
		return nil
	}
	cmd := exec.CommandContext(ctx, "/bin/bash", "-lc", installCommand)
	cmd.Dir = workdir
	cmd.Env = mergedEnv(env)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("install agent %q: %w: %s", agent.ID, err, strings.TrimSpace(string(output)))
	}
	installedCommands[installCommand] = struct{}{}
	return nil
}

func mergedEnv(env map[string]string) []string {
	out := os.Environ()
	for key, value := range env {
		if strings.TrimSpace(key) == "" {
			continue
		}
		out = append(out, key+"="+value)
	}
	return out
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

func (m *Manager) resolveAgent(requested string, workdir string) (config.Agent, string, error) {
	if m.forcedAgent != nil {
		agent := cloneAgent(*m.forcedAgent)
		return agent, agent.ID, nil
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		if local, ok, err := m.localAgentConfig(workdir); err != nil {
			return config.Agent{}, "", err
		} else if ok {
			return local, local.ID, nil
		}
	}
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

type localAgentConfig struct {
	Agent          string    `json:"agent,omitempty"`
	ID             string    `json:"id,omitempty"`
	Name           string    `json:"name,omitempty"`
	InstallCommand *string   `json:"installCommand,omitempty"`
	Command        *[]string `json:"command,omitempty"`
	RunCommand     *string   `json:"runCommand,omitempty"`
}

func (m *Manager) localAgentConfig(workdir string) (config.Agent, bool, error) {
	repoRoot, ok := gitRoot(workdir, m.workingRoot)
	if !ok {
		return config.Agent{}, false, nil
	}
	path, ok := localAgentConfigPath(repoRoot)
	if !ok {
		return config.Agent{}, false, nil
	}
	local, err := readLocalAgentConfig(path)
	if err != nil {
		return config.Agent{}, false, err
	}
	selector := firstNonEmpty(local.Agent, local.ID, local.Name)
	if selector == "" {
		return config.Agent{}, false, fmt.Errorf("local agent config %s must set agent, id, or name", path)
	}
	agent, ok := m.matchAgent(selector)
	if !ok {
		agent = config.Agent{ID: selector, Name: selector}
	}
	agent = applyLocalAgentConfig(agent, local)
	if strings.TrimSpace(agent.ID) == "" {
		return config.Agent{}, false, fmt.Errorf("local agent config %s resolved empty agent id", path)
	}
	if len(agent.Command) == 0 || strings.TrimSpace(agent.Command[0]) == "" {
		return config.Agent{}, false, fmt.Errorf("local agent config %s resolved agent %q without command", path, agent.ID)
	}
	return agent, true, nil
}

func localAgentConfigPath(repoRoot string) (string, bool) {
	for _, name := range []string{"agent.json", "agent-config.json", "sandbox.json"} {
		path := filepath.Join(repoRoot, ".discobox", name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, true
		}
	}
	return "", false
}

func readLocalAgentConfig(path string) (localAgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return localAgentConfig{}, err
	}
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		return localAgentConfig{Agent: name}, nil
	}
	var out localAgentConfig
	if err := json.Unmarshal(data, &out); err != nil {
		return localAgentConfig{}, fmt.Errorf("parse local agent config %s: %w", path, err)
	}
	return out, nil
}

func (m *Manager) matchAgent(selector string) (config.Agent, bool) {
	selector = strings.TrimSpace(selector)
	for _, agents := range []map[string]config.Agent{m.agentConfigs, m.agents} {
		if agent, ok := agents[selector]; ok {
			return cloneAgent(agent), true
		}
		for _, agent := range agents {
			if strings.EqualFold(agent.Name, selector) {
				return cloneAgent(agent), true
			}
		}
	}
	return config.Agent{}, false
}

func applyLocalAgentConfig(agent config.Agent, local localAgentConfig) config.Agent {
	if strings.TrimSpace(local.ID) != "" {
		agent.ID = strings.TrimSpace(local.ID)
	}
	if strings.TrimSpace(local.Name) != "" {
		agent.Name = strings.TrimSpace(local.Name)
	}
	if local.InstallCommand != nil {
		agent.InstallCommand = *local.InstallCommand
	}
	if local.Command != nil {
		agent.Command = append([]string{}, (*local.Command)...)
	}
	if local.RunCommand != nil {
		agent.Command = []string{"/bin/bash", "-lc", *local.RunCommand}
	}
	return agent
}

func gitRoot(workdir, workingRoot string) (string, bool) {
	workdir = filepath.Clean(workdir)
	if output, err := exec.Command("git", "-C", workdir, "rev-parse", "--show-toplevel").Output(); err == nil {
		root := filepath.Clean(strings.TrimSpace(string(output)))
		if insideRoot(root, workingRoot) {
			return root, true
		}
	}
	for dir := workdir; insideRoot(dir, workingRoot); dir = filepath.Dir(dir) {
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
			return dir, true
		}
		if dir == filepath.Clean(workingRoot) || dir == filepath.Dir(dir) {
			break
		}
	}
	return "", false
}

func insideRoot(path, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
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

func cloneAgent(in config.Agent) config.Agent {
	out := in
	out.Command = append([]string{}, in.Command...)
	return out
}

func applyUnitStatus(terminal Terminal, unit UnitStatus) Terminal {
	if unit.Unit != "" {
		terminal.Unit = unit.Unit
	}
	if terminal.Status == StatusStarting && terminal.StartedAt == nil {
		if unit.Error != "" {
			terminal.Error = unit.Error
		}
		return terminal
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

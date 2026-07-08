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
	"github.com/obot-platform/discobox/sandbox-agent/execs"
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

const defaultTerm = "xterm-256color"

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
	Primary     bool              `json:"primary,omitempty"`
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

	// primary and command are set only by the sandbox-agent's own primary
	// terminal launch, never from the terminal create API. command, when set,
	// replaces the resolved agent command and args entirely (used for the
	// relaunch/resume command on subsequent sandbox starts).
	primary bool
	command []string
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
	User        *execs.User
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

// PrimaryStateStore records, durably across sandbox restarts, whether the
// primary terminal has already been launched. It lets EnsurePrimary decide
// between the initial prompt and the relaunch (resume) command.
type PrimaryStateStore interface {
	PrimaryTerminalLaunched(context.Context) (bool, error)
	MarkPrimaryTerminalLaunched(context.Context) error
}

type ManagerConfig struct {
	ResolvedAgentConfig *config.Agent
	Agents              []config.Agent
	WorkingRoot         string
	RuntimeDir          string
	Env                 map[string]string
	ImageConfig         config.ImageConfig
	ImageConfigPath     string
	ExecDefaults        config.ExecDefaults
	DefaultUser         *execs.User
	Units               UnitManager
	Installer           Installer
	Audit               AuditRecorder
	PrimaryState        PrimaryStateStore
}

type Manager struct {
	agents         map[string]config.Agent
	resolvedID     string
	defaultID      string
	workingRoot    string
	defaultWorkdir string
	runtimeDir     string
	logDir         string
	env            map[string]string
	imageConfig    config.ImageConfig
	defaultUser    *execs.User
	hookSocketPath string
	units          UnitManager
	installer      Installer
	audit          AuditRecorder
	primaryState   PrimaryStateStore
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
	imageConfig := cfg.ImageConfig
	if len(imageConfig.Env) == 0 {
		var err error
		imageConfig, err = config.LoadImage(cfg.ImageConfigPath)
		if err != nil {
			return nil, err
		}
	}
	m := &Manager{
		agents:         map[string]config.Agent{},
		workingRoot:    filepath.Clean(workingRoot),
		defaultWorkdir: strings.TrimSpace(cfg.ExecDefaults.Workdir),
		runtimeDir:     filepath.Clean(runtimeDir),
		logDir:         filepath.Join(filepath.Clean(runtimeDir), "logs"),
		env:            cloneMap(cfg.Env),
		imageConfig:    imageConfig,
		defaultUser:    terminalDefaultUser(cfg),
		units:          units,
		installer:      cfg.Installer,
		audit:          cfg.Audit,
		primaryState:   cfg.PrimaryState,
	}
	if m.installer == nil {
		installRuntimeDir := filepath.Join(m.runtimeDir, "installs")
		m.installer = CompositeInstaller{Installers: []Installer{
			CommandInstaller{
				Units:      m.units,
				RuntimeDir: installRuntimeDir,
				LogDir:     m.logDir,
				User:       cloneUser(m.defaultUser),
			},
			HookInstaller{},
			FileInstaller{
				HomeDirectory: cfg.ExecDefaults.HomeDirectory,
				UID:           cfg.ExecDefaults.UID,
				GID:           cfg.ExecDefaults.GID,
			},
		}}
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
	// The sandbox's resolved agent config is the default terminals use when no
	// agent is explicitly requested. It is registered like any other agent so an
	// explicit request can still select a different one.
	if cfg.ResolvedAgentConfig != nil && strings.TrimSpace(cfg.ResolvedAgentConfig.ID) != "" {
		m.resolvedID = strings.TrimSpace(cfg.ResolvedAgentConfig.ID)
		if _, exists := m.agents[m.resolvedID]; !exists {
			m.agents[m.resolvedID] = cloneAgent(*cfg.ResolvedAgentConfig)
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
	env := envWithRuntimeDefaults(mergeEnv(m.env, req.Env), m.defaultUser, m.imageConfig)
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
	var command []string
	if len(req.command) > 0 {
		command = append([]string{}, req.command...)
	} else {
		command = append([]string{}, agent.Command...)
		command = append(command, req.Args...)
	}
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
		Primary:     req.primary,
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
		User:        cloneUser(m.defaultUser),
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

// EnsurePrimary launches the sandbox's primary terminal on sandbox start. On the
// first start it runs the resolved agent with the sandbox prompt as arguments,
// mirroring a normal terminal create. On subsequent starts it runs the agent's
// relaunch command to resume the previous session instead of replaying the
// prompt. The launched terminal is flagged Primary; that flag cannot be set
// through the terminal create API. It is a no-op when a live primary terminal
// already exists or when no agent is configured.
func (m *Manager) EnsurePrimary(ctx context.Context, prompt []string) error {
	for _, existing := range m.List() {
		if existing.Primary && (existing.Status == StatusStarting || existing.Status == StatusRunning) {
			return nil
		}
	}
	workdir, err := m.resolveWorkdir("")
	if err != nil {
		return err
	}
	agent, _, err := m.resolveAgent("", workdir)
	if err != nil {
		// No agent is configured for this sandbox; there is nothing to launch.
		return nil
	}
	launched := false
	if m.primaryState != nil {
		if launched, err = m.primaryState.PrimaryTerminalLaunched(ctx); err != nil {
			return err
		}
	}
	created, err := m.Create(ctx, primaryCreateRequest(agent, prompt, launched))
	if err != nil {
		return err
	}
	if _, err := m.Start(ctx, created.ID); err != nil {
		return err
	}
	if m.primaryState != nil {
		return m.primaryState.MarkPrimaryTerminalLaunched(ctx)
	}
	return nil
}

// primaryCreateRequest builds the create request for the primary terminal. On
// the first start (launched == false) it runs the agent with the prompt as
// arguments. On subsequent starts it resumes the previous session using the
// agent's relaunch command, which replaces the run command entirely; when no
// relaunch command is configured it starts the agent without replaying the
// prompt.
func primaryCreateRequest(agent config.Agent, prompt []string, launched bool) CreateRequest {
	req := CreateRequest{primary: true}
	switch {
	case launched && len(agent.RelaunchCommand) > 0:
		req.command = append([]string{}, agent.RelaunchCommand...)
	case launched:
	default:
		req.Args = append([]string{}, prompt...)
	}
	return req
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

func (m *Manager) Attach(ctx context.Context, w http.ResponseWriter, id string, replay bool) error {
	terminal, ok := m.Get(id)
	if !ok {
		return ErrNotFound
	}
	_ = m.recordEvent(ctx, id, "terminal.attach.opened", "terminal attach opened", map[string]any{"unit": terminal.Unit, "replay": replay})
	defer func() {
		_ = m.recordEvent(context.Background(), id, "terminal.attach.closed", "terminal attach closed", map[string]any{"unit": terminal.Unit})
	}()
	return shimproxy.AttachHTTPUpgrade(ctx, w, terminal.SocketPath, "discobox-agent-terminal", replay)
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

type CommandInstaller struct {
	Units            UnitManager
	RuntimeDir       string
	LogDir           string
	StatusTimeout    time.Duration
	PollInterval     time.Duration
	User             *execs.User
	startFromSocket  func(context.Context, string) (Terminal, error)
	statusFromSocket func(context.Context, string) (Terminal, error)
}

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

type HookInstaller struct {
	ManagedRoot      string
	PublisherCommand string
}

func (i HookInstaller) EnsureInstalled(ctx context.Context, agent config.Agent, workdir string, env map[string]string) error {
	installer := registry.Installer{
		Drivers:          registry.DriverForAgent(harnessAgent(agent)),
		ManagedRoot:      i.ManagedRoot,
		PublisherCommand: i.PublisherCommand,
	}
	return installer.InstallHooks(ctx, harness.HookInstallRequest{
		Agent:            harnessAgent(agent),
		Workdir:          workdir,
		Env:              env,
		ManagedRoot:      i.ManagedRoot,
		PublisherCommand: i.PublisherCommand,
	})
}

// FileInstaller writes an agent's configured files into its home directory.
type FileInstaller struct {
	HomeDirectory string
	UID           *int64
	GID           *int64
}

func (i FileInstaller) EnsureInstalled(_ context.Context, agent config.Agent, _ string, _ map[string]string) error {
	if len(agent.Files) == 0 {
		return nil
	}
	home := strings.TrimSpace(i.HomeDirectory)
	if home == "" {
		return fmt.Errorf("agent %q has files to install but no home directory is configured", agent.ID)
	}
	home = filepath.Clean(home)
	for _, file := range agent.Files {
		path, err := homeRelativePath(home, file.Path)
		if err != nil {
			return fmt.Errorf("agent %q file %q: %w", agent.ID, file.Path, err)
		}
		if err := writeAgentFile(path, file.Content, file.CreateOnly, i.UID, i.GID); err != nil {
			return fmt.Errorf("agent %q file %q: %w", agent.ID, file.Path, err)
		}
	}
	return nil
}

func homeRelativePath(home, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(requested) {
		return "", fmt.Errorf("path %q must be relative to the home directory", requested)
	}
	cleaned := filepath.Clean(filepath.Join(home, requested))
	rel, err := filepath.Rel(home, cleaned)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes home directory", requested)
	}
	return cleaned, nil
}

func writeAgentFile(path, content string, createOnly bool, uid, gid *int64) error {
	createdDirs, err := mkdirAllTracked(filepath.Dir(path), 0o755)
	if err != nil {
		return err
	}
	if createOnly {
		if info, err := os.Stat(path); err == nil {
			if info.IsDir() {
				return fmt.Errorf("%s is a directory", path)
			}
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return err
	}
	if uid == nil || gid == nil {
		return nil
	}
	for _, created := range createdDirs {
		if err := os.Chown(created, int(*uid), int(*gid)); err != nil {
			return err
		}
	}
	return os.Chown(path, int(*uid), int(*gid))
}

// mkdirAllTracked behaves like os.MkdirAll but returns the directories it
// actually created, so callers can chown only new paths and leave
// pre-existing directory ownership untouched.
func mkdirAllTracked(path string, perm os.FileMode) ([]string, error) {
	path = filepath.Clean(path)
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("%s exists and is not a directory", path)
		}
		return nil, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	var created []string
	if parent := filepath.Dir(path); parent != path {
		parentCreated, err := mkdirAllTracked(parent, perm)
		if err != nil {
			return nil, err
		}
		created = append(created, parentCreated...)
	}
	if err := os.Mkdir(path, perm); err != nil && !os.IsExist(err) {
		return nil, err
	}
	return append(created, path), nil
}

func harnessAgent(agent config.Agent) harness.Agent {
	return harness.Agent{
		ID:      agent.ID,
		Name:    agent.Name,
		Command: append([]string{}, agent.Command...),
	}
}

func (i CommandInstaller) EnsureInstalled(ctx context.Context, agent config.Agent, workdir string, env map[string]string) error {
	if len(agent.InstallCommand) == 0 {
		return nil
	}
	installKey := strings.Join(agent.InstallCommand, "\x00")
	installedCommandsMu.Lock()
	defer installedCommandsMu.Unlock()
	if _, ok := installedCommands[installKey]; ok {
		return nil
	}
	status, err := i.run(ctx, agent, workdir, env, agent.InstallCommand)
	if err != nil {
		return err
	}
	if status.ExitCode == nil || *status.ExitCode != 0 || status.Status == StatusFailed {
		detail := strings.TrimSpace(status.Error)
		if detail == "" && status.ExitCode != nil {
			detail = fmt.Sprintf("exit code %d", *status.ExitCode)
		}
		if detail == "" {
			detail = "missing successful exit status"
		}
		return fmt.Errorf("install agent %q failed: %s", agent.ID, detail)
	}
	installedCommands[installKey] = struct{}{}
	return nil
}

func (i CommandInstaller) run(ctx context.Context, agent config.Agent, workdir string, env map[string]string, installCommand []string) (Terminal, error) {
	units := i.Units
	if units == nil {
		units = SystemdRunner{}
	}
	runtimeDir := strings.TrimSpace(i.RuntimeDir)
	if runtimeDir == "" {
		runtimeDir = filepath.Join("/run/discobox/agent-terminals", "installs")
	}
	logDir := strings.TrimSpace(i.LogDir)
	if logDir == "" {
		logDir = filepath.Join(filepath.Dir(runtimeDir), "logs")
	}
	timeout := i.StatusTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	pollInterval := i.PollInterval
	if pollInterval <= 0 {
		pollInterval = 100 * time.Millisecond
	}
	startFromSocket := i.startFromSocket
	if startFromSocket == nil {
		startFromSocket = shimproxy.StartJSON[Terminal]
	}
	socketStatus := i.statusFromSocket
	if socketStatus == nil {
		socketStatus = statusFromSocket
	}

	id, err := newID()
	if err != nil {
		return Terminal{}, err
	}
	id = "install_" + strings.TrimPrefix(id, "agt_")
	unit := "discobox-agent-install-" + safeName(id)
	socketPath := filepath.Join(runtimeDir, safeName(id)+".sock")
	runtimePath := filepath.Join(runtimeDir, safeName(id)+".json")
	if _, err := units.Start(ctx, StartRequest{
		ID:          id,
		AgentID:     agent.ID,
		Unit:        unit,
		Command:     append([]string{}, installCommand...),
		Workdir:     workdir,
		Env:         cloneMap(env),
		User:        cloneUser(i.User),
		SocketPath:  socketPath,
		RuntimePath: runtimePath,
		LogDir:      logDir,
	}); err != nil {
		return Terminal{}, fmt.Errorf("start install agent %q: %w", agent.ID, err)
	}
	if _, err := waitForStatusFunc(ctx, socketStatus, socketPath, 10*time.Second); err != nil {
		return Terminal{}, fmt.Errorf("wait for install agent %q shim: %w", agent.ID, err)
	}
	if _, err := startFromSocket(ctx, socketPath); err != nil {
		return Terminal{}, fmt.Errorf("start install agent %q command: %w", agent.ID, err)
	}
	status, err := waitForTerminalExit(ctx, socketStatus, socketPath, runtimePath, timeout, pollInterval)
	if err != nil {
		return Terminal{}, fmt.Errorf("wait for install agent %q command: %w", agent.ID, err)
	}
	_ = os.Remove(runtimePath)
	_ = os.Remove(socketPath)
	return status, nil
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

func envWithRuntimeDefaults(env map[string]string, user *execs.User, imageConfig config.ImageConfig) map[string]string {
	if env == nil {
		env = map[string]string{}
	}
	if _, ok := env["TERM"]; !ok {
		env["TERM"] = defaultTerm
	}
	if user != nil {
		if name := strings.TrimSpace(user.Name); name != "" {
			if _, ok := env["USER"]; !ok {
				env["USER"] = name
			}
			if _, ok := env["LOGNAME"]; !ok {
				env["LOGNAME"] = name
			}
		}
		if home := strings.TrimSpace(user.HomeDirectory); home != "" {
			if _, ok := env["HOME"]; !ok {
				env["HOME"] = home
			}
		}
	}
	return config.ApplyImageEnvDefaults(env, imageConfig)
}

func terminalDefaultUser(cfg ManagerConfig) *execs.User {
	if cfg.DefaultUser != nil {
		return cloneUser(cfg.DefaultUser)
	}
	defaults := cfg.ExecDefaults
	if strings.TrimSpace(defaults.Username) == "" && defaults.UID == nil && defaults.GID == nil && strings.TrimSpace(defaults.HomeDirectory) == "" {
		return nil
	}
	return cloneUser(&execs.User{
		Name:          defaults.Username,
		UID:           cloneInt64(defaults.UID),
		GID:           cloneInt64(defaults.GID),
		HomeDirectory: defaults.HomeDirectory,
	})
}

// resolveAgent selects the agent for a terminal in precedence order: an explicit
// request, then the sandbox's resolved agent, then a local repo agent config,
// then the configured default.
func (m *Manager) resolveAgent(requested string, workdir string) (config.Agent, string, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		agent, ok := m.agents[requested]
		if !ok {
			return config.Agent{}, "", fmt.Errorf("agent %q is not configured", requested)
		}
		return agent, requested, nil
	}
	if m.resolvedID != "" {
		if agent, ok := m.agents[m.resolvedID]; ok {
			return agent, m.resolvedID, nil
		}
	}
	if local, ok, err := m.localAgentConfig(workdir); err != nil {
		return config.Agent{}, "", err
	} else if ok {
		return local, local.ID, nil
	}
	if m.defaultID == "" {
		return config.Agent{}, "", errors.New("no agent terminals are configured")
	}
	agent, ok := m.agents[m.defaultID]
	if !ok {
		return config.Agent{}, "", fmt.Errorf("default agent %q is not configured", m.defaultID)
	}
	return agent, m.defaultID, nil
}

type localAgentConfig struct {
	Agent          string    `json:"agent,omitempty"`
	ID             string    `json:"id,omitempty"`
	Name           string    `json:"name,omitempty"`
	InstallCommand *[]string `json:"installCommand,omitempty"`
	Command        *[]string `json:"command,omitempty"`
	RunCommand     *[]string `json:"runCommand,omitempty"`
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
	if agent, ok := m.agents[selector]; ok {
		return cloneAgent(agent), true
	}
	for _, agent := range m.agents {
		if strings.EqualFold(agent.Name, selector) {
			return cloneAgent(agent), true
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
		agent.InstallCommand = append([]string{}, (*local.InstallCommand)...)
	}
	if local.Command != nil {
		agent.Command = append([]string{}, (*local.Command)...)
	}
	if local.RunCommand != nil {
		agent.Command = append([]string{}, (*local.RunCommand)...)
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
		requested = m.defaultWorkdir
	}
	if strings.TrimSpace(requested) == "" {
		return m.workingRoot, nil
	}
	if !filepath.IsAbs(requested) {
		requested = filepath.Join(m.workingRoot, requested)
	}
	return filepath.Clean(requested), nil
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

func cloneUser(in *execs.User) *execs.User {
	if in == nil {
		return nil
	}
	out := *in
	out.Name = strings.TrimSpace(out.Name)
	out.HomeDirectory = strings.TrimSpace(out.HomeDirectory)
	out.UID = cloneInt64(in.UID)
	out.GID = cloneInt64(in.GID)
	return &out
}

func cloneInt64(in *int64) *int64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
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
	out.InstallCommand = append([]string{}, in.InstallCommand...)
	out.RelaunchCommand = append([]string{}, in.RelaunchCommand...)
	out.Files = append([]config.AgentFile{}, in.Files...)
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
	return waitForStatusFunc(ctx, statusFromSocket, socketPath, timeout)
}

func waitForStatusFunc(ctx context.Context, status func(context.Context, string) (Terminal, error), socketPath string, timeout time.Duration) (Terminal, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		current, err := status(ctx, socketPath)
		if err == nil {
			return current, nil
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

func waitForTerminalExit(ctx context.Context, status func(context.Context, string) (Terminal, error), socketPath, runtimePath string, timeout, pollInterval time.Duration) (Terminal, error) {
	deadline := time.Now().Add(timeout)
	var last Terminal
	var lastErr error
	for time.Now().Before(deadline) {
		if current, err := status(ctx, socketPath); err == nil {
			last = current
		} else {
			lastErr = err
			if current, readErr := readRuntime(runtimePath); readErr == nil {
				last = current
			}
		}
		if last.Status == StatusExited || last.Status == StatusFailed {
			return last, nil
		}
		select {
		case <-ctx.Done():
			return Terminal{}, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	if lastErr != nil {
		return Terminal{}, lastErr
	}
	return Terminal{}, fmt.Errorf("timed out waiting for terminal exit")
}

func mergeStatus(base, live Terminal) Terminal {
	live.Unit = base.Unit
	live.SocketPath = base.SocketPath
	live.RuntimePath = base.RuntimePath
	live.Primary = base.Primary
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

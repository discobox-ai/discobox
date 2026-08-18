package execs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/sandbox-agent/nestedbridge"
	"github.com/obot-platform/discobox/sandbox-agent/runuser"
	"github.com/obot-platform/discobox/sandbox-agent/shimproxy"
	"github.com/obot-platform/discobox/sandboxconfig"
	"github.com/obot-platform/discobox/sandboxuser"
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

type Exec struct {
	ID      string   `json:"id"`
	Status  Status   `json:"status"`
	Command []string `json:"command"`
	// StartupCommand, when set, is the command line typed into the shell's PTY
	// immediately after it starts, as if a user had typed it: the actual argv
	// (Command) is the resolved login shell, so the process the caller asked for
	// runs as the shell's foreground job rather than as the exec's own session
	// leader. That is what gives it real job control — Ctrl-Z reaches a child
	// process group with a parent in the same session instead of an orphaned one,
	// so the kernel actually stops it and the shell is left to hand back a
	// prompt. It is reported separately from Command because Command must stay
	// the literal argv actually executed.
	StartupCommand []string          `json:"startupCommand,omitempty"`
	Workdir        string            `json:"workdir"`
	Env            map[string]string `json:"env,omitempty"`
	User           *User             `json:"user,omitempty"`
	TTY            bool              `json:"tty"`
	Unit           string            `json:"unit,omitempty"`
	PID            int64             `json:"pid,omitempty"`
	ExitCode       *int64            `json:"exitCode,omitempty"`
	Error          string            `json:"error,omitempty"`
	CreatedAt      time.Time         `json:"createdAt"`
	StartedAt      *time.Time        `json:"startedAt,omitempty"`
	ExitedAt       *time.Time        `json:"exitedAt,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	SocketPath     string            `json:"socketPath,omitempty"`
	RuntimePath    string            `json:"runtimePath,omitempty"`
	// AttacherCount is the number of clients currently attached to this exec's
	// stream, reported live by the shim on every /status query (see
	// shimRuntime.handleStatus) rather than tracked as persisted state.
	AttacherCount int `json:"attacherCount,omitempty"`
	// Title is the window title the program running in the exec last set
	// (OSC 0/2), read live from the shim's screen emulator like AttacherCount.
	// Empty for a program that never set one and for pipe execs, which have
	// no emulator.
	Title string `json:"title,omitempty"`
	// LastAccessedAt is the last time a client acted on this exec — attached,
	// typed, or is attached right now — reported live by the shim like
	// AttacherCount. Absent when no client ever has, or once the shim is gone.
	LastAccessedAt *time.Time `json:"lastAccessedAt,omitempty"`
}

type CreateRequest struct {
	// ID, when set, is used as the exec ID instead of generating a new one. It
	// lets a caller (such as the terminal layer) correlate the exec with state
	// it prepared before creation, e.g. env baked into the systemd unit. When
	// empty a fresh ID is generated.
	ID      string
	Command []string
	// Shell runs the run user's login shell instead of Command, resolved in the
	// sandbox because only the sandbox knows what that user's shell is. It is
	// mutually exclusive with Command.
	Shell bool
	// StartupCommand types this command line into the shell once it starts,
	// as if the caller had typed it themselves. It requires Shell and is
	// mutually exclusive with Command. See Exec.StartupCommand for why.
	StartupCommand []string
	// ShellCommandLine, when set alongside Shell, runs the resolved login
	// shell with `-lc <ShellCommandLine>` instead of an interactive login
	// shell. It exists for callers outside the sandbox — the SSH ingress's
	// `exec "cmd"` channel type carries one opaque command-line string — that
	// cannot resolve the login shell path themselves (ADR 0024 §2).
	ShellCommandLine string
	// Workdir is the working directory for the exec process. An empty value
	// takes the sandbox's configured default (the primary source directory).
	// A leading `~` or `~/` is expanded against the run user's home directory,
	// which is how a caller outside the sandbox asks for "start where a login
	// shell would" without knowing what that path is.
	Workdir  string
	Env      map[string]string
	User     *User
	TTY      bool
	Rows     uint16
	Cols     uint16
	Metadata map[string]string
}

type UnitManager interface {
	Start(context.Context, StartRequest) (StartResult, error)
	Stop(context.Context, string) error
	Status(context.Context, string) (UnitStatus, error)
	List(context.Context) ([]UnitStatus, error)
}

type StartRequest struct {
	ID             string
	Unit           string
	Command        []string
	StartupCommand []string
	Workdir        string
	Env            map[string]string
	User           *User
	TTY            bool
	Metadata       map[string]string
	SocketPath     string
	RuntimePath    string
	// DatabasePath is the sqlite file the exec-shim process opens its own
	// connection to, to write transcript chunks (see LogSink).
	DatabasePath string
	Rows         uint16
	Cols         uint16
}

type StartResult struct {
	Unit string
	PID  int64
}

type UnitStatus struct {
	Unit string
	// Loaded reports whether the unit manager still has a definition for the
	// unit. A unit that never existed, or a transient one lost to a reboot, is
	// reported unloaded rather than as an error, so callers must consult this to
	// tell a vanished exec from an inactive one.
	Loaded    bool
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
	// SaveExecRecord persists an exec's immutable identity/metadata once at
	// create; LoadExecRecords reads it back (joined with latest status). Together
	// they make the DB the durable source of truth for exec metadata, so a shim
	// runtime write that omits metadata cannot lose harnessId/primary, and execs
	// survive the loss of their tmpfs runtime files across a reboot.
	SaveExecRecord(context.Context, Exec) error
	LoadExecRecords(context.Context) ([]Exec, error)
}

// LogChunk is one compressed batch of an exec's stdin/stdout/stderr
// transcript, as persisted by a LogSink. Data is compressed with Codec; the
// caller is what knows how to marshal/unmarshal the entries inside it (see
// AsyncLogger and ReadExecLog) — the sink itself treats it as opaque bytes.
type LogChunk struct {
	BucketStart time.Time
	Codec       string
	Data        []byte
	RawSize     int
}

// LogSink durably persists exec transcript chunks. It is implemented by
// *store.Store; the interface lives here (not in store) so both the main
// sandbox-agent server process and the exec-shim process — which runs as its
// own OS process and cannot share the server's in-memory *store.Store, see
// ShimConfig.Logs — depend on the same narrow contract without execs
// importing store (store already imports execs, so the reverse would cycle).
type LogSink interface {
	AppendExecLogChunk(ctx context.Context, execID string, bucketStart time.Time, codec string, data []byte, rawSize int) error
	ListExecLogChunks(ctx context.Context, execID string) ([]LogChunk, error)
	DeleteExecLog(ctx context.Context, execID string) error
}

type Manager struct {
	workingRoot    string
	defaultWorkdir string
	defaultUser    *User
	runtimeDir     string
	databasePath   string
	env            map[string]string
	units          UnitManager
	audit          AuditRecorder
	logs           LogSink
}

type ManagerConfig struct {
	WorkingRoot    string
	DefaultWorkdir string
	DefaultUser    *User
	RuntimeDir     string
	// DatabasePath is threaded onto each StartRequest so the systemd runner
	// can pass it to the exec-shim process, which opens its own connection
	// to the same sqlite file to write log chunks (see LogSink).
	DatabasePath string
	Env          map[string]string
	Units        UnitManager
	Audit        AuditRecorder
	Logs         LogSink
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
		workingRoot:    filepath.Clean(workingRoot),
		defaultWorkdir: strings.TrimSpace(cfg.DefaultWorkdir),
		defaultUser:    cfg.DefaultUser.Clone(),
		runtimeDir:     runtimeDir,
		databasePath:   strings.TrimSpace(cfg.DatabasePath),
		env:            cloneMap(cfg.Env),
		units:          units,
		audit:          cfg.Audit,
		logs:           cfg.Logs,
	}, nil
}

// MergeEnv overlays override on base, returning nil when both are empty. It is
// exported so the terminal layer can compute the same effective environment the
// exec will run with (e.g. to pass to the harness installer) before creation.
func MergeEnv(base, override map[string]string) map[string]string {
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

// EnvWithRuntimeDefaults fills TERM and USER/LOGNAME/HOME into env without
// overriding existing entries, and resolves sandboxconfig.LocalSubnetsToken
// (pool-agent cannot know the sandbox's own directly-connected networks, so it
// leaves this placeholder in NO_PROXY for the sandbox side to fill in). Image
// env defaults are no longer applied here: pool-agent's Effective() call
// already merges them into the sandbox's effective Env (ADR 0012 §2), so env
// already carries them. Exported for the terminal layer.
//
// The token is resolved here, at exec time, rather than once when sandbox.json
// is loaded: this is the same reasoning runcca.proxyEnv uses for a nested
// container's env, and it holds just as well for an exec's — the nested-Docker
// bridge and any user-created networks appear only after boot, so an exec
// started later must see them too.
func EnvWithRuntimeDefaults(env map[string]string, user *User) map[string]string {
	if env == nil {
		env = map[string]string{}
	}
	// The image's env may still carry %HOME%: the pool agent expands it only
	// when the request stated a home outright, because the account otherwise
	// lives in the image and only the sandbox can look it up. Expanding it there
	// against a blank would turn "$HOME/.config" into "/.config" -- a real path
	// pointing at the wrong place. It is deferred here for the same reason
	// %LOCAL_SUBNETS% is (ADR 0033 §5).
	home := ""
	if user != nil {
		home = strings.TrimSpace(user.HomeDirectory)
	}
	if home == "" {
		home = strings.TrimSpace(env["HOME"])
	}
	if home == "" {
		// Nobody was named, so the exec inherits this process's identity (ADR
		// 0025 §5) — which is still an identity, and its account still has a
		// home. Saying nothing about *who* to run as does not mean knowing
		// nothing about where home is, and a sandbox the server creates for
		// itself names no user: a configure sandbox reached here with %HOME%
		// unexpanded in PATH and no HOME to install a harness's files into.
		if resolved, err := runuser.Resolve(runuser.Layers{Image: runuser.Current()}, sandboxuser.FieldHome); err == nil {
			home = strings.TrimSpace(resolved.HomeDirectory)
		}
	}
	for key, value := range env {
		value = sandboxconfig.ResolveLocalSubnetsToken(value, nestedbridge.LocalSubnets())
		if home != "" {
			value = strings.ReplaceAll(value, harness.HomeToken, home)
		}
		env[key] = value
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
	}
	// Whatever home resolved to, from any layer: an exec that knows where home
	// is must say so, or every client that reads HOME rather than passwd —
	// which is most of them — disagrees with the workdir this same value
	// expanded `~` against.
	if home != "" {
		if _, ok := env["HOME"]; !ok {
			env["HOME"] = home
		}
	}
	return env
}

func (m *Manager) Create(ctx context.Context, req CreateRequest) (Exec, error) {
	// Resolve before anything reads the identity, so the shell, the env
	// defaults, and the persisted record all describe one fully-known user.
	user, err := m.ResolveUser(req)
	if err != nil {
		return Exec{}, err
	}
	env := EnvWithRuntimeDefaults(MergeEnv(m.env, req.Env), user)
	// The workdir is resolved after the run user and env, because `~` expands
	// against the home directory of the identity the exec actually runs as.
	workdir, err := m.resolveWorkdir(req.Workdir, HomeDir(user, env))
	if err != nil {
		return Exec{}, err
	}
	// The shell is resolved against the run user and env the exec will actually
	// have, so it is the shell of the identity the process runs as.
	command, err := resolveCommand(req, user, env)
	if err != nil {
		return Exec{}, err
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		var err error
		if id, err = NewID(); err != nil {
			return Exec{}, err
		}
	}
	unit := "discobox-exec-" + id
	socketPath := m.socketPath(id)
	runtimePath := m.runtimePath(id)
	now := time.Now().UTC()
	exec := Exec{
		ID:             id,
		Status:         StatusStarting,
		Command:        command,
		StartupCommand: append([]string{}, req.StartupCommand...),
		Workdir:        workdir,
		Env:            cloneMap(env),
		User:           user.Clone(),
		TTY:            req.TTY,
		Unit:           unit,
		CreatedAt:      now,
		Metadata:       cloneMap(req.Metadata),
		SocketPath:     socketPath,
		RuntimePath:    runtimePath,
	}
	if err := writeRuntime(runtimePath, exec); err != nil {
		return Exec{}, err
	}
	// Persist the immutable identity/metadata durably before the shim starts, so
	// it is never lost to a metadata-less shim runtime write or a reboot.
	_ = m.saveRecord(ctx, exec)
	_ = m.recordEvent(ctx, id, "exec.created", "exec created", map[string]any{
		"unit":    unit,
		"workdir": workdir,
		"command": exec.Command,
		"tty":     req.TTY,
		"user":    user,
	})
	result, err := m.units.Start(ctx, StartRequest{
		ID:             id,
		Unit:           unit,
		Command:        exec.Command,
		StartupCommand: exec.StartupCommand,
		Workdir:        workdir,
		Env:            cloneMap(env),
		User:           user.Clone(),
		TTY:            req.TTY,
		Metadata:       cloneMap(req.Metadata),
		SocketPath:     socketPath,
		RuntimePath:    runtimePath,
		DatabasePath:   m.databasePath,
		Rows:           req.Rows,
		Cols:           req.Cols,
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
	ctx := context.Background()
	records := m.loadRecords(ctx)
	execs := m.runtimeExecs(ctx)
	seen := make(map[string]bool, len(execs))
	if units, err := m.units.List(ctx); err == nil {
		byUnit := map[string]UnitStatus{}
		for _, unit := range units {
			byUnit[unit.Unit] = unit
		}
		for i := range execs {
			if unit, ok := byUnit[execs[i].Unit]; ok {
				execs[i] = applyUnitStatus(execs[i], unit)
			}
			execs[i] = m.refreshExec(ctx, execs[i], true)
		}
	} else {
		for i := range execs {
			execs[i] = m.refreshExec(ctx, execs[i], true)
		}
	}
	for i := range execs {
		seen[execs[i].ID] = true
		if record, ok := records[execs[i].ID]; ok {
			execs[i] = hydrateMetadata(execs[i], record)
		}
	}
	// Surface execs whose tmpfs runtime files are gone (e.g. after a reboot)
	// from their durable records, so history is not lost.
	for id, record := range records {
		if !seen[id] {
			execs = append(execs, m.refreshExec(ctx, m.withRuntimePaths(record), false))
		}
	}
	sort.Slice(execs, func(i, j int) bool {
		return execs[i].CreatedAt.Before(execs[j].CreatedAt)
	})
	return execs
}

func (m *Manager) Reconcile(ctx context.Context) error {
	for _, exec := range m.runtimeExecs(ctx) {
		_ = m.refreshExec(ctx, exec, true)
	}
	return nil
}

func (m *Manager) Get(id string) (Exec, bool) {
	ctx := context.Background()
	records := m.loadRecords(ctx)
	exec, ok := m.readRuntime(id)
	if !ok {
		// The tmpfs runtime file is gone (e.g. after a reboot); fall back to the
		// durable record so metadata and history survive. Reconcile it against
		// the unit manager before returning it: the last durable observation may
		// still say starting/running even though its runtime and unit are gone.
		if record, ok := records[id]; ok {
			return cloneExec(m.refreshExec(ctx, m.withRuntimePaths(record), false)), true
		}
		return Exec{}, false
	}
	exec = m.refreshExec(ctx, exec, true)
	if record, ok := records[id]; ok {
		exec = hydrateMetadata(exec, record)
	}
	return cloneExec(exec), true
}

func (m *Manager) Logs(ctx context.Context, id string) ([]LogEntry, error) {
	if _, ok := m.Get(id); !ok {
		return nil, ErrNotFound
	}
	return ReadExecLog(ctx, m.logs, id)
}

// Delete stops the exec's unit, removes its runtime and socket files, and
// deletes its durable transcript. It is used for long-lived execs (such as
// harness terminals) that outlive a single command and must be explicitly
// torn down.
func (m *Manager) Delete(ctx context.Context, id string) error {
	exec, ok := m.Get(id)
	if !ok {
		return ErrNotFound
	}
	if err := m.units.Stop(ctx, exec.Unit); err != nil {
		return err
	}
	_ = m.recordEvent(ctx, id, "exec.stop.requested", "exec stop requested", map[string]any{"unit": exec.Unit})
	_ = os.Remove(exec.RuntimePath)
	_ = os.Remove(exec.SocketPath)
	if m.logs != nil {
		_ = m.logs.DeleteExecLog(ctx, id)
	}
	_ = m.recordEvent(ctx, id, "exec.deleted", "exec deleted", map[string]any{"unit": exec.Unit})
	return nil
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

// RelaunchRequest carries the per-run inputs for reviving an existing exec
// record under a new transient-unit generation (ADR 0038). Identity fields —
// id, command, workdir, TTY, metadata — come from the record; env and user are
// supplied fresh by the caller because they are not persisted durably and are
// deliberately re-resolved per run (e.g. live secret sentinels, ADR 0012 §3).
type RelaunchRequest struct {
	ID             string
	Env            map[string]string
	User           *User
	StartupCommand []string
	Rows           uint16
	Cols           uint16
}

// Relaunch revives an ended exec record in place: same exec id, socket, and
// runtime paths, a fresh transient unit generation. The exec id is the durable
// identity a terminal keeps across runs (ADR 0038); Relaunch itself stays a
// generic exec primitive — what command a revived terminal resumes with is the
// terminal layer's business, carried here as StartupCommand like any other.
// A record that is still starting or running is returned untouched.
func (m *Manager) Relaunch(ctx context.Context, req RelaunchRequest) (Exec, error) {
	exec, ok := m.Get(req.ID)
	if !ok {
		return Exec{}, ErrNotFound
	}
	switch exec.Status {
	case StatusExited, StatusFailed, StatusLost:
	default:
		return cloneExec(exec), nil
	}
	if len(exec.Command) == 0 {
		return Exec{}, fmt.Errorf("exec %s has no recorded command to relaunch", req.ID)
	}
	user, err := m.ResolveUser(CreateRequest{User: req.User})
	if err != nil {
		return Exec{}, err
	}
	env := EnvWithRuntimeDefaults(MergeEnv(m.env, req.Env), user)
	// Fence the previous run: the shim outlives its command to serve replay,
	// and it holds the socket path the new generation must bind. Only an ended
	// run is ever fenced — the status switch above returns live ones untouched.
	if err := m.units.Stop(ctx, exec.Unit); err != nil {
		return Exec{}, err
	}
	_ = os.Remove(exec.SocketPath)
	current := m.withRuntimePaths(exec)
	current.Status = StatusStarting
	current.StartupCommand = append([]string{}, req.StartupCommand...)
	current.Env = cloneMap(env)
	current.User = user.Clone()
	current.Unit = nextUnitGeneration(exec.ID, exec.Unit)
	current.PID = 0
	current.ExitCode = nil
	current.Error = ""
	current.StartedAt = nil
	current.ExitedAt = nil
	current.AttacherCount = 0
	current.Title = ""
	current.LastAccessedAt = nil
	if err := writeRuntime(current.RuntimePath, current); err != nil {
		return Exec{}, err
	}
	_ = m.observe(ctx, current)
	_ = m.recordEvent(ctx, current.ID, "exec.relaunched", "exec relaunched", map[string]any{
		"unit":    current.Unit,
		"command": current.Command,
	})
	result, err := m.units.Start(ctx, StartRequest{
		ID:             current.ID,
		Unit:           current.Unit,
		Command:        current.Command,
		StartupCommand: current.StartupCommand,
		Workdir:        current.Workdir,
		Env:            cloneMap(env),
		User:           user.Clone(),
		TTY:            current.TTY,
		Metadata:       cloneMap(current.Metadata),
		SocketPath:     current.SocketPath,
		RuntimePath:    current.RuntimePath,
		DatabasePath:   m.databasePath,
		Rows:           req.Rows,
		Cols:           req.Cols,
	})
	if err != nil {
		current.Status = StatusFailed
		current.Error = err.Error()
		exitedAt := time.Now().UTC()
		current.ExitedAt = &exitedAt
		_ = writeRuntime(current.RuntimePath, current)
		_ = m.observe(ctx, current)
		_ = m.recordEvent(ctx, current.ID, "exec.start.failed", "exec start failed", map[string]any{"error": err.Error()})
		return current, err
	}
	if result.Unit != "" {
		current.Unit = result.Unit
	}
	_ = writeRuntime(current.RuntimePath, current)
	_ = m.observe(ctx, current)
	_ = m.recordEvent(ctx, current.ID, "exec.prepared", "exec prepared", map[string]any{"unit": current.Unit})
	return cloneExec(current), nil
}

// nextUnitGeneration names the transient unit for an exec's next run. Every
// run gets its own unit (ADR 0038 §2): systemd retains transient-unit state
// after exit, so reusing a name would need a reset-failed dance and would make
// the audit trail's unit references ambiguous across runs. The first run is
// the bare "discobox-exec-<id>" (see Create); revives append -g2, -g3, …,
// which the "discobox-exec-*" listing glob still matches.
func nextUnitGeneration(id, current string) string {
	base := "discobox-exec-" + id
	generation := 2
	if suffix, ok := strings.CutPrefix(current, base+"-g"); ok {
		if n, err := strconv.Atoi(suffix); err == nil && n >= generation {
			generation = n + 1
		}
	}
	return base + "-g" + strconv.Itoa(generation)
}

func (m *Manager) Attach(ctx context.Context, w http.ResponseWriter, r *http.Request, id string, replay bool) error {
	exec, ok := m.Get(id)
	if !ok {
		return ErrNotFound
	}
	if err := checkAttachable(exec); err != nil {
		return err
	}
	_ = m.recordEvent(ctx, id, "exec.attach.opened", "exec attach opened", map[string]any{"unit": exec.Unit})
	defer func() {
		_ = m.recordEvent(context.Background(), id, "exec.attach.closed", "exec attach closed", map[string]any{"unit": exec.Unit})
	}()
	return shimproxy.AttachWebSocket(ctx, w, r, exec.SocketPath, "discobox-sandbox-exec", replay)
}

// ConnectOneShot opens a one-shot attach to an exec, for callers that want plain
// request/response rather than a duplex stream (a `cat` in, a `cat` out). The
// caller must connect before starting the exec — a fast command's output is
// broadcast at exit and an unconnected attach misses it — then call Run, and
// Close when done. The exit status is left to the exec record, which is
// authoritative.
func (m *Manager) ConnectOneShot(ctx context.Context, id string) (*shimproxy.OneShot, error) {
	exec, ok := m.Get(id)
	if !ok {
		return nil, ErrNotFound
	}
	if err := checkAttachable(exec); err != nil {
		return nil, err
	}
	oneShot, err := shimproxy.ConnectOneShot(ctx, exec.SocketPath, "discobox-sandbox-exec")
	if err != nil {
		return nil, err
	}
	_ = m.recordEvent(ctx, id, "exec.attach.opened", "exec one-shot attach opened", map[string]any{"unit": exec.Unit})
	return oneShot, nil
}

var ErrNotFound = errors.New("sandbox exec not found")

// ErrSessionGone reports an attach to an exec that has ended and whose shim is
// no longer listening, so there is neither a live command to talk to nor
// buffered output to replay. It is distinct from ErrNotFound: the exec record
// exists, it is just unattachable, and callers render it as a terminal
// condition rather than a transient failure to retry.
var ErrSessionGone = errors.New("sandbox exec session is gone")

// checkAttachable rejects an attach that cannot possibly connect. The shim
// outlives its command so late attachers can replay output, so a finished exec
// is still attachable while its socket is there; once the socket is gone the
// dial would only burn its retry timeout and surface a bare ENOENT.
func checkAttachable(exec Exec) error {
	switch exec.Status {
	case StatusExited, StatusFailed, StatusLost:
	default:
		// A created-but-unstarted exec has no socket yet; the dial waits for it.
		return nil
	}
	if _, err := os.Stat(exec.SocketPath); err == nil {
		return nil
	}
	return ErrSessionGone
}

// WaitForExit polls an exec until it reaches a terminal status (exited or
// failed) or the timeout elapses. It is used to run ephemeral execs, such as
// terminal setup operations, to completion.
func (m *Manager) WaitForExit(ctx context.Context, id string, timeout, poll time.Duration) (Exec, error) {
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	for {
		exec, ok := m.Get(id)
		if !ok {
			return Exec{}, ErrNotFound
		}
		if exec.Status == StatusExited || exec.Status == StatusFailed {
			return exec, nil
		}
		if !time.Now().Before(deadline) {
			return exec, errors.New("timed out waiting for exec exit")
		}
		select {
		case <-ctx.Done():
			return Exec{}, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// ResolveWorkdir resolves a requested workdir against the manager's working
// root and default, expanding a leading `~` against home. It is exported so
// the terminal layer resolves workdirs identically before harness resolution
// and install; home comes from HomeDir.
func (m *Manager) ResolveWorkdir(requested, home string) (string, error) {
	return m.resolveWorkdir(requested, home)
}

func (m *Manager) resolveWorkdir(requested, home string) (string, error) {
	if strings.TrimSpace(requested) == "" {
		requested = m.defaultWorkdir
	}
	if strings.TrimSpace(requested) == "" {
		return m.workingRoot, nil
	}
	requested, err := expandHome(requested, home)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(requested) {
		requested = filepath.Join(m.workingRoot, requested)
	}
	return filepath.Clean(requested), nil
}

// expandHome expands a leading `~` or `~/` against home, leaving every other
// path untouched. A `~` that cannot be expanded is an error rather than a
// silent fallback: the caller asked for the home directory specifically, and
// quietly running somewhere else instead is the kind of difference that only
// shows up later as files written to the wrong place.
func expandHome(path, home string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	if strings.TrimSpace(home) == "" {
		return "", errors.New("cannot expand ~ in workdir: the exec user has no home directory")
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}

// HomeDir resolves the run user's home directory: the explicit or passwd value
// runuser.NameAndHome yields, falling back to env HOME for a user that has no passwd
// entry (a bare UID) but whose environment still names a home. It is the input
// to ResolveWorkdir's `~` expansion, shared so the exec and terminal layers
// cannot disagree about where home is.
func HomeDir(user *User, env map[string]string) string {
	if user != nil && strings.TrimSpace(user.HomeDirectory) != "" {
		return strings.TrimSpace(user.HomeDirectory)
	}
	return strings.TrimSpace(env["HOME"])
}

// resolveCommand yields the argv the exec runs: the requested command, or the
// run user's login shell when the request asks for a shell rather than naming
// one. The resolved argv is what the exec record reports, so a shell exec is
// self-describing after the fact. StartupCommand never changes this: it rides
// along with the shell and is typed into it after start, not exec'd itself.
func resolveCommand(req CreateRequest, user *User, env map[string]string) ([]string, error) {
	if len(req.StartupCommand) > 0 && !req.Shell {
		return nil, errors.New("exec startup command requires shell")
	}
	if req.Shell {
		if len(req.Command) > 0 {
			return nil, errors.New("exec shell and command are mutually exclusive")
		}
		if req.ShellCommandLine != "" {
			shell, err := ResolveShell(user, env)
			if err != nil {
				return nil, err
			}
			return []string{shell, "-lc", req.ShellCommandLine}, nil
		}
		return ShellCommand(user, env)
	}
	if len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "" {
		return nil, errors.New("exec command is required")
	}
	return append([]string{}, req.Command...), nil
}

// ResolveUser answers "who would an exec created from this request run as," and
// is the only way to ask. The layers are the sandbox's own image identity, the
// manifest's declared user, and this request's override; precedence and
// completion both belong to runuser, so this method supplies inputs and does no
// merging of its own (ADR 0033 §1).
//
// The manager owns this because it owns the exec primitive. Layers built on top
// (terminal) ask rather than reconstruct: a second construction of the same
// identity drifts, which is how terminals came to run without the manifest's
// supplementary groups while plain execs kept them. Pass the zero CreateRequest
// for the sandbox's default identity.
//
// An exec needs the whole identity, not just a credential: its environment
// carries USER, LOGNAME and HOME, and `~` in a workdir expands against the home
// of whoever it actually runs as.
func (m *Manager) ResolveUser(req CreateRequest) (*User, error) {
	layers := m.layers(req.User)
	if !sandboxuser.Named(layers.Manifest) && !sandboxuser.Named(layers.Request) {
		// Nobody asked for anybody. The exec inherits this process's identity
		// wholesale, which a nil user is how we express: the launch path sets no
		// Credential at all and the child simply keeps what it was given (ADR
		// 0025 §5). Resolving the image layer here instead would produce a
		// Credential that says the same thing, but only if the image's account
		// happens to have a passwd entry -- and saying nothing cannot fail.
		return nil, nil
	}
	resolved, err := runuser.Resolve(layers, sandboxuser.Complete)
	if err != nil {
		return nil, err
	}
	return &resolved, nil
}

// layers assembles what this sandbox knows about who to run as. The image layer
// is this process's own identity: the agent's unit sets no User=, so the ids it
// runs under are the ones the image's USER directive selected.
func (m *Manager) layers(request *User) runuser.Layers {
	return runuser.Layers{
		Image:    runuser.Current(),
		Manifest: m.defaultUser,
		Request:  request,
	}
}

// DefaultUser returns the sandbox's resolved default user — the identity
// execs and terminals run as when a CreateRequest doesn't specify its own.
// Callers outside this package that need to act as the same identity (e.g.
// sandbox-agent's status endpoint reading sources that identity owns) should
// use this instead of re-deriving it from raw config.
func (m *Manager) DefaultUser() *User {
	return m.defaultUser.Clone()
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
		exec = m.withRuntimePaths(exec)
		out = append(out, m.refreshExec(ctx, exec, true))
	}
	return out
}

func (m *Manager) readRuntime(id string) (Exec, bool) {
	exec, err := readRuntime(m.runtimePath(id))
	if err != nil || exec.ID == "" {
		return Exec{}, false
	}
	return m.withRuntimePaths(exec), true
}

// withRuntimePaths restores manager-owned runtime locations that are not kept
// in the durable exec record. Any Exec returned by Manager must carry these
// paths because start, attach, delete, and status refresh all depend on them.
func (m *Manager) withRuntimePaths(exec Exec) Exec {
	if exec.ID == "" {
		return exec
	}
	if exec.RuntimePath == "" {
		exec.RuntimePath = m.runtimePath(exec.ID)
	}
	if exec.SocketPath == "" {
		exec.SocketPath = m.socketPath(exec.ID)
	}
	return exec
}

func (m *Manager) refreshExec(ctx context.Context, exec Exec, runtimePresent bool) Exec {
	if exec.Status == StatusExited || exec.Status == StatusFailed {
		_ = m.observe(ctx, exec)
		return exec
	}
	// A created-but-not-yet-started exec (e.g. a terminal whose harness install
	// command is still running before Start) legitimately has no live unit yet, so
	// keep it starting rather than declaring it lost while it waits to launch.
	notYetLaunched := runtimePresent && exec.Status == StatusStarting && exec.StartedAt == nil
	if unit, err := m.units.Status(ctx, exec.Unit); err == nil {
		// The unit is gone, so the shim that owned this exec cannot come back and
		// neither can the exec — an exec whose transient unit did not survive a
		// reboot lands here. Say so rather than letting applyUnitStatus report the
		// unknown unit's "inactive" as an ordinary exit, or pin a never-started
		// exec at starting forever: a phantom starting primary terminal makes
		// EnsurePrimary skip the relaunch and every attach dial a socket that will
		// never exist.
		if !unit.Loaded && !notYetLaunched {
			exec.Status = StatusLost
			exec.Error = "exec unit is no longer loaded"
			if exec.ExitedAt == nil {
				exitedAt := time.Now().UTC()
				exec.ExitedAt = &exitedAt
			}
		} else {
			exec = applyUnitStatus(exec, unit)
		}
	} else if exec.ExitedAt == nil && !notYetLaunched {
		exec.Status = StatusLost
		exec.Error = "exec unit status is unavailable"
	}
	// The unit's main process is the shim, which deliberately outlives the
	// command (it lingers so a late attacher can replay output and read the exit
	// code). A live unit therefore does not mean a running command, and writing
	// the unit-derived "running" below would stomp the exit status the shim
	// records in this same file. While the shim is reachable it is the authority:
	// overlay its status so a finished command reads exited here and the
	// early-return above then pins it.
	if exec.Status == StatusStarting || exec.Status == StatusRunning {
		// The stat gate keeps refresh cheap on hot paths: no socket file means no
		// shim to ask (not yet listening, or already torn down), and the probe's
		// dial retry would otherwise charge its full timeout to every list/get.
		if _, err := os.Stat(exec.SocketPath); err == nil {
			if status, err := shimproxy.StatusJSON[Exec](ctx, exec.SocketPath); err == nil {
				exec = mergeExecStatus(exec, status)
			}
		}
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
	base.AttacherCount = status.AttacherCount
	base.Title = status.Title
	base.LastAccessedAt = status.LastAccessedAt
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

func (m *Manager) saveRecord(ctx context.Context, exec Exec) error {
	if m.audit == nil {
		return nil
	}
	return m.audit.SaveExecRecord(ctx, exec)
}

// loadRecords returns the durable exec records keyed by ID, best-effort. It is
// the authoritative source for exec metadata on read.
func (m *Manager) loadRecords(ctx context.Context) map[string]Exec {
	if m.audit == nil {
		return nil
	}
	records, err := m.audit.LoadExecRecords(ctx)
	if err != nil {
		return nil
	}
	out := make(map[string]Exec, len(records))
	for _, record := range records {
		out[record.ID] = record
	}
	return out
}

// hydrateMetadata restores an exec's metadata from its durable record when a
// shim runtime write dropped it. The record is immutable, so it is safe to
// treat as authoritative.
func hydrateMetadata(exec, record Exec) Exec {
	if len(exec.Metadata) == 0 && len(record.Metadata) > 0 {
		exec.Metadata = cloneMap(record.Metadata)
	}
	return exec
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

// NewID mints an exec ID. Exported so the terminal layer can pre-generate an ID
// and correlate installed state with the exec before creating it.
func NewID() (string, error) {
	return id.New(id.PrefixExec)
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

func cloneExec(in Exec) Exec {
	out := in
	out.Command = append([]string{}, in.Command...)
	out.StartupCommand = append([]string{}, in.StartupCommand...)
	out.Env = cloneMap(in.Env)
	out.Metadata = cloneMap(in.Metadata)
	out.SocketPath = in.SocketPath
	out.User = in.User.Clone()
	return out
}

// User is the run identity an exec launches under. It is runuser.User: one
// type, so the exec record, the boot flow, and anything new resolve identity
// through the same rules rather than each keeping their own.
type User = runuser.User

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

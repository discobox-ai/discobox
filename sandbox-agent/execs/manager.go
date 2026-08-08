package execs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/sandbox-agent/runuser"
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

type Exec struct {
	ID          string            `json:"id"`
	Status      Status            `json:"status"`
	Command     []string          `json:"command"`
	Workdir     string            `json:"workdir"`
	Env         map[string]string `json:"env,omitempty"`
	User        *User             `json:"user,omitempty"`
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
	// ID, when set, is used as the exec ID instead of generating a new one. It
	// lets a caller (such as the terminal layer) correlate the exec with state
	// it prepared before creation, e.g. env baked into the systemd unit. When
	// empty a fresh ID is generated.
	ID      string
	Command []string
	// Shell runs the run user's login shell instead of Command, resolved in the
	// sandbox because only the sandbox knows what that user's shell is. It is
	// mutually exclusive with Command.
	Shell    bool
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
	ID          string
	Unit        string
	Command     []string
	Workdir     string
	Env         map[string]string
	User        *User
	TTY         bool
	Metadata    map[string]string
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

type Manager struct {
	workingRoot    string
	defaultWorkdir string
	defaultUser    *User
	runtimeDir     string
	logDir         string
	env            map[string]string
	units          UnitManager
	audit          AuditRecorder
}

type ManagerConfig struct {
	WorkingRoot    string
	DefaultWorkdir string
	DefaultUser    *User
	RuntimeDir     string
	Env            map[string]string
	Units          UnitManager
	Audit          AuditRecorder
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
		logDir:         filepath.Join(runtimeDir, "logs"),
		env:            cloneMap(cfg.Env),
		units:          units,
		audit:          cfg.Audit,
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
// overriding existing entries. Image env defaults are no longer applied here:
// pool-agent's Effective() call already merges them into the sandbox's
// effective Env (ADR 0012 §2), so env already carries them. Exported for the
// terminal layer.
func EnvWithRuntimeDefaults(env map[string]string, user *User) map[string]string {
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
	return env
}

func (m *Manager) Create(ctx context.Context, req CreateRequest) (Exec, error) {
	workdir, err := m.resolveWorkdir(req.Workdir)
	if err != nil {
		return Exec{}, err
	}
	// Resolve before anything reads the identity, so the shell, the env
	// defaults, and the persisted record all describe one fully-known user.
	user, err := m.ResolveUser(req)
	if err != nil {
		return Exec{}, err
	}
	env := EnvWithRuntimeDefaults(MergeEnv(m.env, req.Env), user)
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
		ID:          id,
		Status:      StatusStarting,
		Command:     command,
		Workdir:     workdir,
		Env:         cloneMap(env),
		User:        user.Clone(),
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
		ID:          id,
		Unit:        unit,
		Command:     exec.Command,
		Workdir:     workdir,
		Env:         cloneMap(env),
		User:        user.Clone(),
		TTY:         req.TTY,
		Metadata:    cloneMap(req.Metadata),
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
	return ReadLogs(ctx, m.logDir, id)
}

// Delete stops the exec's unit and removes its runtime and socket files. It is
// used for long-lived execs (such as harness terminals) that outlive a single
// command and must be explicitly torn down.
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
// root and default, exported so the terminal layer resolves workdirs
// identically before harness resolution and install.
func (m *Manager) ResolveWorkdir(requested string) (string, error) {
	return m.resolveWorkdir(requested)
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

// resolveCommand yields the argv the exec runs: the requested command, or the
// run user's login shell when the request asks for a shell rather than naming
// one. The resolved argv is what the exec record reports, so a shell exec is
// self-describing after the fact.
func resolveCommand(req CreateRequest, user *User, env map[string]string) ([]string, error) {
	if req.Shell {
		if len(req.Command) > 0 {
			return nil, errors.New("exec shell and command are mutually exclusive")
		}
		return ShellCommand(user, env)
	}
	if len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "" {
		return nil, errors.New("exec command is required")
	}
	return append([]string{}, req.Command...), nil
}

// ResolveUser answers "who would an exec created from this request run as," and
// is the only way to ask. It returns the identity fully resolved: the request's
// where it gave one and the manifest's where it did not, groups per ADR 0025 §2,
// and every id filled in from passwd per §6 -- so the result never carries a
// name to look up, a nil gid, or an unresolved group.
//
// The manager owns this because it owns the exec primitive. Layers built on top
// (terminal) ask rather than reconstruct: a second construction of the same
// identity drifts, which is how terminals came to run without the manifest's
// supplementary groups while plain execs kept them. Pass the zero CreateRequest
// for the sandbox's default identity.
func (m *Manager) ResolveUser(req CreateRequest) (*User, error) {
	user := m.resolveUser(req)
	if user == nil {
		return nil, nil
	}
	resolved, err := runuser.Resolve(*user)
	if err != nil {
		return nil, err
	}
	return &resolved, nil
}

// resolveUser picks the identity an exec runs as, without inventing any part of
// it. In particular a nil GID stays nil rather than being back-filled from the
// UID: UIDs and GIDs are separate namespaces, so uid==gid is a coincidence of
// common useradd defaults, and the primary group is looked up from the uid at
// launch (userCredential). Back-filling here silently ran the process under
// whatever group happened to hold that number and made that lookup unreachable.
//
// Supplementary groups are all-or-nothing, never merged: a request that names
// none inherits the sandbox manifest's, and a request that names any uses
// exactly those. Merging would make the manifest a floor the caller could not
// get under, so an exec could never run with fewer groups than the sandbox.
// Either way /etc/passwd and /etc/group are consulted only at launch, to resolve
// those names to ids and drop any the image never created (resolveGroups).
//
// Identity and membership are separate choices: naming a user does not by itself
// touch groups. Without this, `exec --user dev` ran with an empty supplementary
// set while the identical default-user exec kept "docker".
func (m *Manager) resolveUser(req CreateRequest) *User {
	// Groups are read off the request before the identity fallback, so asking
	// for groups alone ("the usual user, plus these") keeps them: emptyUser
	// deliberately ignores AdditionalGroups, since a request carrying only
	// groups still names no one to run as.
	var groups []string
	if req.User != nil {
		groups = append([]string(nil), req.User.AdditionalGroups...)
	}
	user := req.User
	if user.Empty() {
		user = m.defaultUser
	}
	resolved := user.Clone()
	if resolved == nil {
		return nil
	}
	if len(groups) == 0 {
		groups = m.manifestGroups()
	}
	resolved.AdditionalGroups = groups
	return resolved
}

// manifestGroups returns the supplementary groups the sandbox manifest declared,
// which an exec runs with unless its request named groups of its own.
func (m *Manager) manifestGroups() []string {
	if m.defaultUser == nil {
		return nil
	}
	return append([]string(nil), m.defaultUser.AdditionalGroups...)
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

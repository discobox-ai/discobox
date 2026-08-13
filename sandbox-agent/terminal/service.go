// Package terminal is the harness-terminal layer built on top of the exec
// primitive. A terminal is an exec whose command is resolved from a harness
// config, whose environment is prepared by an installer before start, and which
// is tagged (harnessId, primary) in exec metadata. The Service owns harness
// resolution, install, and primary-terminal lifecycle; all runtime mechanics
// (systemd unit, shim, PTY, attach, status) belong to execs.Manager.
package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"

	"github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/harness/registry"
	"github.com/obot-platform/discobox/sandbox-agent/config"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
	"github.com/obot-platform/discobox/sandbox-agent/runuser"
	"github.com/obot-platform/discobox/sandboxuser"
)

// ErrNotFound is returned when a terminal (exec) is not found. It aliases the
// exec primitive's error so callers can match either.
var ErrNotFound = execs.ErrNotFound

type CreateRequest struct {
	HarnessID string
	Args      []string
	Workdir   string
	Env       map[string]string
	Metadata  map[string]string
	Rows      uint16
	Cols      uint16

	// primary and command are set only by the sandbox-agent's own primary
	// terminal launch, never from the terminal create API. command, when set,
	// replaces the resolved harness command and args entirely (used for the
	// relaunch/resume command on subsequent sandbox starts).
	primary bool
	command []string
}

type Installer interface {
	EnsureInstalled(context.Context, config.Harness, string, map[string]string) error
}

// PrimaryStateStore records, durably across sandbox restarts, whether the
// primary terminal has already been launched. It lets EnsurePrimary decide
// between the initial prompt and the relaunch (resume) command.
type PrimaryStateStore interface {
	PrimaryTerminalLaunched(context.Context) (bool, error)
	MarkPrimaryTerminalLaunched(context.Context) error
}

type ServiceConfig struct {
	// Execs is the shared runtime primitive that backs both plain execs and
	// terminals. The service creates terminal-mode execs on it and never owns a
	// runtime of its own.
	Execs *execs.Manager
	// Harness is the sandbox's one fully-resolved harness (ADR 0012 §9): already
	// merged from the image and project layers by pool-agent's Effective() call.
	// A zero-value Harness (empty ID) means the sandbox has no harness at all,
	// which resolves to the shell fallback.
	Harness       config.Harness
	SandboxConfig map[string]any
	WorkingRoot   string
	RuntimeDir    string
	Env           map[string]string
	// SecretEnv returns the sandbox's current secret-bound
	// envName->sentinel map, read live from the secrets file (ADR 0012 §3)
	// rather than baked into Env. Called fresh at every exec. May be nil.
	SecretEnv    func() map[string]string
	ExecDefaults config.ExecDefaults
	Units        execs.UnitManager
	Installer    Installer
	PrimaryState PrimaryStateStore
	HarnessMode  string
	// Prompt is the sandbox's initial prompt, retained so the primary terminal
	// can be relaunched on demand (it is ignored once the primary has launched
	// once and relaunch uses the harness's relaunch command instead).
	Prompt []string
}

// Service is the harness-terminal layer over execs.Manager.
type Service struct {
	execs          *execs.Manager
	harness        config.Harness
	env            map[string]string
	secretEnv      func() map[string]string
	defaultUser    *execs.User
	hookSocketPath string
	installer      Installer
	primaryState   PrimaryStateStore
	harnessMode    string
	bootPrompt     []string

	// installing tracks exec IDs whose hook and file setup is still running.
	// The record exists (execs status "starting") before its process launches, so
	// the mapper projects these as the "installing" phase to callers. Terminal is
	// an exec plus this layer, so install belongs here rather than in execs.
	installingMu sync.Mutex
	installing   map[string]struct{}

	// launchMu guards launch, the single in-flight primary-terminal launch.
	// See ensurePrimary for why the launch is single-flighted rather than
	// merely locked (ADR 0039).
	launchMu sync.Mutex
	launch   *primaryLaunch
}

// primaryLaunch is one in-flight attempt to bring the primary terminal up.
// Callers that find one already running join it and take its result instead of
// starting a second launch.
type primaryLaunch struct {
	done chan struct{}
	exec execs.Exec
	err  error
}

func NewService(cfg ServiceConfig) (*Service, error) {
	if strings.TrimSpace(cfg.WorkingRoot) == "" {
		return nil, errors.New("working root is required")
	}
	if cfg.Execs == nil {
		return nil, errors.New("shared exec manager is required")
	}
	// A terminal is an exec, so it runs as the exec primitive's default user,
	// fully resolved -- manifest groups included, ids filled in from passwd.
	// Deriving it here instead would be a second, silently divergent
	// construction of the same identity; that divergence is what dropped every
	// terminal's supplementary groups (e.g. "docker") while plain execs kept
	// them, and what left the shell and HOME resolved against a half-known user.
	defaultUser, err := cfg.Execs.ResolveUser(execs.CreateRequest{})
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox user: %w", err)
	}

	s := &Service{
		execs:        cfg.Execs,
		harness:      cloneHarness(cfg.Harness),
		env:          cloneMap(cfg.Env),
		secretEnv:    cfg.SecretEnv,
		defaultUser:  defaultUser,
		installer:    cfg.Installer,
		primaryState: cfg.PrimaryState,
		harnessMode:  strings.TrimSpace(cfg.HarnessMode),
		bootPrompt:   append([]string(nil), cfg.Prompt...),
		installing:   map[string]struct{}{},
	}
	if s.installer == nil {
		s.installer = CompositeInstaller{Installers: []Installer{
			HookInstaller{},
			FileInstaller{
				Name:          cfg.ExecDefaults.Username,
				HomeDirectory: cfg.ExecDefaults.HomeDirectory,
				UID:           cfg.ExecDefaults.UID,
				GID:           cfg.ExecDefaults.GID,
				SandboxConfig: cfg.SandboxConfig,
			},
		}}
	}
	return s, nil
}

func (s *Service) SetHookSocketPath(path string) {
	s.hookSocketPath = strings.TrimSpace(path)
}

// Delegations to the underlying exec primitive.

func (s *Service) List() []execs.Exec                  { return s.execs.List() }
func (s *Service) Get(id string) (execs.Exec, bool)    { return s.execs.Get(id) }
func (s *Service) Reconcile(ctx context.Context) error { return s.execs.Reconcile(ctx) }
func (s *Service) Start(ctx context.Context, id string) (execs.Exec, error) {
	return s.execs.Start(ctx, id)
}
func (s *Service) Delete(ctx context.Context, id string) error { return s.execs.Delete(ctx, id) }
func (s *Service) Logs(ctx context.Context, id string) ([]execs.LogEntry, error) {
	return s.execs.Logs(ctx, id)
}

// Create resolves the harness, creates a terminal-mode exec (an always-TTY exec
// running the resolved harness command, tagged harnessId and optionally primary in
// metadata), then prepares its hooks and files. The exec record is created
// before setup so the work is observable: while EnsureInstalled runs, the
// record exists and the mapper projects it as the "installing" phase. The unit
// is only launched later by Start, so the record sits idle during install.
func (s *Service) Create(ctx context.Context, req CreateRequest) (execs.Exec, error) {
	harness, harnessID, err := s.resolveHarness(req.HarnessID)
	if err != nil {
		return execs.Exec{}, err
	}
	id, err := execs.NewID()
	if err != nil {
		return execs.Exec{}, err
	}
	base := s.env
	if s.secretEnv != nil {
		// Read fresh at every exec: the secrets file is refreshed
		// independently of sandbox.json (grant approval, rotation, OAuth
		// refresh), so a stale in-memory copy would miss updates (ADR 0012 §3).
		base = execs.MergeEnv(base, s.secretEnv())
	}
	env := execs.EnvWithRuntimeDefaults(execs.MergeEnv(base, req.Env), s.defaultUser)
	// Resolved after env for the same reason the exec layer does it: `~`
	// expands against the run user's home directory.
	workdir, err := s.execs.ResolveWorkdir(req.Workdir, execs.HomeDir(s.defaultUser, env))
	if err != nil {
		return execs.Exec{}, err
	}
	env["DISCOBOX_TERMINAL_ID"] = id
	if s.hookSocketPath != "" {
		env["DISCOBOX_HOOK_SOCKET"] = s.hookSocketPath
	}
	var command []string
	if len(req.command) > 0 {
		command = append([]string{}, req.command...)
	} else {
		command = append([]string{}, harness.Command...)
		command = append(command, req.Args...)
	}
	metadata := cloneMap(req.Metadata)
	if metadata == nil {
		metadata = map[string]string{}
	}
	metadata[metadataHarnessID] = harnessID
	if req.primary {
		metadata[metadataPrimary] = "true"
	}
	execReq := execs.CreateRequest{
		ID:       id,
		Shell:    true,
		Workdir:  workdir,
		Env:      env,
		User:     cloneUser(s.defaultUser),
		TTY:      true,
		Rows:     req.Rows,
		Cols:     req.Cols,
		Metadata: metadata,
	}
	// Every terminal runs as the resolved login shell so its command is the
	// shell's own foreground job rather than the exec's session leader: a
	// session leader's process group is orphaned (Setsid) and the kernel
	// discards SIGTSTP sent to it, so Ctrl-Z typed at a harness would do
	// nothing. A child of the shell is not orphaned, so Ctrl-Z stops it and
	// the shell is left to hand back a prompt, same as any local terminal. The
	// shell fallback harness IS that shell, so it needs no command typed in.
	if harnessID != ShellHarnessID {
		execReq.StartupCommand = command
	}
	created, err := s.execs.Create(ctx, execReq)
	if err != nil {
		return execs.Exec{}, err
	}
	// Mark installing so the record surfaces as the "installing" phase while the
	// hook/file setup runs. On failure the record is removed so no partially
	// prepared terminal lingers.
	s.markInstalling(created.ID)
	defer s.unmarkInstalling(created.ID)
	if err := s.installer.EnsureInstalled(ctx, harness, workdir, env); err != nil {
		_ = s.execs.Delete(context.WithoutCancel(ctx), created.ID)
		return execs.Exec{}, err
	}
	return created, nil
}

// Revive relaunches a dead terminal-mode exec in place under its own id: a
// terminal's exec id is its durable identity (ADR 0038), so attaching to or
// starting an ended terminal resumes it rather than addressing a dead record.
// The harness's relaunch command runs as a fresh login shell's typed-in job
// (ADR 0027) in a new unit generation, with env and secrets re-resolved
// exactly as Create resolves them. A live terminal is returned untouched;
// non-terminal execs are never revived.
func (s *Service) Revive(ctx context.Context, id string) (execs.Exec, error) {
	exec, ok := s.execs.Get(id)
	if !ok {
		return execs.Exec{}, ErrNotFound
	}
	if HarnessID(exec) == "" {
		return execs.Exec{}, fmt.Errorf("exec %s is not a terminal", id)
	}
	switch exec.Status {
	case execs.StatusExited, execs.StatusFailed, execs.StatusLost:
	default:
		return exec, nil
	}
	harness, harnessID, err := s.resolveHarness(HarnessID(exec))
	if err != nil {
		return execs.Exec{}, err
	}
	base := s.env
	if s.secretEnv != nil {
		// Read fresh at every run, same as Create: sentinels rotate
		// independently of sandbox.json (ADR 0012 §3).
		base = execs.MergeEnv(base, s.secretEnv())
	}
	env := execs.EnvWithRuntimeDefaults(execs.MergeEnv(base, nil), s.defaultUser)
	env["DISCOBOX_TERMINAL_ID"] = id
	if s.hookSocketPath != "" {
		env["DISCOBOX_HOOK_SOCKET"] = s.hookSocketPath
	}
	// Hook and file setup is idempotent and re-ensured per run — the previous
	// run's environment may predate a reboot or a config change.
	s.markInstalling(id)
	defer s.unmarkInstalling(id)
	if err := s.installer.EnsureInstalled(ctx, harness, exec.Workdir, env); err != nil {
		return execs.Exec{}, err
	}
	revived, err := s.execs.Relaunch(ctx, execs.RelaunchRequest{
		ID:             id,
		Env:            env,
		User:           cloneUser(s.defaultUser),
		StartupCommand: reviveStartupCommand(harness, harnessID, s.harnessMode),
	})
	if err != nil {
		return execs.Exec{}, err
	}
	return s.execs.Start(ctx, revived.ID)
}

// reviveStartupCommand is the command a revived terminal types into its fresh
// login shell: the harness's relaunch (resume) command when it has one, the
// bare harness command otherwise — never the initial prompt, which belongs to
// the terminal's first run only. The shell harness types nothing (the shell IS
// the terminal, recorded as the exec's own command), and config mode always
// runs the image-owned command exactly (see EnsurePrimary).
func reviveStartupCommand(harness config.Harness, harnessID, harnessMode string) []string {
	switch {
	case harnessID == ShellHarnessID:
		return nil
	case harnessMode == "config":
		return append([]string{}, harness.Command...)
	case len(harness.RelaunchCommand) > 0:
		return append([]string{}, harness.RelaunchCommand...)
	default:
		return append([]string{}, harness.Command...)
	}
}

// markInstalling records that an exec's harness setup is running.
func (s *Service) markInstalling(id string) {
	s.installingMu.Lock()
	s.installing[id] = struct{}{}
	s.installingMu.Unlock()
}

// unmarkInstalling clears an exec's installing marker once install finishes.
func (s *Service) unmarkInstalling(id string) {
	s.installingMu.Lock()
	delete(s.installing, id)
	s.installingMu.Unlock()
}

// IsInstalling reports whether an exec's harness setup is still running.
// The mapper uses it to project the terminal-layer "installing" phase.
func (s *Service) IsInstalling(id string) bool {
	s.installingMu.Lock()
	_, ok := s.installing[id]
	s.installingMu.Unlock()
	return ok
}

// Metadata keys the terminal layer sets on its execs to express terminal-ness.
const (
	metadataHarnessID = "harnessId"
	metadataPrimary   = "primary"
)

// HarnessID returns the harness an exec was created for, if it is a terminal-mode
// exec, reading the value the terminal layer stored in metadata.
func HarnessID(e execs.Exec) string { return e.Metadata[metadataHarnessID] }

// IsPrimary reports whether an exec is the sandbox's primary terminal.
func IsPrimary(e execs.Exec) bool { return e.Metadata[metadataPrimary] == "true" }

// PrimaryExecID is the virtual exec id that always resolves to the sandbox's
// current primary terminal. Attaching or starting it relaunches a stopped
// primary (see ResolvePrimary) rather than addressing a fixed, possibly dead
// exec. The control plane proxies exec ids opaquely, so clients simply use this
// value in place of a real exec id.
const PrimaryExecID = "primary"

// ResolvePrimary ensures the primary terminal is live — relaunching it with the
// harness's relaunch command when it has stopped — and returns it.
//
// It returns the terminal the launch actually produced rather than re-scanning,
// so an attach connects to the exec it waited behind. Only when there was
// nothing to launch does it fall back to a scan, which prefers a
// running/starting primary and otherwise takes the most recent one (e.g. one
// that just exited, so a late attacher can still replay it).
func (s *Service) ResolvePrimary(ctx context.Context) (execs.Exec, error) {
	exec, err := s.ensurePrimary(ctx, s.bootPrompt)
	if err != nil {
		return execs.Exec{}, err
	}
	if strings.TrimSpace(exec.ID) != "" {
		return exec, nil
	}
	if exec, ok := selectLivePrimary(s.List()); ok {
		return exec, nil
	}
	return execs.Exec{}, execs.ErrNotFound
}

// CurrentPrimary returns the sandbox's current primary terminal without
// relaunching it. It is the read-only counterpart to ResolvePrimary, used for
// status reads (e.g. a client's attach done-check) that must observe a genuine
// exit rather than trigger a resume.
func (s *Service) CurrentPrimary() (execs.Exec, bool) {
	return selectLivePrimary(s.List())
}

// selectLivePrimary picks the primary terminal to attach to: a running/starting
// one if present, else the most recently created primary (e.g. one that just
// exited, so a late attacher can still replay it).
func selectLivePrimary(list []execs.Exec) (execs.Exec, bool) {
	var newest *execs.Exec
	for i := range list {
		e := &list[i]
		if !IsPrimary(*e) {
			continue
		}
		if e.Status == execs.StatusStarting || e.Status == execs.StatusRunning {
			return *e, true
		}
		if newest == nil || e.CreatedAt.After(newest.CreatedAt) {
			newest = e
		}
	}
	if newest != nil {
		return *newest, true
	}
	return execs.Exec{}, false
}

// ShellHarnessID names the fallback harness a terminal runs when no harness config
// resolves: an interactive login shell. Every sandbox gets a default terminal,
// so a harnessless sandbox is a shell session rather than an empty sandbox.
const ShellHarnessID = "shell"

// EnsurePrimary launches the sandbox's primary terminal on sandbox start. Every
// sandbox has one: on the first start it runs the resolved harness with the
// sandbox prompt as arguments, on subsequent starts it revives the existing
// primary record in place (ADR 0038) — same exec id, running the harness's
// relaunch command — and when no harness is configured it runs a plain shell.
// It is a no-op when a live primary terminal already exists.
func (s *Service) EnsurePrimary(ctx context.Context, prompt []string) error {
	_, err := s.ensurePrimary(ctx, prompt)
	return err
}

// ensurePrimary brings the primary terminal up and returns it, collapsing
// concurrent callers onto a single launch.
//
// Boot launches the primary in a goroutine started just before the HTTP server
// begins serving, so boot and a first attach are concurrent by construction —
// and with clients no longer polling for a primary before attaching (ADR 0039),
// the attach arrives squarely inside that window. Left as a check-then-act this
// races two ways: the scan runs before the other caller's record is visible
// (execs.Manager keeps no in-process lock; List re-reads from disk), so both
// launch a primary; and both read PrimaryTerminalLaunched as false before
// either marks it, so both pass the prompt as argv instead of one launching and
// one resuming. Reviving in place (ADR 0038) narrows neither race: two callers
// that both find the same dead record still revive it twice.
//
// A zero exec with a nil error means a live primary already existed and nothing
// was launched.
func (s *Service) ensurePrimary(ctx context.Context, prompt []string) (execs.Exec, error) {
	s.launchMu.Lock()
	if launch := s.launch; launch != nil {
		// Someone is already launching. Join them: their result is ours.
		s.launchMu.Unlock()
		return s.awaitPrimaryLaunch(ctx, launch)
	}
	if _, ok := livePrimary(s.List()); ok {
		s.launchMu.Unlock()
		return execs.Exec{}, nil
	}
	launch := &primaryLaunch{done: make(chan struct{})}
	s.launch = launch
	s.launchMu.Unlock()
	// The launch is detached from whichever caller happened to start it: an
	// attach that times out, or a client that disconnects, must not abort an
	// install that boot and every other joiner are waiting on. Each joiner
	// waits under its own context instead.
	go s.runPrimaryLaunch(context.WithoutCancel(ctx), launch, prompt)
	return s.awaitPrimaryLaunch(ctx, launch)
}

// awaitPrimaryLaunch waits for a launch to finish under the caller's own
// context, which is what bounds an attach's wait for a slow harness install.
func (s *Service) awaitPrimaryLaunch(ctx context.Context, launch *primaryLaunch) (execs.Exec, error) {
	select {
	case <-launch.done:
		return launch.exec, launch.err
	case <-ctx.Done():
		return execs.Exec{}, ctx.Err()
	}
}

// runPrimaryLaunch performs the launch and hands its result to everyone joined
// to it. The latch is cleared before the result is published, so a failed
// launch is reported to the current joiners and the next caller retries rather
// than inheriting the failure forever.
func (s *Service) runPrimaryLaunch(ctx context.Context, launch *primaryLaunch, prompt []string) {
	exec, err := s.launchPrimary(ctx, prompt)
	s.launchMu.Lock()
	if s.launch == launch {
		s.launch = nil
	}
	s.launchMu.Unlock()
	launch.exec, launch.err = exec, err
	close(launch.done)
}

// livePrimary is the primary terminal that makes a launch unnecessary: one
// already starting or running. It is deliberately narrower than
// selectLivePrimary, which also settles for an exited primary.
func livePrimary(list []execs.Exec) (execs.Exec, bool) {
	for _, existing := range list {
		if IsPrimary(existing) && (existing.Status == execs.StatusStarting || existing.Status == execs.StatusRunning) {
			return existing, true
		}
	}
	return execs.Exec{}, false
}

// launchPrimary brings the primary terminal up and returns it: a dead primary is
// revived under its own id (ADR 0038), and only a sandbox with no primary record
// at all gets a fresh one. It runs under the single-flight latch, so it is never
// concurrent with itself.
func (s *Service) launchPrimary(ctx context.Context, prompt []string) (execs.Exec, error) {
	var newest *execs.Exec
	for _, existing := range s.List() {
		if !IsPrimary(existing) {
			continue
		}
		if existing.Status == execs.StatusStarting || existing.Status == execs.StatusRunning {
			return existing, nil
		}
		if newest == nil || existing.CreatedAt.After(newest.CreatedAt) {
			e := existing
			newest = &e
		}
	}
	if newest != nil {
		// A dead primary is revived under its own id, never replaced by a
		// sibling record. Newest wins over records that predate ADR 0038.
		revived, err := s.Revive(ctx, newest.ID)
		if err != nil {
			return execs.Exec{}, err
		}
		if s.primaryState != nil {
			if err := s.primaryState.MarkPrimaryTerminalLaunched(ctx); err != nil {
				return execs.Exec{}, err
			}
		}
		return revived, nil
	}
	harness, harnessID, err := s.resolveHarness("")
	if err != nil {
		// A genuine misconfiguration (a requested harness that doesn't match the
		// sandbox's resolved harness). The absent-harness case is not an error: it
		// resolves to the shell harness.
		return execs.Exec{}, err
	}
	launched := false
	if s.primaryState != nil {
		if launched, err = s.primaryState.PrimaryTerminalLaunched(ctx); err != nil {
			return execs.Exec{}, err
		}
	}
	request := primaryCreateRequest(harness, harnessID, prompt, launched)
	if s.harnessMode == "config" {
		// The image-owned config command is exact: never append the normal prompt
		// and never replace it with the normal relaunch command.
		request.command = append([]string{}, harness.Command...)
	}
	created, err := s.Create(ctx, request)
	if err != nil {
		return execs.Exec{}, err
	}
	started, err := s.Start(ctx, created.ID)
	if err != nil {
		return execs.Exec{}, err
	}
	if s.primaryState != nil {
		if err := s.primaryState.MarkPrimaryTerminalLaunched(ctx); err != nil {
			return execs.Exec{}, err
		}
	}
	return started, nil
}

func primaryCreateRequest(harness config.Harness, harnessID string, prompt []string, launched bool) CreateRequest {
	req := CreateRequest{primary: true}
	switch {
	// A shell takes the prompt neither as arguments (it would try to run it as a
	// command) nor as a session to resume; it is the same interactive shell on
	// every start.
	case harnessID == ShellHarnessID:
	case launched && len(harness.RelaunchCommand) > 0:
		req.command = append([]string{}, harness.RelaunchCommand...)
	case launched:
	default:
		req.Args = append([]string{}, prompt...)
	}
	return req
}

// resolveHarness selects the harness for a terminal. The sandbox has exactly
// one resolved harness (ADR 0012 §9, already merged by pool-agent's
// Effective() call before boot) or none at all, in which case the shell
// fallback is used. An explicit request must match the resolved harness (or
// name the shell harness); there is nothing else left to resolve at boot.
func (s *Service) resolveHarness(requested string) (config.Harness, string, error) {
	requested = strings.TrimSpace(requested)
	if strings.TrimSpace(s.harness.ID) == "" {
		if requested != "" && requested != ShellHarnessID {
			return config.Harness{}, "", fmt.Errorf("harness %q is not configured", requested)
		}
		harness, err := s.shellAgent()
		if err != nil {
			return config.Harness{}, "", err
		}
		return harness, ShellHarnessID, nil
	}
	if requested != "" && requested != s.harness.ID {
		return config.Harness{}, "", fmt.Errorf("harness %q is not configured", requested)
	}
	if len(s.harness.Command) == 0 {
		// A declared harness with no command is the login shell: the control
		// plane names the harness, and only the sandbox knows which shell the
		// run user has. Its identity stays the declared one, so the terminal
		// still reports the harness the sandbox was created with.
		shell, err := s.shellAgent()
		if err != nil {
			return config.Harness{}, "", err
		}
		harness := s.harness
		harness.Command = shell.Command
		return harness, harness.ID, nil
	}
	return s.harness, s.harness.ID, nil
}

// shellAgent builds the fallback shell harness: the terminal user's interactive
// login shell, resolved by `execs` so a shell terminal and a `shell: true` exec
// land on the same shell.
func (s *Service) shellAgent() (config.Harness, error) {
	command, err := execs.ShellCommand(s.defaultUser, s.env)
	if err != nil {
		return config.Harness{}, err
	}
	return config.Harness{
		ID:      ShellHarnessID,
		Name:    "Shell",
		Command: command,
	}, nil
}

// --- Installers ---

type CompositeInstaller struct {
	Installers []Installer
}

func (i CompositeInstaller) EnsureInstalled(ctx context.Context, harness config.Harness, workdir string, env map[string]string) error {
	for _, installer := range i.Installers {
		if installer == nil {
			continue
		}
		if err := installer.EnsureInstalled(ctx, harness, workdir, env); err != nil {
			return err
		}
	}
	return nil
}

type HookInstaller struct {
	ManagedRoot      string
	PublisherCommand string
}

func (i HookInstaller) EnsureInstalled(ctx context.Context, h config.Harness, workdir string, env map[string]string) error {
	installer := registry.Installer{
		Drivers:          registry.DriverForHarness(harnessFromConfig(h)),
		ManagedRoot:      i.ManagedRoot,
		PublisherCommand: i.PublisherCommand,
	}
	return installer.InstallHooks(ctx, harness.HookInstallRequest{
		Harness:          harnessFromConfig(h),
		Workdir:          workdir,
		Env:              env,
		ManagedRoot:      i.ManagedRoot,
		PublisherCommand: i.PublisherCommand,
	})
}

func harnessFromConfig(h config.Harness) harness.Harness {
	return harness.Harness{
		ID:      h.ID,
		TypeID:  h.TypeID,
		Name:    h.Name,
		Command: append([]string{}, h.Command...),
	}
}

// FileInstaller writes a harness's configured files into its home directory.
type FileInstaller struct {
	Name          string
	HomeDirectory string
	UID           *int64
	GID           *int64
	SandboxConfig map[string]any
}

func (i FileInstaller) EnsureInstalled(_ context.Context, harness config.Harness, _ string, _ map[string]string) error {
	if len(harness.Files) == 0 {
		return nil
	}
	home, err := i.resolveHome()
	if err != nil {
		return fmt.Errorf("harness %q %w", harness.ID, err)
	}
	home = filepath.Clean(home)
	for _, file := range harness.Files {
		path, err := homeRelativePath(home, file.Path)
		if err != nil {
			return fmt.Errorf("harness %q file %q: %w", harness.ID, file.Path, err)
		}
		content := file.Content
		if file.Template {
			content, err = renderHarnessFileTemplate(file.Path, content, i.SandboxConfig)
			if err != nil {
				return fmt.Errorf("harness %q file %q: %w", harness.ID, file.Path, err)
			}
		}
		if err := writeHarnessFile(path, content, file.CreateOnly, i.UID, i.GID); err != nil {
			return fmt.Errorf("harness %q file %q: %w", harness.ID, file.Path, err)
		}
	}
	return nil
}

func renderHarnessFileTemplate(name, content string, sandboxConfig map[string]any) (string, error) {
	jsonValue := func(value any) (string, error) {
		encoded, err := json.Marshal(value)
		return string(encoded), err
	}
	tmpl, err := template.New(name).Funcs(template.FuncMap{"json": jsonValue}).Option("missingkey=zero").Parse(content)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, sandboxConfig); err != nil {
		return "", fmt.Errorf("render template: %w", err)
	}
	return rendered.String(), nil
}

// resolveHome resolves the home directory to install harness files into,
// matching how process env defaults resolve HOME: an explicit home, then the
// run user's own account entry, then the harness process's own $HOME.
func (i FileInstaller) resolveHome() (string, error) {
	if home := strings.TrimSpace(i.HomeDirectory); home != "" {
		return home, nil
	}
	resolved, err := runuser.Resolve(
		runuser.Layers{Manifest: &runuser.User{Name: i.Name, UID: i.UID, GID: i.GID}},
		sandboxuser.FieldHome,
	)
	home := ""
	if err == nil {
		home = strings.TrimSpace(resolved.HomeDirectory)
	}
	if home == "" {
		home = strings.TrimSpace(os.Getenv("HOME"))
	}
	if home == "" {
		return "", fmt.Errorf("has files to install but no home directory could be resolved for the run user")
	}
	return home, nil
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

func writeHarnessFile(path, content string, createOnly bool, uid, gid *int64) error {
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
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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
// actually created, so callers can chown only new paths and leave pre-existing
// directory ownership untouched.
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

// --- shared helpers ---

func cloneHarness(in config.Harness) config.Harness {
	out := in
	out.Command = append([]string{}, in.Command...)
	out.RelaunchCommand = append([]string{}, in.RelaunchCommand...)
	out.Files = append([]config.HarnessFile{}, in.Files...)
	return out
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
	out.AdditionalGroups = append([]string(nil), in.AdditionalGroups...)
	return &out
}

func cloneInt64(in *int64) *int64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

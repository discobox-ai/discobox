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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"text/template"

	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
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
	// RestoreSecretFiles re-installs the harness files whose delivered sentinel
	// is no longer in the file on disk, and returns the paths it restored. See
	// secretfiles.go for why a delivered credential needs restoring at all.
	RestoreSecretFiles(context.Context, config.Harness, map[string]string) ([]string, error)
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
	// AwaitSources blocks until the sandbox's sources are in place, and is
	// cleared for every sandbox that already had them when its container was
	// created (see sourcesready.Gate). Only the primary terminal's very first
	// launch waits on it: by the time anything is revived, the source it was
	// waiting for has been there for the whole life of the record.
	AwaitSources func(context.Context) error
}

// Service is the harness-terminal layer over execs.Manager.
// configHarnessMode is the sandbox that exists to run a harness's setup once,
// rather than to be worked in. See sandbox-agent/config.
const configHarnessMode = "config"

type Service struct {
	execs          *execs.Manager
	harness        config.Harness
	env            map[string]string
	secretEnv      func() map[string]string
	fileSecrets    []string
	defaultUser    *execs.User
	hookSocketPath string
	installer      Installer
	primaryState   PrimaryStateStore
	harnessMode    string
	bootPrompt     []string
	awaitSources   func(context.Context) error

	// installing tracks exec IDs whose hook and file setup is still running.
	// The record exists (execs status "starting") before its process launches, so
	// the mapper projects these as the "installing" phase to callers. Terminal is
	// an exec plus this layer, so install belongs here rather than in execs.
	installingMu sync.Mutex
	installing   map[string]struct{}

	// launchMu guards launches, the in-flight terminal launches keyed by what
	// is being launched. See singleFlightLaunch for why bringing a terminal up
	// is single-flighted rather than merely locked (ADR 0039).
	launchMu sync.Mutex
	launches map[string]*terminalLaunch
}

// terminalLaunch is one in-flight attempt to bring a terminal up — a first
// launch or a revive. Callers that find one already running join it and take
// its result instead of starting a second.
type terminalLaunch struct {
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
		fileSecrets:  append([]string(nil), cfg.Harness.FileSecrets...),
		defaultUser:  defaultUser,
		installer:    cfg.Installer,
		primaryState: cfg.PrimaryState,
		harnessMode:  strings.TrimSpace(cfg.HarnessMode),
		bootPrompt:   append([]string(nil), cfg.Prompt...),
		awaitSources: cfg.AwaitSources,
		installing:   map[string]struct{}{},
		launches:     map[string]*terminalLaunch{},
	}
	if s.installer == nil {
		s.installer = CompositeInstaller{Installers: []Installer{
			FileInstaller{
				// The run user as the exec layer resolved it, not the manifest
				// fields it was resolved from: a sandbox whose manifest names
				// nobody still runs as somebody, and the files belong to them.
				User:          defaultUser,
				HomeDirectory: cfg.ExecDefaults.HomeDirectory,
				SandboxConfig: cfg.SandboxConfig,
				Secrets:       cfg.SecretEnv,
			},
		}}
	}
	return s, nil
}

// exportedSecretEnv is the sentinel map minus the harness's file-delivered
// credentials. Those still exist as sentinels -- FileInstaller renders them
// into the file the harness reads -- but they must not also appear in its
// environment: a CLI that reads both prefers the variable, and a credential
// arriving that way carries none of the metadata the file does, so exporting it
// silently defeats the file (harness.SecretDeliveryFile).
//
// Withholding is by explicit declaration only. A sentinel nobody declared --
// bound to this sandbox by hand -- is exported exactly as before.
func (s *Service) exportedSecretEnv() map[string]string {
	env := s.secretEnv()
	if len(env) == 0 || len(s.fileSecrets) == 0 {
		return env
	}
	out := make(map[string]string, len(env))
	for name, sentinel := range env {
		if slices.Contains(s.fileSecrets, name) {
			continue
		}
		out[name] = sentinel
	}
	return out
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
		base = execs.MergeEnv(base, s.exportedSecretEnv())
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
	//
	// A configure flow is the exception, and runs its command as the exec
	// itself. What the flow is *for* is the command's exit status — the server
	// reads it to decide whether the setup worked — and a command typed into a
	// shell does not have one the exec can report: the shell outlives it, so
	// the terminal never reaches "exited" until somebody types exit, and the
	// code it reports then is the shell's. Job control is worth a login shell
	// for a harness you sit in front of; it is not worth the answer to "did
	// this succeed" for a program that runs once and ends.
	switch {
	case s.harnessMode == configHarnessMode:
		execReq.Shell = false
		execReq.Command = command
	case harnessID != ShellHarnessID:
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
//
// Concurrent callers collapse onto one revive keyed by the terminal's id.
// Without that they race: both read the same dead record, both derive the next
// unit generation from the same stale unit name and so land on the *same* name,
// and the second one's socket removal (which fences the previous run) deletes
// the socket the first one's shim has just bound, leaving a live run nothing
// can attach to.
func (s *Service) Revive(ctx context.Context, id string) (execs.Exec, error) {
	return s.singleFlightLaunch(ctx, id, func(ctx context.Context) (execs.Exec, error) {
		return s.revive(ctx, id)
	})
}

func (s *Service) revive(ctx context.Context, id string) (execs.Exec, error) {
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
		base = execs.MergeEnv(base, s.exportedSecretEnv())
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
// the terminal, recorded as the exec's own command), and neither does a
// configure flow, whose command is the exec's own for the same reason and is
// what a relaunch re-runs.
func reviveStartupCommand(harness config.Harness, harnessID, harnessMode string) []string {
	switch {
	case harnessID == ShellHarnessID:
		return nil
	case harnessMode == configHarnessMode:
		return nil
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
// the attach arrives squarely inside that window.
//
// The whole decision runs under the latch, not just the launch: the record
// exists in `starting` from the moment execs.Create writes it, long before its
// shim is listening, so a caller that checked liveness outside the latch would
// return a terminal nothing can attach to yet.
func (s *Service) ensurePrimary(ctx context.Context, prompt []string) (execs.Exec, error) {
	return s.singleFlightLaunch(ctx, primaryLaunchKey, func(ctx context.Context) (execs.Exec, error) {
		return s.launchPrimary(ctx, prompt)
	})
}

// primaryLaunchKey is the single-flight key for bringing the primary terminal
// up. It is deliberately the virtual primary exec id, which is not a real exec
// id, so it can never collide with a revive keyed by a terminal's own id.
//
// A revive the primary launch decides on is keyed by the record's id, not by
// this, so an attach addressing that terminal directly contends on the same
// key rather than reviving it a second time.
const primaryLaunchKey = PrimaryExecID

// singleFlightLaunch runs fn for key, collapsing concurrent callers onto one
// run and handing them all its result.
//
// Bringing a terminal up is a check-then-act — read the record, decide it needs
// launching, then create or relaunch it — and nothing underneath serializes it:
// execs.Manager keeps no in-process lock, and List re-reads from disk. Two
// callers therefore both decide to act. For a first launch that means two
// primary terminals and a prompt that runs twice; for a revive it is worse than
// duplicated work, because both compute the next unit generation from the same
// stale record and land on the same unit name, and the second run's
// os.Remove(SocketPath) deletes the socket the first run's shim just bound.
//
// fn runs under a context detached from whichever caller started it: an attach
// that times out, or a client that disconnects, must not abort a launch that
// other joiners — or boot — are waiting on. Each joiner waits under its own
// context instead. The key is released before the result is published, so a
// failed launch is reported to everyone joined to it and the next caller
// retries rather than inheriting the failure.
func (s *Service) singleFlightLaunch(ctx context.Context, key string, fn func(context.Context) (execs.Exec, error)) (execs.Exec, error) {
	s.launchMu.Lock()
	if launch, ok := s.launches[key]; ok {
		// Someone is already launching this terminal. Join them: their result
		// is ours.
		s.launchMu.Unlock()
		return s.awaitLaunch(ctx, launch)
	}
	launch := &terminalLaunch{done: make(chan struct{})}
	s.launches[key] = launch
	s.launchMu.Unlock()
	go func() {
		exec, err := fn(context.WithoutCancel(ctx))
		s.launchMu.Lock()
		if s.launches[key] == launch {
			delete(s.launches, key)
		}
		s.launchMu.Unlock()
		launch.exec, launch.err = exec, err
		close(launch.done)
	}()
	return s.awaitLaunch(ctx, launch)
}

// awaitLaunch waits for a launch to finish under the caller's own context,
// which is what bounds an attach's wait for a slow harness install.
func (s *Service) awaitLaunch(ctx context.Context, launch *terminalLaunch) (execs.Exec, error) {
	select {
	case <-launch.done:
		return launch.exec, launch.err
	case <-ctx.Done():
		return execs.Exec{}, ctx.Err()
	}
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
// at all gets a fresh one. It runs under the primary launch key, so it is never
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
		// Revive latches on that id, so an attach addressing this terminal
		// directly joins this revive instead of starting a second one.
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
	// Nothing has ever run in this sandbox, so this is the launch that would
	// run against a source that has not arrived. A revive above is past this
	// point by construction: the terminal it revives could not have been
	// created before the source was there.
	if s.awaitSources != nil {
		if err := s.awaitSources(ctx); err != nil {
			return execs.Exec{}, fmt.Errorf("wait for the sandbox's source: %w", err)
		}
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
	if s.harnessMode == configHarnessMode {
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

func (i CompositeInstaller) RestoreSecretFiles(ctx context.Context, harness config.Harness, env map[string]string) ([]string, error) {
	var restored []string
	for _, installer := range i.Installers {
		if installer == nil {
			continue
		}
		paths, err := installer.RestoreSecretFiles(ctx, harness, env)
		if err != nil {
			return restored, err
		}
		restored = append(restored, paths...)
	}
	return restored, nil
}

// FileInstaller writes a harness's configured files into its home directory.
type FileInstaller struct {
	// User is the run user, already resolved through every layer the exec
	// path uses. Nil means the manifest named nobody and the harness inherits
	// this process's identity, which is still an identity with a home.
	User *execs.User
	// HomeDirectory is the manifest's explicit home, when it carries one.
	HomeDirectory string
	SandboxConfig map[string]any
	// Secrets returns the sandbox's env-name -> sentinel map, so a templated
	// file can place a sentinel where a harness expects to read a credential.
	//
	// It is read here rather than taken from the process environment on purpose:
	// a harness that authenticates from a file has no reason to also carry the
	// variable, and this is what lets that variable stop being exported. What
	// lands in the file is a sentinel — non-secret by construction — which the
	// proxy swaps on the way out, exactly as it does for the env-var form.
	//
	// Read fresh per install for the same reason the exec env is (ADR 0012 §3):
	// the secrets file is refreshed independently of sandbox.json.
	Secrets func() map[string]string
}

func (i FileInstaller) EnsureInstalled(_ context.Context, harness config.Harness, _ string, env map[string]string) error {
	if len(harness.Files) == 0 {
		return nil
	}
	home, err := i.resolveHome(env)
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
			content, err = renderHarnessFileTemplate(file.Path, content, i.templateContext())
			if err != nil {
				return fmt.Errorf("harness %q file %q: %w", harness.ID, file.Path, err)
			}
		}
		if err := writeHarnessFile(path, content, file.CreateOnly, i.uid(), i.gid()); err != nil {
			return fmt.Errorf("harness %q file %q: %w", harness.ID, file.Path, err)
		}
	}
	return nil
}

// templateContext is the sandbox config a harness file renders against, plus
// `secrets` — the env-name -> sentinel map — under its own key. A copy, so the
// added key never mutates the shared config, and so a config that already
// carried `secrets` cannot be shadowed silently.
func (i FileInstaller) templateContext() map[string]any {
	var sentinels map[string]string
	if i.Secrets != nil {
		sentinels = i.Secrets()
	}
	out := make(map[string]any, len(i.SandboxConfig)+1)
	maps.Copy(out, i.SandboxConfig)
	// Always present, even empty. A template that asks whether a credential
	// exists (`{{ if .secrets.NAME }}`) must be able to ask: with the key
	// missing entirely, that walk fails the render rather than answering "no",
	// which would break a file the sandbox needs over a secret it does not
	// have.
	if sentinels == nil {
		sentinels = map[string]string{}
	}
	out["secrets"] = sentinels
	return out
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

// uid and gid are who the installed files belong to: the resolved run user,
// or nobody in particular when the harness inherits this process's identity —
// which is what the files already get by being written by it.
func (i FileInstaller) uid() *int64 {
	if i.User == nil {
		return nil
	}
	return i.User.UID
}

func (i FileInstaller) gid() *int64 {
	if i.User == nil {
		return nil
	}
	return i.User.GID
}

// resolveHome resolves the home directory to install harness files into,
// exactly as the exec layer resolves HOME for the process that will read them:
// an explicit home, then the run user execs.HomeDir yields against the harness
// env, then this process's own $HOME.
//
// It asks the exec layer rather than resolving again from the manifest. The
// manifest is one of three layers — the image's own identity is another — and
// resolving from it alone fails wherever the manifest names nobody, which is
// every sandbox the server creates for itself. A configure sandbox is created
// with no user at all, and installing a harness's files into it died on a home
// the exec running two lines later had no trouble finding (ADR 0025 §5).
func (i FileInstaller) resolveHome(env map[string]string) (string, error) {
	if home := strings.TrimSpace(i.HomeDirectory); home != "" {
		return home, nil
	}
	if home := execs.HomeDir(i.User, env); home != "" {
		return home, nil
	}
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return home, nil
	}
	return "", fmt.Errorf("has files to install but no home directory could be resolved for the run user")
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

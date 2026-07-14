// Package terminal is the harness-terminal layer built on top of the exec
// primitive. A terminal is an exec whose command is resolved from an harness
// config, whose environment is prepared by an installer before start, and which
// is tagged (harnessId, primary) in exec metadata. The Service owns harness
// resolution, install, and primary-terminal lifecycle; all runtime mechanics
// (systemd unit, shim, PTY, attach, status) belong to execs.Manager.
package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/harness/registry"
	"github.com/obot-platform/discobox/sandbox-agent/config"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
)

const installStatusTimeout = 5 * time.Minute

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
	Execs                 *execs.Manager
	ResolvedHarnessConfig *config.Harness
	Harnesses             []config.Harness
	WorkingRoot           string
	RuntimeDir            string
	Env                   map[string]string
	ImageConfig           config.ImageConfig
	ImageConfigPath       string
	ExecDefaults          config.ExecDefaults
	DefaultUser           *execs.User
	Units                 execs.UnitManager
	Installer             Installer
	PrimaryState          PrimaryStateStore
}

// Service is the harness-terminal layer over execs.Manager.
type Service struct {
	execs          *execs.Manager
	installs       *execs.Manager
	harnesses      map[string]config.Harness
	resolvedID     string
	defaultID      string
	workingRoot    string
	env            map[string]string
	imageConfig    config.ImageConfig
	defaultUser    *execs.User
	hookSocketPath string
	installer      Installer
	primaryState   PrimaryStateStore

	// installing tracks exec IDs whose harness install command is still running.
	// The record exists (execs status "starting") before its process launches, so
	// the mapper projects these as the "installing" phase to callers. Terminal is
	// an exec plus this layer, so install belongs here rather than in execs.
	installingMu sync.Mutex
	installing   map[string]struct{}
}

func NewService(cfg ServiceConfig) (*Service, error) {
	if strings.TrimSpace(cfg.WorkingRoot) == "" {
		return nil, errors.New("working root is required")
	}
	if cfg.Execs == nil {
		return nil, errors.New("shared exec manager is required")
	}
	runtimeDir := strings.TrimSpace(cfg.RuntimeDir)
	if runtimeDir == "" {
		runtimeDir = "/run/discobox/execs"
	}
	imageConfig := cfg.ImageConfig
	if len(imageConfig.Env) == 0 {
		var err error
		if imageConfig, err = config.LoadImage(cfg.ImageConfigPath); err != nil {
			return nil, err
		}
	}
	defaultUser := terminalDefaultUser(cfg)

	terminals := cfg.Execs
	installs, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot:    cfg.WorkingRoot,
		DefaultWorkdir: cfg.ExecDefaults.Workdir,
		DefaultUser:    defaultUser,
		RuntimeDir:     filepath.Join(runtimeDir, "installs"),
		Env:            cfg.Env,
		ImageConfig:    imageConfig,
		Units:          cfg.Units,
	})
	if err != nil {
		return nil, err
	}

	s := &Service{
		execs:        terminals,
		installs:     installs,
		harnesses:    map[string]config.Harness{},
		workingRoot:  filepath.Clean(cfg.WorkingRoot),
		env:          cloneMap(cfg.Env),
		imageConfig:  imageConfig,
		defaultUser:  defaultUser,
		installer:    cfg.Installer,
		primaryState: cfg.PrimaryState,
		installing:   map[string]struct{}{},
	}
	if s.installer == nil {
		s.installer = CompositeInstaller{Installers: []Installer{
			CommandInstaller{Execs: installs, User: defaultUser},
			HookInstaller{},
			FileInstaller{
				Name:          cfg.ExecDefaults.Username,
				HomeDirectory: cfg.ExecDefaults.HomeDirectory,
				UID:           cfg.ExecDefaults.UID,
				GID:           cfg.ExecDefaults.GID,
			},
		}}
	}
	for _, harness := range cfg.Harnesses {
		if strings.TrimSpace(harness.ID) == "" {
			continue
		}
		if _, exists := s.harnesses[harness.ID]; exists {
			return nil, fmt.Errorf("duplicate harness %q", harness.ID)
		}
		s.harnesses[harness.ID] = cloneHarness(harness)
		if s.defaultID == "" || harness.IsDefault {
			s.defaultID = harness.ID
		}
	}
	if cfg.ResolvedHarnessConfig != nil && strings.TrimSpace(cfg.ResolvedHarnessConfig.ID) != "" {
		s.resolvedID = strings.TrimSpace(cfg.ResolvedHarnessConfig.ID)
		if _, exists := s.harnesses[s.resolvedID]; !exists {
			s.harnesses[s.resolvedID] = cloneHarness(*cfg.ResolvedHarnessConfig)
		}
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
// metadata), then runs the harness's install command. The exec record is created
// before install so the install is observable: while EnsureInstalled runs, the
// record exists and the mapper projects it as the "installing" phase. The unit
// is only launched later by Start, so the record sits idle during install.
func (s *Service) Create(ctx context.Context, req CreateRequest) (execs.Exec, error) {
	workdir, err := s.execs.ResolveWorkdir(req.Workdir)
	if err != nil {
		return execs.Exec{}, err
	}
	harness, harnessID, err := s.resolveHarness(req.HarnessID, workdir)
	if err != nil {
		return execs.Exec{}, err
	}
	id, err := execs.NewID()
	if err != nil {
		return execs.Exec{}, err
	}
	env := execs.EnvWithRuntimeDefaults(execs.MergeEnv(s.env, req.Env), s.defaultUser, s.imageConfig)
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
	created, err := s.execs.Create(ctx, execs.CreateRequest{
		ID:       id,
		Command:  command,
		Workdir:  workdir,
		Env:      env,
		User:     cloneUser(s.defaultUser),
		TTY:      true,
		Rows:     req.Rows,
		Cols:     req.Cols,
		Metadata: metadata,
	})
	if err != nil {
		return execs.Exec{}, err
	}
	// Mark installing so the record surfaces as the "installing" phase while the
	// (potentially slow) install command runs. On failure the record is removed so
	// no half-installed terminal lingers, matching the pre-record behavior.
	s.markInstalling(created.ID)
	defer s.unmarkInstalling(created.ID)
	if err := s.installer.EnsureInstalled(ctx, harness, workdir, env); err != nil {
		_ = s.execs.Delete(context.WithoutCancel(ctx), created.ID)
		return execs.Exec{}, err
	}
	return created, nil
}

// markInstalling records that an exec's harness install command is running.
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

// IsInstalling reports whether an exec's harness install command is still running.
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

// ErrNoHarnessConfigured reports that the sandbox has no harness to launch as its
// primary terminal. It is a valid empty state, not a failure: the caller should
// log it and skip launching rather than treat it as an error.
var ErrNoHarnessConfigured = errors.New("no harness is configured for this sandbox")

// EnsurePrimary launches the sandbox's primary terminal on sandbox start. On the
// first start it runs the resolved harness with the sandbox prompt as arguments;
// on subsequent starts it runs the harness's relaunch command to resume the
// previous session. It is a no-op when a live primary terminal already exists.
// It returns ErrNoHarnessConfigured when no harness is configured; any other error
// is a real misconfiguration and is returned to the caller.
func (s *Service) EnsurePrimary(ctx context.Context, prompt []string) error {
	for _, existing := range s.List() {
		if IsPrimary(existing) && (existing.Status == execs.StatusStarting || existing.Status == execs.StatusRunning) {
			return nil
		}
	}
	workdir, err := s.execs.ResolveWorkdir("")
	if err != nil {
		return err
	}
	harness, _, err := s.resolveHarness("", workdir)
	if err != nil {
		// Surface the outcome to the caller. A missing harness is a valid empty
		// state (ErrNoHarnessConfigured); any other error is a genuine
		// misconfiguration (e.g. a malformed local harness config) and must not be
		// swallowed the way it previously was.
		return err
	}
	launched := false
	if s.primaryState != nil {
		if launched, err = s.primaryState.PrimaryTerminalLaunched(ctx); err != nil {
			return err
		}
	}
	created, err := s.Create(ctx, primaryCreateRequest(harness, prompt, launched))
	if err != nil {
		return err
	}
	if _, err := s.Start(ctx, created.ID); err != nil {
		return err
	}
	if s.primaryState != nil {
		return s.primaryState.MarkPrimaryTerminalLaunched(ctx)
	}
	return nil
}

func primaryCreateRequest(harness config.Harness, prompt []string, launched bool) CreateRequest {
	req := CreateRequest{primary: true}
	switch {
	case launched && len(harness.RelaunchCommand) > 0:
		req.command = append([]string{}, harness.RelaunchCommand...)
	case launched:
	default:
		req.Args = append([]string{}, prompt...)
	}
	return req
}

// resolveHarness selects the harness for a terminal in precedence order: an explicit
// request, then the sandbox's resolved harness, then a local repo harness config,
// then the configured default.
func (s *Service) resolveHarness(requested string, workdir string) (config.Harness, string, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		harness, ok := s.harnesses[requested]
		if !ok {
			return config.Harness{}, "", fmt.Errorf("harness %q is not configured", requested)
		}
		return harness, requested, nil
	}
	if s.resolvedID != "" {
		if harness, ok := s.harnesses[s.resolvedID]; ok {
			return harness, s.resolvedID, nil
		}
	}
	if local, ok, err := s.localHarnessConfig(workdir); err != nil {
		return config.Harness{}, "", err
	} else if ok {
		return local, local.ID, nil
	}
	if s.defaultID == "" {
		return config.Harness{}, "", ErrNoHarnessConfigured
	}
	harness, ok := s.harnesses[s.defaultID]
	if !ok {
		return config.Harness{}, "", fmt.Errorf("default harness %q is not configured", s.defaultID)
	}
	return harness, s.defaultID, nil
}

type localHarnessConfig struct {
	Harness        string    `json:"harness,omitempty"`
	ID             string    `json:"id,omitempty"`
	Name           string    `json:"name,omitempty"`
	InstallCommand *[]string `json:"installCommand,omitempty"`
	Command        *[]string `json:"command,omitempty"`
	RunCommand     *[]string `json:"runCommand,omitempty"`
}

func (s *Service) localHarnessConfig(workdir string) (config.Harness, bool, error) {
	repoRoot, ok := gitRoot(workdir, s.workingRoot)
	if !ok {
		return config.Harness{}, false, nil
	}
	path, ok := localHarnessConfigPath(repoRoot)
	if !ok {
		return config.Harness{}, false, nil
	}
	local, err := readLocalHarnessConfig(path)
	if err != nil {
		return config.Harness{}, false, err
	}
	selector := firstNonEmpty(local.Harness, local.ID, local.Name)
	if selector == "" {
		return config.Harness{}, false, fmt.Errorf("local harness config %s must set harness, id, or name", path)
	}
	harness, ok := s.matchHarness(selector)
	if !ok {
		harness = config.Harness{ID: selector, Name: selector}
	}
	harness = applyLocalHarnessConfig(harness, local)
	if strings.TrimSpace(harness.ID) == "" {
		return config.Harness{}, false, fmt.Errorf("local harness config %s resolved empty harness id", path)
	}
	if len(harness.Command) == 0 || strings.TrimSpace(harness.Command[0]) == "" {
		return config.Harness{}, false, fmt.Errorf("local harness config %s resolved harness %q without command", path, harness.ID)
	}
	return harness, true, nil
}

func localHarnessConfigPath(repoRoot string) (string, bool) {
	for _, name := range []string{"harness.json", "harness-config.json", "sandbox.json"} {
		path := filepath.Join(repoRoot, ".discobox", name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, true
		}
	}
	return "", false
}

func readLocalHarnessConfig(path string) (localHarnessConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return localHarnessConfig{}, err
	}
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		return localHarnessConfig{Harness: name}, nil
	}
	var out localHarnessConfig
	if err := json.Unmarshal(data, &out); err != nil {
		return localHarnessConfig{}, fmt.Errorf("parse local harness config %s: %w", path, err)
	}
	return out, nil
}

func (s *Service) matchHarness(selector string) (config.Harness, bool) {
	selector = strings.TrimSpace(selector)
	if harness, ok := s.harnesses[selector]; ok {
		return cloneHarness(harness), true
	}
	for _, harness := range s.harnesses {
		if strings.EqualFold(harness.Name, selector) {
			return cloneHarness(harness), true
		}
	}
	return config.Harness{}, false
}

func applyLocalHarnessConfig(harness config.Harness, local localHarnessConfig) config.Harness {
	if strings.TrimSpace(local.ID) != "" {
		harness.ID = strings.TrimSpace(local.ID)
	}
	if strings.TrimSpace(local.Name) != "" {
		harness.Name = strings.TrimSpace(local.Name)
	}
	if local.InstallCommand != nil {
		harness.InstallCommand = append([]string{}, (*local.InstallCommand)...)
	}
	if local.Command != nil {
		harness.Command = append([]string{}, (*local.Command)...)
	}
	if local.RunCommand != nil {
		harness.Command = append([]string{}, (*local.RunCommand)...)
	}
	return harness
}

func gitRoot(workdir, workingRoot string) (string, bool) {
	workdir = filepath.Clean(workdir)
	if output, err := exec.CommandContext(context.Background(), "git", "-C", workdir, "rev-parse", "--show-toplevel").Output(); err == nil {
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

// CommandInstaller runs an harness's install command to completion as an ephemeral
// exec, once per distinct command.
type CommandInstaller struct {
	Execs         *execs.Manager
	StatusTimeout time.Duration
	PollInterval  time.Duration
	User          *execs.User
}

var (
	installedCommandsMu sync.Mutex
	installedCommands   = map[string]struct{}{}
)

func (i CommandInstaller) EnsureInstalled(ctx context.Context, harness config.Harness, workdir string, env map[string]string) error {
	if len(harness.InstallCommand) == 0 {
		return nil
	}
	installKey := strings.Join(harness.InstallCommand, "\x00")
	installedCommandsMu.Lock()
	defer installedCommandsMu.Unlock()
	if _, ok := installedCommands[installKey]; ok {
		return nil
	}
	timeout := i.StatusTimeout
	if timeout <= 0 {
		timeout = installStatusTimeout
	}
	created, err := i.Execs.Create(ctx, execs.CreateRequest{
		Command: append([]string{}, harness.InstallCommand...),
		Workdir: workdir,
		Env:     env,
		User:    cloneUser(i.User),
	})
	if err != nil {
		return fmt.Errorf("start install harness %q: %w", harness.ID, err)
	}
	defer func() { _ = i.Execs.Delete(context.Background(), created.ID) }()
	if _, err := i.Execs.Start(ctx, created.ID); err != nil {
		return fmt.Errorf("start install harness %q command: %w", harness.ID, err)
	}
	status, err := i.Execs.WaitForExit(ctx, created.ID, timeout, i.PollInterval)
	if err != nil {
		return fmt.Errorf("wait for install harness %q command: %w", harness.ID, err)
	}
	if status.ExitCode == nil || *status.ExitCode != 0 || status.Status == execs.StatusFailed {
		detail := strings.TrimSpace(status.Error)
		if detail == "" && status.ExitCode != nil {
			detail = fmt.Sprintf("exit code %d", *status.ExitCode)
		}
		if detail == "" {
			detail = "missing successful exit status"
		}
		return fmt.Errorf("install harness %q failed: %s", harness.ID, detail)
	}
	installedCommands[installKey] = struct{}{}
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
		Name:    h.Name,
		Command: append([]string{}, h.Command...),
	}
}

// FileInstaller writes an harness's configured files into its home directory.
type FileInstaller struct {
	Name          string
	HomeDirectory string
	UID           *int64
	GID           *int64
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
		if err := writeHarnessFile(path, file.Content, file.CreateOnly, i.UID, i.GID); err != nil {
			return fmt.Errorf("harness %q file %q: %w", harness.ID, file.Path, err)
		}
	}
	return nil
}

// resolveHome resolves the home directory to install harness files into, matching
// how process env defaults resolve HOME (execs.ResolveUser): an explicit home,
// then the run user's /etc/passwd entry, then the harness process's own $HOME.
func (i FileInstaller) resolveHome() (string, error) {
	if home := strings.TrimSpace(i.HomeDirectory); home != "" {
		return home, nil
	}
	_, home, err := execs.ResolveUser(&execs.User{Name: i.Name, UID: i.UID, GID: i.GID})
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
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

func terminalDefaultUser(cfg ServiceConfig) *execs.User {
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

func cloneHarness(in config.Harness) config.Harness {
	out := in
	out.Command = append([]string{}, in.Command...)
	out.InstallCommand = append([]string{}, in.InstallCommand...)
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
	return &out
}

func cloneInt64(in *int64) *int64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

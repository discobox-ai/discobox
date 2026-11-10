package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/cli/internal/console/bridge"
	"github.com/discobox-ai/discobox/pkg/desktopstage"
	"github.com/discobox-ai/discobox/pkg/serverstage"
)

// `discobox desktop` opens the console as a native app: the same console as
// `discobox console`, drawn by a desktop binary (desktop/, a Rust app on Rune)
// that this process starts and serves. The desktop app holds no credentials
// and knows no servers. This process serves it the console core over its
// stdin and stdout (console/bridge), so every transport, login and reconnect
// is the CLI's own.
//
// The design is ADR 26-11-07-876, -983 and -289; desktop/DESIGN.md is the
// app's, and docs/console/parity.md says what it lacks against the TUI.

// desktopBinaryEnv names a desktop binary to run in place of the one this CLI
// would run — for developing the desktop app against a released CLI, or
// running it on a machine that cannot download it.
const desktopBinaryEnv = "DISCOBOX_DESKTOP_BINARY"

func (a *App) newDesktopCommand() *cobra.Command {
	var (
		binary        string
		printSpec     bool
		printManifest bool
	)
	cmd := &cobra.Command{
		Use:   "desktop",
		Short: "Open the discobox console as a desktop app",
		Long: `Open the console as a native desktop app.

It is the same console as "discobox console": the discoboxes, and a box's
terminals beside them. It talks to the server through this command, so
--server, --token and every configured server work as they do for the
console.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if printSpec {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(console.NewSpec())
			}
			if printManifest {
				// What this build would download, and from where: the desktop
				// app's counterpart of `admin server manifest`, and what
				// release:verify checks against the archive the release built,
				// since the linker accepts an -X for a symbol that does not
				// exist without complaint.
				manifest, err := desktopstage.Default()
				if err != nil {
					return err
				}
				return writeJSON(cmd.OutOrStdout(), manifest)
			}
			return a.runDesktop(cmd, binary)
		},
	}
	cmd.Flags().BoolVar(&printSpec, "print-spec", false, "print the console's UX spec the desktop app is handed, and exit")
	_ = cmd.Flags().MarkHidden("print-spec")
	cmd.Flags().BoolVar(&printManifest, "print-manifest", false, "print the desktop app this build would download, and exit; a build that pins none says so and exits non-zero")
	cmd.Flags().StringVar(&binary, "binary", "", "desktop app to run instead of the one this CLI would find (also "+desktopBinaryEnv+")")
	_ = cmd.Flags().MarkHidden("binary")
	return cmd
}

func (a *App) runDesktop(cmd *cobra.Command, binary string) error {
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	resolver := a.desktopResolver(binary)
	progress := a.serverStartupLine()
	resolver.onProgress = func(report serverstage.Progress) {
		progress.set(desktopStageText(report))
	}
	path, release, err := resolver.resolve(ctx)
	progress.clear()
	if err != nil {
		return err
	}
	// Held until the app has exited: it marks the app's directory in use, so
	// no other CLI prunes it from under the running app.
	defer release()
	projectID, err := a.projectIDValue()
	if err != nil {
		return err
	}
	client, err := a.apiClient()
	if err != nil {
		return err
	}
	ds, err := newAPIDataSource(ctx, a, client, projectID)
	if err != nil {
		return err
	}
	defer ds.waitForPushes(cmd.ErrOrStderr())

	child := exec.CommandContext(ctx, path, "--bridge")
	child.Env = desktopEnv(os.Environ())
	// The app logs what the bridge carried, and that is text a box wrote.
	stderr := &terminalSafeLines{w: cmd.ErrOrStderr()}
	child.Stderr = stderr
	toChild, err := child.StdinPipe()
	if err != nil {
		return err
	}
	fromChild, err := child.StdoutPipe()
	if err != nil {
		return err
	}
	if err := child.Start(); err != nil {
		return fmt.Errorf("start the desktop app: %w", err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = bridge.NewServer(ds, toChild).Serve(ctx, fromChild)
	}()
	err = child.Wait()
	stderr.flush()
	cancel()
	<-served
	return err
}

// desktopEnv is the app's environment: this process's, less every discobox
// variable but the app's own (DISCOBOX_DESKTOP_*). The app holds no
// credentials and knows no servers (ADR 26-11-07-983 §1), so neither
// DISCOBOX_TOKEN nor a server's address reaches it.
func desktopEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(name), "DISCOBOX_") && !strings.HasPrefix(strings.ToUpper(name), "DISCOBOX_DESKTOP_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// terminalSafeLines writes the app's stderr to this terminal a line at a
// time, each through terminalSafeMultiline: the app logs requests and answers
// that carry what boxes wrote (names, messages, errors folded from a box's
// stderr), and a control sequence in them is display data, not an
// instruction to the user's terminal.
type terminalSafeLines struct {
	mu      sync.Mutex
	w       io.Writer
	pending []byte
}

func (t *terminalSafeLines) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending = append(t.pending, p...)
	for {
		i := bytes.IndexByte(t.pending, '\n')
		if i < 0 {
			break
		}
		line := string(t.pending[:i+1])
		t.pending = t.pending[i+1:]
		if _, err := io.WriteString(t.w, terminalSafeMultiline(line)); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// flush writes what is left after the app's last newline.
func (t *terminalSafeLines) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pending) > 0 {
		_, _ = io.WriteString(t.w, terminalSafeMultiline(string(t.pending))+"\n")
		t.pending = nil
	}
}

// desktopOSes are the platforms `discobox desktop` runs on. Linux only for now
// (ADR 26-11-07-983 §4): the app is built and tested only there, so no release
// has an app for anywhere else to download.
var desktopOSes = []string{"linux"}

// desktopResolver answers "which desktop app does this CLI run", staging the
// one its release pins when that is what it takes.
//
// It holds what it consults rather than reading it from the process, so a test
// can give it a platform, an executable, a cache and a manifest and get the
// answers a user would.
type desktopResolver struct {
	// binary is --binary, or DISCOBOX_DESKTOP_BINARY.
	binary string
	goos   string
	// executable is the running discobox; a development build looks beside it.
	executable string
	// stageRoot holds one unpacked app per digest. Empty when this machine
	// has no state directory, and then nothing is staged.
	stageRoot string
	// manifest is the desktop manifest this build was linked with.
	manifest   func() (desktopstage.Manifest, error)
	onProgress func(serverstage.Progress)
}

func (a *App) desktopResolver(flag string) desktopResolver {
	binary := flag
	if binary == "" {
		binary = strings.TrimSpace(os.Getenv(desktopBinaryEnv))
	}
	executable, err := os.Executable()
	if err == nil {
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
	}
	return desktopResolver{
		binary:     binary,
		goos:       runtime.GOOS,
		executable: executable,
		stageRoot:  stagedDesktopRoot(),
		manifest:   desktopstage.Default,
	}
}

// resolve returns the app to run and what to call once it has exited.
//
// The order is ADR 26-11-07-983 §3's: --binary, then DISCOBOX_DESKTOP_BINARY,
// run as named on any platform, for developing the app or running it offline.
// Then, where the desktop app is supported, the app this release pins,
// downloaded and verified; a development build, which pins none, runs the one
// beside it. PATH is not searched, for the server's reason (ADR 0099 §6).
func (r desktopResolver) resolve(ctx context.Context) (string, func(), error) {
	if r.binary != "" {
		named, err := filepath.Abs(r.binary)
		if err != nil {
			return "", nil, fmt.Errorf("desktop app %s: %w", r.binary, err)
		}
		if _, err := os.Stat(named); err != nil {
			return "", nil, fmt.Errorf("desktop app %s: %w", named, err)
		}
		return named, func() {}, nil
	}
	if !slices.Contains(desktopOSes, r.goos) {
		return "", nil, fmt.Errorf("discobox desktop is not yet supported on %s; it runs on %s",
			desktopOSName(r.goos), strings.Join(desktopOSNames(), ", "))
	}
	manifest, err := r.manifest()
	if errors.Is(err, desktopstage.ErrNoManifest) {
		if beside, ok := r.beside(); ok {
			return beside, func() {}, nil
		}
		return "", nil, fmt.Errorf("%w, and there is no %s beside %s\nBuild one with `go tool task build:desktop`, or name one with --binary or %s",
			err, desktopAppName(r.goos), r.executableName(), desktopBinaryEnv)
	}
	if err != nil {
		return "", nil, err
	}
	if !manifest.ForThisPlatform() {
		return "", nil, fmt.Errorf("this build's desktop app is for %s, and this is %s/%s", manifest.Platform(), runtime.GOOS, runtime.GOARCH)
	}
	if r.stageRoot == "" {
		return "", nil, fmt.Errorf("there is no state directory to keep the desktop app in: set HOME or XDG_STATE_HOME, or name an app with --binary or %s", desktopBinaryEnv)
	}
	if err := ensureStateDir(r.stageRoot); err != nil {
		return "", nil, err
	}
	// No client of its own: staging's honors the environment's proxy
	// settings, as the server's download does.
	app, err := desktopstage.Stage(ctx, manifest, desktopstage.Options{
		Root:       r.stageRoot,
		OnProgress: r.onProgress,
	})
	if err != nil {
		return "", nil, err
	}
	return app.Path, func() { _ = app.Close() }, nil
}

// beside is a desktop app next to this executable, where `go tool task
// build:desktop` puts one.
func (r desktopResolver) beside() (string, bool) {
	if r.executable == "" {
		return "", false
	}
	path := filepath.Join(filepath.Dir(r.executable), desktopAppName(r.goos))
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}
	return path, true
}

func (r desktopResolver) executableName() string {
	if r.executable == "" {
		return "discobox"
	}
	return r.executable
}

// desktopAppName is what a development build of the app is called.
func desktopAppName(goos string) string {
	if goos == "windows" {
		return "discobox-desktop.exe"
	}
	return "discobox-desktop"
}

// desktopOSName is a GOOS as a person calls it.
func desktopOSName(goos string) string {
	switch goos {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	}
	return goos
}

func desktopOSNames() []string {
	names := make([]string, 0, len(desktopOSes))
	for _, goos := range desktopOSes {
		names = append(names, desktopOSName(goos))
	}
	return names
}

// desktopStageText says what staging the desktop app is doing, in
// serverStageText's words.
func desktopStageText(report serverstage.Progress) string {
	if report.Done {
		return "Desktop app downloaded"
	}
	return "Downloading desktop app" + bytesSuffix(report.Current, report.Total, 0, 0)
}

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
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/cli/internal/console/bridge"
)

// `discobox desktop` opens the console as a native app: the same console as
// `discobox console`, drawn by a desktop binary (desktop/, a Rust app on Rune)
// that this process starts and serves. The desktop app holds no credentials
// and knows no servers. This process serves it the console core over its
// stdin and stdout (console/bridge), so every transport, login and reconnect
// is the CLI's own.
//
// The design is ADR 26-11-07-876, -983 and -289; desktop/README.md says what
// is built so far.

// desktopBinaryEnv names a desktop binary to run in place of the one this CLI
// would run — for developing the desktop app against a released CLI.
const desktopBinaryEnv = "DISCOBOX_DESKTOP_BINARY"

func (a *App) newDesktopCommand() *cobra.Command {
	var (
		binary    string
		printSpec bool
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
			return a.runDesktop(cmd, binary)
		},
	}
	cmd.Flags().BoolVar(&printSpec, "print-spec", false, "print the console's UX spec the desktop app is handed, and exit")
	_ = cmd.Flags().MarkHidden("print-spec")
	cmd.Flags().StringVar(&binary, "binary", "", "desktop app to run instead of the one this CLI would find (also "+desktopBinaryEnv+")")
	_ = cmd.Flags().MarkHidden("binary")
	return cmd
}

func (a *App) runDesktop(cmd *cobra.Command, binary string) error {
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	path, err := desktopBinary(binary)
	if err != nil {
		return err
	}
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

// desktopBinary finds the desktop app: the flag, then the environment, then one
// beside this executable (where `go tool task build:desktop` puts it), then one
// on PATH. A released CLI downloads the app its manifest pins instead (ADR
// 26-11-07-983 §2); until that lands, this is the only way it is found.
func desktopBinary(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := os.Getenv(desktopBinaryEnv); env != "" {
		return env, nil
	}
	name := "discobox-desktop"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if self, err := os.Executable(); err == nil {
		beside := filepath.Join(filepath.Dir(self), name)
		if _, err := os.Stat(beside); err == nil {
			return beside, nil
		}
	}
	if found, err := exec.LookPath(name); err == nil {
		return found, nil
	}
	return "", errors.New("no desktop app found: build it with `go tool task build:desktop`, or name one with --binary or " + desktopBinaryEnv)
}

package clipwatch

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/discobox-ai/discobox/cli/internal/wsl"
)

// The whole directory is embedded rather than the artifacts alone: go:embed
// requires its target to exist, and the artifacts are gitignored. The committed
// README keeps a fresh checkout building with none present.
//
//go:embed artifacts
var artifacts embed.FS

// maxLine is the longest report a line can be: MaxText units are at most three
// bytes each in UTF-8, in base64, after the sequence number and the flag.
var maxLine = base64.StdEncoding.EncodedLen(3*MaxText) + 32

// helperName is what the helper is installed as.
const helperName = "discobox-clipwatch.exe"

// Start watches the Windows clipboard from WSL, through the helper. Off WSL
// there is no Windows clipboard, and the Watcher it returns never goes live.
// Setting the helper up happens behind the call: until it reports, the
// Watcher is not live, and if it cannot be set up Err says why.
func Start(ctx context.Context) *Watcher {
	w := NewWatcher()
	if !wsl.Detected() {
		w.fail(ErrUnsupported)
		return w
	}
	go func() {
		if err := runHelper(ctx, w); err != nil && ctx.Err() == nil {
			w.fail(err)
		}
	}()
	return w
}

// runHelper installs the helper, runs it, and records what it reports until it
// ends.
func runHelper(ctx context.Context, w *Watcher) error {
	path, err := install(ctx)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, path)
	// The helper runs until its stdin closes, which is this process going
	// away: the pipe is held open and never written.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("clipwatch: start %s: %w", path, err)
	}
	lines := bufio.NewScanner(stdout)
	lines.Buffer(nil, maxLine)
	for lines.Scan() {
		text, image, err := decodeLine(lines.Text())
		if err != nil {
			continue
		}
		w.Record(text, image)
	}
	if err := lines.Err(); err != nil {
		// A helper this side can no longer read is one whose reports would
		// stop here while it went on running: end it, and the watch with it.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("clipwatch: read the helper: %w", err)
	}
	err = cmd.Wait()
	if detail := strings.TrimSpace(stderr.String()); detail != "" {
		return fmt.Errorf("clipwatch: helper exited: %w: %s", err, detail)
	}
	return fmt.Errorf("clipwatch: helper exited: %w", err)
}

// install puts the helper where Windows can run it, and returns its path on
// this side.
//
// It goes under %LOCALAPPDATA%\discobox\bin\<digest>, named by what it is:
// two consoles built from the same source share one copy, a different build
// gets its own, and a copy that is there is never rewritten — rewriting an
// executable another console has running fails on Windows. It is on the
// Windows drive rather than in this distribution because a Windows program
// started from a \\wsl$ path runs, but loads every page over 9P.
func install(ctx context.Context) (string, error) {
	exe, err := helper()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(exe)
	appData, err := localAppData(ctx)
	if err != nil {
		return "", err
	}
	bin := filepath.Join(appData, "discobox", "bin")
	digest := hex.EncodeToString(sum[:8])
	dir := filepath.Join(bin, digest)
	path := filepath.Join(dir, helperName)
	defer prune(bin, digest)
	if info, err := os.Stat(path); err == nil && info.Size() == int64(len(exe)) {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("clipwatch: %w", err)
	}
	tmp, err := os.CreateTemp(dir, helperName+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("clipwatch: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(exe); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("clipwatch: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("clipwatch: %w", err)
	}
	//nolint:gosec // an executable: Windows ignores the bits, WSL's interop wants x on drvfs
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", fmt.Errorf("clipwatch: %w", err)
	}
	// Another console may have put it there meanwhile; its copy is this one.
	if err := os.Rename(tmp.Name(), path); err != nil {
		if _, statErr := os.Stat(path); statErr != nil {
			return "", fmt.Errorf("clipwatch: %w", err)
		}
	}
	return path, nil
}

// prune removes the helpers other builds installed under bin, keeping this
// one's. One another console still runs is refused by Windows — an executable
// that is running cannot be deleted — and stays for that console; the rest
// would otherwise pile up, one per build, for as long as the machine has
// Windows on it.
func prune(bin, keep string) {
	entries, err := os.ReadDir(bin)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != keep {
			_ = os.RemoveAll(filepath.Join(bin, entry.Name()))
		}
	}
}

// helper is the embedded Windows helper for this machine's architecture. WSL2
// does not emulate, so Windows runs the architecture this binary was built for.
func helper() ([]byte, error) {
	name := "artifacts/discobox-clipwatch.windows-" + runtime.GOARCH + ".exe.gz"
	compressed, err := artifacts.ReadFile(name)
	if err != nil {
		return nil, errors.New("clipwatch: the Windows helper was not built into this binary; run `go tool task build:clipwatch`")
	}
	r, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("clipwatch: %s: %w", name, err)
	}
	return io.ReadAll(r)
}

// localAppData is %LOCALAPPDATA%, spelled the way this side opens it.
func localAppData(ctx context.Context) (string, error) {
	value, err := wsl.Env(ctx, "LOCALAPPDATA")
	if err != nil {
		return "", fmt.Errorf("clipwatch: %w", err)
	}
	local, err := wsl.LinuxPath(ctx, value)
	if err != nil {
		return "", fmt.Errorf("clipwatch: %w", err)
	}
	return local, nil
}

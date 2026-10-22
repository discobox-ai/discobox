package endpoint

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/adrg/xdg"
)

// An autolaunched server has no terminal: its output is the only account of
// what it did, and it is written where the user can still read it tomorrow.
//
// The file is appended to rather than truncated per launch, because the run
// worth reading is usually the one that already ended — a server that died and
// was restarted by the next command would otherwise take its own explanation
// with it. Each launch writes a banner first so the runs stay separable, and
// the file rotates once so appending forever cannot fill a disk.
const (
	serverLogName = "server.log"
	// serverLogBanner opens every launch's output. logTail looks for the last
	// one so a failure reports this run rather than the history above it.
	serverLogBanner = "=== discobox server launch"
	// serverLogRotateBytes is where the current log becomes the previous one.
	serverLogRotateBytes = 8 << 20
	// serverLogTailBytes is how much of a launch an error carries, and
	// serverLogScanBytes how far back the banner is looked for.
	serverLogTailBytes = 4 << 10
	serverLogScanBytes = 64 << 10
)

// ServerLogPath is where an autolaunched server's output is kept: with the
// server's own state, which is the same directory the server resolves for
// DISCOBOX_STATE_DIR. Not beside the socket, which lives in a runtime directory
// the system clears — a log that is gone by the next login is not a log.
func ServerLogPath() string {
	if dir := strings.TrimSpace(os.Getenv("DISCOBOX_STATE_DIR")); dir != "" {
		return filepath.Join(dir, serverLogName)
	}
	return filepath.Join(xdg.StateHome, "discobox", serverLogName)
}

// PreviousServerLogPath is the log rotated out of the way of the current one.
func PreviousServerLogPath(path string) string { return path + ".1" }

// openServerLog opens the log for a launch about to happen, rotating it first
// when it has outgrown its limit and stamping it with what is being started.
func openServerLog(path, command string, args []string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create server log directory: %w", err)
	}
	if info, err := os.Stat(path); err == nil && info.Size() >= serverLogRotateBytes {
		// Best effort: a log that cannot be rotated is still a log to append to.
		_ = os.Rename(path, PreviousServerLogPath(path))
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open server log: %w", err)
	}
	argv := strings.Join(append([]string{command}, args...), " ")
	if _, err := fmt.Fprintf(file, "\n%s %s: %s\n", serverLogBanner, time.Now().Format(time.RFC3339), argv); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write server log: %w", err)
	}
	return file, nil
}

// lastServerLogLaunch is the end of the log, trimmed to the last launch it
// records. Empty when there is nothing to show, so a caller can append it
// unconditionally.
func lastServerLogLaunch(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	data := make([]byte, min(info.Size(), int64(serverLogScanBytes)))
	if len(data) == 0 {
		return ""
	}
	if _, err := file.ReadAt(data, info.Size()-int64(len(data))); err != nil {
		return ""
	}
	if index := bytes.LastIndex(data, []byte(serverLogBanner)); index >= 0 {
		data = data[index:]
	}
	if len(data) > serverLogTailBytes {
		data = data[len(data)-serverLogTailBytes:]
	}
	return strings.TrimSpace(string(data))
}

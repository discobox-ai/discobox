package shim

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/obot-platform/discobox/sandbox-agent/terminal"
)

func TestRunRecordsExitStatus(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := Run(ctx, Config{
		TerminalID:  "agt_test",
		AgentID:     "test",
		Command:     []string{"sh", "-c", "exit 7"},
		Workdir:     dir,
		SocketPath:  filepath.Join(dir, "shim.sock"),
		RuntimePath: filepath.Join(dir, "runtime.json"),
		Rows:        24,
		Cols:        80,
	})
	if err != nil {
		t.Fatalf("run shim: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "runtime.json"))
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	var status terminal.Terminal
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatalf("parse runtime: %v", err)
	}
	if status.Status != terminal.StatusFailed {
		t.Fatalf("status = %q", status.Status)
	}
	if status.ExitCode == nil || *status.ExitCode != 7 {
		t.Fatalf("exit code = %#v", status.ExitCode)
	}
	if status.ExitedAt == nil {
		t.Fatalf("exitedAt was not recorded")
	}
}

package monitors

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// State is what an instance is doing, as its runner last wrote it.
type State string

const (
	// StateStarting is an enabled instance whose runner has not reported yet.
	StateStarting State = "starting"
	// StateRunning is a streaming monitor running, or a polling one mid-run.
	StateRunning State = "running"
	// StateWaiting is a polling monitor between runs.
	StateWaiting State = "waiting"
	// StateBackoff is a streaming monitor that failed, waiting to restart.
	StateBackoff State = "backoff"
	// StateFailed is an instance that cannot run at all — its declaration is
	// gone or broken — and is waiting for that to change.
	StateFailed State = "failed"
	// StateDone is an instance that ended itself: a final event, or a
	// streaming monitor that exited successfully. Its file is gone.
	StateDone State = "done"
)

// Status is an instance's status file, `status.json` in its state directory.
// Only its runner writes it.
type Status struct {
	Instance  string    `json:"instance"`
	Monitor   string    `json:"monitor"`
	State     State     `json:"state"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Runs counts the times the monitor was started.
	Runs        int        `json:"runs"`
	LastRunAt   *time.Time `json:"lastRunAt,omitempty"`
	NextRunAt   *time.Time `json:"nextRunAt,omitempty"`
	Events      int        `json:"events"`
	LastEventAt *time.Time `json:"lastEventAt,omitempty"`
	// Failing is set from a failed run until one succeeds.
	Failing   bool   `json:"failing,omitempty"`
	LastError string `json:"lastError,omitempty"`
}

const (
	statusFile = "status.json"
	logFile    = "stderr.log"
	// maxLog is where the stderr log is rotated, keeping one older file.
	maxLog = 1 << 20
)

// ReadStatus is the status the instance's runner last wrote; ok is false when
// it has written none.
func ReadStatus(d Dirs, name string) (Status, bool, error) {
	data, err := os.ReadFile(filepath.Join(d.InstanceStateDir(name), statusFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Status{}, false, nil
		}
		return Status{}, false, err
	}
	var st Status
	if err := json.Unmarshal(data, &st); err != nil {
		return Status{}, false, err
	}
	return st, true, nil
}

// Finished is every instance that ended itself and has not been disabled since:
// its file is gone and its status says done. Status lists them so an agent
// sees how they ended.
func Finished(d Dirs) ([]Status, error) {
	entries, err := os.ReadDir(d.State)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Status
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(d.enabledFile(entry.Name())); err == nil {
			continue
		}
		st, ok, err := ReadStatus(d, entry.Name())
		if err != nil || !ok || st.State != StateDone {
			continue
		}
		out = append(out, st)
	}
	return out, nil
}

// LogPaths are the instance's stderr logs, older first; either may be absent.
func LogPaths(d Dirs, name string) []string {
	current := filepath.Join(d.InstanceStateDir(name), logFile)
	return []string{current + ".1", current}
}

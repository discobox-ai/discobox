package sandboxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrHarnessChangeRefused means no new harness may be accepted for this runtime.
var ErrHarnessChangeRefused = errors.New("stop the sandbox before changing its harness")

// The fence lives outside the disposable container, and is never timed out: a
// server or pool restart must not let a start race accepted replacement intent.
type harnessChangeFence struct {
	ID      string
	Applied bool
}

func (r *DockerSandboxRuntime) harnessFencePath(id string) string {
	return filepath.Join(r.sandboxRoot(id), "harness-change.json")
}

func (r *DockerSandboxRuntime) readHarnessFence(id string) (harnessChangeFence, error) {
	var f harnessChangeFence
	data, err := os.ReadFile(r.harnessFencePath(id))
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	err = json.Unmarshal(data, &f)
	return f, err
}

func (r *DockerSandboxRuntime) writeHarnessFence(id string, f harnessChangeFence) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	path := r.harnessFencePath(id)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// reserveHarnessChange holds the same lock as every power transition. Retrying
// the same ID is safe after the replacement has landed, even if it has started.
func (r *DockerSandboxRuntime) reserveHarnessChange(ctx context.Context, id, change string) (*Sandbox, error) {
	lock := r.sandboxLock(id)
	lock.Lock()
	defer lock.Unlock()
	if r.isArchived(id) {
		return nil, ErrArchived
	}
	f, err := r.readHarnessFence(id)
	if err != nil {
		return nil, err
	}
	sb, err := r.GetSandbox(ctx, id)
	if err != nil {
		return nil, err
	}
	if f.ID == change {
		return sb, nil
	}
	if f.ID != "" && !f.Applied {
		return nil, fmt.Errorf("%w: another change is pending", ErrHarnessChangeRefused)
	}
	if sb.Status != StatusStopped && sb.Status != StatusCreated {
		return nil, ErrHarnessChangeRefused
	}
	if err := r.writeHarnessFence(id, harnessChangeFence{ID: change}); err != nil {
		return nil, err
	}
	return sb, nil
}

func (r *DockerSandboxRuntime) harnessStartAllowed(id string) error {
	f, err := r.readHarnessFence(id)
	if err != nil {
		return err
	}
	if f.ID != "" && !f.Applied {
		return fmt.Errorf("%w: harness change is being applied", ErrHarnessChangeRefused)
	}
	return nil
}

// Called under the power lock before any side effects of a create. Retaining
// the applied ID fences late creates from an older server generation too.
func (r *DockerSandboxRuntime) harnessCreateAllowed(id, change string) error {
	f, err := r.readHarnessFence(id)
	if err != nil {
		return err
	}
	if f.ID != "" && f.ID != change {
		return fmt.Errorf("%w: stale harness change", ErrHarnessChangeRefused)
	}
	return nil
}

func (r *DockerSandboxRuntime) finishHarnessChange(id, change string) error {
	if change == "" {
		return nil
	}
	return r.writeHarnessFence(id, harnessChangeFence{ID: change, Applied: true})
}

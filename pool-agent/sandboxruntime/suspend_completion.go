package sandboxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

// A successful freeze must supersede older explicit starts before releasing
// the runtime power lock, including across control-plane instances. A pending
// receipt survives agent restart and a lost callback response. Starts finish
// that receipt before checking their own epoch. It records no running intent.
const suspendCompletionFile = ".discobox-suspend-completion.json"

type suspendCompletion struct {
	RequestID string `json:"requestId"`
	Epoch     int64  `json:"epoch"`
	Frozen    bool   `json:"frozen"`
}

func (r *sandboxCore) finishSuspend(ctx context.Context, sandboxID string) error {
	path := filepath.Join(r.sandboxRoot(sandboxID), suspendCompletionFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var receipt suspendCompletion
	if err := json.Unmarshal(data, &receipt); err != nil {
		return err
	}
	if !receipt.Frozen {
		// Preparation is not a successful freeze. A restart before the runtime
		// acted must not invent one; inspect before any other power transition.
		if r.boxes == nil {
			return fmt.Errorf("cannot verify prepared suspend")
		}
		sb, err := r.boxes.GetSandbox(ctx, sandboxID)
		if errors.Is(err, ErrNotFound) {
			return os.Remove(path)
		}
		if err != nil {
			return err
		}
		if sb.Status != StatusSuspended {
			return os.Remove(path)
		}
	}
	if receipt.RequestID == "" || r.completeSuspend == nil {
		return fmt.Errorf("successful suspend completion is unavailable")
	}
	if err := r.completeSuspend(ctx, sandboxID, receipt.RequestID, receipt.Epoch); err != nil {
		return err
	}
	return os.Remove(path)
}

// Called under the runtime power lock. Stage before freeze so a process death
// cannot lose a completed freeze; an answered refusal removes the staged record
// and changes no wake epoch. A successful completion is idempotent on the server.
func (r *sandboxCore) suspendWithCompletion(ctx context.Context, sandboxID string, req *workerapimodel.PoolSandboxOperationRequest, freeze func() error) error {
	if err := r.finishSuspend(ctx, sandboxID); err != nil {
		return err
	}
	if req == nil || !req.SuspendId.IsSet() {
		return freeze()
	}
	if r.completeSuspend == nil {
		return fmt.Errorf("successful suspend completion is unavailable")
	}
	path := filepath.Join(r.sandboxRoot(sandboxID), suspendCompletionFile)
	receipt := suspendCompletion{RequestID: req.SuspendId.Value, Epoch: req.SuspendEpoch.Value}
	if err := writeSuspendCompletion(path, receipt); err != nil {
		return err
	}
	if err := freeze(); err != nil {
		return errors.Join(err, os.Remove(path))
	}
	receipt.Frozen = true
	if err := writeSuspendCompletion(path, receipt); err != nil {
		return err
	}

	// Freeze already happened; a lost caller must not abandon its ordering.
	done, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return r.finishSuspend(done, sandboxID)
}

func writeSuspendCompletion(path string, receipt suspendCompletion) error {
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

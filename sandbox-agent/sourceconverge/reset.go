package sourceconverge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"

	"github.com/discobox-ai/discobox/pkg/fsdurable"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

type appliedResets struct {
	Owner     string           `json:"owner"`
	Revisions map[string]int64 `json:"revisions"`
	InFlight  map[string]int64 `json:"inFlight,omitempty"`
}

func (c *Converger) readAppliedResets() (appliedResets, error) {
	state := appliedResets{Owner: c.cfg.ResetOwner, Revisions: map[string]int64{}}
	if c.cfg.ResetStatePath == "" {
		return state, errors.New("source resets require persistent agent state")
	}
	data, err := os.ReadFile(c.cfg.ResetStatePath)
	if errors.Is(err, fs.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	var saved appliedResets
	if err := json.Unmarshal(data, &saved); err != nil {
		return state, err
	}
	if saved.Owner == state.Owner && saved.Revisions != nil {
		return saved, nil
	}
	return state, nil
}

// AppliedResets returns the durable acknowledgements for currently known
// source resets. A different sandbox's imported state carries no authority.
func (c *Converger) AppliedResets() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.readAppliedResets()
	if err != nil {
		return nil
	}
	return maps.Clone(state.Revisions)
}

func (c *Converger) resetSource(ctx context.Context, repo *repository, pool string, docRevision int64, source sandboxconfig.RuntimeSource) (string, error) {
	c.mu.Lock()
	if c.desired == nil || c.desired.Revision != docRevision {
		c.mu.Unlock()
		return "", nil
	}
	intent, exists := c.desired.SourceResets[source.Slug]
	homeTarget := c.homeTargets[source.Slug]
	c.mu.Unlock()
	if !exists {
		return "", nil
	}
	if err := intent.Validate(); err != nil {
		return "", err
	}
	if homeTarget {
		return "", errors.New("a skills checkout cannot be reset as a work source")
	}
	state, err := c.readAppliedResets()
	if err != nil {
		return "", err
	}
	if state.Revisions[source.Slug] >= intent.Revision {
		return intent.Commit, nil
	}
	if state.InFlight[source.Slug] >= intent.Revision {
		return "", errors.New("an earlier reset was interrupted before it could be acknowledged; publish a new explicit reset to retry")
	}
	// Git runs as the checkout's owner. Fetch the exact publication ref
	// before changing the index or worktree.
	if err := repo.run(ctx, nil, "fetch", "--no-tags", "--", pool, intent.Ref()); err != nil {
		return "", fmt.Errorf("fetch published reset commit: %w", err)
	}
	commit, err := repo.output(ctx, repo.dir, nil, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", err
	}
	if commit != intent.Commit {
		return "", errors.New("the reset ref does not contain the published commit")
	}
	// Fetch may call back into the credential broker, so only hold the
	// desired-document lock while checking and applying the local reset.
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.desired == nil || c.desired.SourceResets[source.Slug] != intent {
		return "", nil
	}
	if state.InFlight == nil {
		state.InFlight = make(map[string]int64)
	}
	state.InFlight[source.Slug] = intent.Revision
	if err := c.saveAppliedResets(state); err != nil {
		return "", err
	}
	if err := repo.run(ctx, nil, "reset", "--hard", commit); err != nil {
		return "", err
	}
	state.Revisions[source.Slug] = intent.Revision
	delete(state.InFlight, source.Slug)
	if err := c.saveAppliedResets(state); err != nil {
		return "", fmt.Errorf("record reset of %s: %w", filepath.Base(repo.dir), err)
	}
	return commit, nil
}

func (c *Converger) saveAppliedResets(state appliedResets) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return fsdurable.WriteFile(c.cfg.ResetStatePath, data, 0o600)
}

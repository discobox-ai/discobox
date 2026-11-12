package provision

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RetryDir holds the reprovision requests made in the box
// (`discobox-provision retry`, ADR 26-11-11-560 §5). A box's processes run as
// its user and a run needs root, so they ask the agent through a file, as
// discobox-power asks for sleep: every regular file in it is a request. It is
// writable by everyone and sticky, so one user cannot take another's request
// away, and it is on /run, so a request nothing took dies with the boot.
const RetryDir = "/run/discobox/reprovision"

// retryPoll is how often the agent looks for a request, and a requester for
// its request having been taken.
var retryPoll = 250 * time.Millisecond

// retryTaken bounds how long a requester waits for the agent to take its
// request before it gives up and withdraws it.
var retryTaken = 30 * time.Second

// RequestRetry asks the agent for a reprovision and waits until it has taken
// the request: the run it started, or joined, is then in the document. A
// request the agent does not take in time is withdrawn.
func RequestRetry(ctx context.Context, dir string) error {
	request, err := os.CreateTemp(dir, "retry-*")
	if err != nil {
		return fmt.Errorf("ask the sandbox agent to reprovision: %w", err)
	}
	path := request.Name()
	if err := request.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	deadline := time.NewTimer(retryTaken)
	defer deadline.Stop()
	for {
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		select {
		case <-ctx.Done():
			_ = os.Remove(path)
			return ctx.Err()
		case <-deadline.C:
			_ = os.Remove(path)
			return fmt.Errorf("the sandbox agent did not take the reprovision request within %s; is it running?", retryTaken)
		case <-time.After(retryPoll):
		}
	}
}

// WatchRetries makes dir the request directory, and calls reprovision once
// for whatever requests are in it each time it looks, before it removes
// them: a requester sees its request gone only once the run has started or
// been joined. It returns when ctx ends.
func WatchRetries(ctx context.Context, dir string, reprovision func()) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Chmod after, because MkdirAll's mode is filtered through the umask.
	if err := os.Chmod(dir, os.ModeSticky|0o777); err != nil {
		return err
	}
	ticker := time.NewTicker(retryPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		var requests []string
		for _, entry := range entries {
			if entry.Type().IsRegular() && !strings.HasPrefix(entry.Name(), ".") {
				requests = append(requests, filepath.Join(dir, entry.Name()))
			}
		}
		if len(requests) == 0 {
			continue
		}
		reprovision()
		for _, path := range requests {
			_ = os.Remove(path)
		}
	}
}

package provision

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/provisionspec"
)

// waitPoll is how often a waiter reads provision.json again.
var waitPoll = 200 * time.Millisecond

// Wait blocks until every feature named is done in the document at path
// (ADR 26-10-16-423 §5): ready, failed or off. A feature the box does not
// report once its run has started is not one it provisions — its image came
// with it — and is done. waiting is called once, with what is still being
// provisioned, if Wait has to wait at all.
func Wait(ctx context.Context, path string, features []string, waiting func(provisionspec.Status, []string)) (provisionspec.Status, error) {
	told := false
	for {
		status, ok, err := provisionspec.ReadStatus(path)
		if err != nil {
			return status, err
		}
		var pending []string
		for _, name := range features {
			feature, reported := status.Features[name]
			switch {
			case !ok, !reported && status.State == provisionspec.StatePending:
				pending = append(pending, name)
			case reported && !feature.State.Done():
				pending = append(pending, name)
			}
		}
		if len(pending) == 0 {
			return status, nil
		}
		if !told && waiting != nil {
			waiting(status, pending)
			told = true
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-time.After(waitPoll):
		}
	}
}

// Problem is why feature is not usable, by the document: off or failed. It
// is nil for a feature that is ready, or that the box does not provision.
func Problem(status provisionspec.Status, name string) error {
	feature := status.Features[name]
	switch feature.State {
	case provisionspec.StateOff:
		return fmt.Errorf("the %s feature is off (%s): that was this project's choice, so do not install it by hand", name, feature.Reason)
	case provisionspec.StateFailed:
		if feature.Log != "" {
			return fmt.Errorf("the %s feature failed to provision: %s (log: %s)", name, feature.Error, feature.Log)
		}
		return fmt.Errorf("the %s feature failed to provision: %s", name, feature.Error)
	}
	return nil
}

// LookPath finds command on PATH as a placeholder's real command: skipping
// the placeholders themselves, which it would otherwise find again.
func LookPath(command, path string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || filepath.Clean(dir) == PlaceholderDir {
			continue
		}
		candidate := filepath.Join(dir, command)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New(command + ": not found on PATH")
}

// Describe is what a waiter tells a person about the features it waits for.
func Describe(status provisionspec.Status, pending []string) string {
	var parts []string
	for _, name := range slices.Sorted(slices.Values(pending)) {
		feature := status.Features[name]
		part := name
		if feature.Step != "" {
			part += " (step " + feature.Step + ")"
		}
		if feature.Log != "" {
			part += ", log " + feature.Log
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

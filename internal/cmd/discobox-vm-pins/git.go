package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// git runs git in one checkout.
type git struct{ dir string }

// run returns git's output with surrounding whitespace trimmed.
func (g git) run(ctx context.Context, args ...string) (string, error) {
	//nolint:gosec // G204: git, with this tool's own arguments and the release's refs.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// lines returns git's non-empty output lines.
func (g git) lines(ctx context.Context, args ...string) ([]string, error) {
	out, err := g.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var lines []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

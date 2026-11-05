package sandboxgit

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Command constructs a Git operation with this client's authentication policy
// and cancellation of its entire process tree (Git starts SSH and helpers).
func Command(ctx context.Context, dir string, env map[string]string, args ...string) *exec.Cmd {
	if off, _ := ctx.Value(noninteractiveKey{}).(bool); off && os.Getenv("GIT_SSH_COMMAND") == "" && os.Getenv("GIT_SSH") == "" {
		config := exec.CommandContext(ctx, "git", "config", "--get", "core.sshCommand")
		config.Dir = dir
		config.WaitDelay = time.Second
		if command, err := config.Output(); err == nil && strings.TrimSpace(string(command)) != "" {
			env = maps.Clone(env)
			if env == nil {
				env = map[string]string{}
			}
			env["GIT_SSH_COMMAND"] = strings.TrimSpace(string(command))
		}
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for key, value := range Environment(ctx, env) {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.WaitDelay = time.Second
	if off, _ := ctx.Value(noninteractiveKey{}).(bool); off {
		cancelProcessTree(cmd)
	}
	return cmd
}

// Output runs a Git transport or credential operation under the shared policy.
func Output(ctx context.Context, dir string, input []byte, env map[string]string, args ...string) (string, error) {
	cmd := Command(ctx, dir, env, args...)
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

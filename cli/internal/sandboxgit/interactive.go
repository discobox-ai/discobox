package sandboxgit

import (
	"context"
	"maps"
	"os"
	"strings"
)

type noninteractiveKey struct{}

// WithNoninteractive suppresses authentication questions for this operation.
func WithNoninteractive(ctx context.Context) context.Context {
	return context.WithValue(ctx, noninteractiveKey{}, true)
}

// Noninteractive reports whether authentication questions are forbidden.
func Noninteractive(ctx context.Context) bool {
	off, _ := ctx.Value(noninteractiveKey{}).(bool)
	return off
}

// Environment applies the operation's no-question policy, preserving interactive IDE helpers otherwise.
func Environment(ctx context.Context, base map[string]string) map[string]string {
	if off, _ := ctx.Value(noninteractiveKey{}).(bool); !off {
		return base
	}
	env := maps.Clone(base)
	if env == nil {
		env = map[string]string{}
	}
	env["GIT_TERMINAL_PROMPT"] = "0"
	env["GCM_INTERACTIVE"] = "never"
	env["GCM_GUI_PROMPT"] = "0"
	env["GIT_ASKPASS"] = ""
	env["SSH_ASKPASS"] = ""
	env["SSH_ASKPASS_REQUIRE"] = "never"
	// OpenSSH uses the first value, so put policy options before user arguments.
	ssh := env["GIT_SSH_COMMAND"]
	if ssh == "" {
		ssh = os.Getenv("GIT_SSH_COMMAND")
	}
	if ssh == "" {
		if executable := os.Getenv("GIT_SSH"); executable != "" {
			ssh = "'" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "'"
		}
	}
	if ssh == "" {
		ssh = "ssh"
	}
	// Keep a quoted executable (including Git for Windows paths with spaces)
	// intact, then insert OpenSSH's first-value-wins options before its args.
	boundary := len(ssh)
	var quote rune
	escaped := false
	for i, r := range ssh {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if r == ' ' || r == '\t' {
			boundary = i
			break
		}
	}
	env["GIT_SSH_COMMAND"] = ssh[:boundary] + " -oBatchMode=yes -oStrictHostKeyChecking=yes" + ssh[boundary:]

	env["GIT_SSH_VARIANT"] = "ssh"
	return env
}

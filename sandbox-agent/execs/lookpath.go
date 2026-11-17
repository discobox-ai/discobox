package execs

import (
	"fmt"
	"path/filepath"
	"strings"
)

// lookPath resolves a program name against the environment the command will
// run in (env, NAME=value entries), not this process's. They are not the same PATH — the agent is root and the
// command runs as the run user, and a shim's own environment is not the exec's
// (#1076) — and a name is looked up where the command's own PATH says, or the
// wrong binary answers to it.
//
// A name carrying a separator is a path already and is taken as one. Anything
// else is looked for in the PATH's absolute entries only: a relative entry
// would be read here against this process's directory and then run against the
// command's, which are not the same place — that mismatch is the whole of what
// os/exec's ErrDot exists to refuse.
//
// What counts as executable is the platform's (executableIn, with the
// command's PATHEXT on Windows). It is a filter,
// not a permission: this process is root and the command runs as the run user,
// so what it finds is what would be tried, not what will be allowed.
func lookPath(program string, env []string) (string, error) {
	if strings.ContainsAny(program, `/\`) {
		return program, nil
	}
	pathext := envValue(env, "PATHEXT")
	for _, dir := range filepath.SplitList(envValue(env, "PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		if found, ok := executableIn(dir, program, pathext); ok {
			return found, nil
		}
	}
	return "", fmt.Errorf("%s is not on this command's PATH", program)
}

// envValue is the value a process given env would read for name: the last
// entry wins, as os/exec keeps it, and names compare as the platform's do
// (envNameEqual: Windows ignores case, so Path is PATH there).
func envValue(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		key, value, ok := strings.Cut(env[i], "=")
		if ok && envNameEqual(key, name) {
			return value
		}
	}
	return ""
}

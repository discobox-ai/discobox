package dockercache

import (
	"context"
	"fmt"
	"os"
	"syscall"
)

// Run executes a user's docker command line and returns the exit code to use.
//
// Every command is exec'd, replacing this process, so stdio, TTY behavior and
// signal handling are the real docker CLI's with no wrapper in the data path.
// The only work done first is ensuring the pool builder exists, and only for a
// command that is going to use it.
func Run(args []string) int {
	a := Rewrite(args)
	if a.Rewritten {
		// Best-effort: a builder that cannot be created leaves the build to
		// fail against a missing instance with buildx's own error, which says
		// far more than anything this shim could substitute.
		if err := EnsureBuilder(context.Background()); err != nil {
			notice(fmt.Sprintf("pool builder unavailable: %v", err))
		}
	}
	return execDirect(a.Argv)
}

// execDirect replaces this process with the real docker CLI.
func execDirect(argv []string) int {
	//nolint:gosec // Handing this process's own argv to the real docker CLI is the entire point of the shim.
	if err := syscall.Exec(argv[0], argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "discobox-docker: exec %s: %v\n", argv[0], err)
		return 127
	}
	return 0
}

// notice reports something the user should know about the shim itself, marked
// so it is not mistaken for output from their build.
func notice(msg string) {
	fmt.Fprintf(os.Stderr, "discobox-docker: %s\n", msg)
}

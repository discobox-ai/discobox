package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
)

// runPrimary is a foreground job of the terminal's login shell. Only the
// primary terminal uses it; auxiliary shells and tools bypass the gate.
func runPrimary(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "discobox-primary-run: missing source root")
		return 2
	}
	hook := filepath.Join(args[0], ".discobox", "scripts", "pre-harness")
	if code, err := primaryGate(args[0], hook); err != nil {
		fmt.Fprintf(os.Stderr, "discobox: %s failed (status %d): %v\n", hook, code, err)
		return code
	}
	if len(args) == 1 {
		return 0 // Return to the existing login shell, without nesting one.
	}
	path, err := exec.LookPath(args[1])
	if err == nil {
		err = syscall.Exec(path, args[1:], os.Environ())
	}
	fmt.Fprintf(os.Stderr, "discobox: start harness %q: %v\n", args[1], err)
	return 127
}

func primaryGate(root, hook string) (int, error) {
	_, err := os.Lstat(hook)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 126, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return 126, err
	}
	resolved, err := filepath.EvalSymlinks(hook)
	if err != nil {
		return 126, err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || !filepath.IsLocal(rel) {
		return 126, fmt.Errorf("the hook must resolve inside the source checkout")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return 126, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return 126, fmt.Errorf("the hook must be a regular executable file")
	}
	// Both processes share the shell's foreground process group. Let the
	// child receive terminal signals, but keep the runner alive to print the
	// failure and prevent continuation even if the hook catches an interrupt.
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(signals)
	cmd := exec.CommandContext(context.Background(), resolved)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	select {
	case sig := <-signals:
		number, _ := sig.(syscall.Signal)
		return 128 + int(number), fmt.Errorf("interrupted by %s", sig)
	default:
	}
	if err != nil {
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			status, _ := exited.Sys().(syscall.WaitStatus)
			if status.Signaled() {
				return 128 + int(status.Signal()), err
			}
			return exited.ExitCode(), err
		}
		return 126, err
	}
	return 0, nil
}

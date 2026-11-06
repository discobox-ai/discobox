package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// hostCommand enters the VM host through a bind supplied by its Docker driver.
// This is an ordinary local exec; no listener or remote command protocol exists.
func hostCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("host command is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	argv := append([]string{"--target=1", "--mount", "--root", "--wd=/", "--"}, args...)
	// Keep this process in its container root: Docker exec resolves its next
	// executable through the container's init process, which must still see /relay.
	command := exec.CommandContext(ctx, "/usr/bin/nsenter", argv...)
	command.SysProcAttr = &syscall.SysProcAttr{Chroot: "/host"}
	command.Dir = "/"
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

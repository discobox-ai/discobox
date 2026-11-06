package main

import (
	"context"
	"os/exec"
	"strconv"
	"time"
)

// taskkill reaches Docker's buildx plugin and builder setup's task children.
func cancelProcessTree(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return cmd.Process.Kill()
	}
}

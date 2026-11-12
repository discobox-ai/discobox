package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// chmodPath is macOS's chmod, named absolutely so no PATH can put another
// program in its place.
const chmodPath = "/bin/chmod"

// clearACL removes every access control entry from what is at path. macOS
// keeps a directory's ACL through a change of its owner or mode, so a
// directory the account once held can still let the account add and remove
// names in it after the pool has made it root's 0755 again: its mode bits do
// not say who may write it until the ACL is gone (chmod -N).
func clearACL(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, chmodPath, "-N", path).CombinedOutput(); err != nil {
		return fmt.Errorf("clear the ACL of %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

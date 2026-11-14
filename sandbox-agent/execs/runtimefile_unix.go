//go:build !windows

package execs

import (
	"os"

	"github.com/discobox-ai/discobox/pkg/fsdurable"
)

// replaceRuntimeFile renames a staged runtime record over the live one. A
// rename replaces its target atomically, whoever has it open.
func replaceRuntimeFile(staged *fsdurable.File) error {
	return staged.Commit()
}

// readRuntimeFile reads a runtime record. A reader racing a rename sees the
// old file or the new one, never neither.
func readRuntimeFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

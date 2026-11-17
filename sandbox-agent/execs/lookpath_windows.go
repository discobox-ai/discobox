//go:build windows

package execs

import (
	"os"
	"path/filepath"
	"strings"
)

// executableIn is dir's program as Windows finds one: the name as given when
// it has an extension, else the name with each of pathext's (the command's
// PATHEXT) in turn, or Windows' default list when it sets none. Windows has no
// executable bit; the extension is what makes a file one.
func executableIn(dir, program, pathext string) (string, bool) {
	exts := strings.Split(pathext, ";")
	if strings.TrimSpace(pathext) == "" {
		exts = []string{".com", ".exe", ".bat", ".cmd"}
	}
	candidates := make([]string, 0, len(exts)+1)
	if filepath.Ext(program) != "" {
		candidates = append(candidates, program)
	}
	for _, ext := range exts {
		if ext = strings.TrimSpace(ext); ext != "" {
			candidates = append(candidates, program+ext)
		}
	}
	for _, name := range candidates {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, true
		}
	}
	return "", false
}

// envNameEqual compares environment names as Windows does: ignoring case.
func envNameEqual(a, b string) bool {
	return strings.EqualFold(a, b)
}

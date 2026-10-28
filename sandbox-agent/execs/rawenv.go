package execs

import (
	"os"
	"sort"
)

// rawEnvList reads each env-delivered raw secret's value from its file, as
// NAME=value entries for a command's environment (ADR 26-10-27-840 §3). One
// whose file is gone — the binding removed since the exec was asked for — is
// left out: the command starts without it rather than not at all.
func rawEnvList(raw map[string]string) []string {
	if len(raw) == 0 {
		return nil
	}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		value, err := os.ReadFile(raw[name])
		if err != nil {
			continue
		}
		out = append(out, name+"="+string(value))
	}
	return out
}

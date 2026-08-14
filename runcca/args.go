package runcca

import "strings"

// RealRunc is the runc this wrapper delegates to. It is referenced by absolute
// path, never by name: the wrapper installs itself as `runc` on a directory
// that is prepended to containerd's and dockerd's PATH, so resolving "runc"
// through PATH would re-exec the wrapper.
const RealRunc = "/usr/bin/runc"

// BundleDir returns the OCI bundle directory named on a runc command line.
//
// Presence of --bundle is the whole test: of runc's verbs only create, run and
// restore accept it, and those are exactly the ones about to materialize a
// container from a spec. Everything else (start, kill, state, --version, ...)
// has no bundle and is passed straight through, which keeps this wrapper out
// of the way of verbs it has no business touching without needing to model
// runc's global flag grammar.
func BundleDir(args []string) (string, bool) {
	for i, a := range args {
		if a == "--bundle" || a == "-b" {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		}
		if v, ok := strings.CutPrefix(a, "--bundle="); ok {
			return v, true
		}
		if v, ok := strings.CutPrefix(a, "-b="); ok {
			return v, true
		}
	}
	return "", false
}

// IsDelete reports whether this command line tears a container down, which is
// when its staged trust store can be reclaimed.
func IsDelete(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			// A global flag's separate value is not the verb: `--root /r
			// delete ...` must not read "/r" as the subcommand.
			if isFlagExpectingValue(a) {
				i++
			}
			continue
		}
		// The first bare word is the verb; container IDs only appear after it.
		return a == "delete"
	}
	return false
}

// ContainerID returns the container ID a runc command line operates on.
//
// runc puts it last on every verb this wrapper cares about
// (`create --bundle X --pid-file Y <id>`, `run --bundle X --keep <id>`,
// `delete --force <id>`), so the trailing operand is the ID. A trailing flag
// means this is not one of those forms.
func ContainerID(args []string) string {
	if len(args) == 0 {
		return ""
	}
	last := len(args) - 1
	if strings.HasPrefix(args[last], "-") {
		return ""
	}
	// Guard against consuming a preceding flag's value as the ID.
	if last > 0 && isFlagExpectingValue(args[last-1]) {
		return ""
	}
	return args[last]
}

// isFlagExpectingValue reports whether a bare flag takes a separate value, so
// that value is not mistaken for a container ID.
func isFlagExpectingValue(arg string) bool {
	switch arg {
	case "--bundle", "-b", "--pid-file", "--console-socket", "--root", "--log",
		"--log-format", "--criu", "--preserve-fds":
		return true
	}
	return false
}

// Package version reports the release a binary was cut from.
//
// It lives in the root module because both the CLI and the server answer the
// question — `discobox --version` and the server's /healthz — and one binary
// contains both. A version per package would need a linker flag per package,
// and two of them are one too many to keep in step.
package version

import (
	"runtime/debug"
	"strings"
)

// Version is the release this binary was cut from, set by the release build's
// linker flags. An ordinary build leaves it empty and reports its commit
// instead — a developer asking what they are running wants the answer, and
// "dev" is not one.
var Version = ""

// String is the version to show a human.
//
// A release says v1.2.3. A build from a checkout says the commit it came from,
// with "+dirty" when the tree had uncommitted changes, because a bug report
// naming a commit that does not describe the binary is worse than one naming
// no commit at all. Go stamps both into every build from a repository; only a
// build from outside one falls through to "unknown".
func String() string {
	if Version != "" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var revision, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}
	if revision == "" {
		return "unknown"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	var b strings.Builder
	b.WriteString(revision)
	if modified == "true" {
		b.WriteString("+dirty")
	}
	return b.String()
}

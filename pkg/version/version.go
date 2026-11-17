// Package version reports the release a binary was cut from.
//
// It lives in the root module because both the CLI and the server answer the
// question — `discobox --version` and the server's /healthz — and one binary
// contains both. A version per package would need a linker flag per package,
// and two of them are one too many to keep in step.
package version

import (
	"fmt"
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

// mainBuildMarker is what sets a main build's version apart from a tagged
// release's: v<next patch>-main.<count>.g<sha7> (ADR 26-11-16-249 §5).
const mainBuildMarker = "-main."

// MainBuild reports whether v is the version of a main build, a release of a
// main commit published apart from the tagged releases and deleted after
// fourteen days, rather than a tagged release.
func MainBuild(v string) bool {
	return strings.HasPrefix(v, "v") && strings.Contains(v, mainBuildMarker)
}

// MainBuildVersion is the version of a main build of the commit sha, the
// first-parent count-th on main, made after the release latest (the highest
// vMAJOR.MINOR.PATCH tag). It is a semver prerelease of latest's next patch,
// so it sorts above latest and below the next dot release, and above every
// main build before it.
func MainBuildVersion(latest string, count int, sha string) (string, error) {
	var major, minor, patch int
	var rest string
	if n, _ := fmt.Sscanf(latest, "v%d.%d.%d%s", &major, &minor, &patch, &rest); n != 3 ||
		fmt.Sprintf("v%d.%d.%d", major, minor, patch) != latest {
		return "", fmt.Errorf("%q is not a vMAJOR.MINOR.PATCH release", latest)
	}
	if count <= 0 {
		return "", fmt.Errorf("commit count %d is not positive", count)
	}
	if len(sha) < 7 || strings.Trim(sha, "0123456789abcdef") != "" {
		return "", fmt.Errorf("%q is not a commit hash", sha)
	}
	return fmt.Sprintf("v%d.%d.%d%s%d.g%s", major, minor, patch+1, mainBuildMarker, count, sha[:7]), nil
}

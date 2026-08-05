// Package dockercache rewrites a sandbox user's `docker build` invocation to
// import and export a BuildKit local cache on the pool-shared cache volume, so
// a build in one sandbox is reused by every other sandbox in the same pool.
//
// This is a PATH shim over the `docker` CLI rather than daemon configuration
// because there is no daemon- or buildx-side setting for a default
// --cache-from/--cache-to: the flags exist only on the build command line, so
// the command line is the only place they can be injected. The sandbox user's
// own Dockerfiles and commands stay unchanged, which is the same requirement
// that shaped docs/adr/0015.
//
// A single shared cache directory is deliberate. BuildKit's local cache is an
// OCI layout whose blobs are content-addressed, so two sandboxes exporting the
// same layer converge on one file; giving each sandbox its own directory would
// instead store a private copy of every layer (mode=max exports intermediate
// layers too, so the duplication is total). Concurrent export is safe enough to
// rely on: buildkit's client/ociindex takes an exclusive flock around index.json
// and merges into it rather than overwriting, and content-store blob writes land
// via ingest-then-rename. See Limitations below for the residual race.
//
// # Limitations
//
// index.json is locked with TryLock, not a blocking Lock, so two builds whose
// exports overlap in that brief final window can surface "could not lock" and
// fail the solve. Re-running the build succeeds (and hits the cache it just
// wrote). This shim deliberately does not capture output to detect and retry
// that: piping the build's stderr would drop Docker out of TTY progress mode,
// which is a worse everyday cost than a rare, self-healing failure.
package dockercache

import (
	"os"
	"path/filepath"
	"strings"
)

// RealDocker is the docker CLI this shim wraps. The shim installs as
// /usr/local/bin/docker, ahead of this on PATH.
const RealDocker = "/usr/bin/docker"

// cacheSubdir is appended to the user's home directory. It sits under
// ~/.cache so it rides whatever the sandbox already backs that directory with,
// rather than naming a second cache location of its own.
const cacheSubdir = ".cache/discobox/buildkit"

// CacheDir returns the shared BuildKit cache directory for the given home
// directory.
func CacheDir(home string) string {
	return filepath.Join(home, cacheSubdir)
}

// Args is the result of rewriting a docker command line.
type Args struct {
	// Argv is the full argument vector to exec, including argv[0].
	Argv []string
	// Injected reports whether cache flags were added. False means the
	// command was passed through untouched.
	Injected bool
}

// Rewrite returns the command line to exec for a user's `docker` invocation.
// args excludes argv[0]. Anything that is not a build command, and any build
// that already carries its own cache flags, is passed through unchanged so an
// explicit user choice always wins.
func Rewrite(args []string, home string) Args {
	pass := func() Args {
		return Args{Argv: append([]string{RealDocker}, args...)}
	}

	// Without an absolute home there is no well-defined cache location, and a
	// relative one would be created wherever the user happened to be.
	if !filepath.IsAbs(home) {
		return pass()
	}
	kind, idx := buildCommand(args)
	if kind == notBuild {
		return pass()
	}
	if hasCacheFlag(args[idx:]) {
		return pass()
	}

	dir := CacheDir(home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		// The cache volume may be missing or read-only. A build that runs
		// without cache is strictly better than one that fails to start.
		return pass()
	}

	rewritten := make([]string, 0, len(args)+4)
	if kind == buildLegacy {
		// `docker build` runs the classic builder, which has no --cache-to.
		// Only `docker buildx build` speaks the BuildKit exporter flags, so
		// the invocation is promoted. The containerd image store
		// (daemon.json features.containerd-snapshotter) is what makes the
		// result of a buildx build directly runnable, so this promotion does
		// not strand the image behind a missing --load.
		rewritten = append(rewritten, args[:idx]...)
		rewritten = append(rewritten, "buildx", "build")
		rewritten = append(rewritten, args[idx+1:]...)
	} else {
		rewritten = append(rewritten, args...)
	}

	// Import only when a previous export exists: pointing --cache-from at an
	// empty directory makes BuildKit report a missing-cache error.
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err == nil {
		rewritten = append(rewritten, "--cache-from", "type=local,src="+dir)
	}
	// mode=max exports intermediate layers, not just the final ones, which is
	// what makes a *different* Dockerfile sharing early stages hit the cache.
	rewritten = append(rewritten, "--cache-to", "type=local,dest="+dir+",mode=max")

	return Args{Argv: append([]string{RealDocker}, rewritten...), Injected: true}
}

type buildKind int

const (
	notBuild buildKind = iota
	buildLegacy
	buildBuildx
)

// buildCommand locates the build subcommand, returning its kind and the index
// in args of the token that names it ("build" for both forms). Global flags may
// precede the subcommand (`docker --context foo build .`), so the search skips
// leading flags rather than assuming args[0] is the subcommand.
func buildCommand(args []string) (buildKind, int) {
	i, ok := nextOperand(args, 0)
	if !ok {
		return notBuild, 0
	}
	switch args[i] {
	case "build":
		return buildLegacy, i
	case "buildx":
		j, ok := nextOperand(args, i+1)
		if !ok || args[j] != "build" {
			return notBuild, 0
		}
		return buildBuildx, j
	default:
		return notBuild, 0
	}
}

// nextOperand returns the index of the first non-flag token at or after start.
// A global flag that takes a separate value would otherwise have its value
// mistaken for the subcommand, so known value-taking global flags consume the
// token after them.
func nextOperand(args []string, start int) (int, bool) {
	for i := start; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return i, true
		}
		if strings.Contains(a, "=") {
			continue // --flag=value carries its value inline
		}
		if globalFlagsWithValue[a] {
			i++ // skip the separate value
		}
	}
	return 0, false
}

// globalFlagsWithValue are the `docker` global flags that take a separate
// value. Only these can hide a subcommand behind them.
var globalFlagsWithValue = map[string]bool{
	"--config": true, "--context": true, "-c": true, "--host": true, "-H": true,
	"--log-level": true, "-l": true, "--tlscacert": true, "--tlscert": true,
	"--tlskey": true,
}

// hasCacheFlag reports whether the user already specified cache handling.
func hasCacheFlag(args []string) bool {
	for _, a := range args {
		if a == "--cache-from" || a == "--cache-to" ||
			strings.HasPrefix(a, "--cache-from=") || strings.HasPrefix(a, "--cache-to=") {
			return true
		}
	}
	return false
}

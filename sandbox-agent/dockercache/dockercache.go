// Package dockercache points a sandbox user's `docker build` at the pool-shared
// BuildKit builder, so a build in one sandbox is reused by every other sandbox
// in the same pool.
//
// This is a PATH shim over the `docker` CLI because `docker build` cannot be
// pointed at a builder any other way. Measured against a mediator counting
// solves, with a clean DOCKER_CONFIG holding exactly one builder:
//
//	docker buildx build (no flag)                  -> uses the selected builder
//	docker build --builder <name>                  -> uses it
//	BUILDX_BUILDER=<name> docker build             -> uses it, but loses --load
//	docker build, selected via `buildx use`        -> IGNORES it
//
// Bare `docker build` pins the `default` instance whatever is selected —
// buildx's own --debug output reports `building with "default" instance using
// docker driver` — so only an explicit flag routes it. BUILDX_BUILDER routes but
// drops the image into the build cache unless every invocation also passes
// --load, which fails the "works out of the box" requirement.
//
// Only build commands are rewritten; everything else is exec'd straight through,
// and `docker buildx` is left entirely alone. `docker buildx use default` remains
// the user's escape hatch back to the in-sandbox builder.
//
// See docs/adr/0039-builds-run-on-a-pool-shared-buildkit.md.
package dockercache

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RealDocker is the docker CLI this shim wraps. The shim installs as
// /usr/local/bin/docker, ahead of this on PATH.
const RealDocker = "/usr/bin/docker"

const (
	// BuilderName is the buildx instance pointing at the pool's mediator.
	BuilderName = "discobox-pool"

	// MediatorURL is the mediator's endpoint. The host is the pool's network
	// alias, so the mTLS ServerName check holds whatever address the pool has.
	MediatorURL = "tcp://discobox-pool-proxy:17081"

	// proxyMaterial holds the client certificate the sandbox already uses for
	// the egress proxy. Reusing it is the point: client ID is the sandbox ID,
	// which is already the proxy's tenant boundary, so a build and its egress
	// are attributed to the same subject and no new key material exists.
	proxyMaterial = "/etc/discobox/proxy"
)

// materialDir is where the sandbox's client material lives. It is a variable
// only so tests can point it at a fixture; production never reassigns it.
var materialDir = proxyMaterial

// Args is the result of rewriting a docker command line.
type Args struct {
	// Argv is the full argument vector to exec, including argv[0].
	Argv []string
	// Rewritten reports whether the command was pointed at the pool builder.
	// False means it was passed through untouched.
	Rewritten bool
}

// Rewrite returns the command line to exec for a user's `docker` invocation.
// args excludes argv[0].
func Rewrite(args []string) Args {
	pass := func() Args {
		return Args{Argv: append([]string{RealDocker}, args...)}
	}
	kind, idx := buildCommand(args)
	if kind == notBuild {
		return pass()
	}
	// An explicit --builder is the user saying where this build goes; a shim
	// that overrode it would take away the escape hatch.
	if hasBuilderFlag(args[idx:]) {
		return pass()
	}
	if !poolBuilderAvailable() {
		// No client material means no pool builder to reach. Building locally
		// is worse than sharing a cache, but far better than failing.
		return pass()
	}

	rewritten := make([]string, 0, len(args)+6)
	rewritten = append(rewritten, args[:idx]...)
	// `docker build` is an alias for `docker buildx build`, but it pins the
	// default instance. Spelling out the subcommand is what lets --builder take
	// effect.
	rewritten = append(rewritten, "buildx", "build", "--builder", BuilderName)
	if !hasOutputFlag(args[idx:]) {
		// `docker build` puts its result in the local image store. A remote
		// builder does not, so without this the image silently vanishes into
		// the build cache — including for an untagged build, which has no name
		// to push anywhere and can only come back this way.
		rewritten = append(rewritten, "--load")
	}
	rewritten = append(rewritten, args[idx+1:]...)
	return Args{Argv: append([]string{RealDocker}, rewritten...), Rewritten: true}
}

// poolBuilderAvailable reports whether this sandbox has the client material the
// mediator requires. Without it the connection cannot be authenticated, so
// there is no point rewriting the command.
func poolBuilderAvailable() bool {
	for _, name := range []string{"mtls-ca.crt", "client.crt", "client.key"} {
		if _, err := os.Stat(filepath.Join(materialDir, name)); err != nil {
			return false
		}
	}
	return true
}

// EnsureBuilder creates the buildx instance if it does not exist yet.
//
// It runs lazily, on the first build, rather than at boot: buildx state lives
// under the invoking user's $DOCKER_CONFIG, which is on the sandbox's
// persistent data volume, so a boot-time provision would have to guess the user
// and would leave a stale definition behind whenever the endpoint or the
// certificate paths changed.
func EnsureBuilder(ctx context.Context) error {
	if exec.CommandContext(ctx, RealDocker, "buildx", "inspect", BuilderName).Run() == nil {
		return nil
	}
	opts := strings.Join([]string{
		"cacert=" + filepath.Join(materialDir, "mtls-ca.crt"),
		"cert=" + filepath.Join(materialDir, "client.crt"),
		"key=" + filepath.Join(materialDir, "client.key"),
		"servername=discobox-pool-proxy",
	}, ",")
	//nolint:gosec // Every argument is a package constant; none comes from the user's command line.
	return exec.CommandContext(ctx, RealDocker, "buildx", "create",
		"--name", BuilderName, "--driver", "remote",
		"--driver-opt", opts, MediatorURL).Run()
}

type buildKind int

const (
	notBuild buildKind = iota
	buildLegacy
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
		// `docker buildx` is left alone: a user reaching for it directly has
		// chosen their own builder semantics.
		return notBuild, 0
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

// hasBuilderFlag reports whether the user chose a builder themselves.
func hasBuilderFlag(args []string) bool {
	for _, a := range args {
		if a == "--builder" || strings.HasPrefix(a, "--builder=") {
			return true
		}
	}
	return false
}

// hasOutputFlag reports whether the user already said where the result goes.
// Adding --load on top of --push or an explicit --output would override a
// deliberate choice.
func hasOutputFlag(args []string) bool {
	for _, a := range args {
		switch {
		case a == "--load", a == "--push", a == "-o", a == "--output":
			return true
		case strings.HasPrefix(a, "--output="), strings.HasPrefix(a, "-o="):
			return true
		}
	}
	return false
}

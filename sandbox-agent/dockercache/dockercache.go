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
	"strings"
)

// RealDocker is the docker CLI this shim wraps. The shim installs as
// /usr/local/bin/docker, ahead of this on PATH.
const RealDocker = "/usr/bin/docker"

const (
	// BuilderName is the buildx instance pointing at the pool's mediator.
	BuilderName = "discobox-pool"

	// MediatorURL is the sandbox-local forwarder that fronts the pool's
	// BuildKit mediator. It is plaintext loopback, private to this sandbox.
	//
	// buildx is not pointed at the mediator directly, even though it can carry
	// a client certificate: builds run as the sandbox user, the mTLS client key
	// is root-owned, and making that key readable by every process in the
	// sandbox to suit one tool would hand out the sandbox's proxy identity.
	// The forwarder holds the key instead, exactly as the HTTP proxy bridge
	// does for every other client that cannot present one.
	MediatorURL = "tcp://127.0.0.1:17082"

	// bridgeConfig is written by pool-agent for a sandbox whose pool runs a
	// builder. Its absence is what tells this shim to leave a build alone.
	bridgeConfig = "/etc/discobox/proxy/bridge-buildkit.json"

	// PoolRegistry is the pool's build-output registry. A build pushes here and
	// the result is pulled back, instead of --load streaming the whole image
	// over the session every time.
	//
	// --load serializes the entire image on every build, even one that was
	// fully cached, because the docker exporter has no way to ask the local
	// daemon what it already holds. A registry is content-addressed at both
	// ends: the push uploads only blobs the registry lacks, and the pull fetches
	// only blobs this daemon lacks.
	PoolRegistry = "discobox-pool-proxy:5000"

	// untaggedRepo holds results of builds that named no tag. There is nothing
	// to push without a name, so one is synthesized per build; the local tag is
	// dropped afterwards so `docker images` shows the dangling entry a plain
	// `docker build` produces.
	untaggedRepo = "_build"
)

// bridgeConfigPath locates the forwarder's config. It is a variable only so
// tests can point it at a fixture; production never reassigns it.
var bridgeConfigPath = bridgeConfig

// Args is the result of rewriting a docker command line.
type Args struct {
	// Argv is the full argument vector to exec, including argv[0].
	Argv []string
	// Rewritten reports whether the command was pointed at the pool builder.
	// False means it was passed through untouched.
	Rewritten bool
	// RegistryRef is the synthesized reference this build pushes to and is
	// pulled back from.
	RegistryRef string
	// Tags are the tags the user asked for, applied locally after the pull.
	Tags []string
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
		// No pool builder to reach. Building locally
		// is worse than sharing a cache, but far better than failing.
		return pass()
	}

	// An explicit output is the user saying where the result goes; pushing it
	// somewhere of our choosing as well would be wrong.
	if hasOutputFlag(args[idx:]) {
		return pass()
	}

	tags, rest := splitTags(args[idx+1:])
	ref := newRegistryRef()
	rewritten := make([]string, 0, len(args)+8)
	rewritten = append(rewritten, args[:idx]...)
	// `docker build` is an alias for `docker buildx build`, but it pins the
	// default instance. Spelling out the subcommand is what lets --builder take
	// effect.
	rewritten = append(rewritten, "buildx", "build", "--builder", BuilderName, "--push", "-t", ref)
	rewritten = append(rewritten, rest...)
	return Args{
		Argv:        append([]string{RealDocker}, rewritten...),
		Rewritten:   true,
		RegistryRef: ref,
		Tags:        tags,
	}
}

// poolBuilderAvailable reports whether this sandbox's pool runs a builder. The
// forwarder's config is written only when it does, so its absence means there
// is nothing to point a build at.
func poolBuilderAvailable() bool {
	_, err := os.Stat(bridgeConfigPath)
	return err == nil
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
	// No TLS options: the forwarder terminates mTLS on this sandbox's behalf,
	// so what buildx dials is plaintext loopback.
	//nolint:gosec // Every argument is a package constant; none comes from the user's command line.
	return exec.CommandContext(ctx, RealDocker, "buildx", "create",
		"--name", BuilderName, "--driver", "remote", MediatorURL).Run()
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

// splitTags removes the user's -t/--tag flags, returning their values and the
// remaining arguments. They are applied locally after the pull instead: a tag
// may name any registry, and rewriting arbitrary tag syntax to be pushable to
// the pool would be a source of surprises.
func splitTags(args []string) (tags, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-t" || a == "--tag":
			if i+1 < len(args) {
				tags = append(tags, args[i+1])
				i++
			}
		case strings.HasPrefix(a, "--tag="):
			tags = append(tags, strings.TrimPrefix(a, "--tag="))
		case strings.HasPrefix(a, "-t="):
			tags = append(tags, strings.TrimPrefix(a, "-t="))
		default:
			rest = append(rest, a)
		}
	}
	return tags, rest
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

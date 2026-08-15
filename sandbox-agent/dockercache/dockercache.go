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
// See docs/adr/0044-builds-run-on-a-pool-shared-buildkit.md.
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

	// buildRepo is the repository every build's result is pushed to. A push
	// needs a name and the user's own tag cannot serve as one — it may already
	// name a registry this pool cannot reach, or one they did not mean to
	// publish to — so a reference is synthesized here per build.
	//
	// The leading component may not start with `_`: a repository path component
	// must begin with an alphanumeric, and a name that violates the reference
	// grammar is rejected by the client before any build starts.
	buildRepo = "discobox-build"
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
	// Quiet reports that the user asked for `-q`, whose whole contract is the
	// one line it prints on stdout.
	Quiet bool
	// IIDFile is the path the user asked the image id to be written to. It is
	// `-q` in file form and needs the same correction.
	IIDFile string
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
	// Pushing is what turns attestations on: buildx attaches provenance by
	// default whenever the output is a registry, which wraps the image in an
	// index alongside an attestation manifest. That provenance records this
	// build — its start time and its own reference — so two builds of identical
	// content produce different index digests, and `docker images` shows a
	// different ID in each sandbox even on a complete cache hit. Plain
	// `docker build` attests nothing, and the transport should not change what
	// the user gets.
	if !hasAttestationFlag(args[idx:]) {
		rewritten = append(rewritten, "--provenance=false")
	}
	rewritten = append(rewritten, rest...)
	return Args{
		Argv:        append([]string{RealDocker}, rewritten...),
		Rewritten:   true,
		RegistryRef: ref,
		Tags:        tags,
		Quiet:       hasQuietFlag(args[idx:]),
		IIDFile:     iidFile(args[idx:]),
	}
}

// iidFile returns the path `--iidfile` names, or "".
//
// It is `-q` in file form and wrong in the same way: buildx writes a digest of
// what it pushed, which the local daemon does not answer to. CI reads that file
// to decide what to run or scan next.
func iidFile(args []string) string {
	for i, a := range args {
		if path, ok := strings.CutPrefix(a, "--iidfile="); ok {
			return path
		}
		if a == "--iidfile" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// hasQuietFlag reports whether the user asked for `-q`.
//
// `docker build -q` exists to print one thing: the local image id, which
// `IMG=$(docker build -q .)` then runs. Pushing changes what buildx prints
// there — a digest of the pushed artifact, which the local daemon does not
// answer to — so the shim has to supply the answer itself.
func hasQuietFlag(args []string) bool {
	for _, a := range args {
		switch {
		case a == "-q", a == "--quiet":
			return true
		case strings.HasPrefix(a, "--quiet="):
			// The one spelling that turns it back off.
			return a == "--quiet=true"
		case len(a) > 1 && a[0] == '-' && !strings.HasPrefix(a, "--"):
			if quietInShortCluster(a[1:]) {
				return true
			}
		}
	}
	return false
}

// quietInShortCluster reads a run of combined short flags: `-qf Dockerfile` is
// `-q -f Dockerfile`. Scanning for a bare 'q' is not enough, because a short
// flag that takes a value swallows the rest of the token — `-fDockerfileq` is
// a filename ending in q, not a request for quiet.
func quietInShortCluster(cluster string) bool {
	for _, c := range cluster {
		if c == 'q' {
			return true
		}
		if strings.ContainsRune("tfom", c) {
			// Everything after this is that flag's value.
			return false
		}
	}
	return false
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

// hasAttestationFlag reports whether the user has already said what to attest.
// Someone who asked for provenance or an SBOM wants it, and disabling it under
// them would silently drop what they asked for.
func hasAttestationFlag(args []string) bool {
	for _, a := range args {
		switch {
		case a == "--provenance", a == "--sbom", a == "--attest":
			return true
		case strings.HasPrefix(a, "--provenance="), strings.HasPrefix(a, "--sbom="), strings.HasPrefix(a, "--attest="):
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

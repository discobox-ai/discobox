package dockercache

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/distribution/reference"
)

// A build's local base images, made reachable from the pool builder.
//
// A local `docker build` resolves `FROM discobox-sandbox-agent:local` against
// the daemon's own image store, because with the docker driver BuildKit runs
// inside dockerd and that store is right there. The pool builder is another
// daemon on another host, so the name normalises to docker.io/library and goes
// to Hub, where it does not exist. That is the one way a pool-shared build
// visibly is not a local one.
//
// So the shim publishes those bases into this sandbox's registry namespace and
// points the build at them with `--build-context`, which BuildKit's dockerfile
// frontend resolves *before* it resolves an image. Nothing about the Dockerfile
// or the command line changes. See ADR 0045.
//
// It is done here rather than in the mediator because the mediator cannot see
// what a build is built from: for a dockerfile build the frontend generates its
// LLB inside the daemon, so the solve carries no source list (ADR 0044). The
// shim has the Dockerfile and the local image list; the mediator has neither.

// registryNamespaceFile is the sandbox's namespace in the pool build registry,
// staged by pool-agent with the rest of its proxy material.
const registryNamespaceFile = "/etc/discobox/proxy/registry-namespace"

// namespacePath is a variable only so tests can point it at a fixture;
// production never reassigns it.
var namespacePath = registryNamespaceFile

// localBaseContexts returns the `--build-context` arguments that point this
// build's local base images at the pool registry, publishing each one first.
//
// Everything here degrades to nothing. A build whose bases are all remote, a
// sandbox with no namespace staged, an unreadable Dockerfile: each returns no
// arguments and leaves the build exactly as it was, because the alternative to
// redirecting a base is the behavior that shipped before this existed.
func localBaseContexts(ctx context.Context, args []string) []string {
	namespace := readNamespace()
	if namespace == "" {
		return nil
	}
	dockerfile, err := os.ReadFile(dockerfilePath(args))
	if err != nil {
		return nil
	}
	var out []string
	for _, base := range localBases(ctx, scanBases(string(dockerfile), buildArgs(args))) {
		published, err := publishBase(ctx, namespace, base)
		if err != nil {
			// The build can still succeed — the base may resolve remotely after
			// all — so this is worth saying and not worth failing over.
			notice(fmt.Sprintf("could not publish %s for the pool builder: %v", base, err))
			continue
		}
		out = append(out, "--build-context", base+"=docker-image://"+published)
	}
	return out
}

func readNamespace() string {
	data, err := os.ReadFile(namespacePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// dockerfilePath is the Dockerfile this build reads: what -f names, else the
// default inside the context directory.
func dockerfilePath(args []string) string {
	kind, idx := buildCommand(args)
	if kind == notBuild {
		return "Dockerfile"
	}
	rest := args[idx+1:]
	for i, a := range rest {
		if path, ok := strings.CutPrefix(a, "--file="); ok {
			return path
		}
		if (a == "-f" || a == "--file") && i+1 < len(rest) {
			return rest[i+1]
		}
	}
	return filepath.Join(buildContextDir(rest), "Dockerfile")
}

// buildContextDir is the build's context argument: the last bare word on the
// line, which is where docker takes it from.
func buildContextDir(rest []string) string {
	dir := "."
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if strings.HasPrefix(a, "-") {
			if takesSeparateValue(a) && i+1 < len(rest) {
				i++
			}
			continue
		}
		dir = a
	}
	return dir
}

// takesSeparateValue reports whether a flag consumes the next argument. Only
// the ones that can precede the context need listing: a flag this does not
// know simply leaves its value looking like a context, and the last bare word
// still wins.
func takesSeparateValue(flag string) bool {
	if strings.Contains(flag, "=") {
		return false
	}
	switch flag {
	case "-f", "--file", "-t", "--tag", "--build-arg", "--build-context", "--target",
		"--platform", "--builder", "--output", "-o", "--cache-from", "--cache-to",
		"--iidfile", "--secret", "--ssh", "--network", "--add-host", "--label", "--allow":
		return true
	}
	return false
}

// buildArgs are the `--build-arg NAME=value` pairs on the command line, which a
// base name may be written in terms of: `FROM ${SANDBOX_AGENT_IMAGE}` is how
// this repository's own harness images name their base.
func buildArgs(args []string) map[string]string {
	out := map[string]string{}
	for i, a := range args {
		var pair string
		switch {
		case strings.HasPrefix(a, "--build-arg="):
			pair = strings.TrimPrefix(a, "--build-arg=")
		case a == "--build-arg" && i+1 < len(args):
			pair = args[i+1]
		default:
			continue
		}
		if name, value, ok := strings.Cut(pair, "="); ok {
			out[name] = value
		}
	}
	return out
}

// scanBases returns the image references a Dockerfile builds FROM, in the form
// they are written in.
//
// Stage names are tracked so `FROM builder` is not mistaken for an image, and
// ARG defaults are read so a base named by one resolves the way the frontend
// will resolve it. A reference this cannot make sense of is skipped: the build
// then behaves as it did before, which is the outcome to degrade to.
func scanBases(dockerfile string, cliArgs map[string]string) []string {
	args := map[string]string{}
	for name, value := range cliArgs {
		args[name] = value
	}
	stages := map[string]bool{}
	var bases []string
	seen := map[string]bool{}

	for _, line := range logicalLines(dockerfile) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "ARG":
			// Only a default fills in what the command line did not give.
			name, value, hasDefault := strings.Cut(fields[1], "=")
			if _, given := args[name]; !given && hasDefault {
				args[name] = value
			} else if !hasDefault {
				if _, given := args[name]; !given {
					args[name] = ""
				}
			}
		case "FROM":
			ref, alias := parseFrom(fields[1:])
			if alias != "" {
				stages[strings.ToLower(alias)] = true
			}
			ref = expandArgs(ref, args)
			if ref == "" || strings.EqualFold(ref, "scratch") || stages[strings.ToLower(ref)] {
				continue
			}
			if !seen[ref] {
				seen[ref] = true
				bases = append(bases, ref)
			}
		}
	}
	return bases
}

// parseFrom reads the reference and the stage alias out of a FROM's arguments,
// stepping over flags like --platform.
func parseFrom(fields []string) (ref, alias string) {
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if strings.HasPrefix(field, "--") {
			continue
		}
		if ref == "" {
			ref = field
			continue
		}
		if strings.EqualFold(field, "AS") && i+1 < len(fields) {
			return ref, fields[i+1]
		}
	}
	return ref, ""
}

// expandArgs substitutes ${NAME} and $NAME, including the :-default form, the
// way the frontend does for a base name. A reference still holding a variable
// after this is returned empty: acting on half a name would be worse than
// leaving the base alone.
func expandArgs(ref string, args map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(ref); i++ {
		if ref[i] != '$' {
			b.WriteByte(ref[i])
			continue
		}
		name, rest, ok := readVariable(ref[i+1:])
		if !ok {
			return ""
		}
		fallback := ""
		if n, f, hasDefault := strings.Cut(name, ":-"); hasDefault {
			name, fallback = n, f
		}
		value := args[name]
		if value == "" {
			value = fallback
		}
		if value == "" {
			return ""
		}
		b.WriteString(value)
		ref = rest
		i = -1
	}
	return b.String()
}

// readVariable reads one variable reference off the front of s, returning its
// name and what follows it.
func readVariable(s string) (name, rest string, ok bool) {
	if strings.HasPrefix(s, "{") {
		end := strings.Index(s, "}")
		if end < 0 {
			return "", "", false
		}
		return s[1:end], s[end+1:], true
	}
	end := len(s)
	for i := 0; i < len(s); i++ {
		if s[i] != '_' && (s[i] < 'a' || s[i] > 'z') && (s[i] < 'A' || s[i] > 'Z') && (s[i] < '0' || s[i] > '9') {
			end = i
			break
		}
	}
	if end == 0 {
		return "", "", false
	}
	return s[:end], s[end:], true
}

// logicalLines joins a Dockerfile's continuations and drops its comments, so a
// FROM split across lines still reads as one instruction.
func logicalLines(dockerfile string) []string {
	var out []string
	var current strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(dockerfile))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		if continued := strings.HasSuffix(line, "\\"); continued {
			current.WriteString(strings.TrimSuffix(line, "\\"))
			current.WriteString(" ")
			continue
		}
		current.WriteString(line)
		if joined := strings.TrimSpace(current.String()); joined != "" {
			out = append(out, joined)
		}
		current.Reset()
	}
	if joined := strings.TrimSpace(current.String()); joined != "" {
		out = append(out, joined)
	}
	return out
}

// localBases narrows a Dockerfile's bases to the ones this daemon holds and the
// pool builder could not resolve for itself.
//
// A reference naming a registry is left alone whatever is local: it says where
// it comes from, and the builder can reach the same place. What is redirected
// is the unqualified name — the one a local build answers from the image store
// and a remote build sends to Hub.
func localBases(ctx context.Context, refs []string) []string {
	var out []string
	for _, ref := range refs {
		if namesARegistry(ref) {
			continue
		}
		//nolint:gosec // The reference comes from the Dockerfile this build was told to read.
		if err := exec.CommandContext(ctx, dockerCLI, "image", "inspect", ref).Run(); err != nil {
			continue
		}
		out = append(out, ref)
	}
	return out
}

// namesARegistry reports whether a reference carries an explicit host, which is
// what distinguishes `ghcr.io/x/y` from `y`.
func namesARegistry(ref string) bool {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return true // Unparseable is not ours to redirect.
	}
	return reference.Domain(named) != "docker.io" || strings.Contains(strings.SplitN(ref, "/", 2)[0], ".")
}

// publishBase copies a local image into this sandbox's registry namespace and
// returns the reference the builder pulls it by.
//
// The push is cheap for anything the pool itself built: its layers are already
// in the registry under the reference that build pushed, and a registry mounts
// a blob it already holds across repositories rather than accepting it again.
func publishBase(ctx context.Context, namespace, ref string) (string, error) {
	target, err := namespacedRef(namespace, ref)
	if err != nil {
		return "", err
	}
	if code := runQuiet(ctx, "tag", ref, target); code != 0 {
		return "", fmt.Errorf("tag %s as %s", ref, target)
	}
	if code := runQuiet(ctx, "push", target); code != 0 {
		return "", fmt.Errorf("push %s", target)
	}
	// The local name has served its purpose; the image keeps the tag the user
	// knows it by, and `docker images` should not grow a second entry per build.
	_ = runQuiet(ctx, "rmi", target)
	return target, nil
}

// namespacedRef is where a local image lives in the pool registry: this
// sandbox's namespace, then the image's own path and tag.
func namespacedRef(namespace, ref string) (string, error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", ref, err)
	}
	// The familiar name, not the path: normalising adds the implicit `library/`
	// to an unqualified name, and carrying that into the namespace would name
	// the image something the user never wrote.
	path := reference.FamiliarName(named)
	tag := "latest"
	if tagged, ok := named.(reference.Tagged); ok {
		tag = tagged.Tag()
	}
	// Path separators would make one image's name a namespace of its own inside
	// this sandbox's, which is still unambiguous but reads as a directory tree.
	return fmt.Sprintf("%s/%s/%s:%s", PoolRegistry, namespace, strings.ReplaceAll(path, "/", "_"), tag), nil
}

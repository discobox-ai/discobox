package dockercache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Run executes a user's docker command line and returns the exit code to use.
//
// Anything that is not a build we point at the pool builder is exec'd, replacing
// this process, so stdio, TTY behavior and signals are the real docker CLI's
// with no wrapper in the data path.
//
// A build is different: its result has to come back from the pool, and that is
// three commands rather than one (push, pull, tag). See buildViaRegistry.
func Run(args []string) int {
	a := Rewrite(args)
	if !a.Rewritten {
		return execDirect(a.Argv)
	}
	ctx := context.Background()
	if err := EnsureBuilder(ctx); err != nil {
		// A builder that cannot be created leaves the build to fail against a
		// missing instance with buildx's own error, which says far more than
		// anything this shim could substitute.
		notice(fmt.Sprintf("pool builder unavailable: %v", err))
	}
	return buildViaRegistry(ctx, a)
}

// buildViaRegistry runs the build with --push to the pool registry, pulls the
// result back, and applies the user's tags.
//
// The push/pull pair replaces --load deliberately. --load serializes the whole
// image over the session on every build, even a fully cached one, because the
// docker exporter cannot ask the local daemon what it already holds. Both ends
// of a registry are content-addressed, so only missing blobs move.
func buildViaRegistry(ctx context.Context, a Args) int {
	// Under -q, buildx's own stdout is discarded. Pushing changes what it
	// prints there to a digest of the pushed artifact, which the local daemon
	// does not answer to, where `docker build -q` promises an id that
	// `docker run $(docker build -q .)` can use. The real answer is not known
	// until the pull lands, so it is printed below.
	if code := runPassthrough(ctx, a.Argv, a.Quiet); code != 0 {
		return code
	}
	// Pull before tagging: there is nothing local to tag until it lands.
	if code := runQuiet(ctx, "pull", a.RegistryRef); code != 0 {
		notice("build succeeded but its result could not be pulled from the pool registry")
		return code
	}
	// Both of these report the built image's id, and buildx filled them in
	// with a digest of what it pushed. Correct them from the local daemon,
	// which is the only thing that can say what the id is here.
	if a.Quiet || a.IIDFile != "" {
		id, err := imageID(ctx, a.RegistryRef)
		if err != nil {
			notice(fmt.Sprintf("build succeeded but its image id could not be read: %v", err))
			return 1
		}
		if a.Quiet {
			fmt.Println(id)
		}
		if a.IIDFile != "" {
			if err := os.WriteFile(a.IIDFile, []byte(id), 0o644); err != nil { //nolint:gosec // The user asked for this file; docker writes it world-readable too.
				notice(fmt.Sprintf("build succeeded but its image id could not be written to %s: %v", a.IIDFile, err))
				return 1
			}
		}
	}
	for _, tag := range a.Tags {
		if code := runQuiet(ctx, "tag", a.RegistryRef, tag); code != 0 {
			notice(fmt.Sprintf("could not tag the result as %s", tag))
			return code
		}
	}
	if len(a.Tags) > 0 {
		// The synthesized reference has served its purpose. The image survives
		// under the user's own tags, so dropping this one leaves `docker images`
		// looking exactly as it would after a local build.
		_ = runQuiet(ctx, "rmi", a.RegistryRef)
	}
	// An untagged build keeps it. Nothing else names the image, and removing an
	// image's last reference deletes the image — there is no way to untag a
	// pulled image into the dangling `<none>:<none>` entry a local build
	// leaves. So an untagged build shows up under this name instead of that
	// one; the image and its ID are the same either way.
	return 0
}

// imageID returns the local image id of a reference, in the `sha256:...` form
// `docker build -q` prints.
func imageID(ctx context.Context, ref string) (string, error) {
	//nolint:gosec // Both arguments are this shim's own.
	out, err := exec.CommandContext(ctx, RealDocker, "image", "inspect", "--format", "{{.Id}}", ref).Output()
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", errors.New("no id reported")
	}
	return id, nil
}

// runPassthrough runs a command with this process's stdio, so build progress
// renders normally. discardStdout drops what the command writes there, for the
// one case where this shim owns stdout rather than the build.
func runPassthrough(ctx context.Context, argv []string, discardStdout bool) int {
	//nolint:gosec // argv is this shim's own rewrite of the user's command line.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if discardStdout {
		cmd.Stdout = io.Discard
	}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "discobox-docker: %v\n", err)
		return 1
	}
	return 0
}

// runQuiet runs a docker subcommand, surfacing only its error output. The
// transfer steps are plumbing; their progress is not the user's build log.
func runQuiet(ctx context.Context, args ...string) int {
	//nolint:gosec // Every argument is either a package constant or this shim's own reference.
	cmd := exec.CommandContext(ctx, RealDocker, args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		return 1
	}
	return 0
}

// newRegistryRef returns a reference unique to one build. It is synthesized
// rather than derived from the user's tag so that no tag syntax — a tag naming
// another registry, most of all — has to be rewritten to be pushable here.
func newRegistryRef() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%s/%s/fallback:build", PoolRegistry, buildRepo)
	}
	return fmt.Sprintf("%s/%s/%s:build", PoolRegistry, buildRepo, hex.EncodeToString(raw[:]))
}

// execDirect replaces this process with the real docker CLI.
func execDirect(argv []string) int {
	//nolint:gosec // Handing this process's own argv to the real docker CLI is the entire point of the shim.
	if err := syscall.Exec(argv[0], argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "discobox-docker: exec %s: %v\n", argv[0], err)
		return 127
	}
	return 0
}

// notice reports something the user should know about the shim itself, marked
// so it is not mistaken for output from their build.
func notice(msg string) {
	fmt.Fprintf(os.Stderr, "discobox-docker: %s\n", msg)
}

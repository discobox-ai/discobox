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
	"time"
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
	// The pool builder has no access to this daemon's image store, so a base
	// image that only exists here is published and redirected first. See
	// localbase.go.
	a.Argv = append(a.Argv, localBaseContexts(ctx, args)...)
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
	// The synthesized reference has served its purpose once the image is here,
	// and it is dropped however this returns. On success the image survives
	// under the user's own tags, so dropping this one leaves `docker images`
	// looking exactly as it would after a local build. On failure it would
	// otherwise stay behind as a discobox-build/<hex>:build entry that names a
	// whole image and that nothing will ever clean up.
	//
	// An untagged build keeps it. Nothing else names the image, and removing an
	// image's last reference deletes the image — there is no way to untag a
	// pulled image into the dangling `<none>:<none>` entry a local build
	// leaves. So an untagged build shows up under this name instead of that
	// one; the image and its ID are the same either way.
	if len(a.Tags) > 0 {
		defer func() { _ = runQuiet(ctx, "rmi", a.RegistryRef) }()
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
		if err := tagResult(ctx, a.RegistryRef, tag); err != nil {
			notice(fmt.Sprintf("could not tag the result as %s: %v", tag, err))
			return 1
		}
	}
	return 0
}

// tagAttempts is how many times a tag is tried before the build is called
// failed, and tagRetryPause is the wait between them. Three is not a guess at
// how contended the daemon is: the losing tag is complete by the time it is
// rejected, so the retry contends with nothing unless a third build lands in
// the same instant.
const (
	tagAttempts   = 3
	tagRetryPause = 100 * time.Millisecond
)

// tagResult applies one of the user's tags to the pulled image, retrying a tag
// another build raced us for.
//
// `docker tag` is not atomic in the containerd image store: the daemon creates
// the image record, and finding the name taken, deletes it and creates it
// again. Two builds tagging the same name with different targets can interleave
// so that the second create finds the name back and fails with AlreadyExists —
// which is what `task dev`'s image watcher rebuilding an image does to a
// hand-run `task build:images` tagging the same `:local` name. There is nothing
// wrong with the losing tag, and it wins as soon as it is asked again.
//
// This shim is why it is worth retrying at all: a local `docker build -t` has
// the builder name the image as part of the build, while a build that comes
// back from the pool registry has to be tagged afterwards, so every build here
// goes through the racy path.
//
// Output is captured rather than passed through: a first attempt that is going
// to be retried should not print an error the user cannot act on, and only the
// last one is worth reporting.
func tagResult(ctx context.Context, ref, tag string) error {
	var last error
	for attempt := 0; attempt < tagAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(tagRetryPause):
			}
		}
		//nolint:gosec // Both arguments are this shim's own reference and the user's tag.
		out, err := exec.CommandContext(ctx, dockerCLI, "tag", ref, tag).CombinedOutput()
		if err == nil {
			return nil
		}
		last = err
		if detail := strings.TrimSpace(string(out)); detail != "" {
			last = errors.New(detail)
		}
	}
	return last
}

// imageID returns the local image id of a reference, in the `sha256:...` form
// `docker build -q` prints.
func imageID(ctx context.Context, ref string) (string, error) {
	//nolint:gosec // Both arguments are this shim's own.
	out, err := exec.CommandContext(ctx, dockerCLI, "image", "inspect", "--format", "{{.Id}}", ref).Output()
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
	cmd := exec.CommandContext(ctx, dockerCLI, args...)
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

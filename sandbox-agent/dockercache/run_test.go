package dockercache_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/dockercache"
)

// stubDocker installs a shell script in place of the docker CLI. It appends
// every invocation to a log, and fails the first failTags `tag` calls with the
// error the daemon returns when another build raced this one for the name.
//
// The stub is a /bin/sh script and the build it stands in for runs /bin/true,
// so every test built on it is POSIX-only. That costs nothing: discobox-docker
// is the docker shim inside the Linux sandbox and never ships for Windows.
func stubDocker(t *testing.T, failTags int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the docker stub is a /bin/sh script; discobox-docker only runs in the Linux guest")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	const daemonRace = `Error response from daemon: failed to create an image ` +
		`docker.io/library/x:local with target sha256:abc after deleting the ` +
		`existing one: AlreadyExists: image "docker.io/library/x:local": already exists`
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %[1]s
if [ "$1" = tag ]; then
  attempts=$(grep -c '^tag ' %[1]s)
  if [ "$attempts" -le %[2]d ]; then
    echo '%[3]s' >&2
    exit 1
  fi
fi
exit 0
`, log, failTags, daemonRace)

	path := filepath.Join(dir, "docker")
	//nolint:gosec // A stub standing in for a CLI has to be executable.
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub docker: %v", err)
	}
	dockercache.SetDockerCLI(path)
	t.Cleanup(func() { dockercache.SetDockerCLI(dockercache.DefaultDockerCLI) })
	return log
}

// callsTo counts the stub invocations whose command line starts with prefix.
func callsTo(t *testing.T, log, prefix string) int {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// rewrittenBuild is what Rewrite hands back for a build with one tag. Argv is a
// command that simply succeeds: this exercises what happens around the build,
// not the build.
func rewrittenBuild() dockercache.Args {
	return dockercache.Args{
		Argv:        []string{"/bin/true"},
		Rewritten:   true,
		RegistryRef: "discobox-pool-proxy:5000/discobox-build/abc:build",
		Tags:        []string{"discobox-sandbox-agent:local"},
	}
}

// A tag another build raced us for is retried rather than failing the build.
// The losing tag is complete by the time the daemon rejects it, so asking again
// is all it takes.
func TestARacedTagIsRetried(t *testing.T) {
	log := stubDocker(t, 1)

	if code := dockercache.BuildViaRegistry(context.Background(), rewrittenBuild()); code != 0 {
		t.Fatalf("exit = %d, want a build that succeeded on the retry", code)
	}
	if got := callsTo(t, log, "tag "); got != 2 {
		t.Fatalf("tag attempts = %d, want the raced one and its retry", got)
	}
}

// A tag that keeps failing is still a failed build, and gives up rather than
// retrying forever.
func TestATagThatKeepsFailingFailsTheBuild(t *testing.T) {
	log := stubDocker(t, 99)

	if code := dockercache.BuildViaRegistry(context.Background(), rewrittenBuild()); code == 0 {
		t.Fatal("a tag that never succeeds should fail the build")
	}
	if got := callsTo(t, log, "tag "); got != 3 {
		t.Fatalf("tag attempts = %d, want the bounded set", got)
	}
}

// The synthesized reference is dropped however the build ends. Left behind, it
// is a discobox-build/<hex>:build entry naming a whole image that nothing will
// ever clean up.
func TestTheRegistryReferenceIsDroppedOnFailureToo(t *testing.T) {
	log := stubDocker(t, 99)

	dockercache.BuildViaRegistry(context.Background(), rewrittenBuild())

	if got := callsTo(t, log, "rmi "); got != 1 {
		t.Fatalf("rmi calls = %d, want the reference dropped after the failed tag", got)
	}
}

func TestTheRegistryReferenceIsDroppedOnSuccess(t *testing.T) {
	log := stubDocker(t, 0)

	if code := dockercache.BuildViaRegistry(context.Background(), rewrittenBuild()); code != 0 {
		t.Fatalf("exit = %d, want success", code)
	}
	if got := callsTo(t, log, "rmi "); got != 1 {
		t.Fatalf("rmi calls = %d, want the reference dropped", got)
	}
}

// An untagged build has nothing else naming the image, so removing its last
// reference would delete the image itself.
func TestAnUntaggedBuildKeepsItsReference(t *testing.T) {
	log := stubDocker(t, 0)
	build := rewrittenBuild()
	build.Tags = nil

	if code := dockercache.BuildViaRegistry(context.Background(), build); code != 0 {
		t.Fatalf("exit = %d, want success", code)
	}
	if got := callsTo(t, log, "rmi "); got != 0 {
		t.Fatalf("rmi calls = %d, want the reference kept", got)
	}
}

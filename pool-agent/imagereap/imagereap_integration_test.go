package imagereap

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/harness"
)

const dockerIntegrationEnv = "DISCOBOX_DOCKER_INTEGRATION"

// TestIntegrationReclaimRemovesOnlyUnusedLabeledImages exercises the parts unit
// tests cannot: that the label filter and LastTagTime actually behave as the
// design assumes on a real daemon, and that untag-then-remove disposes of a
// multi-tag image without Force.
func TestIntegrationReclaimRemovesOnlyUnusedLabeledImages(t *testing.T) {
	if os.Getenv(dockerIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run Docker integration tests", dockerIntegrationEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}
	defer func() { _ = cli.Close() }()

	unique := fmt.Sprint(time.Now().UnixNano())
	repository := "discobox-imagereap-" + unique

	// A superseded build and the one that replaced it, in one repository. The
	// superseded one carries two tags, because removal by ID alone fails on a
	// multi-tagged image — the case that forced untag-first.
	superseded := buildTestImage(ctx, t, cli, repository+":v1", true)
	tagImage(ctx, t, cli, superseded, repository+":v1-alias")
	current := buildTestImage(ctx, t, cli, repository+":v2", true)
	if superseded == current {
		t.Fatal("test images are identical; the newest-per-repository rule cannot be exercised")
	}
	used := buildTestImage(ctx, t, cli, "discobox-imagereap-used-"+unique+":a", true)
	unlabeled := buildTestImage(ctx, t, cli, "discobox-imagereap-unlabeled-"+unique+":a", false)
	t.Cleanup(func() {
		for _, image := range []string{superseded, current, used, unlabeled} {
			_, _ = cli.ImageRemove(context.Background(), image, client.ImageRemoveOptions{Force: true, PruneChildren: true})
		}
	})

	// A *stopped* container still counts as usage, which is the property that
	// keeps a stopped sandbox's image alive.
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: used, Cmd: []string{"true"}},
		Name:   "discobox-imagereap-" + unique,
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() {
		_, _ = cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	})

	// Every image was just built, so a retention of an hour must reclaim
	// nothing: freshly arrived images are exactly what retention protects.
	fresh, err := Reclaim(ctx, cli, Options{Retention: time.Hour})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(fresh.Removed) != 0 {
		t.Fatalf("reclaimed %v within the retention window", fresh.Removed)
	}

	// Now age everything out. Only the superseded labeled image may go.
	result, err := Reclaim(ctx, cli, Options{Retention: time.Nanosecond, Now: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !containsID(result.Removed, superseded) {
		t.Fatalf("removed = %v, want the superseded image %s", result.Removed, superseded)
	}
	// The newest of a repository is the current build by construction — the one
	// a mutable tag points at and the next build layers on — and reclaiming it
	// is what stranded a developer's watcher.
	if containsID(result.Removed, current) {
		t.Fatalf("removed the newest image of its repository: %s", current)
	}
	if containsID(result.Removed, used) {
		t.Fatalf("removed the image a stopped container still uses: %s", used)
	}
	if containsID(result.Removed, unlabeled) {
		t.Fatalf("removed an unlabeled image Discobox does not own: %s", unlabeled)
	}
	if _, err := cli.ImageInspect(ctx, superseded); err == nil || !cerrdefs.IsNotFound(err) {
		t.Fatalf("multi-tagged image survived removal: %v", err)
	}
	for _, survivor := range []string{current, used, unlabeled} {
		if _, err := cli.ImageInspect(ctx, survivor); err != nil {
			t.Fatalf("image %s did not survive: %v", survivor, err)
		}
	}

	// Surviving is not enough for an in-use image: the daemon refuses to delete
	// one but will untag it without complaint, so an unguarded pass leaves the
	// container running with an image nothing can name again.
	inspect, err := cli.ImageInspect(ctx, used)
	if err != nil {
		t.Fatalf("inspect in-use image: %v", err)
	}
	if len(inspect.RepoTags) == 0 {
		t.Fatalf("in-use image %s was stripped of its tags", used)
	}

	// Keep must win over age for an image nothing runs, which is how a
	// development base image survives.
	kept, err := Reclaim(ctx, cli, Options{Retention: time.Nanosecond, Now: time.Now().Add(time.Minute), Keep: []string{used}})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if containsID(kept.Removed, used) {
		t.Fatalf("removed a kept image: %s", used)
	}
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// buildTestImage builds a minimal scratch image and returns its ID.
func buildTestImage(ctx context.Context, t *testing.T, cli *client.Client, reference string, labeled bool) string {
	t.Helper()
	dockerfile := "FROM scratch\nCOPY marker /marker\n"
	if labeled {
		dockerfile = fmt.Sprintf("FROM scratch\nLABEL %s=%s\nCOPY marker /marker\n", harness.ReclaimLabel, harness.ReclaimLabelValue)
	}
	context := buildContext(t, map[string]string{"Dockerfile": dockerfile, "marker": reference})

	build, err := cli.ImageBuild(ctx, context, client.ImageBuildOptions{Tags: []string{reference}, Remove: true})
	if err != nil {
		t.Fatalf("build %s: %v", reference, err)
	}
	body, err := io.ReadAll(build.Body)
	_ = build.Body.Close()
	if err != nil {
		t.Fatalf("read build output for %s: %v", reference, err)
	}
	if strings.Contains(string(body), `"error"`) {
		t.Fatalf("build %s failed: %s", reference, body)
	}
	inspect, err := cli.ImageInspect(ctx, reference)
	if err != nil {
		t.Fatalf("inspect %s: %v", reference, err)
	}
	return inspect.ID
}

func tagImage(ctx context.Context, t *testing.T, cli *client.Client, source, target string) {
	t.Helper()
	if _, err := cli.ImageTag(ctx, client.ImageTagOptions{Source: source, Target: target}); err != nil {
		t.Fatalf("tag %s as %s: %v", source, target, err)
	}
}

func buildContext(t *testing.T, files map[string]string) io.Reader {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := tar.NewWriter(buffer)
	for name, content := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer
}

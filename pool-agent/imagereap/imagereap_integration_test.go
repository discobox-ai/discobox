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

	"github.com/obot-platform/discobox/harness"
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
	// Two tags on one image: removal by ID alone fails on this, which is the
	// case that forced untag-first.
	unused := buildTestImage(ctx, t, cli, "discobox-imagereap-unused-"+unique+":a", true)
	tagImage(ctx, t, cli, unused, "discobox-imagereap-unused-"+unique+":b")
	used := buildTestImage(ctx, t, cli, "discobox-imagereap-used-"+unique+":a", true)
	unlabeled := buildTestImage(ctx, t, cli, "discobox-imagereap-unlabeled-"+unique+":a", false)
	t.Cleanup(func() {
		for _, image := range []string{unused, used, unlabeled} {
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

	// Now age everything out. Only the unused labeled image may go.
	result, err := Reclaim(ctx, cli, Options{Retention: time.Nanosecond, Now: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !containsID(result.Removed, unused) {
		t.Fatalf("removed = %v, want the unused labeled image %s", result.Removed, unused)
	}
	if containsID(result.Removed, used) {
		t.Fatalf("removed the image a stopped container still uses: %s", used)
	}
	if containsID(result.Removed, unlabeled) {
		t.Fatalf("removed an unlabeled image Discobox does not own: %s", unlabeled)
	}
	if _, err := cli.ImageInspect(ctx, unused); err == nil || !cerrdefs.IsNotFound(err) {
		t.Fatalf("multi-tagged image survived removal: %v", err)
	}
	if _, err := cli.ImageInspect(ctx, used); err != nil {
		t.Fatalf("in-use image did not survive: %v", err)
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

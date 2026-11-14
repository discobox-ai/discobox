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
	others := labeledImageIDs(ctx, t, cli)

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
	//
	// Every pass keeps what was on the daemon before the test, and names the
	// test's repository by a tag no image has, which gives the repository its
	// newest-image floor without keeping any image by name.
	keep := append([]string{repository + ":current"}, others...)
	fresh, err := Reclaim(ctx, cli, Options{Retention: time.Hour, Keep: keep})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(fresh.Removed) != 0 {
		t.Fatalf("reclaimed %v within the retention window", fresh.Removed)
	}

	// Now age everything out. Only the superseded labeled image may go.
	result, err := Reclaim(ctx, cli, Options{Retention: time.Nanosecond, Now: time.Now().Add(time.Minute), Keep: keep})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !containsID(result.Removed, superseded) {
		t.Fatalf("removed = %v, want the superseded image %s", result.Removed, superseded)
	}
	// The newest of a kept repository is the current build by construction —
	// the one a mutable tag points at and the next build layers on — and
	// reclaiming it is what stranded a developer's watcher.
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
	kept, err := Reclaim(ctx, cli, Options{Retention: time.Nanosecond, Now: time.Now().Add(time.Minute), Keep: append([]string{used, repository + ":current"}, others...)})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if containsID(kept.Removed, used) {
		t.Fatalf("removed a kept image: %s", used)
	}
}

// An image that arrives by `docker load` — how a VM pool's daemon gets every
// Discobox image, from the image cache — must still be reclaimable once nothing
// uses it. The classic image store (overlay2) stamps LastTagTime only on `docker
// tag`, never on a load or a pull, so a pool that only ever loads images reports
// every one of them with no arrival at all (#1094).
func TestIntegrationReclaimRemovesUnusedLoadedImages(t *testing.T) {
	if os.Getenv(dockerIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run Docker integration tests", dockerIntegrationEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cli := newIntegrationClient(t)

	others := labeledImageIDs(ctx, t, cli)
	repository := "discobox-imagereap-loaded-" + fmt.Sprint(time.Now().UnixNano())
	superseded, current := loadTestImages(ctx, t, cli, repository)
	keep := append([]string{repository + ":current"}, others...)

	// Two days on, with nothing using either image, an ordinary pass must
	// reclaim the superseded one: it stamps both, the later build last, and the
	// two days are counted from that stamp.
	result, err := Reclaim(ctx, cli, Options{Retention: time.Hour, Now: time.Now().Add(48 * time.Hour), Keep: keep})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !containsID(result.Removed, superseded) {
		inspect, _ := cli.ImageInspect(ctx, superseded)
		t.Fatalf("removed = %v, want the superseded loaded image %s (daemon reports LastTagTime %s)",
			result.Removed, superseded, inspect.Metadata.LastTagTime)
	}
	if containsID(result.Removed, current) {
		t.Fatalf("removed the newest image of a kept repository: %s", current)
	}
}

// An ordinary pass starts an unknown age rather than counting it: on the
// classic store, the first pass that sees a loaded image stamps it, and the
// image is then only as old as that pass.
func TestIntegrationReclaimStartsAnUnknownAge(t *testing.T) {
	if os.Getenv(dockerIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run Docker integration tests", dockerIntegrationEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cli := newIntegrationClient(t)

	others := labeledImageIDs(ctx, t, cli)
	repository := "discobox-imagereap-unstamped-" + fmt.Sprint(time.Now().UnixNano())
	superseded, current := loadTestImages(ctx, t, cli, repository)
	if knownArrivalOf(ctx, t, cli, superseded) {
		t.Skip("this daemon records a load's arrival (the containerd image store): nothing to start")
	}

	result, err := Reclaim(ctx, cli, Options{Retention: time.Hour, Keep: others})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(result.Removed) != 0 {
		t.Fatalf("removed %v on the pass that started their age", result.Removed)
	}
	for _, image := range []string{superseded, current} {
		if !knownArrivalOf(ctx, t, cli, image) {
			t.Fatalf("image %s still has no arrival after a pass", image)
		}
	}
	older, _ := cli.ImageInspect(ctx, superseded)
	newer, _ := cli.ImageInspect(ctx, current)
	if !newer.Metadata.LastTagTime.After(older.Metadata.LastTagTime) {
		t.Fatalf("the later build was stamped at %s, not after the earlier one's %s",
			newer.Metadata.LastTagTime, older.Metadata.LastTagTime)
	}
}

// An explicit clear removes an unused image the daemon recorded no arrival
// for at once, and Stamp is what gives a just-loaded image an arrival for the
// clear's floor to see.
func TestIntegrationClearRemovesAnUnknownAgeAndSparesAStampedOne(t *testing.T) {
	if os.Getenv(dockerIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run Docker integration tests", dockerIntegrationEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cli := newIntegrationClient(t)

	others := labeledImageIDs(ctx, t, cli)
	repository := "discobox-imagereap-clear-" + fmt.Sprint(time.Now().UnixNano())
	ageless, stamped := loadTestImages(ctx, t, cli, repository)
	if knownArrivalOf(ctx, t, cli, ageless) {
		t.Skip("this daemon records a load's arrival (the containerd image store): no unknown age to clear")
	}
	if err := Stamp(ctx, cli, repository+":v2"); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if !knownArrivalOf(ctx, t, cli, stamped) {
		t.Fatalf("Stamp left %s with no arrival", stamped)
	}

	result, err := Reclaim(ctx, cli, Options{Retention: time.Hour, Clear: true, Keep: others})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !containsID(result.Removed, ageless) {
		t.Fatalf("removed = %v, want the unused image of unknown age %s", result.Removed, ageless)
	}
	if containsID(result.Removed, stamped) {
		t.Fatalf("a clear removed %s, stamped within its floor", stamped)
	}
}

func newIntegrationClient(t *testing.T) *client.Client {
	t.Helper()
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// labeledImageIDs lists the labeled images already on the daemon, which every
// pass a test runs keeps: a developer's daemon holds images of its own, and a
// test must reclaim only what it made.
func labeledImageIDs(ctx context.Context, t *testing.T, cli *client.Client) []string {
	t.Helper()
	candidates, err := labeledImages(ctx, cli)
	if err != nil {
		t.Fatalf("list labeled images: %v", err)
	}
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.ID)
	}
	return ids
}

// loadTestImages builds two labeled images in repository, v1 then v2, and
// round-trips them through save and load, so the daemon knows them only as
// loaded images, the way a pool's image cache delivers them. Each is loaded on
// its own, v1 first, so a store that records a load's arrival orders them as
// they were built.
func loadTestImages(ctx context.Context, t *testing.T, cli *client.Client, repository string) (string, string) {
	t.Helper()
	v1 := buildTestImage(ctx, t, cli, repository+":v1", true)
	// A second apart, so the two builds' Created times order them.
	time.Sleep(time.Second)
	v2 := buildTestImage(ctx, t, cli, repository+":v2", true)
	t.Cleanup(func() {
		for _, image := range []string{v1, v2} {
			_, _ = cli.ImageRemove(context.Background(), image, client.ImageRemoveOptions{Force: true, PruneChildren: true})
		}
	})
	saved := map[string][]byte{}
	for _, tag := range []string{"v1", "v2"} {
		archive, err := cli.ImageSave(ctx, []string{repository + ":" + tag})
		if err != nil {
			t.Fatalf("save %s: %v", tag, err)
		}
		saved[tag], err = io.ReadAll(archive)
		_ = archive.Close()
		if err != nil {
			t.Fatalf("read saved image %s: %v", tag, err)
		}
	}
	for _, image := range []string{v1, v2} {
		if _, err := cli.ImageRemove(ctx, image, client.ImageRemoveOptions{Force: true}); err != nil {
			t.Fatalf("remove %s before load: %v", image, err)
		}
	}
	for _, tag := range []string{"v1", "v2"} {
		load, err := cli.ImageLoad(ctx, bytes.NewReader(saved[tag]), client.ImageLoadWithQuiet(true))
		if err != nil {
			t.Fatalf("load %s: %v", tag, err)
		}
		_, _ = io.Copy(io.Discard, load)
		_ = load.Close()
	}
	return v1, v2
}

func knownArrivalOf(ctx context.Context, t *testing.T, cli *client.Client, image string) bool {
	t.Helper()
	inspect, err := cli.ImageInspect(ctx, image)
	if err != nil {
		t.Fatalf("inspect %s: %v", image, err)
	}
	created, _ := time.Parse(time.RFC3339Nano, inspect.Created)
	return Candidate{LastLocal: inspect.Metadata.LastTagTime, Created: created}.knownArrival()
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

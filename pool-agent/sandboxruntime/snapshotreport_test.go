package sandboxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

// A Docker pool lists its snapshot images by key, with the plan each names,
// its size and when it was built, and how many of its sandboxes' containers
// run it; a snapshot committed before it named its plan is listed by its key
// alone (ADR 26-11-17-316 §§2-3).
func TestADockerPoolReportsItsSnapshots(t *testing.T) {
	plan, err := json.Marshal(SnapshotPlanName{Base: "sha256:base", Agent: "sha256:agent", Off: []string{"docker"}})
	if err != nil {
		t.Fatal(err)
	}
	var listed string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.45")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			listed = r.URL.Query().Get("filters")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Id": "img_new", "RepoTags": []string{snapshotRepository + ":knew"}, "Created": 1_763_400_000, "Size": 3 << 30, "Labels": map[string]string{sandboxLabelPool: "pool_1", snapshotPlanLabel: string(plan)}},
				{"Id": "img_old", "RepoTags": []string{snapshotRepository + ":kold"}, "Created": 1_763_300_000, "Size": 2 << 30, "Labels": map[string]string{sandboxLabelPool: "pool_1"}},
			})
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			// Counted by what each container records it was made from,
			// never by the image it runs: ctr_old runs the snapshot's image
			// but was made before the label, and says nothing.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Id": "ctr_1", "ImageID": "img_new", "Labels": map[string]string{sandboxLabelStartedFrom: "snapshot:knew"}},
				{"Id": "ctr_2", "ImageID": "img_new", "Labels": map[string]string{sandboxLabelStartedFrom: "snapshot:knew"}},
				{"Id": "ctr_3", "ImageID": "sha256:base", "Labels": map[string]string{sandboxLabelStartedFrom: "base"}},
				{"Id": "ctr_old", "ImageID": "img_new", "Labels": map[string]string{}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	r := testDockerRuntime(&DockerSandboxRuntime{root: withTestRoot(t), client: cli, projectID: "proj_1", poolID: "pool_1"})
	report, err := r.Snapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed, snapshotRepository) {
		t.Errorf("listed with filters %s, want the snapshot repository", listed)
	}
	if len(report.Held) != 2 || report.Building == nil || report.Failed == nil {
		t.Fatalf("report = %+v, want two held and empty, not absent, lists", report)
	}
	newest, oldest := report.Held[0], report.Held[1]
	if newest.Key != "knew" || newest.Boxes != 2 || newest.SizeBytes == nil || *newest.SizeBytes != 3<<30 || !newest.BuiltAt.Equal(time.Unix(1_763_400_000, 0)) || newest.LastUsedAt != nil {
		t.Errorf("newest = %+v", newest)
	}
	if newest.Plan == nil || newest.Plan.Base != "sha256:base" || newest.Plan.Off[0] != "docker" {
		t.Errorf("newest plan = %+v", newest.Plan)
	}
	if oldest.Key != "kold" || oldest.Plan != nil || oldest.Boxes != 0 {
		t.Errorf("oldest = %+v, want its key alone, no plan, no box", oldest)
	}
}

// A build is reported from when it is asked for: queued while another holds
// the build lock, and started once it does.
func TestASnapshotBuildIsReportedQueuedThenStarted(t *testing.T) {
	r := testDockerRuntime(&DockerSandboxRuntime{root: withTestRoot(t), projectID: "proj_1", poolID: "pool_1"})
	first, err := r.newSnapshotPlan("sha256:base", snapshotCreate("sbx_1", "a"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.newSnapshotPlan("sha256:base", snapshotCreate("sbx_2", "c"))
	if err != nil {
		t.Fatal(err)
	}
	release, running := make(chan struct{}), make(chan struct{})
	r.requestSnapshot(first, func(context.Context, snapshotPlan) error {
		close(running)
		<-release
		return nil
	})
	<-running
	r.requestSnapshot(second, func(context.Context, snapshotPlan) error { return nil })
	report, err := r.snapshotReport(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Building) != 2 {
		t.Fatalf("building = %+v, want both builds", report.Building)
	}
	byKey := map[string]BuildingSnapshot{}
	for _, build := range report.Building {
		byKey[build.Key] = build
	}
	if build := byKey[first.Key()]; build.StartedAt == nil || build.RequestedAt.IsZero() || build.Plan.Base != "sha256:base" || len(build.Plan.Bundles) != 2 {
		t.Errorf("the running build = %+v, want it started, by its plan", build)
	}
	if build := byKey[second.Key()]; build.StartedAt != nil {
		t.Errorf("the queued build = %+v, want no start", build)
	}
	close(release)
	waitForSnapshotBuilds(t, r)
	if report, _ := r.snapshotReport(nil); len(report.Building) != 0 || len(report.Failed) != 0 {
		t.Errorf("after both built: %+v", report)
	}
}

// A failed plan is reported with why and until when, and a failure a pool
// wrote before it kept the reason — a bare time — still holds the plan back
// and reads as one with no reason and no plan.
func TestAFailedPlanIsReportedWithItsReason(t *testing.T) {
	r := testDockerRuntime(&DockerSandboxRuntime{root: withTestRoot(t), projectID: "proj_1", poolID: "pool_1"})
	plan, err := r.newSnapshotPlan("sha256:base", snapshotCreate("sbx_1", "a"))
	if err != nil {
		t.Fatal(err)
	}
	r.requestSnapshot(plan, func(context.Context, snapshotPlan) error { return errors.New("feature go failed: exit status 2") })
	waitForSnapshotBuilds(t, r)
	now := time.Now().UTC()
	old := filepath.Join(filepath.Dir(r.snapshotFailurePath(plan.Key())), "kbare")
	if err := os.WriteFile(old, []byte(now.Add(-time.Minute).Format(time.RFC3339)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(old), "kexpired"), []byte(now.Add(-2*snapshotRetryAfter).Format(time.RFC3339)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !r.snapshotFailedRecently("kbare", now) {
		t.Fatal("a failure written as a bare time no longer holds its plan back")
	}
	report, err := r.snapshotReport(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failed) != 2 {
		t.Fatalf("failed = %+v, want the two within the retry", report.Failed)
	}
	byKey := map[string]FailedSnapshot{}
	for _, failed := range report.Failed {
		byKey[failed.Key] = failed
	}
	built := byKey[plan.Key()]
	if built.Error != "feature go failed: exit status 2" || built.Plan == nil || built.Plan.Base != "sha256:base" || !built.RetryAfter.Equal(built.FailedAt.Add(snapshotRetryAfter)) {
		t.Errorf("the failed build = %+v", built)
	}
	if bare := byKey["kbare"]; bare.Error != "" || bare.Plan != nil || bare.FailedAt.IsZero() {
		t.Errorf("the bare failure = %+v, want a time alone", bare)
	}
}

// The reason a failure keeps is bounded, its head and its end kept, and cut
// on a rune boundary.
func TestASnapshotFailureKeepsABoundedReason(t *testing.T) {
	long := "its provisioning failed (status 1): " + strings.Repeat("é", snapshotErrorLimit) + " the end"
	got := boundSnapshotError(long)
	if len(got) > snapshotErrorLimit || !utf8.ValidString(got) {
		t.Fatalf("bounded to %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	if !strings.HasPrefix(got, "its provisioning failed") || !strings.HasSuffix(got, " the end") {
		t.Errorf("bounded = %q, want its head and its end", got)
	}
	if short := "exit status 2"; boundSnapshotError(short) != short {
		t.Error("a short reason was changed")
	}
}

// A sandbox container records what it was made from, and the state channel
// carries it from the container's labels on each delta and each sync; one
// made before has no such label and says nothing (ADR 26-11-17-316 §5).
func TestAContainerSaysWhatItWasMadeFrom(t *testing.T) {
	r := testDockerRuntime(&DockerSandboxRuntime{projectID: "proj_1", poolID: "pool_1"})
	for name, tc := range map[string]struct {
		snapshot string
		want     *StartedFrom
	}{
		"from a snapshot": {"k1", &StartedFrom{Snapshot: "k1"}},
		"from the base":   {"", &StartedFrom{}},
	} {
		labels := r.labels("sbx_1", "", "", tc.snapshot)
		if got := startedFromLabels(labels); got == nil || *got != *tc.want {
			t.Errorf("%s: read back %+v, want %+v", name, got, tc.want)
		}
		observed, ok := observationFromEvent(events.Message{Action: events.ActionStart, Actor: events.Actor{Attributes: labels}})
		if !ok || observed.StartedFrom == nil || *observed.StartedFrom != *tc.want {
			t.Errorf("%s: the start event observed %+v", name, observed)
		}
		if gone, _ := observationFromEvent(events.Message{Action: events.ActionDestroy, Actor: events.Actor{Attributes: labels}}); gone.StartedFrom != nil {
			t.Errorf("%s: a destroyed container said it was made from %+v", name, gone.StartedFrom)
		}
	}
	for _, value := range []string{"", "snapshot:", "image:x"} {
		if got := startedFromLabels(map[string]string{sandboxLabelStartedFrom: value}); got != nil {
			t.Errorf("label %q read as %+v, want unknown", value, got)
		}
	}
	if got := startedFromLabels(map[string]string{sandboxLabelSandbox: "sbx_1"}); got != nil {
		t.Errorf("a container made before the label read as %+v, want unknown", got)
	}
}

// A Docker pool lists its snapshots by repository alone and keeps its own by
// their pool label itself: a daemon-side label filter makes a containerd
// image store read every image's config on the daemon, so the list's cost
// grows with the host's images, not the pool's snapshots.
func TestADockerPoolListsItsSnapshotsWithoutADaemonLabelFilter(t *testing.T) {
	var listed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.45")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			listed = append(listed, r.URL.Query().Get("filters"))
			// The repository holds every pool's snapshots on a shared daemon.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Id": "img_mine", "RepoTags": []string{snapshotRepository + ":kmine"}, "Created": 1_763_400_000, "Labels": map[string]string{sandboxLabelPool: "pool_1"}},
				{"Id": "img_other", "RepoTags": []string{snapshotRepository + ":kother"}, "Created": 1_763_300_000, "Labels": map[string]string{sandboxLabelPool: "pool_2"}},
			})
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	r := testDockerRuntime(&DockerSandboxRuntime{root: withTestRoot(t), client: cli, projectID: "proj_1", poolID: "pool_1"})
	report, err := r.Snapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, filters := range listed {
		var parsed map[string]map[string]bool
		if err := json.Unmarshal([]byte(filters), &parsed); err != nil {
			t.Fatalf("filters %q: %v", filters, err)
		}
		if _, ok := parsed["label"]; ok {
			t.Errorf("listed with filters %s, want the repository alone, no label filter", filters)
		}
		if !parsed["reference"][snapshotRepository] {
			t.Errorf("listed with filters %s, want the snapshot repository", filters)
		}
	}
	if len(report.Held) != 1 || report.Held[0].Key != "kmine" {
		t.Errorf("held = %+v, want this pool's snapshot alone", report.Held)
	}
}

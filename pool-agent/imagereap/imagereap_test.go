package imagereap

import (
	"testing"
	"time"
)

const retention = 24 * time.Hour

func ids(candidates []Candidate) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.ID)
	}
	return out
}

func set(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func assertIDs(t *testing.T, got []Candidate, want ...string) {
	t.Helper()
	gotIDs := ids(got)
	if len(gotIDs) != len(want) {
		t.Fatalf("reclaimable = %v, want %v", gotIDs, want)
	}
	for i, id := range want {
		if gotIDs[i] != id {
			t.Fatalf("reclaimable = %v, want %v", gotIDs, want)
		}
	}
}

func TestReclaimableAgesOutUnusedImages(t *testing.T) {
	now := time.Now()
	candidates := []Candidate{
		{ID: "sha256:fresh", LastLocal: now.Add(-time.Hour)},
		{ID: "sha256:stale", LastLocal: now.Add(-retention - time.Minute)},
		// Exactly at the boundary is still within retention: the window is how
		// long an image is kept, so it must elapse before the image goes.
		{ID: "sha256:boundary", LastLocal: now.Add(-retention)},
	}

	assertIDs(t, Reclaimable(candidates, nil, nil, retention, now), "sha256:stale", "sha256:boundary")
}

// The regression that ate a developer's images: the watcher had built a new
// sandbox base, but the server's keep set was the manifest it loaded at startup,
// which still named the previous one. Age then made the *current* image — the
// one the next harness build layers on — look like garbage, and reclaiming it
// stranded the watcher, which only rebuilds when a source file changes.
func TestReclaimableNeverTakesTheNewestImageOfARepository(t *testing.T) {
	now := time.Now()
	old := now.Add(-4 * time.Hour)
	current := now.Add(-30 * time.Minute)
	candidates := []Candidate{
		// The image the stale keep set still names.
		{ID: "sha256:previous", RepoTags: []string{"discobox-sandbox-agent:dev-old"}, LastLocal: old},
		// The build in flight: newer, in no keep set yet, and past retention.
		{ID: "sha256:current", RepoTags: []string{"discobox-sandbox-agent:local", "discobox-sandbox-agent:dev-new"}, LastLocal: current},
	}
	keep := set("discobox-sandbox-agent:dev-old")

	assertIDs(t, Reclaimable(candidates, nil, keep, 15*time.Minute, now))

	// Without the stale keep entry the superseded image still goes; only the
	// newest is protected, so this reclaims rather than hoarding.
	assertIDs(t, Reclaimable(candidates, nil, nil, 15*time.Minute, now), "sha256:previous")
}

func TestReclaimableTracksNewestPerRepositoryIndependently(t *testing.T) {
	now := time.Now()
	// Everything here is well past retention, so only the newest-per-repository
	// rule decides what survives.
	stale := now.Add(-30 * 24 * time.Hour)
	candidates := []Candidate{
		{ID: "sha256:agent-old", RepoTags: []string{"discobox-sandbox-agent:dev-a"}, LastLocal: stale},
		{ID: "sha256:agent-new", RepoTags: []string{"discobox-sandbox-agent:dev-b"}, LastLocal: stale.Add(time.Hour)},
		{ID: "sha256:pool-old", RepoTags: []string{"discobox-pool-agent:dev-a"}, LastLocal: stale},
		{ID: "sha256:pool-new", RepoTags: []string{"discobox-pool-agent:dev-b"}, LastLocal: stale.Add(2 * time.Hour)},
		// A registry reference with a port must not be split at the port colon,
		// which would make every port a repository of its own.
		{ID: "sha256:ported", RepoTags: []string{"localhost:5000/discobox:v1"}, LastLocal: stale},
		// Untagged images belong to no repository, so nothing protects them.
		{ID: "sha256:dangling", LastLocal: stale},
	}

	// The single ported image is the newest of its own repository, so it stays.
	assertIDs(t, Reclaimable(candidates, nil, nil, retention, now),
		"sha256:agent-old", "sha256:pool-old", "sha256:dangling")
}

func TestReclaimableKeepsImagesAnyContainerUses(t *testing.T) {
	now := time.Now()
	// Old enough to go, but a container — which may well be a stopped sandbox —
	// still refers to it.
	candidates := []Candidate{{ID: "sha256:running", LastLocal: now.Add(-30 * 24 * time.Hour)}}

	assertIDs(t, Reclaimable(candidates, set("sha256:running"), nil, retention, now))
}

func TestReclaimableHonorsKeepByIDTagAndDigest(t *testing.T) {
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)
	candidates := []Candidate{
		{ID: "sha256:byid", LastLocal: old},
		{ID: "sha256:bytag", RepoTags: []string{"discobox-sandbox-agent:dev-abc"}, LastLocal: old},
		{ID: "sha256:bydigest", RepoDigests: []string{"ghcr.io/x/y@sha256:dd"}, LastLocal: old},
		// Older than the kept one in the same repository, so newest-per-repository
		// does not protect it and only the keep set is under test here.
		{ID: "sha256:unkept", RepoTags: []string{"discobox-sandbox-agent:dev-old"}, LastLocal: old.Add(-time.Hour)},
	}
	keep := set("sha256:byid", "discobox-sandbox-agent:dev-abc", "ghcr.io/x/y@sha256:dd")

	// The keep set is what protects a base image nothing runs directly: on a dev
	// host only harness images have containers, so the sandbox-agent base they
	// are built FROM would otherwise age out from under the next build.
	assertIDs(t, Reclaimable(candidates, nil, keep, retention, now), "sha256:unkept")
}

func TestReclaimableSkipsImagesWithNoKnownLocalAge(t *testing.T) {
	now := time.Now()
	candidates := []Candidate{{ID: "sha256:ageless"}}

	assertIDs(t, Reclaimable(candidates, nil, nil, retention, now))
}

func TestReclaimableSkipsImagesWithoutAnID(t *testing.T) {
	now := time.Now()
	candidates := []Candidate{{LastLocal: now.Add(-retention - time.Minute)}}

	assertIDs(t, Reclaimable(candidates, nil, nil, retention, now))
}

func TestReclaimableIgnoresEmptyKeepEntries(t *testing.T) {
	now := time.Now()
	// An empty keep entry must not match an image that has no tags, which would
	// otherwise pin every untagged image on the daemon.
	candidates := []Candidate{{ID: "sha256:untagged", RepoTags: []string{""}, LastLocal: now.Add(-retention - time.Minute)}}

	assertIDs(t, Reclaimable(candidates, nil, set(""), retention, now), "sha256:untagged")
}

func TestReclaimInterval(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retention time.Duration
		want      time.Duration
	}{
		// The production window keeps the hourly sweep it has always had.
		{name: "production", retention: DefaultRetention, want: time.Hour},
		// The development window is useless if the sweep still runs hourly, so
		// the cadence has to come down with it.
		{name: "development", retention: DevelopmentRetention, want: DevelopmentRetention / 2},
		// A pathological setting must not become a busy loop...
		{name: "clamped low", retention: time.Second, want: minReclaimInterval},
		// ...nor an effectively dead one.
		{name: "clamped high", retention: 30 * 24 * time.Hour, want: maxReclaimInterval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReclaimInterval(tc.retention); got != tc.want {
				t.Fatalf("ReclaimInterval(%s) = %s, want %s", tc.retention, got, tc.want)
			}
		})
	}
}

func TestRetentionFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset", value: "", want: DefaultRetention},
		{name: "override", value: "72h", want: 72 * time.Hour},
		{name: "unparsable", value: "one week", wantErr: true},
		{name: "zero disables reclamation silently", value: "0", wantErr: true},
		{name: "negative", value: "-1h", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.value != "" {
				t.Setenv(RetentionEnv, tc.value)
			}
			got, err := RetentionFromEnv()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("RetentionFromEnv() = %s, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("RetentionFromEnv() error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("RetentionFromEnv() = %s, want %s", got, tc.want)
			}
		})
	}
}

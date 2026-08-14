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
		{ID: "sha256:unkept", RepoTags: []string{"discobox-sandbox-agent:dev-old"}, LastLocal: old},
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

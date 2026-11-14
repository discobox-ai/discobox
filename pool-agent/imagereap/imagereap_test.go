package imagereap

import (
	"strings"
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

	assertIDs(t, Reclaimable(candidates, nil, nil, retention, now, false), "sha256:stale", "sha256:boundary")
}

// A build clamped to SOURCE_DATE_EPOCH, which the Nix dev shell sets to 1980, is
// stamped with that epoch for both Created and LastTagTime on the containerd
// image store. It was reclaimed seconds after `task build:harness-image` made
// it, as decades old, because the watcher's retagged build of the same
// repository had the later — real — arrival (ADR 0143).
func TestReclaimableKeepsAnImageWhoseArrivalIsItsBuildTime(t *testing.T) {
	now := time.Now()
	epoch := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	candidates := []Candidate{
		{ID: "sha256:clamped", RepoTags: []string{"discobox-harness-shell:local"}, LastLocal: epoch, Created: epoch},
		{ID: "sha256:watcher", RepoTags: []string{"discobox-harness-shell:dev-abc"}, LastLocal: now.Add(-time.Minute), Created: epoch},
		// Retagging stamps a real arrival, so a superseded build is still
		// reclaimed however it was built.
		{ID: "sha256:superseded", RepoTags: []string{"discobox-harness-shell:dev-old"}, LastLocal: now.Add(-retention - time.Hour), Created: epoch},
		// No arrival reported at all is not reclaimed by an ordinary pass.
		{ID: "sha256:undescribed"},
	}

	assertIDs(t, Reclaimable(candidates, nil, nil, retention, now, false), "sha256:superseded")
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
	keep := []string{"discobox-sandbox-agent:dev-old"}

	assertIDs(t, Reclaimable(candidates, nil, keep, 15*time.Minute, now, false))

	// A keep reference naming the repository by another tag protects the
	// newest of it as well: the floor is the repository's, not the tag's.
	assertIDs(t, Reclaimable(candidates, nil, []string{"discobox-sandbox-agent:local-gone"}, 15*time.Minute, now, false),
		"sha256:previous")

	// A repository no keep reference names has no floor at all.
	assertIDs(t, Reclaimable(candidates, nil, nil, 15*time.Minute, now, false), "sha256:previous", "sha256:current")
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

	keep := []string{"discobox-sandbox-agent:local", "discobox-pool-agent:local", "localhost:5000/discobox:v2"}

	// The single ported image is the newest of its own repository, so it stays.
	assertIDs(t, Reclaimable(candidates, nil, keep, retention, now, false),
		"sha256:agent-old", "sha256:pool-old", "sha256:dangling")
}

// The reported pool: every per-harness and sandbox-agent repository was
// obsolete once every box ran the sandbox base, and the newest image of each
// was kept forever (#1094). Only a repository the keep set names keeps a floor.
func TestReclaimableGivesNoFloorToARepositoryNothingKeeps(t *testing.T) {
	now := time.Now()
	stale := now.Add(-30 * 24 * time.Hour)
	candidates := []Candidate{
		{ID: "sha256:shell-old", RepoTags: []string{"ghcr.io/discobox-ai/discobox-harness-shell:v0.17.0"}, LastLocal: stale},
		{ID: "sha256:shell-last", RepoTags: []string{"ghcr.io/discobox-ai/discobox-harness-shell:v0.18.0"}, LastLocal: stale.Add(time.Hour)},
		{ID: "sha256:base-old", RepoTags: []string{"ghcr.io/discobox-ai/discobox-sandbox-base:v0.19.0"}, LastLocal: stale},
		{ID: "sha256:base", RepoTags: []string{"ghcr.io/discobox-ai/discobox-sandbox-base:v0.19.3"}, LastLocal: stale.Add(time.Hour)},
	}
	keep := []string{"ghcr.io/discobox-ai/discobox-sandbox-base:v0.19.3"}

	assertIDs(t, Reclaimable(candidates, nil, keep, retention, now, false),
		"sha256:shell-old", "sha256:shell-last", "sha256:base-old")
}

// A keep reference written fully qualified names the same repository a daemon
// reports by its short name.
func TestReclaimableMatchesAKeptRepositoryByItsNormalizedName(t *testing.T) {
	now := time.Now()
	stale := now.Add(-30 * 24 * time.Hour)
	candidates := []Candidate{
		{ID: "sha256:old", RepoTags: []string{"discobox-sandbox-base:dev-a"}, LastLocal: stale},
		{ID: "sha256:new", RepoTags: []string{"discobox-sandbox-base:dev-b"}, LastLocal: stale.Add(time.Hour)},
	}

	assertIDs(t, Reclaimable(candidates, nil, []string{"docker.io/library/discobox-sandbox-base:local"}, retention, now, false),
		"sha256:old")
}

// With no known arrival in a kept repository, the floor falls back to the
// latest build, so a clear still leaves the current one.
func TestReclaimableRanksAKeptRepositoryWithNoArrivalByBuild(t *testing.T) {
	now := time.Now()
	candidates := []Candidate{
		{ID: "sha256:older", RepoTags: []string{"discobox-sandbox-base:dev-a"}, Created: now.Add(-2 * time.Hour)},
		{ID: "sha256:newer", RepoTags: []string{"discobox-sandbox-base:dev-b"}, Created: now.Add(-time.Hour)},
	}
	keep := []string{"discobox-sandbox-base:local"}

	assertIDs(t, Reclaimable(candidates, nil, keep, time.Hour, now, true), "sha256:older")
}

// An explicit clear removes an image the daemon recorded no arrival for at
// once: an ordinary pass waits for its stamp, a clear does not (#1094). What a
// container uses or the keep set names stays either way.
func TestReclaimableClearingRemovesAnUnknownAge(t *testing.T) {
	now := time.Now()
	candidates := []Candidate{
		{ID: "sha256:ageless", RepoTags: []string{"ghcr.io/discobox-ai/discobox-harness-codex:v0.18.0"}},
		{ID: "sha256:untagged"},
		{ID: "sha256:used", RepoTags: []string{"ghcr.io/discobox-ai/discobox-harness-shell:v0.18.0"}},
		{ID: "sha256:base", RepoTags: []string{"ghcr.io/discobox-ai/discobox-sandbox-base:v0.19.3"}},
		// Within the floor with a real arrival: what another pool's create
		// just pulled onto a shared daemon.
		{ID: "sha256:just-pulled", RepoTags: []string{"ghcr.io/discobox-ai/discobox-harness-pi:v0.18.0"}, LastLocal: now.Add(-time.Minute)},
	}
	keep := []string{"ghcr.io/discobox-ai/discobox-sandbox-base:v0.19.3"}

	assertIDs(t, Reclaimable(candidates, set("sha256:used"), keep, time.Hour, now, false))
	assertIDs(t, Reclaimable(candidates, set("sha256:used"), keep, time.Hour, now, true),
		"sha256:ageless", "sha256:untagged")
}

// A keep entry pinned by digest, or written fully qualified, keeps the image
// it names: keep entries and an image's references are matched in the same
// normalized forms the floor's repositories are.
func TestReclaimableKeepsAnImageNamedByAPinnedOrQualifiedReference(t *testing.T) {
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)
	digest := "sha256:" + strings.Repeat("a", 64)
	candidates := []Candidate{
		{ID: "sha256:pinned", RepoTags: []string{"ghcr.io/discobox-ai/discobox-sandbox-base:v0.19.3"}, LastLocal: old},
		{ID: "sha256:short", RepoTags: []string{"discobox-sandbox-base:local"}, LastLocal: old},
		{ID: "sha256:bydigest", RepoDigests: []string{"ghcr.io/x/y@" + digest}, LastLocal: old},
	}
	keep := []string{
		"ghcr.io/discobox-ai/discobox-sandbox-base:v0.19.3@" + digest,
		"docker.io/library/discobox-sandbox-base:local",
		"ghcr.io/x/y:v1@" + digest,
	}

	assertIDs(t, Reclaimable(candidates, nil, keep, retention, now, false))
}

// The upgrade order: the server stamps the new sandbox base as it lands, and
// the next pass stamps the older bases an upgraded pool holds with no age. The
// image that held the floor before those stamps is stamped again after them,
// so the floor stays on it rather than moving to an obsolete build.
func TestFloorHoldersRestampTheNewestArrivalOfATouchedKeptRepository(t *testing.T) {
	now := time.Now()
	epoch := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	candidates := []Candidate{
		{ID: "sha256:base-old", RepoTags: []string{"ghcr.io/discobox-ai/discobox-sandbox-base:v0.19.0"}, Created: epoch},
		{ID: "sha256:base-new", RepoTags: []string{"ghcr.io/discobox-ai/discobox-sandbox-base:v0.19.3"}, Created: epoch, LastLocal: now.Add(-time.Hour)},
		{ID: "sha256:base-older", RepoTags: []string{"ghcr.io/discobox-ai/discobox-sandbox-base:v0.18.0"}, Created: epoch, LastLocal: now.Add(-2 * time.Hour)},
		// A repository nothing keeps has no floor to hold.
		{ID: "sha256:shell", RepoTags: []string{"ghcr.io/discobox-ai/discobox-harness-shell:v0.18.0"}, Created: epoch},
		{ID: "sha256:shell-known", RepoTags: []string{"ghcr.io/discobox-ai/discobox-harness-shell:v0.17.0"}, Created: epoch, LastLocal: now},
	}
	keep := keepSet([]string{"ghcr.io/discobox-ai/discobox-sandbox-base:v0.19.3"})

	holders := floorHolders(candidates, []int{0, 3}, keep)
	if len(holders) != 1 || candidates[holders[0]].ID != "sha256:base-new" {
		t.Fatalf("floor holders = %v, want only sha256:base-new", holders)
	}
	// Nothing stamped in a kept repository: nothing to hold.
	if holders := floorHolders(candidates, []int{3}, keep); len(holders) != 0 {
		t.Fatalf("floor holders = %v, want none", holders)
	}
}

// The holder of a kept repository's floor is stamped again at its own build,
// not after every other stamp: a newer build that arrived unstamped (a failed
// stamp, a pull in a pool console) goes after it and takes the floor, while
// older builds and equal ones go before it.
func TestStampOrderPutsTheFloorHolderAtItsOwnBuild(t *testing.T) {
	now := time.Now()
	built := func(hours int) time.Time { return now.Add(time.Duration(hours) * time.Hour) }
	candidates := []Candidate{
		{ID: "sha256:holder", RepoTags: []string{"discobox-sandbox-base:dev-b"}, Created: built(-10), LastLocal: now.Add(-time.Hour)},
		{ID: "sha256:newer", RepoTags: []string{"discobox-sandbox-base:dev-c"}, Created: built(-5)},
		{ID: "sha256:older", RepoTags: []string{"discobox-sandbox-base:dev-a"}, Created: built(-20)},
		{ID: "sha256:equal", RepoTags: []string{"discobox-sandbox-base:dev-b2"}, Created: built(-10)},
	}
	keep := keepSet([]string{"discobox-sandbox-base:local"})

	got := make([]string, 0, len(candidates))
	for _, i := range stampOrder(candidates, keep) {
		got = append(got, candidates[i].ID)
	}
	want := []string{"sha256:older", "sha256:equal", "sha256:holder", "sha256:newer"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("stamp order = %v, want %v", got, want)
	}
}

func TestStampTagPrefersTheArrivalReference(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image string
		tags  []string
		want  string
	}{
		{name: "arrived by tag", image: "ghcr.io/x/y:v2", tags: []string{"ghcr.io/x/y:v1", "ghcr.io/x/y:v2"}, want: "ghcr.io/x/y:v2"},
		{name: "arrived by digest", image: "ghcr.io/x/y:v2@sha256:dd", tags: []string{"ghcr.io/x/y:v2"}, want: "ghcr.io/x/y:v2"},
		{name: "untagged", image: "sha256:abc", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stampTag(tc.image, tc.tags); got != tc.want {
				t.Fatalf("stampTag(%q, %v) = %q, want %q", tc.image, tc.tags, got, tc.want)
			}
		})
	}
}

func TestReclaimableKeepsImagesAnyContainerUses(t *testing.T) {
	now := time.Now()
	// Old enough to go, but a container — which may well be a stopped sandbox —
	// still refers to it.
	candidates := []Candidate{{ID: "sha256:running", LastLocal: now.Add(-30 * 24 * time.Hour)}}

	assertIDs(t, Reclaimable(candidates, set("sha256:running"), nil, retention, now, false))
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
	keep := []string{"sha256:byid", "discobox-sandbox-agent:dev-abc", "ghcr.io/x/y@sha256:dd"}

	// The keep set is what protects a base image nothing runs directly: on a dev
	// host only harness images have containers, so the sandbox-agent base they
	// are built FROM would otherwise age out from under the next build.
	assertIDs(t, Reclaimable(candidates, nil, keep, retention, now, false), "sha256:unkept")
}

func TestReclaimableSkipsImagesWithNoKnownLocalAge(t *testing.T) {
	now := time.Now()
	candidates := []Candidate{{ID: "sha256:ageless"}}

	assertIDs(t, Reclaimable(candidates, nil, nil, retention, now, false))
	assertIDs(t, Reclaimable(candidates, nil, nil, retention, now, true), "sha256:ageless")
}

func TestReclaimableSkipsImagesWithoutAnID(t *testing.T) {
	now := time.Now()
	candidates := []Candidate{{LastLocal: now.Add(-retention - time.Minute)}}

	assertIDs(t, Reclaimable(candidates, nil, nil, retention, now, false))
}

func TestReclaimableIgnoresEmptyKeepEntries(t *testing.T) {
	now := time.Now()
	// An empty keep entry must not match an image that has no tags, which would
	// otherwise pin every untagged image on the daemon.
	candidates := []Candidate{{ID: "sha256:untagged", RepoTags: []string{""}, LastLocal: now.Add(-retention - time.Minute)}}

	assertIDs(t, Reclaimable(candidates, nil, []string{""}, retention, now, false), "sha256:untagged")
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

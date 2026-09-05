package poolruntime

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// A volume's scope decides whether a cache path is one directory for the pool or
// one per uid (ADR 0094), and it is declared at one end of a five-hop journey
// and read at the other. This is the hop where the value leaves the control
// plane; a converter that rebuilds the struct field by field drops it in silence
// and every sandbox quietly partitions /nix.
func TestPoolHarnessVolumesForwardTheScope(t *testing.T) {
	volumes := poolHarnessVolumes([]harness.Volume{
		{Path: "/nix", Volume: harness.VolumeCache, Scope: harness.VolumeScopeShared, UID: "0", GID: "0", Mode: "0755"},
		{Path: "%HOME%/.cache", Volume: harness.VolumeCache, UID: "%UID%"},
	})
	if len(volumes) != 2 {
		t.Fatalf("volumes = %d, want 2", len(volumes))
	}
	if got := string(volumes[0].Scope.Or("")); got != string(harness.VolumeScopeShared) {
		t.Fatalf("/nix scope = %q, want %q", got, harness.VolumeScopeShared)
	}
	// Absent stays absent: an image built before the field must not arrive
	// looking like one that chose the default.
	if volumes[1].Scope.Set {
		t.Fatalf("undeclared scope = %q, want it unset", volumes[1].Scope.Value)
	}
}

func TestPoolCreateRequestForwardsSourceDataKeys(t *testing.T) {
	primaryKey := strings.Repeat("a", 64)
	refKey := strings.Repeat("b", 64)
	primary := model.GitSource{Kind: "git"}
	request := poolCreateRequestFromOptions("sandbox-1", sandbox.CreateOptions{
		Source:                      &primary,
		SourceDataKey:               primaryKey,
		SourceCodeReferences:        model.SourceCodeReferences{"library": {Kind: "git"}},
		SourceCodeReferenceDataKeys: map[string]string{"library": refKey},
	})

	gotPrimary, ok := request.Config.Source.Get()
	if !ok || gotPrimary.DataKey.Or("") != primaryKey {
		t.Fatalf("primary data key = %q, want %q", gotPrimary.DataKey.Or(""), primaryKey)
	}
	refs, ok := request.Config.SourceCodeReferences.Get()
	if !ok || refs["library"].DataKey.Or("") != refKey {
		t.Fatalf("reference data key = %q, want %q", refs["library"].DataKey.Or(""), refKey)
	}
}

// The slug is the name every other component addresses a source by, so the
// pool has to be sent the one the control plane assigned rather than deriving
// its own from the reference key.
func TestPoolCreateRequestForwardsSourceSlugs(t *testing.T) {
	primarySlug, refSlug := "primary", "hooks"
	primary := model.GitSource{Kind: "git", Slug: &primarySlug}
	request := poolCreateRequestFromOptions("sandbox-1", sandbox.CreateOptions{
		Source:               &primary,
		SourceCodeReferences: model.SourceCodeReferences{"/home/user/src/hooks": {Kind: "git", Slug: &refSlug}},
	})

	gotPrimary, ok := request.Config.Source.Get()
	if !ok || gotPrimary.Slug.Or("") != primarySlug {
		t.Fatalf("primary slug = %q, want %q", gotPrimary.Slug.Or(""), primarySlug)
	}
	refs, ok := request.Config.SourceCodeReferences.Get()
	if !ok || refs["/home/user/src/hooks"].Slug.Or("") != refSlug {
		t.Fatalf("reference slug = %q, want %q", refs["/home/user/src/hooks"].Slug.Or(""), refSlug)
	}
}

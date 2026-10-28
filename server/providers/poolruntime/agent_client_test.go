package poolruntime

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// A volume's scope decides whether a cache path is one directory for the pool or
// one per uid (ADR 0094, pool cache), and it is declared at one end of a five-hop journey
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

// excludeFromExport is declared in the image label and read by the sandbox
// agent's export mode, and this is the hop that leaves the control plane. A
// converter that drops it exports every nested Docker store again, silently
// (ADR 0129 §2).
func TestPoolHarnessVolumesForwardExcludeFromExport(t *testing.T) {
	volumes := poolHarnessVolumes([]harness.Volume{
		{Path: "/var/lib/docker", Volume: harness.VolumeData, ExcludeFromExport: true},
		{Path: "%HOME%", Volume: harness.VolumeData},
	})
	if !volumes[0].ExcludeFromExport.Or(false) {
		t.Fatal("/var/lib/docker lost excludeFromExport on the way to the pool")
	}
	if volumes[1].ExcludeFromExport.Set {
		t.Fatalf("home carries excludeFromExport = %v, want it unset", volumes[1].ExcludeFromExport.Value)
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

// The upstream URL is a network remote the sandbox fetches from itself, so it
// reaches the pool in both delivery modes — including push, which withholds the
// client's own location from the pool.
func TestPoolGitSourceForwardsTheUpstreamURLInEitherDelivery(t *testing.T) {
	local := "/home/dev/project"
	upstream := "git@github.com:example/project.git"
	for _, delivery := range []string{model.GitSourceDeliveryClone, model.GitSourceDeliveryPush} {
		out, err := poolGitSource(model.GitSource{
			Kind:           "git",
			Delivery:       delivery,
			LocalDirectory: &local,
			UpstreamURL:    &upstream,
		}, "")
		if err != nil {
			t.Fatalf("%s: poolGitSource: %v", delivery, err)
		}
		if got := out.UpstreamUrl.Or(""); got != upstream {
			t.Fatalf("%s: upstream URL = %q, want %q", delivery, got, upstream)
		}
	}
}

// The sandbox's skills reach the pool whole, the executable bit only where it
// is set (ADR 26-10-09-395 §2).
func TestPoolCreateRequestCarriesSkills(t *testing.T) {
	request := poolCreateRequestFromOptions("sandbox-1", sandbox.CreateOptions{
		Skills: sandboxconfig.Skills{"foo": {Skill: "# foo", Files: []sandboxconfig.SkillFile{
			{Path: "run.sh", Content: []byte("x"), Executable: true},
			{Path: "a.md", Content: []byte("y")},
		}}},
	})
	foo, ok := request.Config.Skills.Or(nil)["foo"]
	if !ok || foo.Skill != "# foo" || len(foo.Files) != 2 {
		t.Fatalf("skills = %#v, want foo with its two files", request.Config.Skills)
	}
	if !foo.Files[0].Executable.Or(false) || foo.Files[1].Executable.Set || string(foo.Files[1].Content) != "y" {
		t.Fatalf("files = %#v", foo.Files)
	}
	if request := poolCreateRequestFromOptions("sandbox-1", sandbox.CreateOptions{}); request.Config.Skills.Set {
		t.Fatal("skills set for a sandbox created without any")
	}
}

// The effective egress policy is sent on every create, unrestricted included,
// so a container built again never keeps a policy recorded for an earlier one.
func TestPoolCreateRequestForwardsTheEgressPolicy(t *testing.T) {
	restricted := poolCreateRequestFromOptions("sandbox-1", sandbox.CreateOptions{
		Egress: egress.Policy{DenyByDefault: true, Allow: []string{"github.com"}},
	})
	got, ok := restricted.Egress.Get()
	if !ok || !got.DenyByDefault.Or(false) {
		t.Fatalf("restricted policy = %+v, set %v", got, ok)
	}
	if allow, _ := got.Allow.Get(); len(allow) != 1 || allow[0] != "github.com" {
		t.Fatalf("allow = %v", allow)
	}

	open := poolCreateRequestFromOptions("sandbox-1", sandbox.CreateOptions{})
	got, ok = open.Egress.Get()
	if !ok || got.DenyByDefault.Or(true) {
		t.Fatalf("unrestricted policy = %+v, set %v; want it sent, unrestricted", got, ok)
	}
}

// The repository's features reach the pool, and none are sent for a sandbox
// created without them (ADR 26-10-16-423 §4).
func TestPoolCreateRequestCarriesFeatures(t *testing.T) {
	request := poolCreateRequestFromOptions("sandbox-1", sandbox.CreateOptions{
		Features: map[string]sandboxconfig.Feature{"brew": {Enabled: false}},
	})
	if got := request.Config.Features.Or(nil); len(got) != 1 || got["brew"].Enabled {
		t.Fatalf("features = %#v", request.Config.Features)
	}
	if request := poolCreateRequestFromOptions("sandbox-1", sandbox.CreateOptions{}); request.Config.Features.Set {
		t.Fatal("features set for a sandbox created without any")
	}
}

// The pool is sent what a sandbox is delivered, the agent and the bundles in
// order, with nothing saying which bundle is which (ADR 26-10-24-953 §§1–2);
// none is no field at all.
func TestPoolCreateRequestForwardsBundlePins(t *testing.T) {
	agent := "sha256:" + strings.Repeat("3", 64)
	platform := "sha256:" + strings.Repeat("1", 64)
	harness := "sha256:" + strings.Repeat("2", 64)
	request := poolCreateRequestFromOptions("sandbox-1", sandbox.CreateOptions{Delivery: sandbox.Delivery{
		Agent:   sandbox.BundlePin{Name: "discobox-agent", Digest: agent},
		Bundles: []sandbox.BundlePin{{Name: "discobox-platform", Digest: platform}, {Name: "discobox-harness-codex", Digest: harness}},
	}})
	pins, ok := request.Bundles.Get()
	if !ok || len(pins) != 2 || pins[0].Name != "discobox-platform" || pins[0].Digest != platform || pins[1].Name != "discobox-harness-codex" || pins[1].Digest != harness {
		t.Fatalf("bundles = %+v", pins)
	}
	if got, ok := request.Agent.Get(); !ok || got.Name != "discobox-agent" || got.Digest != agent {
		t.Fatalf("agent = %+v", request.Agent)
	}
	if request := poolCreateRequestFromOptions("sandbox-1", sandbox.CreateOptions{}); request.Bundles.IsSet() || request.Agent.IsSet() {
		t.Fatalf("bundles = %+v, agent %+v for a sandbox pinned to none", request.Bundles, request.Agent)
	}
}

// A raw binding rides beside the sentinels by its handle, and only when there
// is one: a create with none sends a pool that predates raw bindings nothing
// it would refuse (ADR 26-10-27-840).
func TestPoolCreateRequestCarriesRawSecretsOnlyWhenThereAreAny(t *testing.T) {
	none := poolCreateRequestFromOptions("sandbox-1", sandbox.CreateOptions{Sentinels: []string{"s-1"}})
	if none.RawSecrets.IsSet() {
		t.Fatalf("raw secrets sent with none: %#v", none.RawSecrets)
	}
	request := poolCreateRequestFromOptions("sandbox-1", sandbox.CreateOptions{
		RawSecrets: map[string]sandbox.RawSecret{"raw-1": {EnvName: "AWS_SECRET_ACCESS_KEY", Delivery: sandboxconfig.RawDeliveryFile}},
	})
	raw, ok := request.RawSecrets.Get()
	if !ok || raw["raw-1"].EnvName != "AWS_SECRET_ACCESS_KEY" || string(raw["raw-1"].Delivery) != sandboxconfig.RawDeliveryFile {
		t.Fatalf("raw secrets = %#v, want the handle's delivery", raw)
	}
	if request.Sentinels.IsSet() {
		t.Fatalf("sentinels = %#v, want none: a raw handle is not a sentinel", request.Sentinels)
	}
}

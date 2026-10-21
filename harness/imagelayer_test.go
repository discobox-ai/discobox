package harness

import (
	"strings"
	"testing"
)

func baseLayerLabel(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		ImageLayerLabelPrefix + SandboxBaseLayer: `{
			"apiVersion": "discobox.dev/image/v1",
			"env": {"DISPLAY": ":0", "PATH": "/usr/local/bin:/usr/bin"},
			"volumes": [
				{"path": "%HOME%", "volume": "data"},
				{"path": "/nix", "volume": "cache"}
			],
			"additionalGroups": ["docker"],
			"harness": {"files": [{"path": ".config/pnpm/config.yaml", "content": "storeDir: x\n"}]}
		}`,
	}
}

// An image that declares nothing is its inherited layers: the base's env,
// volumes, groups and seed files, and no harness of its own. This is `shell`,
// and it is any image someone builds FROM the base that installs an agent as
// the conventional harness.RunCommand (ADR 0086 §5).
func TestResolveImageLabelsWithNoOwnLayer(t *testing.T) {
	metadata, hasBase, err := ResolveImageLabels(baseLayerLabel(t))
	if err != nil {
		t.Fatal(err)
	}
	if !hasBase {
		t.Fatal("base layer not detected")
	}
	if metadata.Env["DISPLAY"] != ":0" {
		t.Errorf("env = %v, want the base layer's DISPLAY", metadata.Env)
	}
	if len(metadata.Volumes) != 2 || metadata.Volumes[0].Path != "%HOME%" {
		t.Errorf("volumes = %v, want the base layer's two", metadata.Volumes)
	}
	if len(metadata.AdditionalGroups) != 1 || metadata.AdditionalGroups[0] != "docker" {
		t.Errorf("groups = %v, want [docker]", metadata.AdditionalGroups)
	}
	if metadata.Harness == nil || len(metadata.Harness.Files) != 1 {
		t.Errorf("harness = %+v, want the base layer's one seed file", metadata.Harness)
	}
}

// A leaf merges over what it inherits, by identity and not by position: it
// overrides the entries it names, keeps the rest in the base's order, and
// appends its own.
func TestResolveImageLabelsMergesLeafOverBase(t *testing.T) {
	labels := baseLayerLabel(t)
	labels[ImageLabel] = `{
		"apiVersion": "discobox.dev/image/v1",
		"env": {"PATH": "/opt/agent/bin:/usr/bin"},
		"volumes": [
			{"path": "/nix", "volume": "data"},
			{"path": "%HOME%/.cache", "volume": "cache"}
		],
		"additionalGroups": ["docker", "video"],
		"harness": {
			"id": "agent", "name": "Agent",
			"files": [{"path": ".agent/settings.json", "content": "{}"}],
			"secrets": [{"name": "AGENT_TOKEN", "required": true}]
		}
	}`
	metadata, _, err := ResolveImageLabels(labels)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Env["PATH"] != "/opt/agent/bin:/usr/bin" {
		t.Errorf("PATH = %q, want the leaf's", metadata.Env["PATH"])
	}
	if metadata.Env["DISPLAY"] != ":0" {
		t.Errorf("DISPLAY = %q, want the base's, which the leaf never mentioned", metadata.Env["DISPLAY"])
	}
	// /nix is replaced where it stood rather than moved to the end, so the
	// base's ordering survives a leaf overriding one member of it.
	if len(metadata.Volumes) != 3 {
		t.Fatalf("volumes = %v, want 3", metadata.Volumes)
	}
	if metadata.Volumes[1].Path != "/nix" || metadata.Volumes[1].Volume != VolumeData {
		t.Errorf("volumes[1] = %+v, want /nix overridden in place to data", metadata.Volumes[1])
	}
	if metadata.Volumes[2].Path != "%HOME%/.cache" {
		t.Errorf("volumes[2] = %+v, want the leaf's appended volume", metadata.Volumes[2])
	}
	if len(metadata.AdditionalGroups) != 2 {
		t.Errorf("groups = %v, want docker unioned with video", metadata.AdditionalGroups)
	}
	if metadata.Harness.ID != "agent" || len(metadata.Harness.Secrets) != 1 {
		t.Errorf("harness = %+v, want the leaf's identity and secret", metadata.Harness)
	}
	// The base's seed file is inherited alongside the leaf's own.
	if len(metadata.Harness.Files) != 2 || metadata.Harness.Files[0].Path != ".config/pnpm/config.yaml" {
		t.Errorf("files = %+v, want the base's file first and the leaf's after", metadata.Harness.Files)
	}
}

// Layers merge in ascending order of the name in their label key, so a
// contributed layer's position is fixed by the author who set it.
func TestResolveImageLabelsOrdersLayersByName(t *testing.T) {
	labels := map[string]string{
		ImageLayerLabelPrefix + "20-desktop":     `{"env": {"SEAT": "desktop"}}`,
		ImageLayerLabelPrefix + SandboxBaseLayer: `{"env": {"SEAT": "base", "ONLY": "base"}}`,
		ImageLayerLabelPrefix + "30-tools":       `{"env": {"SEAT": "tools"}}`,
	}
	metadata, hasBase, err := ResolveImageLabels(labels)
	if err != nil {
		t.Fatal(err)
	}
	if !hasBase {
		t.Fatal("base layer not detected")
	}
	if metadata.Env["SEAT"] != "tools" {
		t.Errorf("SEAT = %q, want the highest-numbered layer's", metadata.Env["SEAT"])
	}
	if metadata.Env["ONLY"] != "base" {
		t.Errorf("ONLY = %q, want the base's, which nothing overrode", metadata.Env["ONLY"])
	}
}

// An image with no base layer did not come from the sandbox agent base, and
// cannot run a sandbox whatever else it declares (ADR 0086 §1).
func TestResolveImageLabelsReportsMissingBase(t *testing.T) {
	labels := map[string]string{ImageLabel: `{"harness": {"id": "agent", "name": "Agent"}}`}
	_, hasBase, err := ResolveImageLabels(labels)
	if err != nil {
		t.Fatal(err)
	}
	if hasBase {
		t.Fatal("hasBase = true for an image carrying only its own layer")
	}
}

// A label set but never filled in is a build that forgot its argument. Naming
// the argument is the whole point: treating it as absent would report the base
// image as not being the base image.
func TestResolveImageLabelsRejectsEmptyLayer(t *testing.T) {
	labels := map[string]string{ImageLayerLabelPrefix + SandboxBaseLayer: ""}
	_, _, err := ResolveImageLabels(labels)
	if err == nil {
		t.Fatal("expected an error for an empty layer label")
	}
	if !strings.Contains(err.Error(), LayerMetadataBuildArg) {
		t.Errorf("error = %v, want it to name %s", err, LayerMetadataBuildArg)
	}
}

func TestResolveImageLabelsIgnoresUnrelatedLabels(t *testing.T) {
	labels := baseLayerLabel(t)
	labels[ReclaimLabel] = ReclaimLabelValue
	labels["org.opencontainers.image.source"] = "https://example.invalid"
	if _, _, err := ResolveImageLabels(labels); err != nil {
		t.Fatal(err)
	}
}

// platformLabels is a platform bundle's label set: its one layer, providing
// the platform API version.
func platformLabels(layer string) map[string]string {
	return map[string]string{ImageLayerLabelPrefix + PlatformLayer: layer}
}

const platformLayerJSON = `{"apiVersion":"discobox.dev/image/v1","platformAPIVersion":"discobox.ai/platform/v1","env":{"DISPLAY":":0","PATH":"/usr/bin"},"volumes":[{"path":"%HOME%","volume":"data"}]}`

// The platform's layer is composed under the harness bundle's, which a harness
// no longer inherits: the harness's own layer is last, and later wins.
func TestComposeBundleLabelsPutsThePlatformUnderTheHarness(t *testing.T) {
	metadata, err := ComposeBundleLabels(platformLabels(platformLayerJSON), map[string]string{
		ImageLayerLabelPrefix + "60-acme": `{"env":{"ACME":"1"}}`,
		ImageLabel:                        `{"platformAPIVersion":"discobox.ai/platform/v1","env":{"PATH":"/opt/bin"},"harness":{"id":"acme","name":"Acme"}}`,
	})
	if err != nil {
		t.Fatalf("ComposeBundleLabels() error = %v", err)
	}
	if metadata.Env["DISPLAY"] != ":0" || metadata.Env["ACME"] != "1" || metadata.Env["PATH"] != "/opt/bin" {
		t.Fatalf("env = %v, want the platform's under the harness's layers", metadata.Env)
	}
	if len(metadata.Volumes) != 1 || metadata.Harness == nil || metadata.Harness.ID != "acme" {
		t.Fatalf("metadata = %+v, want the platform's volume and the harness's identity", metadata)
	}
}

func TestComposeBundleLabelsRefuses(t *testing.T) {
	harnessOwn := `{"platformAPIVersion":"discobox.ai/platform/v1"}`
	for name, tc := range map[string]struct {
		platform, harness map[string]string
		says              string
	}{
		"a harness built FROM the platform": {
			platformLabels(platformLayerJSON),
			map[string]string{ImageLayerLabelPrefix + PlatformLayer: platformLayerJSON, ImageLabel: harnessOwn},
			"built FROM the platform",
		},
		"a harness built FROM the sandbox agent image": {
			platformLabels(platformLayerJSON),
			map[string]string{ImageLayerLabelPrefix + SandboxBaseLayer: `{}`, ImageLabel: harnessOwn},
			"built FROM the platform",
		},
		"a platform with no layer": {
			map[string]string{ImageLabel: `{}`},
			map[string]string{ImageLabel: harnessOwn},
			"carries no " + ImageLayerLabelPrefix + PlatformLayer,
		},
		"a platform providing no version": {
			platformLabels(`{"env":{"DISPLAY":":0"}}`),
			map[string]string{ImageLabel: harnessOwn},
			"declares no platformAPIVersion",
		},
		"a harness naming no version": {
			platformLabels(platformLayerJSON),
			map[string]string{ImageLabel: `{"harness":{"id":"acme"}}`},
			"names no platformAPIVersion",
		},
		"a harness declaring nothing": {
			platformLabels(platformLayerJSON),
			map[string]string{},
			"names no platformAPIVersion",
		},
		"a harness needing another platform": {
			platformLabels(platformLayerJSON),
			map[string]string{ImageLabel: `{"platformAPIVersion":"discobox.ai/platform/v2"}`},
			`needs platform "discobox.ai/platform/v2"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ComposeBundleLabels(tc.platform, tc.harness)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("err = %v, want one saying %q", err, tc.says)
			}
		})
	}
}

// A third party's bundle built FROM a harness bundle carries the harness's
// contributed layers and its own label, and names the platform once: the
// version a lower layer names is the one the harness's own may restate.
func TestComposeBundleLabelsReadsTheVersionFromAnyHarnessLayer(t *testing.T) {
	metadata, err := ComposeBundleLabels(platformLabels(platformLayerJSON), map[string]string{
		ImageLayerLabelPrefix + "60-claude-code": `{"platformAPIVersion":"discobox.ai/platform/v1","env":{"BASE":"1"}}`,
		ImageLabel:                               `{"env":{"ACME":"1"}}`,
	})
	if err != nil {
		t.Fatalf("ComposeBundleLabels() error = %v", err)
	}
	if metadata.Env["BASE"] != "1" || metadata.Env["ACME"] != "1" {
		t.Fatalf("env = %v, want both harness layers", metadata.Env)
	}
}

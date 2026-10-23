package releasemanifest

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestReleaseInventoryComesFromRoles(t *testing.T) {
	m, err := Read("examples/v0.8.0.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"darwin", "linux", "windows"} {
		refs := m.Images.References(host, "amd64")
		for _, ref := range m.Images.Harnesses {
			if !slices.Contains(refs, ref) {
				t.Fatalf("%s omits harness %s", host, ref)
			}
		}
		if slices.Contains(refs, m.Images.VM) != (host != "windows") {
			t.Fatalf("wrong VM inventory for %s", host)
		}
		if slices.Contains(refs, m.Images.Kernel) != (host == "linux") {
			t.Fatalf("wrong kernel inventory for %s", host)
		}
	}
	m.Images.Harnesses["extra"] = m.Images.SandboxAgent
	refs := m.Images.References("linux", "amd64")
	if len(refs) != 7 {
		t.Fatalf("duplicate image staged: %v", refs)
	}
}

// v0.8.0 was cut before the Docker driver owned a sandbox base image, so its
// manifest names none and stages nothing for one; a manifest that names one
// stages it beside the pool agent.
func TestSandboxBaseIsStagedWhenTheReleaseNamesOne(t *testing.T) {
	m, err := Read("examples/v0.8.0.json")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(m.Images.References("linux", "amd64"), "") {
		t.Fatal("a release with no sandbox base stages an empty reference")
	}
	m.Images.SandboxBase = "ghcr.io/discobox-ai/discobox-sandbox-base:v0.9.0"
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.Images.References("darwin", "arm64"), m.Images.SandboxBase) {
		t.Fatal("the sandbox base is not staged")
	}
}

func TestRejectIncompleteAndAmbiguousManifests(t *testing.T) {
	for _, mutate := range []func(*Manifest){
		func(m *Manifest) { m.Format = 2 },
		func(m *Manifest) { m.Images.PoolAgent = "" },
		func(m *Manifest) { m.Images.Harnesses["shell"] = "shell:local" },
		func(m *Manifest) { m.Images.SandboxAgent = "sandbox:latest" },
		func(m *Manifest) { m.Images.SandboxBase = "sandbox-base:local" },
		func(m *Manifest) { m.Images.Harnesses = nil },
	} {
		m, err := Read("examples/v0.8.0.json")
		if err != nil {
			t.Fatal(err)
		}
		mutate(&m)
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(data); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
	if _, err := Parse([]byte(`{"format":1,"typo":true}`)); err == nil {
		t.Fatal("accepted unknown field")
	}
}

// withBundles names a bundle for the platform and for each harness in place
// of the runnable images, as a release from harness bundles on does.
func withBundles(m *Manifest) {
	m.Bundles = Bundles{Platform: "ghcr.io/discobox-ai/discobox-platform:v0.8.0", Harnesses: map[string]string{}}
	for slug := range m.Images.Harnesses {
		m.Bundles.Harnesses[slug] = "ghcr.io/discobox-ai/discobox-harness-" + slug + ":v0.8.0"
	}
	m.Images.SandboxAgent, m.Images.Harnesses = "", nil
}

// A release from bundles on ships no runnable image of a harness, nor the
// sandbox agent's, so nothing of either is staged (ADR 26-10-16-423 §10).
func TestAReleaseOfBundlesStagesNoHarnessImage(t *testing.T) {
	m, err := Read("examples/v0.8.0.json")
	if err != nil {
		t.Fatal(err)
	}
	withBundles(&m)
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("a release of bundles alone refused: %v", err)
	}
	for _, host := range []string{"darwin", "linux", "windows"} {
		for _, ref := range m.Images.References(host, "amd64") {
			if strings.Contains(ref, "discobox-sandbox-agent") || strings.Contains(ref, "discobox-harness-") {
				t.Fatalf("%s stages %s", host, ref)
			}
		}
	}
}

func TestAReleaseFromBeforeBundlesStillParses(t *testing.T) {
	m, err := Read("examples/v0.8.0.json")
	if err != nil {
		t.Fatal(err)
	}
	if m.HasBundles() {
		t.Fatal("v0.8.0 names bundles")
	}
}

// What is staged is loaded into pool daemons, for the pool's platform. A pool
// runs no bundle, and a harness bundle is one manifest for every platform,
// which staging for an arm64 pool would refuse.
func TestBundlesAreNotStaged(t *testing.T) {
	m, err := Read("examples/v0.8.0.json")
	if err != nil {
		t.Fatal(err)
	}
	withBundles(&m)
	for _, host := range []string{"darwin", "linux", "windows"} {
		refs := m.Images.References(host, "arm64")
		if slices.Contains(refs, m.Bundles.Platform) {
			t.Fatalf("%s stages the platform bundle: %v", host, refs)
		}
		for _, ref := range m.Bundles.Harnesses {
			if slices.Contains(refs, ref) {
				t.Fatalf("%s stages harness bundle %s", host, ref)
			}
		}
	}
}

func TestRejectIncompleteBundles(t *testing.T) {
	for name, mutate := range map[string]func(*Manifest){
		"no platform":      func(m *Manifest) { m.Bundles.Platform = "" },
		"a local platform": func(m *Manifest) { m.Bundles.Platform = "discobox-platform:local" },
		"no harnesses":     func(m *Manifest) { m.Bundles.Harnesses = nil },
		"an unnamed harness": func(m *Manifest) {
			m.Bundles.Harnesses[""] = m.Bundles.Harnesses["shell"]
		},
		"a latest harness": func(m *Manifest) { m.Bundles.Harnesses["shell"] = "shell:latest" },
	} {
		t.Run(name, func(t *testing.T) {
			m, err := Read("examples/v0.8.0.json")
			if err != nil {
				t.Fatal(err)
			}
			withBundles(&m)
			if err := m.Validate(); err != nil {
				t.Fatalf("complete bundles refused: %v", err)
			}
			mutate(&m)
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(data); err == nil {
				t.Fatalf("accepted %s", data)
			}
		})
	}
}

package dockerworker

import (
	"testing"

	"github.com/obot-platform/discobox/devimage"
)

func testImage(reference string, args map[string]string) devimage.Image {
	return devimage.Image{
		Reference: reference,
		Build:     &devimage.BuildSpec{Dockerfile: "Dockerfile", Context: "/src", Args: args},
	}
}

func TestOrderBuildsPutsDependenciesFirst(t *testing.T) {
	// A harness image is built FROM the sandbox base, passed as a build
	// argument, so the base must be built first regardless of input order.
	images := []devimage.Image{
		testImage("harness:dev-2", map[string]string{"SANDBOX_AGENT_IMAGE": "sandbox:dev-1"}),
		testImage("sandbox:dev-1", nil),
	}

	ordered, err := orderBuilds(images)
	if err != nil {
		t.Fatalf("orderBuilds: %v", err)
	}
	if len(ordered) != 2 {
		t.Fatalf("ordered = %d images, want 2", len(ordered))
	}
	if ordered[0].Reference != "sandbox:dev-1" {
		t.Fatalf("ordered[0] = %q, want the dependency sandbox:dev-1", ordered[0].Reference)
	}
	if ordered[1].Reference != "harness:dev-2" {
		t.Fatalf("ordered[1] = %q, want harness:dev-2", ordered[1].Reference)
	}
}

func TestOrderBuildsIsStableWithoutDependencies(t *testing.T) {
	images := []devimage.Image{
		testImage("c:dev", nil),
		testImage("a:dev", nil),
		testImage("b:dev", nil),
	}

	ordered, err := orderBuilds(images)
	if err != nil {
		t.Fatalf("orderBuilds: %v", err)
	}
	want := []string{"a:dev", "b:dev", "c:dev"}
	for i, reference := range want {
		if ordered[i].Reference != reference {
			t.Fatalf("ordered[%d] = %q, want %q", i, ordered[i].Reference, reference)
		}
	}
}

func TestOrderBuildsRejectsCycles(t *testing.T) {
	images := []devimage.Image{
		testImage("a:dev", map[string]string{"OTHER": "b:dev"}),
		testImage("b:dev", map[string]string{"OTHER": "a:dev"}),
	}

	if _, err := orderBuilds(images); err == nil {
		t.Fatal("orderBuilds should reject a dependency cycle")
	}
}

// An image whose build argument happens to equal its own reference must not
// deadlock the ordering by appearing to depend on itself.
func TestOrderBuildsIgnoresSelfReference(t *testing.T) {
	images := []devimage.Image{
		testImage("a:dev", map[string]string{"SELF": "a:dev"}),
	}

	ordered, err := orderBuilds(images)
	if err != nil {
		t.Fatalf("orderBuilds: %v", err)
	}
	if len(ordered) != 1 || ordered[0].Reference != "a:dev" {
		t.Fatalf("ordered = %#v, want just a:dev", ordered)
	}
}

// Build-mode images carry no ID, so the manifest must accept them; copy-mode
// images must still require one.
func TestManifestAcceptsBuildModeImagesWithoutID(t *testing.T) {
	if _, err := devimage.NewManifest([]devimage.Image{testImage("pool:dev-1", nil)}); err != nil {
		t.Fatalf("build-mode manifest should be valid: %v", err)
	}
	if _, err := devimage.NewManifest([]devimage.Image{{Reference: "pool:dev-1"}}); err == nil {
		t.Fatal("copy-mode image without an ID should be rejected")
	}
}

package releasemanifest

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

const libkrunImage = "ghcr.io/discobox-ai/discobox-libkrun@sha256:3b1c7f0e2d6a95e8c4f1b0a7d9e2c6f5a8b3d0e1f4c7a9b2e5d8f1a3c6b9e0d2"

// currentFormat is the v0.8.0 release rewritten in this package's format.
func currentFormat(t *testing.T) Manifest {
	t.Helper()
	m, err := Read("examples/v0.8.0.json")
	if err != nil {
		t.Fatal(err)
	}
	m.Format = Format
	m.Images.Libkrun = libkrunImage
	return m
}

func TestReleaseInventoryComesFromRoles(t *testing.T) {
	old, err := Read("examples/v0.8.0.json")
	if err != nil {
		t.Fatal(err)
	}
	if old.Images.Libkrun != "" {
		t.Fatalf("format 1 gave a libkrun image %q", old.Images.Libkrun)
	}
	current := currentFormat(t)
	for _, m := range []Manifest{old, current} {
		for _, platform := range [][2]string{{"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"}} {
			host, arch := platform[0], platform[1]
			refs := m.Images.References(host, arch)
			for _, ref := range m.Images.Harnesses {
				if !slices.Contains(refs, ref) {
					t.Fatalf("format %d %s/%s omits harness %s", m.Format, host, arch, ref)
				}
			}
			if slices.Contains(refs, m.Images.VM) != (host == "darwin") {
				t.Fatalf("format %d: wrong VM inventory for %s/%s", m.Format, host, arch)
			}
			wantLibkrun := m.Images.Libkrun != "" && host == "linux" && arch == "amd64"
			if m.Images.Libkrun != "" && slices.Contains(refs, m.Images.Libkrun) != wantLibkrun {
				t.Fatalf("format %d: wrong libkrun inventory for %s/%s", m.Format, host, arch)
			}
			if slices.Contains(refs, "") {
				t.Fatalf("format %d %s/%s stages an empty reference: %v", m.Format, host, arch, refs)
			}
		}
	}
	// The format-1 release has no libkrun image, so Linux stages only the
	// agents and the three harnesses; format 2 adds its libkrun image.
	for _, tc := range []struct {
		m    Manifest
		want int
	}{{old, 5}, {current, 6}} {
		tc.m.Images.Harnesses["extra"] = tc.m.Images.SandboxAgent
		refs := tc.m.Images.References("linux", "amd64")
		if len(refs) != tc.want {
			t.Fatalf("format %d: duplicate image staged: %v", tc.m.Format, refs)
		}
	}
}

func TestCurrentFormatRoundTrips(t *testing.T) {
	m := currentFormat(t)
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Format != Format || got.Images.Libkrun != libkrunImage {
		t.Fatalf("round trip lost the libkrun image: %+v", got.Images)
	}
}

func TestRejectIncompleteAndAmbiguousManifests(t *testing.T) {
	for _, mutate := range []func(*Manifest){
		func(m *Manifest) { m.Format = Format + 1 },
		func(m *Manifest) { m.Images.PoolAgent = "" },
		func(m *Manifest) { m.Images.Libkrun = "" },
		func(m *Manifest) { m.Images.Libkrun = "libkrun:latest" },
		func(m *Manifest) { m.Images.Harnesses["shell"] = "shell:local" },
		func(m *Manifest) { m.Images.SandboxAgent = "sandbox:latest" },
		func(m *Manifest) { m.Images.Harnesses = nil },
	} {
		m := currentFormat(t)
		mutate(&m)
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(data); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
	// Format 1 still validates a kernel it names before dropping it, and
	// cannot name a libkrun image.
	v1, err := os.ReadFile("examples/v0.8.0.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(map[string]any){
		func(images map[string]any) { images["kernel"] = "kernel:latest" },
		func(images map[string]any) { images["libkrun"] = libkrunImage },
	} {
		var doc map[string]any
		if err := json.Unmarshal(v1, &doc); err != nil {
			t.Fatal(err)
		}
		mutate(doc["images"].(map[string]any))
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(data); err == nil {
			t.Fatalf("accepted format 1 %s", data)
		}
	}
	if _, err := Parse([]byte(`{"format":1,"typo":true}`)); err == nil {
		t.Fatal("accepted unknown field")
	}
	if _, err := Parse([]byte(`{"format":2,"typo":true}`)); err == nil {
		t.Fatal("accepted unknown field")
	}
}

// A format-1 manifest read and written back names no kernel, and still reads:
// `discobox admin server manifest` prints what it read.
func TestFormatOneRoundTrips(t *testing.T) {
	m, err := Read("examples/v0.8.0.json")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("a format-1 manifest written back does not read: %v", err)
	}
	if got.Format != 1 || got.Images.VM != m.Images.VM || got.Images.Libkrun != "" {
		t.Fatalf("round trip = %+v, want format 1 with the same guest and no libkrun image", got.Images)
	}
}

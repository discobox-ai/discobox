package releasemanifest

import (
	"encoding/json"
	"slices"
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

func TestRejectIncompleteAndAmbiguousManifests(t *testing.T) {
	for _, mutate := range []func(*Manifest){
		func(m *Manifest) { m.Format = 2 },
		func(m *Manifest) { m.Images.PoolAgent = "" },
		func(m *Manifest) { m.Images.Harnesses["shell"] = "shell:local" },
		func(m *Manifest) { m.Images.SandboxAgent = "sandbox:latest" },
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

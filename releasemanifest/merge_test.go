package releasemanifest

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/serverstage"
)

func platformManifest(t *testing.T, targetOS string) Manifest {
	t.Helper()
	m, err := Read("examples/v0.8.0.json")
	if err != nil {
		t.Fatal(err)
	}
	binary := func(name string) serverstage.Manifest {
		return serverstage.Manifest{
			Version: m.Version, OS: targetOS, Arch: "amd64", Command: name,
			Assets: []serverstage.Asset{{Name: name, URLs: []string{"https://example.com/" + name}, SHA256: strings.Repeat("a", 64), Size: 10, Executable: true}},
		}
	}
	m.Revision = "source-commit"
	m.Servers = []serverstage.Manifest{binary("discobox-server")}
	m.Clients = []serverstage.Manifest{binary("discobox")}
	return m
}

func TestMergeFullRelease(t *testing.T) {
	m, err := Merge([]Manifest{platformManifest(t, "windows"), platformManifest(t, "linux")})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Clients) != 2 || len(m.Servers) != 2 || m.Clients[0].OS != "linux" || m.Revision != "source-commit" {
		t.Fatalf("merged manifest = %+v", m)
	}
}

func TestMergeRejectsMixedOrIncompleteBuilds(t *testing.T) {
	for name, mutate := range map[string]func(*Manifest){
		"source":         func(m *Manifest) { m.Revision = "different" },
		"images":         func(m *Manifest) { m.Images.PoolAgent = "example.com/pool:v9" },
		"missing CLI":    func(m *Manifest) { m.Clients = nil },
		"wrong platform": func(m *Manifest) { m.Clients[0].Arch = "arm64" },
		"wrong version":  func(m *Manifest) { m.Clients[0].Version = "v9" },
		"duplicate":      func(m *Manifest) { m.Servers = append(m.Servers, m.Servers[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			a, b := platformManifest(t, "linux"), platformManifest(t, "windows")
			mutate(&b)
			if _, err := Merge([]Manifest{a, b}); err == nil {
				t.Fatal("accepted inconsistent release")
			}
		})
	}
}

package serverstage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func validManifest() Manifest {
	return Manifest{
		Version: "v1.2.3",
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		Command: "discobox-server",
		Assets: []Asset{{
			Name:       "discobox-server",
			URL:        "https://example.invalid/discobox-server-linux-amd64",
			SHA256:     strings.Repeat("ab", 32),
			Size:       94 << 20,
			Executable: true,
		}},
	}
}

// The manifest crosses a Taskfile, a shell and the linker before anything reads
// it, so the trip it makes is the thing worth testing.
func TestManifestSurvivesTheLinker(t *testing.T) {
	want := validManifest()
	encoded, err := EncodeManifest(want)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(encoded, " \t\n'\"") {
		t.Fatalf("encoded manifest %q contains something a shell would have an opinion about", encoded)
	}
	got, err := DecodeManifest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !got.sameAssets(want) || got.Version != want.Version {
		t.Fatalf("decoded %+v, want %+v", got, want)
	}
}

func TestManifestValidationRejects(t *testing.T) {
	tests := map[string]func(*Manifest){
		"no version":                 func(m *Manifest) { m.Version = "" },
		"a version that escapes":     func(m *Manifest) { m.Version = "../elsewhere" },
		"no assets":                  func(m *Manifest) { m.Assets = nil },
		"no command":                 func(m *Manifest) { m.Command = "" },
		"a command it does not have": func(m *Manifest) { m.Command = "something-else" },
		"an asset with no name":      func(m *Manifest) { m.Assets[0].Name = "" },
		// The one thing verification cannot make safe: a name that writes
		// outside the directory being staged into.
		"an asset that escapes":    func(m *Manifest) { m.Assets[0].Name = "../evil" },
		"an absolute asset":        func(m *Manifest) { m.Assets[0].Name = "/etc/evil" },
		"an asset over the record": func(m *Manifest) { m.Assets[0].Name = manifestFileName },
		"no digest":                func(m *Manifest) { m.Assets[0].SHA256 = "" },
		"no size":                  func(m *Manifest) { m.Assets[0].Size = 0 },
		"a negative size":          func(m *Manifest) { m.Assets[0].Size = -1 },
		"a truncated digest":       func(m *Manifest) { m.Assets[0].SHA256 = "abcd" },
		"a scheme it cannot fetch": func(m *Manifest) { m.Assets[0].URL = "file:///etc/passwd" },
		"a URL with no host":       func(m *Manifest) { m.Assets[0].URL = "https:///discobox-server" },
	}
	for name, breakIt := range tests {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			breakIt(&m)
			if err := m.Validate(); err == nil {
				t.Fatalf("a manifest with %s validated", name)
			}
		})
	}
}

func TestManifestValidationRejectsADuplicateAsset(t *testing.T) {
	m := validManifest()
	m.Assets = append(m.Assets, m.Assets[0])
	if err := m.Validate(); err == nil {
		t.Fatal("a manifest listing the same asset twice validated")
	}
}

// A field nobody reads is a manifest that does not say what its author thought
// it said, and the failure it causes otherwise surfaces far from the typo — an
// asset staged without its execute bit, in this one's case.
func TestParseManifestRejectsAnUnknownField(t *testing.T) {
	data, err := json.Marshal(map[string]any{
		"version": "v1", "os": runtime.GOOS, "arch": runtime.GOARCH,
		"command": "discobox-server",
		"assets": []map[string]any{{
			"name": "discobox-server", "url": "https://example.invalid/s",
			"sha256": strings.Repeat("ab", 32), "size": 1, "exec-utable": true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManifest(data); err == nil {
		t.Fatal("a manifest with a misspelled field parsed")
	}
}

func TestDefaultIsAbsentFromAnOrdinaryBuild(t *testing.T) {
	if DefaultManifest != "" {
		t.Fatalf("a test binary carries a server manifest: %q", DefaultManifest)
	}
	if _, err := Default(); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("Default() on a build with no manifest = %v, want ErrNoManifest", err)
	}
}

func TestLoadReadsAFile(t *testing.T) {
	want := validManifest()
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("Load(%q): %v", path, err)
	}
	if !got.sameAssets(want) {
		t.Fatalf("Load(%q) = %+v", path, got)
	}
}

// A manifest fetched over the network is the capability ADR 0099 §3 rejected
// and §8 deferred: its digests are what every download is checked against, so
// one that arrived over TLS and nothing else moves the trust root out of the
// binary. The refusal says what to do instead rather than reporting a missing
// file.
func TestLoadRefusesAURL(t *testing.T) {
	_, err := Load(context.Background(), "https://example.invalid/manifest.json")
	if err == nil {
		t.Fatal("Load accepted a URL")
	}
	if !strings.Contains(err.Error(), "download it first") {
		t.Fatalf("error %q does not say what to do instead", err)
	}
}

func TestForThisPlatform(t *testing.T) {
	m := validManifest()
	if !m.ForThisPlatform() {
		t.Fatalf("a manifest for %s is not for this platform", m.Platform())
	}
	m.Arch = "somethingelse"
	if m.ForThisPlatform() {
		t.Fatal("a manifest for another architecture claims to be for this one")
	}
}

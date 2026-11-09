package desktopstage

import (
	"errors"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/serverstage"
)

func validManifest() Manifest {
	return Manifest{
		Version: "v1.2.3",
		OS:      "linux",
		Arch:    "amd64",
		Command: "discobox-desktop",
		Asset: serverstage.Asset{
			Name:   "discobox-desktop-linux-amd64.tar.gz",
			URLs:   []string{"https://example.com/v1.2.3/discobox-desktop-linux-amd64.tar.gz"},
			SHA256: strings.Repeat("0", 64),
			Size:   42,
		},
	}
}

func TestManifestRoundTrips(t *testing.T) {
	encoded, err := EncodeManifest(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeManifest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Command != "discobox-desktop" || decoded.Asset.SHA256 != validManifest().Asset.SHA256 {
		t.Fatalf("decoded %+v", decoded)
	}
}

func TestDefaultWithoutAManifest(t *testing.T) {
	saved := DefaultManifest
	t.Cleanup(func() { DefaultManifest = saved })
	DefaultManifest = ""
	if _, err := Default(); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("got %v, want ErrNoManifest", err)
	}
}

func TestManifestValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Manifest){
		"command escapes":    func(m *Manifest) { m.Command = "../discobox-desktop" },
		"command absolute":   func(m *Manifest) { m.Command = "/usr/bin/discobox-desktop" },
		"command unclean":    func(m *Manifest) { m.Command = "a//b" },
		"command missing":    func(m *Manifest) { m.Command = "" },
		"no digest":          func(m *Manifest) { m.Asset.SHA256 = "" },
		"no size":            func(m *Manifest) { m.Asset.Size = 0 },
		"asset name is path": func(m *Manifest) { m.Asset.Name = "a/b.tar.gz" },
		"no platform":        func(m *Manifest) { m.OS = "" },
		"version traverses":  func(m *Manifest) { m.Version = "../x" },
	} {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			mutate(&m)
			err := m.Validate()
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), "desktop manifest") {
				t.Fatalf("error %q does not say which manifest", err)
			}
		})
	}
	// A bundle's executable lives below its root.
	m := validManifest()
	m.Command = "discobox.app/Contents/MacOS/discobox"
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestParseManifestRefusesUnknownFields(t *testing.T) {
	if _, err := ParseManifest([]byte(`{"version":"v1","os":"linux","arch":"amd64","command":"x","asset":{},"extra":1}`)); err == nil {
		t.Fatal("accepted an unknown field")
	}
}

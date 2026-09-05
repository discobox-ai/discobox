package harness

import (
	"encoding/json"
	"strings"
	"testing"
)

var testRuntime = VolumeRuntime{Home: "/home/darren", UID: 1000, GID: 1000}

// The default is the safe one. A cache path that says nothing about scope
// belongs to the sandbox user, because that is what a directory the sandbox user
// fills needs, and because an image that never considered the question must not
// be answered with "share it" (ADR 0094 §3).
func TestResolveVolumesScopesToTheUserByDefault(t *testing.T) {
	volumes, err := ResolveVolumes([]Volume{
		{Path: "%HOME%/.cache", Volume: VolumeCache, UID: "%UID%", GID: "%GID%"},
		// Root-owned and still the user's: ownership of the mountpoint is not a
		// claim about who may share it.
		{Path: "/opt/build-cache", Volume: VolumeCache, UID: "0", GID: "0"},
		{Path: "%HOME%", Volume: VolumeData, UID: "%UID%"},
	}, testRuntime)
	if err != nil {
		t.Fatalf("resolve volumes: %v", err)
	}
	for _, v := range volumes {
		if v.Scope != VolumeScopeUser {
			t.Errorf("volume %q scope = %q, want %q", v.Path, v.Scope, VolumeScopeUser)
		}
	}
}

// Sharing is the claim that has to be made out loud, and it survives resolution
// intact so boot can act on it.
func TestResolveVolumesKeepsADeclaredSharedScope(t *testing.T) {
	volumes, err := ResolveVolumes([]Volume{
		{Path: "/nix", Volume: VolumeCache, Scope: VolumeScopeShared, UID: "0", GID: "0"},
	}, testRuntime)
	if err != nil {
		t.Fatalf("resolve volumes: %v", err)
	}
	if volumes[0].Scope != VolumeScopeShared {
		t.Fatalf("/nix scope = %q, want %q", volumes[0].Scope, VolumeScopeShared)
	}
}

// A data volume is one sandbox's own tree, so nothing could carry out a claim to
// share it. Refusing names the path; honoring it silently would leave the image
// believing in sharing that never happens.
func TestResolveVolumesRejectsASharedDataPath(t *testing.T) {
	_, err := ResolveVolumes([]Volume{
		{Path: "%HOME%", Volume: VolumeData, Scope: VolumeScopeShared},
	}, testRuntime)
	if err == nil {
		t.Fatal("resolve volumes = nil error, want a shared data path refused")
	}
	if !strings.Contains(err.Error(), "/home/darren") || !strings.Contains(err.Error(), "cache") {
		t.Fatalf("error = %q, want the path and the reason named", err)
	}
}

func TestResolveVolumesRejectsAnUnknownScope(t *testing.T) {
	_, err := ResolveVolumes([]Volume{
		{Path: "/nix", Volume: VolumeCache, Scope: "pool"},
	}, testRuntime)
	if err == nil {
		t.Fatal("resolve volumes = nil error, want an unknown scope refused")
	}
	if !strings.Contains(err.Error(), "pool") {
		t.Fatalf("error = %q, want the unknown scope named", err)
	}
}

// The scope travels in the image label and the manifest as ordinary JSON, and an
// image built before the field existed has to keep meaning what it meant: no
// scope, so the sandbox user's.
func TestVolumeScopeRoundTripsAndIsOmittedWhenUnset(t *testing.T) {
	encoded, err := json.Marshal(Volume{Path: "/nix", Volume: VolumeCache, Scope: VolumeScopeShared})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"scope":"shared"`) {
		t.Fatalf("encoded = %s, want the scope carried", encoded)
	}
	if encoded, err = json.Marshal(Volume{Path: "%HOME%/.cache", Volume: VolumeCache}); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "scope") {
		t.Fatalf("encoded = %s, want no scope key when unset", encoded)
	}

	var decoded Volume
	if err := json.Unmarshal([]byte(`{"path":"/nix","volume":"cache"}`), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Scope != "" {
		t.Fatalf("decoded scope = %q, want it absent for an image built before the field", decoded.Scope)
	}
}

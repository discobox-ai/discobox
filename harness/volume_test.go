package harness

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

var testRuntime = VolumeRuntime{Home: "/home/darren", UID: 1000, GID: 1000}

// The default is the safe one. A cache path that says nothing about scope
// belongs to the sandbox user, because that is what a directory the sandbox user
// fills needs, and because an image that never considered the question must not
// be answered with "share it" (ADR 0094 §3 on the pool cache).
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

// A declaration's mode is a POSIX mode word, and os.FileMode is not. The bits
// that differ are the ones an image needs when it cannot know the uid that will
// use a path: setgid is how a tree is handed to a group instead. Casting the
// parsed octal straight to os.FileMode drops them with no error, so a prefix
// declared "2775" would be created 0775 and every directory made under it would
// land in the wrong group.
func TestResolveVolumesModeCarriesSetgid(t *testing.T) {
	for _, tc := range []struct {
		mode string
		perm os.FileMode
		flag os.FileMode
	}{
		{"0755", 0o755, 0},
		{"0711", 0o711, 0},
		{"2775", 0o775, os.ModeSetgid},
		{"4755", 0o755, os.ModeSetuid},
		{"1777", 0o777, os.ModeSticky},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			resolved, err := ResolveVolumes(
				[]Volume{{Path: "/x", Volume: VolumeData, Mode: tc.mode}},
				VolumeRuntime{Home: "/home/u", UID: 1000, GID: 1000},
			)
			if err != nil {
				t.Fatalf("ResolveVolumes: %v", err)
			}
			if resolved[0].Mode == nil {
				t.Fatal("mode was not resolved")
			}
			got := *resolved[0].Mode
			if got.Perm() != tc.perm {
				t.Errorf("perm = %#o, want %#o", got.Perm(), tc.perm)
			}
			// os.Chmod reads only these flags, so a bit that is not one of them
			// reaches the filesystem as nothing at all.
			if want := tc.flag; got&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != want {
				t.Errorf("high bits = %v, want %v", got&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky), want)
			}
		})
	}
}

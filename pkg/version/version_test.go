package version

import (
	"testing"

	"golang.org/x/mod/semver"
)

func TestMainBuildVersion(t *testing.T) {
	got, err := MainBuildVersion("v0.19.6", 4210, "0123abcdef4567")
	if err != nil {
		t.Fatal(err)
	}
	if want := "v0.19.7-main.4210.g0123abc"; got != want {
		t.Fatalf("MainBuildVersion = %q, want %q", got, want)
	}
	if !MainBuild(got) {
		t.Errorf("MainBuild(%q) = false", got)
	}
	// The order every reader of versions relies on: above the release it
	// follows, below the next one, and above every main build before it.
	earlier, _ := MainBuildVersion("v0.19.6", 4209, "fffffff")
	for _, pair := range [][2]string{
		{"v0.19.6", got},
		{got, "v0.19.7"},
		{earlier, got},
		// A sha that starts with digits is still one alphanumeric identifier.
		{"v0.19.7-main.9.g1234567", "v0.19.7-main.10.g0000000"},
	} {
		if !semver.IsValid(pair[0]) || !semver.IsValid(pair[1]) || semver.Compare(pair[0], pair[1]) >= 0 {
			t.Errorf("want %s < %s", pair[0], pair[1])
		}
	}
}

func TestMainBuildVersionRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		latest string
		count  int
		sha    string
	}{
		"a prerelease":   {"v0.19.6-rc.1", 1, "abcdef0"},
		"no v":           {"0.19.6", 1, "abcdef0"},
		"a partial tag":  {"v0.19", 1, "abcdef0"},
		"no commits":     {"v0.19.6", 0, "abcdef0"},
		"a short sha":    {"v0.19.6", 1, "abc"},
		"not a sha":      {"v0.19.6", 1, "ABCDEFG"},
		"a leading zero": {"v0.019.6", 1, "abcdef0"},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := MainBuildVersion(tc.latest, tc.count, tc.sha); err == nil {
				t.Fatalf("MainBuildVersion accepted it: %q", got)
			}
		})
	}
}

func TestMainBuild(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.19.7-main.4210.g0123abc": true,
		"v0.19.7":                    false,
		"v0.19.7-rc.1":               false,
		"accd52c7":                   false,
		"":                           false,
	} {
		if got := MainBuild(v); got != want {
			t.Errorf("MainBuild(%q) = %v, want %v", v, got, want)
		}
	}
}

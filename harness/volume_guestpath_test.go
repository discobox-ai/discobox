package harness

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/platform"
	"github.com/discobox-ai/discobox/sandboxpath"
)

// A volume path is a path inside the sandbox, judged by the sandbox's platform:
// never by the host's, which may be the Windows one the control plane runs on,
// where filepath refused every declared volume. Only a Linux sandbox has
// declared volumes at all (ADR 0145 §6); on any other platform a declaration
// is refused rather than ignored, and %UID% and %GID% go with it.
func TestResolveVolumesJudgesPathsByTheSandboxPlatform(t *testing.T) {
	for _, tc := range []struct {
		name    string
		os      string
		volume  Volume
		want    string
		refused string
	}{
		{name: "linux absolute", os: "linux", volume: Volume{Path: "/home/ada/.cache/", Volume: VolumeCache}, want: "/home/ada/.cache"},
		{name: "linux home token", os: "linux", volume: Volume{Path: "%HOME%/.cache", Volume: VolumeCache, UID: "%UID%"}, want: "/home/ada/.cache"},
		{name: "linux relative", os: "linux", volume: Volume{Path: "relative/cache", Volume: VolumeCache}, refused: "must be absolute"},
		{name: "linux windows path", os: "linux", volume: Volume{Path: `C:\Users\ada`, Volume: VolumeCache}, refused: "must be absolute"},
		{name: "darwin", os: "darwin", volume: Volume{Path: "/Users/ada/.cache", Volume: VolumeCache}, refused: "Linux container mechanism"},
		{name: "windows", os: "windows", volume: Volume{Path: `C:\Users\ada\.cache`, Volume: VolumeCache}, refused: "Linux container mechanism"},
		{name: "windows uid token", os: "windows", volume: Volume{Path: "%HOME%", Volume: VolumeData, UID: "%UID%"}, refused: "Linux container mechanism"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := sandboxpath.For(platform.Platform{OS: tc.os, Arch: "arm64"})
			got, err := ResolveVolumes(paths, []Volume{tc.volume}, VolumeRuntime{Home: "/home/ada", UID: 1000, GID: 1000})
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("ResolveVolumes = %+v, %v; want a refusal saying %q", got, err, tc.refused)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve volumes: %v", err)
			}
			if len(got) != 1 || got[0].Path != tc.want {
				t.Fatalf("resolved %+v, want path %q", got, tc.want)
			}
		})
	}
}

// No declaration is fine on every platform: there is nothing to refuse.
func TestResolveVolumesWithNoneIsNoneEverywhere(t *testing.T) {
	for _, os := range []string{"linux", "darwin", "windows"} {
		got, err := ResolveVolumes(sandboxpath.For(platform.Platform{OS: os, Arch: "amd64"}), nil, VolumeRuntime{})
		if err != nil || got != nil {
			t.Fatalf("%s: ResolveVolumes(nil) = %+v, %v", os, got, err)
		}
	}
}

package harness

import "testing"

// A volume path is a path inside the sandbox: Linux on every host, including
// the Windows one the control plane may be running on. filepath judged it
// against the host and refused every declared volume there.
func TestResolveVolumesJudgesGuestPathsAsLinuxPaths(t *testing.T) {
	got, err := ResolveVolumes([]Volume{{Path: "/home/ada/.cache", Volume: VolumeCache}}, VolumeRuntime{Home: "/home/ada", UID: 1000, GID: 1000})
	if err != nil {
		t.Fatalf("resolve volumes: %v", err)
	}
	if len(got) != 1 || got[0].Path != "/home/ada/.cache" {
		t.Fatalf("resolved %+v, want the guest path unchanged", got)
	}
	// And a genuinely relative one is still refused.
	if _, err := ResolveVolumes([]Volume{{Path: "relative/cache", Volume: VolumeCache}}, VolumeRuntime{Home: "/home/ada"}); err == nil {
		t.Fatal("a relative volume path was accepted")
	}
	// A Windows path is not a guest path, and is relative by the only rule
	// that matters here.
	if _, err := ResolveVolumes([]Volume{{Path: `C:\Users\ada`, Volume: VolumeCache}}, VolumeRuntime{Home: "/home/ada"}); err == nil {
		t.Fatal("a Windows host path was accepted as a guest path")
	}
}

package boot

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/provision"
)

// A machine has a data volume of its own only where its driver mounted one at
// /.discobox/data; a pool's container, which has a config volume, is wired
// as a container.
func TestMachineDataVolumeIsTheDriversMount(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only a Linux guest wires a data volume")
	}
	if _, err := os.Stat(configMountPath); err == nil {
		t.Skip("this test runs inside a pool's container")
	}
	saved := mountInfoPath
	t.Cleanup(func() { mountInfoPath = saved })
	mountInfoPath = filepath.Join(t.TempDir(), "mountinfo")
	for _, tc := range []struct {
		name, mountinfo string
		want            bool
	}{
		{"mounted", "36 1 0:32 / /.discobox/data rw - ext4 /dev/vdb rw\n", true},
		{"a directory of the disk", "36 1 0:32 / / rw - ext4 /dev/vda rw\n", false},
	} {
		if err := os.WriteFile(mountInfoPath, []byte(tc.mountinfo), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := machineDataVolume(); got != tc.want {
			t.Errorf("%s: machineDataVolume = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The data volume keeps a source at its target's own path, as it keeps a
// declared data path.
func TestMachineSourceDirMirrorsTheTarget(t *testing.T) {
	for target, want := range map[string]string{
		"/workspace":          "/.discobox/data/workspace",
		"/workspace/repo/":    "/.discobox/data/workspace/repo",
		"/home/dev/../dev/ab": "/.discobox/data/home/dev/ab",
	} {
		if got := machineSourceDir(target); got != want {
			t.Errorf("machineSourceDir(%q) = %q, want %q", target, got, want)
		}
	}
}

// A boot's wiring of a machine's data volume is left alone where there is no
// volume, where this boot wired it already, and on a disk not yet provisioned
// for the bootstrap, whose provisioning wires it: none of them mounts
// anything, and none fails.
func TestWireMachineBootLeavesAlone(t *testing.T) {
	dir := t.TempDir()
	useRecords(t, dir)
	savedVolume, savedWired, savedData := machineDataVolume, machineWiredPath, machineDataWiredFile
	t.Cleanup(func() { machineDataVolume, machineWiredPath, machineDataWiredFile = savedVolume, savedWired, savedData })
	machineWiredPath = filepath.Join(dir, "run", "machine-wired")
	machineDataWiredFile = filepath.Join(dir, "data-wired")
	bootstrap := writeBootstrap(t, dir, "sbx_1")
	b, git := newFakeGitBooter(nil)

	machineDataVolume = func() bool { return false }
	if err := b.wireMachineBoot(slog.Default(), bootstrap); err != nil {
		t.Fatalf("no data volume: %v", err)
	}
	machineDataVolume = func() bool { return true }
	if err := b.wireMachineBoot(slog.Default(), bootstrap); err != nil {
		t.Fatalf("a disk not provisioned yet: %v", err)
	}
	if err := (provision.Records{Dir: recordsDir}).RecordSandbox("sbx_1"); err != nil {
		t.Fatal(err)
	}
	if err := markMachineWired("sbx_1"); err != nil {
		t.Fatal(err)
	}
	if err := b.wireMachineBoot(slog.Default(), bootstrap); err != nil {
		t.Fatalf("wired already this boot: %v", err)
	}
	if len(git.runs) != 0 {
		t.Fatalf("runs = %v, want nothing done", git.runs)
	}
	token, err := os.ReadFile(machineWiredPath)
	if err != nil || len(strings.TrimSpace(string(token))) != 32 {
		t.Fatalf("this start's token = %q, %v; want a random one", token, err)
	}
	if data, err := os.ReadFile(machineDataWiredFile); err != nil || string(data) != "sbx_1 "+string(token) {
		t.Fatalf("the data volume's record = %q, %v; want the sandbox's ID and this start's token %q", data, err, token)
	}
}

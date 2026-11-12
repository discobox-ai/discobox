package boot

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
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

// A start that mounted the machine's volumes already is left alone: a
// second mount in one start would stack its mounts.
func TestWireMachineBootOncePerStart(t *testing.T) {
	dir := t.TempDir()
	useRecords(t, dir)
	savedWired := machineWiredPath
	t.Cleanup(func() { machineWiredPath = savedWired })
	machineWiredPath = filepath.Join(dir, "run", "machine-wired")
	if err := os.MkdirAll(filepath.Dir(machineWiredPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(machineWiredPath, []byte("token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, git := newFakeGitBooter(nil)
	if err := b.wireMachineBoot(slog.Default(), sandboxconfig.Config{SandboxID: "sbx_1"}, identity{}); err != nil {
		t.Fatalf("mounted already this start: %v", err)
	}
	if len(git.runs) != 0 {
		t.Fatalf("runs = %v, want nothing done", git.runs)
	}
}

// Every start that mounts keeps a random token of itself in /run; only a
// machine whose data root is its driver's volume says on it that the
// sandbox's data is kept there, naming that token. Roots on the machine's own
// disk say nothing, so the pool never gives that machine a new root (ADR
// 26-11-02-023 §1).
func TestOnlyADataVolumeIsRecordedAsKeepingTheData(t *testing.T) {
	for name, volume := range map[string]bool{"a data volume": true, "roots on the disk": false} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			savedVolume, savedWired, savedData := machineDataVolume, machineWiredPath, machineDataWiredFile
			t.Cleanup(func() { machineDataVolume, machineWiredPath, machineDataWiredFile = savedVolume, savedWired, savedData })
			machineDataVolume = func() bool { return volume }
			machineWiredPath = filepath.Join(dir, "run", "machine-wired")
			machineDataWiredFile = filepath.Join(dir, "data-wired")
			if err := markMachineWired("sbx_1"); err != nil {
				t.Fatal(err)
			}
			token, err := os.ReadFile(machineWiredPath)
			if err != nil || len(strings.TrimSpace(string(token))) != 32 {
				t.Fatalf("this start's token = %q, %v; want a random one", token, err)
			}
			data, err := os.ReadFile(machineDataWiredFile)
			if volume && (err != nil || string(data) != "sbx_1 "+string(token)) {
				t.Fatalf("the data volume's record = %q, %v; want the sandbox's ID and this start's token %q", data, err, token)
			}
			if !volume && !os.IsNotExist(err) {
				t.Fatalf("roots on the disk recorded %q (%v); want nothing", data, err)
			}
		})
	}
}

package boot

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// inPlaceFixture is a machine's bootstrap with a declared data path that
// holds what the machine had there before its mounts, and a source, on roots
// standing in for its data and cache roots.
func inPlaceFixture(t *testing.T) (*booter, *[]string, identity, sandboxconfig.Config, string) {
	t.Helper()
	id := selfIdentity(t)
	b, _ := newFakeGitBooter(nil)
	binds := useMachineRoots(t, b)
	root := t.TempDir()
	state := filepath.Join(root, "var", "lib", "discobox")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "before"), []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bootstrap := sandboxconfig.Config{
		SandboxID:    "sbx_1",
		AgentRuntime: sandboxconfig.AgentRuntime{WorkingRoot: filepath.Join(root, "workspace")},
		Sources:      []sandboxconfig.Source{{Slug: "primary", Target: filepath.Join(root, "workspace", "repo")}},
		Volumes:      []harness.Volume{{Path: state, Volume: harness.VolumeData}},
	}
	return b, binds, id, bootstrap, root
}

// A first placement on a running machine has no mounts this start: the
// wiring in place mounts every path, moving what the machine had there in
// first, as a boot's does, and records what it mounted.
func TestAWiringInPlaceWithNoMountsThisStartMountsEverything(t *testing.T) {
	b, binds, id, bootstrap, root := inPlaceFixture(t)
	if err := b.wireMachineBoot(slog.Default(), bootstrap, id, wireMachine, true); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "var", "lib", "discobox")
	want := []string{
		filepath.Join(dataMountPath, workingRootDirName) + " -> " + filepath.Join(root, "workspace"),
		filepath.Join(dataMountPath, state) + " -> " + state,
		machineSourceDir(filepath.Join(root, "workspace", "repo")) + " -> " + filepath.Join(root, "workspace", "repo"),
	}
	if !slices.Equal(*binds, want) {
		t.Fatalf("binds = %v, want %v", *binds, want)
	}
	if data, err := os.ReadFile(filepath.Join(dataMountPath, state, "before")); err != nil || string(data) != "kept\n" {
		t.Fatalf("what the machine had at %s = %q, %v; want it moved into its volume", state, data, err)
	}
	mounts, ok, err := readMachineMounts()
	if err != nil || !ok || len(mounts) != 3 {
		t.Fatalf("this start's mounts = %v, %v, %v; want the three recorded", mounts, ok, err)
	}
	if !fileExists(machineWiredPath) {
		t.Fatal("no token of this start's wiring")
	}
}

// Placed again with nothing changed, the wiring in place mounts nothing; a
// bootstrap that declares a new source has that target alone mounted; one
// that drops a path leaves it bound.
func TestAWiringInPlaceMountsOnlyWhatThisStartLacks(t *testing.T) {
	b, binds, id, bootstrap, root := inPlaceFixture(t)
	if err := b.wireMachineBoot(slog.Default(), bootstrap, id, wireMachine, false); err != nil {
		t.Fatal(err)
	}
	*binds = nil

	if err := b.wireMachineBoot(slog.Default(), bootstrap, id, wireMachine, true); err != nil {
		t.Fatal(err)
	}
	if len(*binds) != 0 {
		t.Fatalf("binds = %v, want none: the mounts are unchanged", *binds)
	}

	moved := bootstrap
	added := filepath.Join(root, "workspace", "docs")
	moved.Sources = []sandboxconfig.Source{{Slug: "docs", Target: added}}
	if err := b.wireMachineBoot(slog.Default(), moved, id, wireMachine, true); err != nil {
		t.Fatal(err)
	}
	if want := []string{machineSourceDir(added) + " -> " + added}; !slices.Equal(*binds, want) {
		t.Fatalf("binds = %v, want %v: only the new source", *binds, want)
	}
	mounts, _, err := readMachineMounts()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mounts[filepath.Join(root, "workspace", "repo")]; !ok {
		t.Fatalf("mounts = %v; the dropped source stays bound, and recorded, until the next boot", mounts)
	}
	if mounts[added] != machineSourceDir(added) {
		t.Fatalf("mounts = %v; want the new source recorded", mounts)
	}
}

// A wiring in place cut short leaves what it mounted recorded, and the next
// apply mounts only the rest.
func TestAWiringInPlaceCutShortIsFinishedByTheNext(t *testing.T) {
	b, binds, id, bootstrap, root := inPlaceFixture(t)
	if err := b.wireMachineBoot(slog.Default(), bootstrap, id, wireMachine, false); err != nil {
		t.Fatal(err)
	}
	moved := bootstrap
	first, second := filepath.Join(root, "srv", "a"), filepath.Join(root, "srv", "b")
	moved.Volumes = append(slices.Clone(bootstrap.Volumes),
		harness.Volume{Path: first, Volume: harness.VolumeData},
		harness.Volume{Path: second, Volume: harness.VolumeData})
	bind := b.bind
	b.bind = func(source, target string) error {
		if target == second {
			return errors.New("cut short")
		}
		return bind(source, target)
	}
	*binds = nil
	if err := b.wireMachineBoot(slog.Default(), moved, id, wireMachine, true); err == nil {
		t.Fatal("a wiring cut short reported no error")
	}
	b.bind = bind
	*binds = nil
	if err := b.wireMachineBoot(slog.Default(), moved, id, wireMachine, true); err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(dataMountPath, second) + " -> " + second}; !slices.Equal(*binds, want) {
		t.Fatalf("binds = %v, want %v: only what the first did not mount", *binds, want)
	}
}

// A start whose mounts were not recorded -- wired at the agent's start, or
// by an agent from before the record -- is mounted nothing more in place: a
// second mount of a path would stack on the first.
func TestAWiringInPlaceMountsNothingOnAStartItCannotRead(t *testing.T) {
	b, binds, id, bootstrap, _ := inPlaceFixture(t)
	if err := os.MkdirAll(filepath.Dir(machineWiredPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(machineWiredPath, []byte("token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.wireMachineBoot(slog.Default(), bootstrap, id, wireMachine, true); err != nil {
		t.Fatal(err)
	}
	if len(*binds) != 0 {
		t.Fatalf("binds = %v, want none", *binds)
	}
}

// A newly declared path that holds one this start mounted is left to the
// next boot, which mounts parents first: moving it in would rename the inner
// mount point, and binding over it would hide the inner mount.
func TestAWiringInPlaceLeavesANewParentOfAMountToTheNextBoot(t *testing.T) {
	b, binds, id, bootstrap, root := inPlaceFixture(t)
	if err := b.wireMachineBoot(slog.Default(), bootstrap, id, wireMachine, false); err != nil {
		t.Fatal(err)
	}
	*binds = nil
	moved := bootstrap
	parent := filepath.Join(root, "var", "lib")
	moved.Volumes = append(slices.Clone(bootstrap.Volumes), harness.Volume{Path: parent, Volume: harness.VolumeData})
	if err := b.wireMachineBoot(slog.Default(), moved, id, wireMachine, true); err != nil {
		t.Fatal(err)
	}
	if len(*binds) != 0 {
		t.Fatalf("binds = %v, want none: the new parent waits for the next boot", *binds)
	}
	if mounts, _, _ := readMachineMounts(); mounts[parent] != "" {
		t.Fatalf("mounts = %v; the parent is not mounted, and not recorded", mounts)
	}
}

// A working root the wiring in place newly mounts is durable on the data
// volume, and the volume's line says so, naming this start's token, as a
// boot's would.
func TestAWiringInPlaceThatMountsTheWorkingRootSaysSoOnTheVolume(t *testing.T) {
	b, binds, id, bootstrap, root := inPlaceFixture(t)
	machineDataVolume = func() bool { return true }
	backed := bootstrap
	backed.Sources = []sandboxconfig.Source{{Slug: "primary", Target: filepath.Join(root, "workspace")}}
	if err := b.wireMachineBoot(slog.Default(), backed, id, wireMachine, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(machineDataWiredFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	*binds = nil
	// The source that backed the working root is gone.
	unbacked := bootstrap
	unbacked.Sources = nil
	if err := b.wireMachineBoot(slog.Default(), unbacked, id, wireMachine, true); err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(dataMountPath, workingRootDirName) + " -> " + filepath.Join(root, "workspace")}; !slices.Equal(*binds, want) {
		t.Fatalf("binds = %v, want %v", *binds, want)
	}
	token, err := os.ReadFile(machineWiredPath)
	if err != nil {
		t.Fatal(err)
	}
	line, err := os.ReadFile(machineDataWiredFile)
	if want := sandboxconfig.MachineDataWiredLine("sbx_1", strings.TrimSpace(string(token)), true) + "\n"; err != nil || string(line) != want {
		t.Fatalf("the data volume's line = %q, %v; want %q", line, err, want)
	}
}

// A container runtime's state directory, which a box made before ADR
// 26-11-23-993 still declares, is never mounted on a machine: at a boot, or
// in place under the Docker and containerd running on it.
func TestAMachineNeverMountsAContainerRuntimesState(t *testing.T) {
	for _, inPlace := range []bool{false, true} {
		b, binds, id, bootstrap, _ := inPlaceFixture(t)
		bootstrap.Volumes = append(bootstrap.Volumes, harness.Volume{Path: "/var/lib/docker", Volume: harness.VolumeData})
		if err := b.wireMachineBoot(slog.Default(), bootstrap, id, wireMachine, inPlace); err != nil {
			t.Fatal(err)
		}
		for _, bind := range *binds {
			if strings.HasSuffix(bind, " -> /var/lib/docker") {
				t.Fatalf("in place %v: bound %s", inPlace, bind)
			}
		}
		if mounts, _, _ := readMachineMounts(); mounts["/var/lib/docker"] != "" {
			t.Fatalf("in place %v: recorded /var/lib/docker as mounted", inPlace)
		}
	}
}

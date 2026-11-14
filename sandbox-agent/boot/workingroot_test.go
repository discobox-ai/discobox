package boot

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// #1156: a box with no source works in its working root, and every rebuild
// replaces everything but the pool's volumes. What the box wrote there before
// this boot -- in a container made before the working root was a volume, or
// carried into a new one from the container it replaced -- is moved onto the
// data root and the data root's directory is bound over the working root, in a
// container as on a machine, so the next rebuild keeps it (ADR 26-11-14-302
// §§1-3).
func TestAWorkingRootNoSourceBacksIsMovedOntoTheDataRootAndBound(t *testing.T) {
	for name, mode := range map[string]wireMode{"a container": wireContainer, "a machine's boot": wireMachine} {
		t.Run(name, func(t *testing.T) {
			id := selfIdentity(t)
			b, _ := newFakeGitBooter(nil)
			binds := useMachineRoots(t, b)
			root := filepath.Join(t.TempDir(), "workspace")
			writeFile(t, filepath.Join(root, "notes.txt"), "the user's work")
			writeFile(t, filepath.Join(root, "project", "main.go"), "package main")

			durable, err := b.wireWorkingRoot(root, nil, nil, id, mode)
			if err != nil {
				t.Fatalf("wireWorkingRoot: %v", err)
			}
			if !durable {
				t.Fatal("wireWorkingRoot reported the working root not durable after binding it")
			}
			dir := workingRootDir()
			if got := readFile(t, filepath.Join(dir, "notes.txt")); got != "the user's work" {
				t.Fatalf("notes.txt on the data root = %q, want the user's work moved in", got)
			}
			if got := readFile(t, filepath.Join(dir, "project", "main.go")); got != "package main" {
				t.Fatalf("project/main.go on the data root = %q, want it moved in", got)
			}
			if entries, _ := os.ReadDir(root); len(entries) != 0 {
				t.Fatalf("the working root still holds %d entries; want everything moved onto the data root", len(entries))
			}
			if want := dir + " -> " + root; !slices.Contains(*binds, want) {
				t.Fatalf("binds = %v, want %s", *binds, want)
			}
			layout, ok, err := readLayout(workingRootRecordPath(root))
			if err != nil || !ok || layout != layoutBind {
				t.Fatalf("working root's record = %q, %v, %v; want a bind", layout, ok, err)
			}

			// The next boot moves nothing and binds the same directory again.
			*binds = nil
			if _, err := b.wireWorkingRoot(root, nil, nil, id, mode); err != nil {
				t.Fatalf("second wireWorkingRoot: %v", err)
			}
			if want := dir + " -> " + root; !slices.Equal(*binds, []string{want}) {
				t.Fatalf("second boot's binds = %v, want only %s", *binds, want)
			}
		})
	}
}

// A working root a source or a declared volume already backs is theirs: a
// primary source's target is the working root, and a working root inside home
// is home's. Nothing is bound over it, and it counts as durable.
func TestAWorkingRootASourceOrADeclaredVolumeBacksIsLeftToIt(t *testing.T) {
	home := "/home/dev"
	for name, tc := range map[string]struct {
		root    string
		sources []sandboxconfig.Source
		volumes []harness.ResolvedVolume
	}{
		"the primary source":       {root: "/workspace", sources: []sandboxconfig.Source{{Slug: "primary", Target: "/workspace"}}},
		"a source above it":        {root: "/workspace/app", sources: []sandboxconfig.Source{{Slug: "primary", Target: "/workspace"}}},
		"home's volume":            {root: home + "/workspace", volumes: []harness.ResolvedVolume{{Path: home, Kind: harness.VolumeData}}},
		"a volume at the root too": {root: "/workspace", volumes: []harness.ResolvedVolume{{Path: "/workspace", Kind: harness.VolumeData}}},
	} {
		t.Run(name, func(t *testing.T) {
			if !workingRootDurable(tc.root, tc.volumes, tc.sources) {
				t.Fatalf("working root %s not durable under %v / %v", tc.root, tc.sources, tc.volumes)
			}
		})
	}
	for name, tc := range map[string]struct {
		sources []sandboxconfig.Source
		volumes []harness.ResolvedVolume
	}{
		"no source":               {},
		"a source nested in it":   {sources: []sandboxconfig.Source{{Slug: "api", Target: "/workspace/api"}}},
		"a volume nested in it":   {volumes: []harness.ResolvedVolume{{Path: "/workspace/.cache", Kind: harness.VolumeCache}}},
		"a source beside it":      {sources: []sandboxconfig.Source{{Slug: "srv", Target: "/srv/code"}}},
		"a path sharing a prefix": {sources: []sandboxconfig.Source{{Slug: "w", Target: "/work"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if workingRootDurable("/workspace", tc.volumes, tc.sources) {
				t.Fatalf("/workspace durable under %v / %v; want it to need a volume of its own", tc.sources, tc.volumes)
			}
		})
	}
}

// At an agent's start on a machine (no wire unit ran at this boot) the
// machine's services are running on its paths, so a working root that already
// holds work and was never moved in stays where it is, and is not reported
// durable: the pool then rebuilds the machine on the root it has, and that
// boot moves it in (ADR 26-11-14-302 §4).
func TestAnAgentStartLeavesAWorkingRootWithWorkOnTheMachinesRoot(t *testing.T) {
	id := selfIdentity(t)
	b, _ := newFakeGitBooter(nil)
	binds := useMachineRoots(t, b)
	machineDataVolume = func() bool { return true }
	root := filepath.Join(t.TempDir(), "workspace")
	writeFile(t, filepath.Join(root, "notes.txt"), "the user's work")

	durable, err := b.wireWorkingRoot(root, nil, nil, id, wireMachineStarted)
	if err != nil {
		t.Fatalf("wireWorkingRoot: %v", err)
	}
	if durable {
		t.Fatal("a working root left on the machine's root was reported durable")
	}
	if len(*binds) != 0 {
		t.Fatalf("binds = %v, want nothing bound over running services", *binds)
	}
	if got := readFile(t, filepath.Join(root, "notes.txt")); got != "the user's work" {
		t.Fatalf("notes.txt = %q, want it left where it is", got)
	}
}

// A machine keeps a source nested in the working root at its target's path on
// the data root (machineSourceDir). The working root's own directory is apart
// from that tree, so a machine whose disk has the source's mountpoint under its
// working root moves it in without colliding with the source's checkout, and
// the source is still bound from its own directory.
func TestAMachinesWorkingRootDoesNotCollideWithASourceNestedInIt(t *testing.T) {
	id := selfIdentity(t)
	b, _ := newFakeGitBooter(nil)
	binds := useMachineRoots(t, b)
	root := filepath.Join(t.TempDir(), "workspace")
	source := filepath.Join(root, "repo")
	// What an earlier agent left: the source's checkout on the data root, and
	// its (unmounted) mountpoint on the machine's disk.
	writeFile(t, filepath.Join(machineSourceDir(source), "README"), "checkout")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "notes.txt"), "the user's work")
	bootstrap := sandboxconfig.Config{
		AgentRuntime: sandboxconfig.AgentRuntime{WorkingRoot: root},
		Sources:      []sandboxconfig.Source{{Slug: "repo", Target: source}},
	}

	durable, err := b.provision(slog.Default(), id, &bootstrap, wireMachine)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if !durable {
		t.Fatal("provision reported the working root not durable")
	}
	if got := readFile(t, filepath.Join(workingRootDir(), "notes.txt")); got != "the user's work" {
		t.Fatalf("notes.txt on the data root = %q, want it moved in", got)
	}
	if got := readFile(t, filepath.Join(machineSourceDir(source), "README")); got != "checkout" {
		t.Fatalf("the source's checkout = %q, want it untouched", got)
	}
	for _, want := range []string{workingRootDir() + " -> " + root, machineSourceDir(source) + " -> " + source} {
		if !slices.Contains(*binds, want) {
			t.Fatalf("binds = %v, want %s", *binds, want)
		}
	}
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// What the pool carries from a removed container into a new one's layer can
// reach a working root already recorded and bound: the restore lands in a
// container whose box booted once before it. A boot moves it in all the same,
// rather than binding the volume over it, and a name the volume already has
// is set aside beside it, not refused: the box still boots, and neither copy
// is lost (ADR 26-11-14-302 §3).
func TestCarriedWorkReachingARecordedWorkingRootIsMovedIn(t *testing.T) {
	id := selfIdentity(t)
	b, _ := newFakeGitBooter(nil)
	binds := useMachineRoots(t, b)
	root := filepath.Join(t.TempDir(), "workspace")
	writeFile(t, filepath.Join(root, "notes.txt"), "first")
	if _, err := b.wireWorkingRoot(root, nil, nil, id, wireContainer); err != nil {
		t.Fatal(err)
	}
	// The next container's layer, given the carried work.
	writeFile(t, filepath.Join(root, "notes.txt"), "carried")
	writeFile(t, filepath.Join(root, "new.txt"), "carried too")

	if _, err := b.wireWorkingRoot(root, nil, nil, id, wireContainer); err != nil {
		t.Fatalf("wireWorkingRoot: %v", err)
	}
	dir := workingRootDir()
	for name, want := range map[string]string{"notes.txt": "first", "notes.txt.carried": "carried", "new.txt": "carried too"} {
		if got := readFile(t, filepath.Join(dir, name)); got != want {
			t.Fatalf("%s on the data root = %q, want %q", name, got, want)
		}
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("the working root still holds %d entries under the bind", len(entries))
	}
	if layout, ok, err := readLayout(workingRootRecordPath(root)); err != nil || !ok || layout != layoutBind {
		t.Fatalf("record = %q, %v, %v; want a bind again", layout, ok, err)
	}
	if want := dir + " -> " + root; (*binds)[len(*binds)-1] != want {
		t.Fatalf("binds = %v, want the last %s", *binds, want)
	}
}

// At an agent's start an empty working root is recorded and bound as it
// stands: nothing is moved there, as nothing ever is at an agent's start.
func TestAnAgentStartBindsAnEmptyWorkingRootWithoutMoving(t *testing.T) {
	id := selfIdentity(t)
	b, git := newFakeGitBooter(nil)
	binds := useMachineRoots(t, b)
	machineDataVolume = func() bool { return true }
	root := filepath.Join(t.TempDir(), "workspace")

	durable, err := b.wireWorkingRoot(root, nil, nil, id, wireMachineStarted)
	if err != nil || !durable {
		t.Fatalf("wireWorkingRoot = %v, %v; want it bound and durable", durable, err)
	}
	if want := workingRootDir() + " -> " + root; !slices.Equal(*binds, []string{want}) {
		t.Fatalf("binds = %v, want %s", *binds, want)
	}
	if layout, ok, err := readLayout(workingRootRecordPath(root)); err != nil || !ok || layout != layoutBind {
		t.Fatalf("record = %q, %v, %v; want a bind", layout, ok, err)
	}
	if len(git.runs) != 0 {
		t.Fatalf("runs = %v, want nothing copied", git.runs)
	}
}

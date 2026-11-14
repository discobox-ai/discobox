package boot

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/sandbox-agent/provision"
	"github.com/discobox-ai/discobox/sandbox-agent/runuser"
)

// A machine is provisioned from its bootstrap alone: the working root, the
// home skeleton, the git identity and the direnv whitelist are all seeded for
// its user, and each source's target is made and bound from the target's own
// path on the machine's data root, so the intake's clone has somewhere to
// land. A machine with no data volume made no target outside the home, and a
// box with more than one source failed to clone into the missing directories
// (#1030).
func TestAMachineIsProvisionedAndItsSourcesMounted(t *testing.T) {
	id := selfIdentity(t)
	b, git := newFakeGitBooter(nil)
	root := t.TempDir()
	binds := useMachineRoots(t, b)
	workingRoot := filepath.Join(root, "workspace")
	source := filepath.Join(root, "workspace", "repo")
	bootstrap := sandboxconfig.Config{
		AgentRuntime: sandboxconfig.AgentRuntime{WorkingRoot: workingRoot},
		Git:          sandboxconfig.GitIdentity{UserName: "Dev", UserEmail: "dev@example.com"},
		Sources:      []sandboxconfig.Source{{Slug: "primary", Target: source}},
	}

	if _, err := b.provision(slog.Default(), id, &bootstrap, wireMachine); err != nil {
		t.Fatalf("provision: %v", err)
	}

	if info, err := os.Stat(workingRoot); err != nil || !info.IsDir() {
		t.Fatalf("working root %s was not created: %v", workingRoot, err)
	}
	if got := git.configWrites(); got["user.name"] != "Dev" || got["user.email"] != "dev@example.com" {
		t.Fatalf("git config writes = %v, want the bootstrap's identity", got)
	}
	direnv, ok := readDirenvConfig(t, id.home)
	if !ok || !strings.Contains(direnv, `"`+source+`"`) {
		t.Fatalf("direnv.toml = %q (written %v), want the source target whitelisted", direnv, ok)
	}
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		t.Fatalf("source target %s was not made: %v", source, err)
	}
	if want := machineSourceDir(source) + " -> " + source; !slices.Contains(*binds, want) {
		t.Fatalf("binds = %v, want %s", *binds, want)
	}
	skel := false
	for _, run := range git.runs {
		if run[0] == "cp" && len(run) > 2 && run[2] == "/etc/skel" {
			skel = true
		}
	}
	// The skeleton is the host's own /etc/skel, which macOS has none of:
	// seedHome leaves a home it cannot seed as it is.
	if want := dirExists("/etc/skel"); skel != want {
		t.Fatalf("runs = %v, want the home seeded from /etc/skel: %v", git.runs, want)
	}
}

// Boot reads no DISCOBOX_USER_*, which a unit does not see and a VM guest is
// never given: the user is sandbox.json's own, and a stray environment does not
// override it.
func TestBootstrapIdentityIsTheBootstrapsUser(t *testing.T) {
	skipWithoutPOSIXIDs(t)
	t.Cleanup(runuser.FixedDatabase())
	t.Setenv("DISCOBOX_USER_UID", "1500")
	t.Setenv("DISCOBOX_USER_NAME", "image")
	uid := int64(1000)
	id, err := bootstrapIdentity(sandboxconfig.Config{User: sandboxconfig.User{UID: &uid, Name: "dev"}})
	if err != nil {
		t.Fatalf("bootstrap identity: %v", err)
	}
	if !id.configured || id.uid != 1000 || id.gid != 2000 || id.name != "dev" || id.home != "/home/dev" {
		t.Fatalf("identity = %#v, want the bootstrap's dev 1000/2000", id)
	}
}

func TestBootstrapIdentityRejectsGidAndGroupTogether(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	uid, gid := int64(1000), int64(2000)
	user := sandboxconfig.User{UID: &uid, GID: &gid, GroupName: "docker", Name: "dev"}
	if _, err := bootstrapIdentity(sandboxconfig.Config{User: user}); err == nil {
		t.Fatal("expected an error when gid and group name are both set")
	}
}

// A sandbox is provisioned once: an agent restarted after a crash, or a machine
// stopped and started, finds the records naming its own sandbox and runs
// nothing -- no account changes under the user's running processes, no
// re-seeding.
func TestProvisionRunsOncePerSandbox(t *testing.T) {
	// Provisioning resolves the image's user by uid: Linux's alone.
	skipWithoutPOSIXIDs(t)
	dir := t.TempDir()
	useRecords(t, dir)
	bootstrap := writeBootstrap(t, dir, "sbx_1")
	records := provision.Records{Dir: recordsDir}
	if err := records.RecordSandbox("sbx_1"); err != nil {
		t.Fatal(err)
	}
	b := &booter{
		run: func(name string, args ...string) error {
			t.Fatalf("ran %s %v on a sandbox already provisioned", name, args)
			return nil
		},
	}
	useMachineRoots(t, b)
	if err := b.provisionMachine(slog.Default(), bootstrap, false); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := records.Sandbox(); got != "sbx_1" {
		t.Fatalf("provisioned for %q, want sbx_1", got)
	}
}

// A disk provisioned by an agent that kept the marker the records replaced is
// not provisioned again: the marker is carried into the records and removed.
func TestProvisionCarriesTheLegacyMarkerIntoTheRecords(t *testing.T) {
	skipWithoutPOSIXIDs(t)
	dir := t.TempDir()
	useRecords(t, dir)
	bootstrap := writeBootstrap(t, dir, "sbx_1")
	if err := os.WriteFile(legacyProvisionedPath, []byte("sbx_1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &booter{
		run: func(name string, args ...string) error {
			t.Fatalf("ran %s %v on a sandbox the legacy marker names", name, args)
			return nil
		},
	}
	useMachineRoots(t, b)
	if err := b.provisionMachine(slog.Default(), bootstrap, false); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got := (provision.Records{Dir: recordsDir}).Sandbox(); got != "sbx_1" {
		t.Fatalf("records name %q, want sbx_1", got)
	}
	if _, err := os.Stat(legacyProvisionedPath); !os.IsNotExist(err) {
		t.Fatalf("legacy marker still there: %v", err)
	}
}

func useRecords(t *testing.T, dir string) {
	t.Helper()
	savedRecords, savedLegacy := recordsDir, legacyProvisionedPath
	recordsDir = filepath.Join(dir, "provision")
	legacyProvisionedPath = filepath.Join(dir, "provisioned")
	t.Cleanup(func() { recordsDir, legacyProvisionedPath = savedRecords, savedLegacy })
}

func writeBootstrap(t *testing.T, dir, sandboxID string) string {
	t.Helper()
	path := filepath.Join(dir, "sandbox.json")
	if err := os.WriteFile(path, []byte(`{"sandboxId":"`+sandboxID+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The features' wiring goes before the account, with the bootstrap's
// features, and a feature provisioning reports off is not wired: its direnv
// whitelist is not seeded.
func TestProvisionWiresFeaturesFirstAndSkipsWhatIsOff(t *testing.T) {
	id := selfIdentity(t)
	b, git := newFakeGitBooter(nil)
	useMachineRoots(t, b)
	root := t.TempDir()
	b.statusPath = filepath.Join(root, "provision.json")
	status := `{"state":"provisioning","ready":false,"updatedAt":"2026-10-20T00:00:00Z","features":{"direnv":{"state":"off","reason":"turned off"}}}`
	if err := os.WriteFile(b.statusPath, []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
	var wired map[string]sandboxconfig.Feature
	runsBefore := -1
	b.wireFeatures = func(_ *slog.Logger, features map[string]sandboxconfig.Feature) {
		wired, runsBefore = features, len(git.runs)
	}
	source := filepath.Join(root, "workspace", "repo")
	bootstrap := sandboxconfig.Config{
		AgentRuntime: sandboxconfig.AgentRuntime{WorkingRoot: filepath.Join(root, "workspace")},
		Sources:      []sandboxconfig.Source{{Slug: "primary", Target: source}},
		Features:     map[string]sandboxconfig.Feature{"direnv": {Enabled: false}},
	}
	if _, err := b.provision(slog.Default(), id, &bootstrap, wireMachine); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if runsBefore != 0 || len(wired) != 1 || wired["direnv"].Enabled {
		t.Fatalf("wired %v after %d runs; want the bootstrap's features, before anything ran", wired, runsBefore)
	}
	if direnv, ok := readDirenvConfig(t, id.home); ok {
		t.Fatalf("direnv.toml = %q; direnv is off", direnv)
	}
}

// A skills source's target is home-relative in the bootstrap (ADR
// 26-11-01-692 §3); provision expands it against the home it resolved before
// anything reads it, so the direnv whitelist names where it is.
func TestProvisionExpandsAHomeRelativeSourceTarget(t *testing.T) {
	id := selfIdentity(t)
	b, _ := newFakeGitBooter(nil)
	useMachineRoots(t, b)
	root := t.TempDir()
	bootstrap := sandboxconfig.Config{
		AgentRuntime: sandboxconfig.AgentRuntime{WorkingRoot: filepath.Join(root, "workspace")},
		Sources:      []sandboxconfig.Source{{Slug: "tools", Target: "~/.discobox/skill-sources/tools"}},
	}
	if _, err := b.provision(slog.Default(), id, &bootstrap, wireMachine); err != nil {
		t.Fatalf("provision: %v", err)
	}
	want := filepath.Join(id.home, ".discobox", "skill-sources", "tools")
	direnv, ok := readDirenvConfig(t, id.home)
	if !ok || !strings.Contains(direnv, `"`+want+`"`) || strings.Contains(direnv, "~") {
		t.Fatalf("direnv.toml = %q (written %v), want %s whitelisted", direnv, ok, want)
	}
	if bootstrap.Sources[0].Target != "~/.discobox/skill-sources/tools" {
		t.Fatalf("provision rewrote the bootstrap it was given: %q", bootstrap.Sources[0].Target)
	}
}

// useMachineRoots points a machine's data and cache roots, and the records of
// its mounts, at a test's own directory, and has b record its binds rather
// than make them: "source -> target".
func useMachineRoots(t *testing.T, b *booter) *[]string {
	t.Helper()
	// A machine is provisioned as its Linux guest's user, by POSIX ids,
	// which a darwin or Windows sandbox does not have.
	skipWithoutPOSIXIDs(t)
	dir := t.TempDir()
	savedData, savedCache := dataMountPath, cacheMountPath
	savedWired, savedDataWired, savedVolume := machineWiredPath, machineDataWiredFile, machineDataVolume
	t.Cleanup(func() {
		dataMountPath, cacheMountPath = savedData, savedCache
		machineWiredPath, machineDataWiredFile, machineDataVolume = savedWired, savedDataWired, savedVolume
	})
	dataMountPath, cacheMountPath = filepath.Join(dir, "data"), filepath.Join(dir, "cache")
	machineWiredPath, machineDataWiredFile = filepath.Join(dir, "run", "machine-wired"), filepath.Join(dir, "data-wired")
	machineDataVolume = func() bool { return false }
	binds := &[]string{}
	b.bind = func(source, target string) error {
		*binds = append(*binds, source+" -> "+target)
		return nil
	}
	return binds
}

// At the agent's start the wire unit did not run at this boot (an older pool
// placed no drop-in) and the machine's services are running on its paths, so
// nothing is moved out from under them. Roots on the machine's disk mount
// nothing: a declared path keeps what it has, and a source's target is made
// where it is, for the intake to clone into, as before ADR 26-11-02-023.
func TestAMachinesAgentStartMovesNothingOnItsDisk(t *testing.T) {
	id := selfIdentity(t)
	b, _ := newFakeGitBooter(nil)
	binds := useMachineRoots(t, b)
	root := t.TempDir()
	docker := filepath.Join(root, "var", "lib", "docker")
	if err := os.MkdirAll(docker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docker, "engine-id"), []byte("boxd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "workspace", "repo")
	bootstrap := sandboxconfig.Config{
		AgentRuntime: sandboxconfig.AgentRuntime{WorkingRoot: filepath.Join(root, "workspace")},
		Sources:      []sandboxconfig.Source{{Slug: "primary", Target: source}},
		Volumes:      []harness.Volume{{Path: docker, Volume: harness.VolumeData}},
	}
	if _, err := b.provision(slog.Default(), id, &bootstrap, wireMachineStarted); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if len(*binds) != 0 {
		t.Fatalf("binds = %v; roots on the disk mount nothing at the agent's start", *binds)
	}
	if data, err := os.ReadFile(filepath.Join(docker, "engine-id")); err != nil || string(data) != "boxd\n" {
		t.Fatalf("engine-id = %q, %v; want it left where dockerd has it", data, err)
	}
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		t.Fatalf("source target %s was not made: %v", source, err)
	}
	if _, err := os.Stat(dataMountPath); err == nil {
		if entries, _ := os.ReadDir(dataMountPath); len(entries) != 0 {
			t.Fatalf("the data root holds %d entries; nothing should have moved there", len(entries))
		}
	}
}

// On a data volume, the agent's start binds a source as an agent before ADR
// 26-11-02-023 did, without moving what the target holds into it.
func TestAMachinesAgentStartBindsASourceWithoutMovingIt(t *testing.T) {
	id := selfIdentity(t)
	b, _ := newFakeGitBooter(nil)
	binds := useMachineRoots(t, b)
	machineDataVolume = func() bool { return true }
	source := filepath.Join(t.TempDir(), "workspace", "repo")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sources := []sandboxconfig.Source{{Slug: "primary", Target: source}}
	if err := b.wireMachineSources(sources, id, wireMachineStarted); err != nil {
		t.Fatal(err)
	}
	if want := machineSourceDir(source) + " -> " + source; !slices.Equal(*binds, []string{want}) {
		t.Fatalf("binds = %v, want %s", *binds, want)
	}
	if _, err := os.Stat(filepath.Join(source, "README")); err != nil {
		t.Fatalf("the target's content was moved: %v", err)
	}
	if _, ok, _ := readLayout(sourceRecordPath(source)); ok {
		t.Fatal("the agent's start recorded a layout for a source it did not move in")
	}
}

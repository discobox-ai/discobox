// Package agentstore_test runs the image's agent-store script the way a
// sandbox's login sequence does, and is the only thing in this directory.
//
// The pool-cached agent store (ADR 0114) is a shell script in the base image,
// because what it does is symlinks, directory names and npm — the sandbox's own
// vocabulary, in a language the image can run from /etc/profile.d with nothing
// resident. That leaves its rules — pin once and keep it, prefer the store only
// when it is demonstrably newer, ask the registry twice a day, keep two
// versions and anything still in use — provable only by running the script,
// which is what this test does (the same argument as harness/internal/
// launchertest for the launchers).
package agentstore_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scriptPath is the agent-store script as it sits in the image context,
// relative to this directory. The Dockerfile installs it as
// /usr/local/bin/discobox-agent-store.
const scriptPath = "../image/agent-store"

// box is one sandbox's view of the world: its own HOME (where the pin lives, on
// what would be its data volume) and a store directory (what would be the pool
// cache, shared by every box in a test that makes more than one).
type box struct {
	t     *testing.T
	home  string
	store string
	// npmLog records the arguments of every npm invocation the script made, so
	// a test can prove the registry was not asked rather than only that nothing
	// was downloaded.
	npmLog string
	binDir string
	conf   string
	imgDir string
}

const (
	agentPackage = "@anthropic-ai/claude-code"
	agentBin     = "claude"
	slug         = "anthropic-ai-claude-code"
)

// newBox builds a sandbox whose image ships imageVersion of the agent, sharing
// store with any box built from the same store path.
func newBox(t *testing.T, store, imageVersion string) *box {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no POSIX shell to run the store script with: %v", err)
	}
	for _, tool := range []string{"flock", "timeout", "sort"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available: %v", tool, err)
		}
	}
	b := &box{t: t, home: t.TempDir(), store: store}
	b.binDir = filepath.Join(t.TempDir(), "bin")
	b.npmLog = filepath.Join(b.binDir, "npm.log")
	b.imgDir = filepath.Join(t.TempDir(), "node_modules")
	b.conf = filepath.Join(t.TempDir(), "agent.conf")

	if imageVersion != "" {
		writeFile(t, filepath.Join(b.imgDir, agentPackage, "package.json"),
			fmt.Sprintf("{\n  \"version\": %q\n}\n", imageVersion), 0o644)
	}
	writeFile(t, b.conf, fmt.Sprintf("AGENT_PACKAGE=%s\nAGENT_BIN=%s\nAGENT_IMAGE_DIRS=%s\n",
		agentPackage, agentBin, b.imgDir), 0o644)
	b.writeNPMStub()
	// The store lives at a fixed path under HOME, so point HOME's copy at the
	// shared directory the test owns — that is exactly what the cache volume
	// does at boot.
	linkStore(t, b.home, store)
	return b
}

// writeNPMStub installs an npm that answers `npm view <pkg> version` from
// b.latest and builds a version tree for `npm install`, recording every call.
// Nothing in these tests reaches the network.
func (b *box) writeNPMStub() {
	b.t.Helper()
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + b.npmLog + `"
latest=$(cat "` + b.latestFile() + `" 2>/dev/null || true)
case "$1" in
view)
	[ -n "$latest" ] || exit 1
	printf '%s\n' "$latest"
	;;
install)
	[ ! -f "` + b.failFile() + `" ] || exit 1
	prefix=
	spec=
	while [ "$#" -gt 0 ]; do
		if [ "$1" = "--prefix" ]; then
			prefix=$2
		fi
		spec=$1
		shift
	done
	# Stand in for another sandbox's touch_pin recreating the version
	# directory while this install runs: the store is the prefix's parent,
	# and the version is what comes after the last @ in the spec.
	if [ -f "` + b.recreateFile() + `" ]; then
		mkdir -p "$(dirname "$prefix")/${spec##*@}/pins"
	fi
	mkdir -p "$prefix/bin"
	printf '#!/bin/sh\necho agent\n' > "$prefix/bin/` + agentBin + `"
	chmod 0755 "$prefix/bin/` + agentBin + `"
	;;
esac
`
	writeFile(b.t, filepath.Join(b.binDir, "npm"), script, 0o755)
}

func (b *box) latestFile() string   { return filepath.Join(b.binDir, "latest") }
func (b *box) failFile() string     { return filepath.Join(b.binDir, "install-fails") }
func (b *box) recreateFile() string { return filepath.Join(b.binDir, "recreate-target") }

// recreateTargetDuringInstall makes the stub put the version directory back
// while npm is running, which is what a sibling sandbox's pin does to a version
// being refetched.
func (b *box) recreateTargetDuringInstall() {
	b.t.Helper()
	writeFile(b.t, b.recreateFile(), "", 0o644)
}

// publish is the registry: the version `npm view` will answer with.
func (b *box) publish(version string) {
	b.t.Helper()
	writeFile(b.t, b.latestFile(), version+"\n", 0o644)
}

// breakInstall makes the next fetch fail after the version query, the way a
// sandbox powering off mid-download does.
func (b *box) breakInstall() {
	b.t.Helper()
	writeFile(b.t, b.failFile(), "", 0o644)
}

func (b *box) run(args ...string) string {
	b.t.Helper()
	//nolint:gosec // The command is the image's own script and arguments this test wrote.
	cmd := exec.CommandContext(b.t.Context(), "sh", append([]string{absScript(b.t)}, args...)...)
	cmd.Env = append(os.Environ(),
		"HOME="+b.home,
		"PATH="+b.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DISCOBOX_AGENT_CONF="+b.conf,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		b.t.Fatalf("agent-store %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// runFailing runs the script expecting it to fail, and returns what it said on
// both streams. The failure paths are the point of these tests: a command that
// could not do what was asked has to say so and exit non-zero.
func (b *box) runFailing(args ...string) string {
	b.t.Helper()
	//nolint:gosec // The command is the image's own script and arguments this test wrote.
	cmd := exec.CommandContext(b.t.Context(), "sh", append([]string{absScript(b.t)}, args...)...)
	cmd.Env = append(os.Environ(),
		"HOME="+b.home,
		"PATH="+b.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DISCOBOX_AGENT_CONF="+b.conf,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		b.t.Fatalf("agent-store %v succeeded, want it to report failure\n%s", args, out)
	}
	return string(out)
}

// pinTarget is what ~/.local/bin/<agent> points at, or "" when unpinned.
func (b *box) pinTarget() string {
	b.t.Helper()
	target, err := os.Readlink(filepath.Join(b.home, ".local", "bin", agentBin))
	if err != nil {
		return ""
	}
	return target
}

// pinnedVersion is the store version this box runs, or "" when it runs the
// image's own copy.
func (b *box) pinnedVersion() string {
	b.t.Helper()
	target := b.pinTarget()
	if target == "" {
		return ""
	}
	// The link records the store by the path this sandbox reaches it at, under
	// its own HOME — which is where the cache volume is mounted for real.
	rel, err := filepath.Rel(filepath.Join(b.home, ".local", "share", "discobox", "agents", slug), target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	return strings.Split(rel, string(filepath.Separator))[0]
}

func (b *box) npmCalls() []string {
	b.t.Helper()
	data, err := os.ReadFile(b.npmLog)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// storeVersions is every version directory currently in the shared store.
func storeVersions(t *testing.T, store string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(store, slug))
	if err != nil {
		return nil
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			versions = append(versions, e.Name())
		}
	}
	return versions
}

// seedVersion puts a complete version in the store without going through a
// fetch, standing in for a version some other sandbox downloaded earlier.
func seedVersion(t *testing.T, store, version string) {
	t.Helper()
	writeFile(t, filepath.Join(store, slug, version, "bin", agentBin), "#!/bin/sh\n", 0o755)
}

func absScript(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// linkStore points HOME's store path at the shared directory the test owns,
// which is what the cache volume does for the real one.
func linkStore(t *testing.T, home, store string) {
	t.Helper()
	shared := filepath.Join(home, ".local", "share", "discobox", "agents")
	if err := os.MkdirAll(filepath.Dir(shared), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store, shared); err != nil {
		t.Fatal(err)
	}
}

// age backdates a path's mtime, which is how both windows in the script are
// measured: the check stamp and a version's pins.
func age(t *testing.T, path string, d time.Duration) {
	t.Helper()
	when := time.Now().Add(-d)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// A new sandbox runs the newest version the pool already downloaded — the
// store, not the registry, and without asking npm anything.
func TestPinTakesTheNewestVersionInTheStore(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	seedVersion(t, store, "2.1.9")
	seedVersion(t, store, "2.1.10")
	b := newBox(t, store, "2.0.0")

	b.run("pin")

	if got := b.pinnedVersion(); got != "2.1.10" {
		t.Fatalf("pinned %q, want the newest in the store (sorted as versions, not text)", got)
	}
	if calls := b.npmCalls(); len(calls) != 0 {
		t.Fatalf("pin ran npm %v, want a pin decided from the store alone", calls)
	}
}

// The whole point of the pin: what a sandbox runs does not change under it.
func TestPinIsNeverRewritten(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	seedVersion(t, store, "2.1.0")
	b := newBox(t, store, "2.0.0")
	b.run("pin")
	first := b.pinTarget()

	// Another sandbox on the pool downloads a newer one, and this sandbox
	// restarts.
	seedVersion(t, store, "2.2.0")
	b.run("pin")

	if got := b.pinTarget(); got != first {
		t.Fatalf("pin moved to %q, want it to stay at %q across a restart", got, first)
	}
}

// An empty or stale store leaves the image's own copy on PATH, with nothing
// linked and nothing copied.
func TestPinPrefersTheImageWhenTheStoreIsNotNewer(t *testing.T) {
	t.Parallel()
	t.Run("empty store", func(t *testing.T) {
		t.Parallel()
		b := newBox(t, t.TempDir(), "2.1.0")
		b.run("pin")
		if got := b.pinTarget(); got != "" {
			t.Fatalf("pinned %q from an empty store, want the image's own copy used", got)
		}
	})
	t.Run("older than the image", func(t *testing.T) {
		t.Parallel()
		store := t.TempDir()
		seedVersion(t, store, "2.0.9")
		b := newBox(t, store, "2.1.0")
		b.run("pin")
		if got := b.pinTarget(); got != "" {
			t.Fatalf("pinned %q, want the image's newer copy used", got)
		}
	})
}

// A link the sandbox's user put there is theirs.
func TestPinLeavesAnExistingLinkAlone(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	seedVersion(t, store, "2.2.0")
	b := newBox(t, store, "2.0.0")
	mine := filepath.Join(b.home, "my-claude")
	writeFile(t, mine, "#!/bin/sh\n", 0o755)
	if err := os.MkdirAll(filepath.Join(b.home, ".local", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mine, filepath.Join(b.home, ".local", "bin", agentBin)); err != nil {
		t.Fatal(err)
	}

	b.run("pin")

	if got := b.pinTarget(); got != mine {
		t.Fatalf("pin replaced a link it did not write: %q", got)
	}
}

// The store is caught up by whoever logs in next, and every login inside the
// window is free — ten sandboxes starting is one registry query.
func TestRefreshAsksTheRegistryOncePerWindow(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	b := newBox(t, store, "2.0.0")
	b.publish("2.3.0")

	b.run("refresh")
	if got := storeVersions(t, store); len(got) != 1 || got[0] != "2.3.0" {
		t.Fatalf("store holds %v, want the published version fetched", got)
	}
	calls := len(b.npmCalls())

	// A second sandbox starting inside the window asks nothing.
	other := newBox(t, store, "2.0.0")
	other.publish("2.4.0")
	other.run("refresh")
	if got := other.npmCalls(); len(got) != 0 {
		t.Fatalf("a second sandbox ran npm %v inside the check window, want silence", got)
	}

	// Once the window has passed, the next login catches up — no box had to
	// stay running for it.
	age(t, filepath.Join(store, slug, ".last-check"), 13*time.Hour)
	other.run("refresh")
	if got := storeVersions(t, store); len(got) != 2 {
		t.Fatalf("store holds %v after the window passed, want the newer version too", got)
	}
	if len(b.npmCalls()) != calls {
		t.Fatalf("the first sandbox's npm was used for another sandbox's refresh")
	}
}

// A download killed by an idle poweroff leaves no half-version behind, and
// costs the window rather than a retry at every login.
func TestRefreshSurvivesAKilledFetch(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	b := newBox(t, store, "2.0.0")
	b.publish("2.3.0")
	b.breakInstall()

	b.run("refresh")

	if got := storeVersions(t, store); len(got) != 0 {
		t.Fatalf("store holds %v after a failed fetch, want nothing pinnable", got)
	}
	b.run("pin")
	if got := b.pinTarget(); got != "" {
		t.Fatalf("pinned %q after a failed fetch", got)
	}
	before := len(b.npmCalls())
	b.run("refresh")
	if got := len(b.npmCalls()); got != before {
		t.Fatalf("refresh asked the registry again inside the window after a failure (%d calls, was %d)", got, before)
	}
}

// Retention: the newest two always, plus anything a sandbox still says it is
// using, and nothing else.
func TestRefreshKeepsTheNewestTwoAndAnythingStillPinned(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		seedVersion(t, store, v)
	}
	// A sandbox that is still on the oldest version says so, the way a login
	// does.
	writeFile(t, filepath.Join(store, slug, "1.0.0", "pins", "sbx_live"), "", 0o644)
	// And one that stopped using 1.1.0 a couple of months ago.
	writeFile(t, filepath.Join(store, slug, "1.1.0", "pins", "sbx_gone"), "", 0o644)
	age(t, filepath.Join(store, slug, "1.1.0", "pins", "sbx_gone"), 60*24*time.Hour)

	b := newBox(t, store, "0.9.0")
	b.publish("1.3.0")
	b.run("refresh")

	got := storeVersions(t, store)
	want := map[string]bool{"1.0.0": true, "1.2.0": true, "1.3.0": true}
	if len(got) != len(want) {
		t.Fatalf("store holds %v, want the newest two plus the one still in use", got)
	}
	for _, v := range got {
		if !want[v] {
			t.Fatalf("store holds %v, want %v — 1.1.0's last user is long gone", got, want)
		}
	}
}

// The by-hand "now": fetch outside the window and move this sandbox onto it.
func TestUpgradeRefreshesAndRepins(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	seedVersion(t, store, "2.1.0")
	b := newBox(t, store, "2.0.0")
	b.run("pin")
	if got := b.pinnedVersion(); got != "2.1.0" {
		t.Fatalf("pinned %q, want the store's version", got)
	}
	b.publish("2.5.0")
	// Inside the check window, which upgrade ignores.
	writeFile(t, filepath.Join(store, slug, ".last-check"), "", 0o644)

	out := b.run("upgrade")

	if got := b.pinnedVersion(); got != "2.5.0" {
		t.Fatalf("pinned %q after upgrade, want the version it just fetched", got)
	}
	if !strings.Contains(out, "2.1.0") || !strings.Contains(out, "2.5.0") {
		t.Fatalf("upgrade said %q, want it to name the version it moved from and to", out)
	}
}

// An image that installs no agent ships no agent.conf, and every command is a
// no-op there rather than an error in the login sequence.
func TestNoAgentConfIsSilentlyNothing(t *testing.T) {
	t.Parallel()
	b := newBox(t, t.TempDir(), "2.0.0")
	b.conf = filepath.Join(t.TempDir(), "absent.conf")

	for _, arg := range []string{"pin", "refresh", "upgrade", "status"} {
		if out := b.run(arg); out != "" {
			t.Fatalf("%s said %q on an image with no agent, want silence", arg, out)
		}
	}
}

// An image whose version cannot be read must not be overridden by the store:
// the cache routinely lags a freshly built image, and a pin is for life.
func TestPinDeclinesWhenTheImageVersionIsUnreadable(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	seedVersion(t, store, "2.1.0")
	b := newBox(t, store, "") // no package.json for the script to read

	b.run("pin")

	if got := b.pinTarget(); got != "" {
		t.Fatalf("pinned %q with the image's own version unknown, want the image's copy left on PATH", got)
	}
}

// A version directory that lost its executable — an interrupted delete, or an
// install that died after its rename — must not wedge the store.
func TestRefreshRebuildsAndPrunesAnUnusableVersion(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	seedVersion(t, store, "2.3.0")
	// What is left after a delete that was killed part way through.
	if err := os.Remove(filepath.Join(store, slug, "2.3.0", "bin", agentBin)); err != nil {
		t.Fatal(err)
	}
	b := newBox(t, store, "2.0.0")
	b.publish("2.3.0")

	b.run("refresh")

	// The registry still names 2.3.0, so the fetch has to rebuild it rather
	// than see a directory and skip.
	b.run("pin")
	if got := b.pinnedVersion(); got != "2.3.0" {
		t.Fatalf("pinned %q, want the rebuilt version — a directory that is not a usable version must not count as one", got)
	}
}

// Wreckage nothing can pin is still wreckage: retention has to be able to see
// it, or the store grows without bound.
func TestPruneDeletesADirectoryThatIsNotAVersion(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	for _, v := range []string{"3.0.0", "3.1.0", "3.2.0"} {
		seedVersion(t, store, v)
	}
	// 1.0.0 is old, unpinned, and lost its executable, so no pass that walked
	// versions alone could ever reach it.
	seedVersion(t, store, "1.0.0")
	if err := os.Remove(filepath.Join(store, slug, "1.0.0", "bin", agentBin)); err != nil {
		t.Fatal(err)
	}
	// And a temp tree from a fetch that was killed, whatever its age.
	writeFile(t, filepath.Join(store, slug, ".tmp-999", "bin", agentBin), "#!/bin/sh\n", 0o755)

	b := newBox(t, store, "0.9.0")
	b.publish("3.2.0")
	b.run("refresh")

	if _, err := os.Stat(filepath.Join(store, slug, "1.0.0")); !os.IsNotExist(err) {
		t.Fatalf("a directory that is not a usable version survived retention (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(store, slug, ".tmp-999")); !os.IsNotExist(err) {
		t.Fatalf("a killed fetch's temp tree survived retention (%v)", err)
	}
}

// `upgrade` must never report success it did not achieve — the one answer a
// person cannot act on.
func TestUpgradeReportsWhatActuallyFailed(t *testing.T) {
	t.Parallel()
	t.Run("registry unreachable", func(t *testing.T) {
		t.Parallel()
		store := t.TempDir()
		seedVersion(t, store, "2.1.0")
		b := newBox(t, store, "2.0.0")
		b.run("pin")
		// Nothing published: the stub's `npm view` fails the way no network does.

		out := b.runFailing("upgrade")

		if strings.Contains(out, "already the newest") {
			t.Fatalf("upgrade said %q without reaching the registry", out)
		}
		if !strings.Contains(out, "registry") {
			t.Fatalf("upgrade said %q, want it to name what went wrong", out)
		}
	})
	t.Run("install failed", func(t *testing.T) {
		t.Parallel()
		store := t.TempDir()
		b := newBox(t, store, "2.0.0")
		b.publish("2.9.0")
		b.breakInstall()

		out := b.runFailing("upgrade")

		if strings.Contains(out, "already the newest") {
			t.Fatalf("upgrade said %q after the install failed", out)
		}
	})
}

// `mv` moves a source into an existing directory rather than over it, so a
// target that came back during the install must not be renamed into — the tree
// would land a level down, where nothing can pin it, and the fetch would report
// success for a download it threw away.
func TestFetchDoesNotBuryTheTreeInAReappearingTarget(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	b := newBox(t, store, "2.0.0")
	b.publish("2.9.0")
	b.recreateTargetDuringInstall()

	b.run("refresh")

	// Either the fetch replaced the target or it reported failure; what it must
	// never do is leave an unusable tree behind and call it fetched.
	entries, err := os.ReadDir(filepath.Join(store, slug, "2.9.0"))
	if err != nil {
		t.Fatalf("no version directory at all: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("the installed tree was buried at 2.9.0/%s", e.Name())
		}
	}
	b.run("pin")
	if got := b.pinnedVersion(); got != "2.9.0" {
		t.Fatalf("pinned %q, want the fetched version — a fetch that reports success must leave something pinnable", got)
	}
}

// npm's `latest` is a tag, not a high-water mark: a vendor who ships a bad
// release moves it back. Fetching what retention would delete on the next line
// is a download-and-discard loop that repeats every window, forever.
func TestRefreshDoesNotFetchAVersionItWouldImmediatelyDelete(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	seedVersion(t, store, "2.5.0")
	seedVersion(t, store, "2.6.0")
	b := newBox(t, store, "2.0.0")
	// The vendor rolls `latest` back off the bad 2.6.0.
	b.publish("2.4.0")

	b.run("refresh")

	if _, err := os.Stat(filepath.Join(store, slug, "2.4.0")); err == nil {
		t.Fatal("fetched a version retention deletes on the next line, which repeats every window")
	}
	for _, call := range b.npmCalls() {
		if strings.HasPrefix(call, "install") {
			t.Fatalf("ran %q for a version nothing would ever pin", call)
		}
	}
	// And the store is left as it was, so the next window is the same no-op.
	if got := storeVersions(t, store); len(got) != 2 {
		t.Fatalf("store holds %v, want the two versions it started with", got)
	}
}

// The same test also skips what the image already satisfies: a version at or
// below the image's own copy would be downloaded for nobody, since a pin only
// prefers the store when it beats the image.
func TestRefreshDoesNotFetchWhatTheImageAlreadyBeats(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	b := newBox(t, store, "3.0.0")
	b.publish("2.9.0")

	b.run("refresh")

	for _, call := range b.npmCalls() {
		if strings.HasPrefix(call, "install") {
			t.Fatalf("ran %q for a version older than the one the image ships", call)
		}
	}
}

// And a person asking for an upgrade in that state is told what is actually
// going on, rather than "already the newest available".
func TestUpgradeSaysWhenTheRegistryIsBehind(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	seedVersion(t, store, "2.6.0")
	b := newBox(t, store, "2.0.0")
	b.run("pin")
	b.publish("2.4.0")

	out := b.run("upgrade")

	if strings.Contains(out, "already the newest") {
		t.Fatalf("upgrade said %q when the registry is publishing something older", out)
	}
	if !strings.Contains(out, "2.4.0") || !strings.Contains(out, "2.6.0") {
		t.Fatalf("upgrade said %q, want it to name what the registry has and what this box runs", out)
	}
	if got := b.pinnedVersion(); got != "2.6.0" {
		t.Fatalf("pin moved to %q on a rolled-back tag", got)
	}
}

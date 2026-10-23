package boot

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
)

// layoutFixture is a volume directory and a layout record path that does not
// exist yet, both in a test's own temporary directory.
func layoutFixture(t *testing.T) (volDir, record string) {
	t.Helper()
	// Slashes throughout: the code under test joins guest paths with "path",
	// which on a Windows runner splits nothing filepath.Join made.
	root := filepath.ToSlash(t.TempDir())
	return path.Join(root, "data", "home", "ada"), path.Join(root, "data", layoutsDir, "%2Fhome%2Fada")
}

func mustMkdir(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func assertRecorded(t *testing.T, record string, want volumeLayout) {
	t.Helper()
	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("layout record: %v", err)
	}
	if got := volumeLayout(strings.TrimSpace(string(b))); got != want {
		t.Fatalf("recorded %q, want %q", got, want)
	}
}

// A volume seen for the first time takes today's rule, and keeps it.
func TestDecideLayoutRecordsAFreshVolume(t *testing.T) {
	for _, c := range []struct {
		nonEmpty bool
		want     volumeLayout
	}{
		{true, layoutOverlay},
		{false, layoutBind},
	} {
		volDir, record := layoutFixture(t)
		got, err := decideLayout(harness.VolumeData, volDir, record, c.nonEmpty)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Fatalf("target non-empty %v: layout %q, want %q", c.nonEmpty, got, c.want)
		}
		assertRecorded(t, record, c.want)
		entries, err := os.ReadDir(path.Dir(record))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("layouts dir holds %v, want only the record", entries)
		}
	}
}

// An empty volume directory holds nothing to keep, so it is still fresh.
func TestDecideLayoutTreatsAnEmptyVolumeDirAsFresh(t *testing.T) {
	volDir, record := layoutFixture(t)
	mustMkdir(t, volDir)
	got, err := decideLayout(harness.VolumeData, volDir, record, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != layoutOverlay {
		t.Fatalf("layout %q, want overlay", got)
	}
	assertRecorded(t, record, layoutOverlay)
}

// The incident: an overlayed home re-pinned onto an image that leaves the
// target empty must not come up as a bind showing ~/upper and ~/work.
func TestDecideLayoutKeepsARecordedOverlayOverAnEmptyTarget(t *testing.T) {
	volDir, record := layoutFixture(t)
	if err := writeLayout(record, layoutOverlay); err != nil {
		t.Fatal(err)
	}
	got, err := decideLayout(harness.VolumeData, volDir, record, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != layoutOverlay {
		t.Fatalf("layout %q, want the recorded overlay", got)
	}
}

// The reverse: a bound volume whose image starts shipping content at the
// target must not flip to an overlay with an empty upper.
func TestDecideLayoutKeepsARecordedBindOverANonEmptyTarget(t *testing.T) {
	volDir, record := layoutFixture(t)
	if err := writeLayout(record, layoutBind); err != nil {
		t.Fatal(err)
	}
	got, err := decideLayout(harness.VolumeData, volDir, record, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != layoutBind {
		t.Fatalf("layout %q, want the recorded bind", got)
	}
}

// A volume overlayed before layouts were recorded is known by what overlayfs
// left in it: upper and work, with the kernel's own work/work.
func TestDecideLayoutInfersALegacyOverlay(t *testing.T) {
	volDir, record := layoutFixture(t)
	upper, work := overlayDirs(volDir)
	mustMkdir(t, path.Join(upper, ".config"), path.Join(work, "work"))
	got, err := decideLayout(harness.VolumeData, volDir, record, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != layoutOverlay {
		t.Fatalf("layout %q, want overlay", got)
	}
	assertRecorded(t, record, layoutOverlay)
}

// A boot that made upper and work and then failed to mount left them empty
// and alone; that volume was an overlay, and a bind would show ~/upper and
// ~/work.
func TestDecideLayoutInfersALegacyOverlayThatNeverMounted(t *testing.T) {
	volDir, record := layoutFixture(t)
	upper, work := overlayDirs(volDir)
	mustMkdir(t, upper, work)
	got, err := decideLayout(harness.VolumeData, volDir, record, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != layoutOverlay {
		t.Fatalf("layout %q, want overlay", got)
	}
	assertRecorded(t, record, layoutOverlay)
}

// Empty upper and work beside anything else are a user's directories in a
// bound volume.
func TestDecideLayoutInfersALegacyBindWithEmptyDirsNamedUpperAndWork(t *testing.T) {
	volDir, record := layoutFixture(t)
	upper, work := overlayDirs(volDir)
	mustMkdir(t, upper, work, path.Join(volDir, "src"))
	got, err := decideLayout(harness.VolumeData, volDir, record, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != layoutBind {
		t.Fatalf("layout %q, want bind", got)
	}
}

// Writing one volume's record never touches another's, even one whose escaped
// name extends it.
func TestWriteLayoutLeavesAnotherVolumesRecordAlone(t *testing.T) {
	dir := path.Join(filepath.ToSlash(t.TempDir()), layoutsDir)
	x := path.Join(dir, "%2Fsrv%2Fx")
	xtmp := path.Join(dir, "%2Fsrv%2Fx.tmp")
	if err := writeLayout(xtmp, layoutOverlay); err != nil {
		t.Fatal(err)
	}
	if err := writeLayout(x, layoutBind); err != nil {
		t.Fatal(err)
	}
	assertRecorded(t, x, layoutBind)
	assertRecorded(t, xtmp, layoutOverlay)
}

// A bound home whose user happens to keep directories named upper and work is
// still a bind: only overlayfs makes work/work.
func TestDecideLayoutInfersALegacyBindWithUserDirsNamedUpperAndWork(t *testing.T) {
	volDir, record := layoutFixture(t)
	upper, work := overlayDirs(volDir)
	mustMkdir(t, path.Join(upper, "notes"), path.Join(work, "projects"))
	got, err := decideLayout(harness.VolumeData, volDir, record, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != layoutBind {
		t.Fatalf("layout %q, want bind", got)
	}
	assertRecorded(t, record, layoutBind)
}

func TestDecideLayoutInfersALegacyBind(t *testing.T) {
	volDir, record := layoutFixture(t)
	mustMkdir(t, path.Join(volDir, "src"))
	got, err := decideLayout(harness.VolumeData, volDir, record, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != layoutBind {
		t.Fatalf("layout %q, want bind", got)
	}
	assertRecorded(t, record, layoutBind)
}

// A cache volume is shared across sandboxes and never an overlay (ADR 0007),
// and has no layout to record.
func TestDecideLayoutNeverOverlaysOrRecordsACache(t *testing.T) {
	volDir, record := layoutFixture(t)
	got, err := decideLayout(harness.VolumeCache, volDir, record, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != layoutBind {
		t.Fatalf("layout %q, want bind", got)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("cache volume recorded a layout: %v", err)
	}
}

func TestDecideLayoutRefusesAnUnknownRecord(t *testing.T) {
	volDir, record := layoutFixture(t)
	if err := writeLayout(record, "sideways"); err != nil {
		t.Fatal(err)
	}
	if _, err := decideLayout(harness.VolumeData, volDir, record, true); err == nil {
		t.Fatal("an unknown layout record was accepted")
	}
}

func TestLayoutRecordPath(t *testing.T) {
	for p, want := range map[string]string{
		"/home/ada":                  "/.discobox/data/.layouts/%2Fhome%2Fada",
		"/home/ada/go/pkg/mod/":      "/.discobox/data/.layouts/%2Fhome%2Fada%2Fgo%2Fpkg%2Fmod",
		"/nix/var/nix/daemon-socket": "/.discobox/data/.layouts/%2Fnix%2Fvar%2Fnix%2Fdaemon-socket",
	} {
		if got := layoutRecordPath(harness.ResolvedVolume{Path: p, Kind: harness.VolumeData}); got != want {
			t.Errorf("layoutRecordPath(%q) = %q, want %q", p, got, want)
		}
	}
}

// The records sit beside mirrored target paths, so the directory's name has to
// be one no image can declare as a data target.
func TestLayoutsDirCannotBeAMirroredTarget(t *testing.T) {
	if !strings.HasPrefix(layoutsDir, ".") {
		t.Fatalf("layouts dir %q is a plausible absolute path element", layoutsDir)
	}
}

package dockerworker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGuestImageSourceDirRejectsWhatIsNotACheckout(t *testing.T) {
	dir := t.TempDir()
	const dockerfile = "server/providers/vz/image/Dockerfile"

	for name, source := range map[string]string{
		"empty":          "",
		"relative":       "server",
		"missing":        filepath.Join(dir, "absent"),
		"not a checkout": dir,
		"a file, not a dir": func() string {
			path := filepath.Join(dir, "file")
			if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			return path
		}(),
	} {
		if _, err := guestImageSourceDir(source, dockerfile); err == nil {
			t.Errorf("%s: guestImageSourceDir(%q) accepted it", name, source)
		}
	}

	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, filepath.Dir(dockerfile)), 0o755); err != nil {
		t.Fatalf("create checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, dockerfile), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	got, err := guestImageSourceDir(checkout+string(filepath.Separator), dockerfile)
	if err != nil {
		t.Fatalf("guestImageSourceDir(a checkout) = %v", err)
	}
	if got != filepath.Clean(checkout) {
		t.Errorf("guestImageSourceDir = %q, want %q", got, filepath.Clean(checkout))
	}
}

// A pool may be booting from the destination while this runs, so the artifacts
// are staged and swapped rather than written in place.
func TestPublishGuestImageSwapsAndKeepsTheOldOneUntilItIs(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "local")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatalf("create destination: %v", err)
	}
	if err := os.WriteFile(filepath.Join(destination, "vmlinux"), []byte("old kernel"), 0o600); err != nil {
		t.Fatalf("seed destination: %v", err)
	}

	staging := filepath.Join(root, ".build-1")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("create staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "vmlinux"), []byte("new kernel"), 0o600); err != nil {
		t.Fatalf("seed staging: %v", err)
	}

	if err := publishGuestImage(staging, destination); err != nil {
		t.Fatalf("publishGuestImage: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "vmlinux"))
	if err != nil {
		t.Fatalf("read published artifact: %v", err)
	}
	if string(got) != "new kernel" {
		t.Errorf("published artifact = %q, want the newly built one", got)
	}
	// Nothing is left beside it: a stale .previous directory is a second copy
	// of a half-gigabyte root filesystem.
	if _, err := os.Stat(destination + ".previous"); !os.IsNotExist(err) {
		t.Errorf("the previous guest image was left behind (stat err = %v)", err)
	}
}

// A build whose final stage exported nothing must not replace a working guest
// with an empty directory: the driver's resolver would then find an incomplete
// local build, skip it silently, and boot the published image instead.
func TestPublishGuestImageRefusesAnEmptyBuild(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "local")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatalf("create destination: %v", err)
	}
	if err := os.WriteFile(filepath.Join(destination, "vmlinux"), []byte("old kernel"), 0o600); err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	staging := filepath.Join(root, ".build-empty")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("create staging: %v", err)
	}

	if err := publishGuestImage(staging, destination); err == nil {
		t.Fatal("publishGuestImage accepted a build that exported nothing")
	}
	got, err := os.ReadFile(filepath.Join(destination, "vmlinux"))
	if err != nil || string(got) != "old kernel" {
		t.Fatalf("the working guest image was disturbed: %q, %v", got, err)
	}
}

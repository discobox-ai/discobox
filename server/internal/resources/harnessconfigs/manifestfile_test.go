package harnessconfigs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/platform"
)

// overlayManifest is a darwin overlay's manifest file: the sandbox agent's base
// layer for the platform, and a harness's own layer over it.
const overlayManifest = `{
	"io.discobox.image.v1.10-sandbox-base": {
		"apiVersion": "discobox.dev/image/v1",
		"platform": "darwin/arm64",
		"imageKind": "discovm/vz",
		"account": "discobox",
		"shell": "/bin/zsh",
		"env": {"PATH": "/usr/local/bin:/usr/bin:/bin"}
	},
	"io.discobox.image.v1": {
		"apiVersion": "discobox.dev/image/v1",
		"harness": {"id": "agent", "name": "Agent", "secrets": [{"name": "AGENT_TOKEN"}]}
	}
}`

// fileReference is the file:// reference a harness config names a manifest
// file by.
func fileReference(path string) string {
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		// A Windows path, C:/x, is file:///C:/x.
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// The resolver reads a manifest file through the same entry point as an
// image: the layers merge as labels do, the digest is the file's own, and the
// platform the file declares is the one platform the harness is published for.
func TestInspectReadsAManifestFile(t *testing.T) {
	overlayDir := t.TempDir()
	path := filepath.Join(overlayDir, "darwin-arm64", "manifest.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(overlayManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	inspected, err := defaultImageInspector{overlayDir: overlayDir}.Inspect(context.Background(), fileReference(path))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(overlayManifest))
	if want := "sha256:" + hex.EncodeToString(sum[:]); inspected.Digest != want {
		t.Errorf("digest = %q, want the file's own %q", inspected.Digest, want)
	}
	darwin := platform.Platform{OS: "darwin", Arch: "arm64"}
	if len(inspected.Platforms) != 1 || !inspected.Platforms.Contains(darwin) {
		t.Errorf("platforms = %v, want only %v", inspected.Platforms, darwin)
	}
	if inspected.ImageKind != platform.DiscoVM("vz") {
		t.Errorf("image kind = %v, want the discovm/vz the file declares", inspected.ImageKind)
	}
	if inspected.Account != "discobox" || inspected.Shell != "/bin/zsh" {
		t.Errorf("account, shell = %q, %q; want the base layer's", inspected.Account, inspected.Shell)
	}
	if inspected.Harness == nil || inspected.Harness.ID != "agent" || len(inspected.Harness.Secrets) != 1 {
		t.Errorf("harness = %+v, want the leaf layer's", inspected.Harness)
	}
	if inspected.Env["PATH"] != "/usr/local/bin:/usr/bin:/bin" {
		t.Errorf("env = %v, want the base layer's PATH", inspected.Env)
	}
}

// A configured overlay directory may be relative, as any configured path may;
// the reference is absolute, and still reads.
func TestInspectReadsAManifestFileUnderARelativeOverlayDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll("overlays", 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "overlays", "manifest.json")
	if err := os.WriteFile(path, []byte(overlayManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (defaultImageInspector{overlayDir: "overlays"}).Inspect(context.Background(), fileReference(path)); err != nil {
		t.Fatal(err)
	}
}

func TestInspectRefusesAManifestFileReferenceItCannotRead(t *testing.T) {
	overlayDir := t.TempDir()
	// A readable file outside the overlay directory, and a symlink inside it
	// that points there: neither may be read, and the refusal must not say
	// whether the file exists or what it holds.
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.json")
	if err := os.WriteFile(outside, []byte(`{"auths": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// An overlay also holds the agent's binaries: something too large to be a
	// manifest is refused without being read whole, and so is a directory.
	large := filepath.Join(overlayDir, "discobox-sandbox-agent")
	if err := os.WriteFile(large, make([]byte, maxManifestFileBytes+1), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(overlayDir, "darwin-arm64"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(overlayDir, "link.json")
	symlinked := os.Symlink(outside, link) == nil
	climb, err := filepath.Rel(overlayDir, outside)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		ref        string
		overlayDir string
		says       string
	}{
		"another host":         {"file://example.com/manifest.json", overlayDir, `names host "example.com"`},
		"no path":              {"file://", overlayDir, "must name an absolute path"},
		"a missing one":        {fileReference(filepath.Join(overlayDir, "absent.json")), overlayDir, "read manifest file"},
		"outside the overlays": {fileReference(outside), overlayDir, "is not in the overlay directory"},
		"climbing out":         {fileReference(overlayDir) + "/" + filepath.ToSlash(climb), overlayDir, "is not in the overlay directory"},
		"the directory itself": {fileReference(overlayDir), overlayDir, "is not in the overlay directory"},
		"no overlay directory": {fileReference(outside), "", "has no overlay directory"},
		"a binary":             {fileReference(large), overlayDir, "which no manifest is"},
		"a directory":          {fileReference(filepath.Join(overlayDir, "darwin-arm64")), overlayDir, "not a regular file"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := defaultImageInspector{overlayDir: tc.overlayDir}.Inspect(context.Background(), tc.ref)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want one saying %q", err, tc.says)
			}
			if err != nil && strings.Contains(err.Error(), "auths") {
				t.Errorf("err = %v, which says what the file outside holds", err)
			}
		})
	}
	if symlinked {
		_, err := defaultImageInspector{overlayDir: overlayDir}.Inspect(context.Background(), fileReference(link))
		if err == nil || strings.Contains(err.Error(), "auths") {
			t.Errorf("a symlink out of the overlay directory: err = %v, want a refusal that reads nothing", err)
		}
	}
}

// What a manifest file must say, and what a platform without the Linux
// container mechanisms may not, is refused at registration with the reason.
func TestParseManifestFileRefuses(t *testing.T) {
	base := func(fields string) string {
		return `{"io.discobox.image.v1.10-sandbox-base": {"imageKind": "discovm/vz", ` + fields + `}}`
	}
	for name, tc := range map[string]struct {
		file string
		says string
	}{
		"no base layer": {`{"io.discobox.image.v1": {"platform": "darwin/arm64", "account": "a", "shell": "/bin/zsh"}}`,
			"not built on the sandbox agent's overlay"},
		"no platform": {base(`"account": "a", "shell": "/bin/zsh"`), "declares no platform"},
		"no image kind": {`{"io.discobox.image.v1.10-sandbox-base": {"platform": "darwin/arm64", "account": "a", "shell": "/bin/zsh"}}`,
			"declares no disco-vm image kind"},
		"an OCI image kind": {`{"io.discobox.image.v1.10-sandbox-base": {"platform": "linux/arm64", "imageKind": "oci"}}`,
			"declares no disco-vm image kind"},
		"no account": {base(`"platform": "darwin/arm64", "shell": "/bin/zsh"`), "must name the one account"},
		"volumes": {base(`"platform": "darwin/arm64", "account": "a", "shell": "/bin/zsh", "volumes": [{"path": "/nix", "volume": "cache"}]`),
			"manifest file: a darwin manifest declares no volumes"},
		"groups":  {base(`"platform": "darwin/arm64", "account": "a", "shell": "/bin/zsh", "additionalGroups": ["staff"]`), "declares no additionalGroups"},
		"desktop": {base(`"platform": "darwin/arm64", "account": "a", "shell": "/bin/zsh", "features": {"desktop": true}`), "declares no desktop"},
		"docker":  {base(`"platform": "windows/amd64", "account": "a", "shell": "C:\\Windows\\cmd.exe", "features": {"docker": true}`), "declares no nested Docker"},
		// The rules every manifest is held to hold for a file too.
		"a bad secret": {base(`"platform": "darwin/arm64", "account": "a", "shell": "/bin/zsh", "harness": {"secrets": [{"name": "bad name"}]}`),
			"manifest file has invalid secret"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseManifestFile("sha256:test", []byte(tc.file))
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want one saying %q", err, tc.says)
			}
		})
	}
}

// A Linux template built as a disco-vm image — a boxd sandbox's — is a
// manifest file like any other platform's (ADR 26-10-09-106 §4): its kind,
// not its platform, is what keeps it off a Docker pool of linux/arm64.
func TestParseManifestFileReadsALinuxDiscoVMTemplate(t *testing.T) {
	file := `{"io.discobox.image.v1.10-sandbox-base": {"platform": "linux/arm64", "imageKind": "discovm/boxd", "features": {"docker": true}}}`
	inspected, err := parseManifestFile("sha256:test", []byte(file))
	if err != nil {
		t.Fatal(err)
	}
	if inspected.ImageKind != platform.DiscoVM("boxd") {
		t.Errorf("image kind = %v, want discovm/boxd", inspected.ImageKind)
	}
	if !slices.Equal(inspected.Platforms, platform.Set{{OS: "linux", Arch: "arm64"}}) {
		t.Errorf("platforms = %v, want linux/arm64", inspected.Platforms)
	}
}

// An image's platforms are what its registry publishes, so a label claiming
// one is refused rather than read beside that answer — and an image is a Linux
// container, so a label naming an account or a shell is refused too.
func TestParseImageMetadataRefusesWhatOnlyAManifestFileSays(t *testing.T) {
	for name, tc := range map[string]struct {
		own  string
		says string
	}{
		"a platform": {`{"platform": "darwin/arm64"}`, "declares platform darwin/arm64"},
		"a kind":     {`{"imageKind": "discovm/boxd"}`, "declares image kind discovm/boxd"},
		"an account": {`{"account": "ada"}`, "names no account"},
		"a shell":    {`{"shell": "/bin/zsh"}`, "names no shell"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseImageMetadata("sha256:test", withBaseLayer(tc.own))
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want one saying %q", err, tc.says)
			}
		})
	}
	inspected, err := parseImageMetadata("sha256:test", withBaseLayer(`{"features": {"desktop": true}}`))
	if err != nil {
		t.Errorf("a Linux image declaring the desktop: %v", err)
	}
	if inspected.ImageKind != platform.OCI {
		t.Errorf("an image's kind = %v, want oci", inspected.ImageKind)
	}
}

package cli

import (
	"path/filepath"
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

const thisHost = "hst_thismachine0001"

func sandboxWithOrigin(t *testing.T, hostID, hostname, localDir string) (*apimodel.Sandbox, applySourceEntry) {
	t.Helper()
	sandbox := &apimodel.Sandbox{}
	origin := apiclientgen.Origin{HostId: hostID, ProjectPath: localDir}
	if hostname != "" {
		origin.Hostname = apiclientgen.NewOptString(hostname)
	}
	sandbox.Origin = apiclientgen.NewOptOrigin(origin)
	source := apimodel.GitSource{Slug: apiclientgen.NewOptString("primary")}
	if localDir != "" {
		source.LocalDirectory = apiclientgen.NewOptString(localDir)
	}
	return sandbox, applySourceEntry{slug: "primary", source: source}
}

// The sandbox was created here, from a directory that is still there: the one
// case that needs no --dir.
func TestResolveHostDirUsesRecordedDirectoryOnTheSameMachine(t *testing.T) {
	dir := t.TempDir()
	sandbox, entry := sandboxWithOrigin(t, thisHost, "laptop", dir)

	got, origin, err := resolveApplyHostDir(sandbox, thisHost, entry, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != dir {
		t.Fatalf("dir = %q, want %q", got, dir)
	}
	if origin != hostDirFromSandboxOrigin {
		t.Fatalf("origin = %q, want %q", origin, hostDirFromSandboxOrigin)
	}
}

// Applying a sandbox created elsewhere would cherry-pick onto whatever
// repository happens to sit at the recorded path here, which is not the same
// repository. It has to be named explicitly instead.
func TestResolveHostDirRefusesADifferentMachine(t *testing.T) {
	dir := t.TempDir()
	sandbox, entry := sandboxWithOrigin(t, "hst_othermachine0002", "build-box", dir)

	_, _, err := resolveApplyHostDir(sandbox, thisHost, entry, nil)
	if err == nil {
		t.Fatal("a sandbox from another machine resolved a directory anyway")
	}
	for _, want := range []string{"different machine", "hst_othermachine0002", `"build-box"`, thisHost, "--dir primary=PATH"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestResolveHostDirRefusesASandboxWithNoOrigin(t *testing.T) {
	_, entry := sandboxWithOrigin(t, thisHost, "", t.TempDir())

	_, _, err := resolveApplyHostDir(&apimodel.Sandbox{}, thisHost, entry, nil)
	if err == nil {
		t.Fatal("a sandbox with no origin resolved a directory anyway")
	}
	if !strings.Contains(err.Error(), "no recorded origin") {
		t.Fatalf("error = %q", err)
	}
}

// A source cloned from a remote never had a local checkout here to go back to.
func TestResolveHostDirRefusesASourceWithNoLocalDirectory(t *testing.T) {
	sandbox, entry := sandboxWithOrigin(t, thisHost, "", "")

	_, _, err := resolveApplyHostDir(sandbox, thisHost, entry, nil)
	if err == nil {
		t.Fatal("a remote-cloned source resolved a directory anyway")
	}
	if !strings.Contains(err.Error(), "cloned from a remote") {
		t.Fatalf("error = %q", err)
	}
}

// Right machine, but the checkout has since been moved or deleted. Silently
// failing later inside git would blame the wrong thing.
func TestResolveHostDirRefusesAMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	sandbox, entry := sandboxWithOrigin(t, thisHost, "", missing)

	_, _, err := resolveApplyHostDir(sandbox, thisHost, entry, nil)
	if err == nil {
		t.Fatal("a missing directory resolved anyway")
	}
	if !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "--dir primary=PATH") {
		t.Fatalf("error = %q", err)
	}
}

// --dir is the escape hatch for every case above, so it deliberately skips the
// identity check — and the report says so rather than implying the sandbox
// vouched for the directory.
func TestResolveHostDirOverrideSkipsTheIdentityCheck(t *testing.T) {
	sandbox, entry := sandboxWithOrigin(t, "hst_othermachine0002", "", "/gone/on/this/machine")

	got, origin, err := resolveApplyHostDir(sandbox, thisHost, entry, map[string]string{"primary": "/home/ada/src/web"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "/home/ada/src/web" {
		t.Fatalf("dir = %q", got)
	}
	if origin != hostDirFromOverride {
		t.Fatalf("origin = %q, want %q", origin, hostDirFromOverride)
	}

	explanation := formatHostDirOrigin(applySourceReport{Slug: "primary", HostPath: got, HostPathOrigin: origin})
	if !strings.Contains(explanation, "--dir primary=/home/ada/src/web") || !strings.Contains(explanation, "not consulted") {
		t.Fatalf("override explained as %q", explanation)
	}
}

func TestFormatHostDirOriginNamesTheSandboxOrigin(t *testing.T) {
	got := formatHostDirOrigin(applySourceReport{HostPathOrigin: hostDirFromSandboxOrigin})
	if !strings.Contains(got, "created on this machine") {
		t.Fatalf("sandbox-origin explained as %q", got)
	}
}

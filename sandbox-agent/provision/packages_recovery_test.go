package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// Model a new agent run over a filesystem whose previous apt transaction
// was interrupted. No command in this test changes the host package database.
func TestPackagesRecoverInterruptedDpkg(t *testing.T) {
	b := newBox(t)
	b.feature("f", map[string]string{provisionspec.FileName: head + "steps:\n  - name: packages\n    packages: {debian: [git]}\n"})
	interrupted := true
	installed := false
	var calls []string
	b.runner.Command = func(_ context.Context, _ io.Writer, _ string, _ []string, tool string, args ...string) error {
		calls = append(calls, strings.Join(append([]string{tool}, args...), " "))
		if tool == "dpkg" && slices.Contains(args, "--configure") && slices.Contains(args, "-a") {
			interrupted = false
			return nil
		}
		if tool == "apt-get" && len(args) > 0 && args[0] == "install" {
			if interrupted {
				//nolint:revive // Model dpkg's actual diagnostic.
				return errors.New("E: dpkg was interrupted, you must manually run 'dpkg --configure -a' to correct the problem.")
			}
			installed = true
		}
		return nil
	}
	if _, err := b.apply(); err != nil {
		t.Fatalf("provisioning did not recover interrupted dpkg: %v; calls=%v", err, calls)
	}
	if !installed {
		t.Fatal("package installation never completed")
	}
}

func TestAptRecoveryFailureAndRetry(t *testing.T) {
	for _, script := range []bool{false, true} {
		name := "packages"
		definition := "    packages: {debian: [git]}\n"
		if script {
			name = "script"
			definition = "    apt: true\n    run: install.sh\n"
		}
		t.Run(name, func(t *testing.T) {
			b := newBox(t)
			b.feature("f", map[string]string{provisionspec.FileName: head + "steps:\n  - name: install\n" + definition, "install.sh": "#!/bin/sh\nexit 0\n"})
			tracker := b.tracked()
			broken := true
			work := 0
			recovery := errors.New("configuration failed")
			b.runner.Command = func(_ context.Context, _ io.Writer, dir string, env []string, tool string, args ...string) error {
				if tool == "dpkg-query" {
					return errors.New("package missing")
				}
				if tool == "dpkg" {
					if dir != "/" || !slices.Contains(env, "DEBIAN_FRONTEND=noninteractive") || !slices.Contains(args, "--force-confdef") || !slices.Contains(args, "--force-confold") {
						t.Errorf("recovery is not noninteractive: dir=%q env=%v args=%v", dir, env, args)
					}
					if broken {
						return recovery
					}
					return nil
				}
				work++
				return nil
			}
			if _, err := b.apply(); !errors.Is(err, recovery) {
				t.Fatalf("recovery error = %v, want %v", err, recovery)
			}
			if _, recorded, err := b.runner.Records.Step("f", "install"); err != nil || recorded {
				t.Fatalf("failed recovery recorded: %v, %v", recorded, err)
			}
			if work != 0 || tracker.Status().Features["f"].State != provisionspec.StateFailed {
				t.Fatalf("work=%d status=%+v", work, tracker.Status())
			}
			broken = false
			if _, err := b.apply(); err != nil {
				t.Fatal(err)
			}
			if _, recorded, err := b.runner.Records.Step("f", "install"); err != nil || !recorded {
				t.Fatalf("retry not recorded: %v, %v", recorded, err)
			}
			if work == 0 || tracker.Status().Features["f"].State != provisionspec.StateReady {
				t.Fatalf("work=%d status=%+v", work, tracker.Status())
			}
		})
	}
}

// A machine killed during an update leaves a package list cut short, which
// apt keeps as current and fails to parse on every later run, and every step
// using apt fails with it. Recovery removes the lists, so the update that
// follows fetches them whole, and keeps what is not a list.
func TestAptRecoversUnreadablePackageLists(t *testing.T) {
	const truncated = "var/lib/apt/lists/deb.debian.org_debian_dists_sid_main_binary-amd64_Packages.lz4"
	for _, script := range []bool{false, true} {
		name := "packages"
		definition := "    packages: {debian: [git]}\n"
		if script {
			name = "script"
			definition = "    apt: true\n    run: install.sh\n"
		}
		t.Run(name, func(t *testing.T) {
			b := newBox(t)
			list := filepath.Join(b.root, truncated)
			// The script models its own apt-get update failing on the list.
			b.feature("f", map[string]string{
				provisionspec.FileName: head + "steps:\n  - name: install\n" + definition,
				"install.sh":           "#!/bin/sh\n[ ! -e '" + list + "' ] || { echo 'E: LZ4F: Unexpected end of file' >&2; exit 100; }\n",
			})
			tracker := b.tracked()
			for path, content := range map[string]string{
				truncated:                             "cut short",
				"var/lib/apt/lists/lock":              "",
				"var/lib/apt/lists/partial/in-flight": "x",
				"var/cache/apt/pkgcache.bin":          "stale",
				"var/cache/apt/archives/git_1.0.deb":  "kept",
				"var/lib/apt/lists/other_InRelease":   "fine",
			} {
				b.executable(path, content, 0o644)
			}
			apt := b.runner.Command
			b.runner.Command = func(ctx context.Context, out io.Writer, dir string, env []string, tool string, args ...string) error {
				if (tool == "apt-cache" || tool == "apt-get") && b.exists(truncated) {
					// apt's own diagnostic, which names the list from the box's root.
					fmt.Fprintf(out, "E: LZ4F: /%s Unexpected end of file\n", truncated)
					return errors.New("exit status 100")
				}
				return apt(ctx, out, dir, env, tool, args...)
			}
			if _, err := b.apply(); err != nil {
				t.Fatalf("unreadable lists were not recovered: %v", err)
			}
			if _, recorded, err := b.runner.Records.Step("f", "install"); err != nil || !recorded {
				t.Fatalf("recovered step not recorded: %v, %v", recorded, err)
			}
			if state := tracker.Status().Features["f"].State; state != provisionspec.StateReady {
				t.Fatalf("feature is %s", state)
			}
			for _, gone := range []string{truncated, "var/lib/apt/lists/other_InRelease", "var/lib/apt/lists/partial", "var/cache/apt/pkgcache.bin"} {
				if b.exists(gone) {
					t.Errorf("%s was kept", gone)
				}
			}
			for _, kept := range []string{"var/lib/apt/lists/lock", "var/cache/apt/archives/git_1.0.deb"} {
				if !b.exists(kept) {
					t.Errorf("%s was removed", kept)
				}
			}
			if log := b.read(LogDir + "/f.log"); !strings.Contains(log, "cannot read its package lists; removing them") {
				t.Errorf("the log does not say the lists were removed:\n%s", log)
			}
		})
	}
}

// Lists are removed only when apt names one of them as what it cannot read:
// readable lists, and a failure apt pins on something else (a malformed
// sources entry), keep them, and the step's own apt reports the failure.
func TestAptListsKeptUnlessAptNamesOne(t *testing.T) {
	for name, read := range map[string]string{
		"readable":   "",
		"bad source": "E: Malformed entry 1 in list file /etc/apt/sources.list.d/x.list (Suite)\nE: The list of sources could not be read.\n",
	} {
		t.Run(name, func(t *testing.T) {
			b := newBox(t)
			b.feature("f", map[string]string{provisionspec.FileName: head + "steps:\n  - name: install\n    packages: {debian: [git]}\n"})
			b.executable("var/lib/apt/lists/x_Packages", "index", 0o644)
			apt := b.runner.Command
			var probe []string
			b.runner.Command = func(ctx context.Context, out io.Writer, dir string, env []string, tool string, args ...string) error {
				if tool == "apt-cache" {
					probe = args
					if read != "" {
						_, _ = io.WriteString(out, read)
						return errors.New("exit status 100")
					}
					return nil
				}
				return apt(ctx, out, dir, env, tool, args...)
			}
			if _, err := b.apply(); err != nil {
				t.Fatal(err)
			}
			if !b.exists("var/lib/apt/lists/x_Packages") {
				t.Fatal("lists apt did not name were removed")
			}
			if !slices.Contains(b.tools, "apt-get update --error-on=any") {
				t.Fatalf("the step's apt did not run: %v", b.tools)
			}
			// The read writes no cache and takes no lock.
			if !slices.Equal(probe, aptListsRead) || !slices.Contains(probe, "Dir::Cache::pkgcache=") || !slices.Contains(probe, "Debug::NoLocking=true") {
				t.Fatalf("apt-cache %v", probe)
			}
		})
	}
}

package provision

import (
	"context"
	"errors"
	"io"
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

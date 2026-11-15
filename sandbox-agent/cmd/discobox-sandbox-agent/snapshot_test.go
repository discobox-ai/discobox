package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// A snapshot is sealed only once every feature is done and none failed, as
// `discobox-provision wait` reads the box's status; a feature turned off is
// done (ADR 26-11-03-986 §1).
func TestASnapshotIsSealedOnlyWhenProvisionedWhole(t *testing.T) {
	for name, tc := range map[string]struct {
		status provisionspec.Status
		want   string
	}{
		"every feature ready or off": {
			status: provisionspec.Status{State: provisionspec.StateReady, Features: map[string]provisionspec.FeatureStatus{
				"core": {State: provisionspec.StateReady}, "brew": {State: provisionspec.StateOff},
			}},
		},
		"a feature failed": {
			status: provisionspec.Status{State: provisionspec.StateFailed, Features: map[string]provisionspec.FeatureStatus{
				"core": {State: provisionspec.StateReady}, "docker": {State: provisionspec.StateFailed, Error: "exit status 1"},
			}},
			want: "the docker feature failed to provision",
		},
		"a feature still provisioning": {
			status: provisionspec.Status{State: provisionspec.StateProvisioning, Features: map[string]provisionspec.FeatureStatus{
				"nix": {State: provisionspec.StateProvisioning},
			}},
			want: "the nix feature is still",
		},
		"provisioning that failed to start": {
			status: provisionspec.Status{State: provisionspec.StateFailed, Error: "load bundle trees"},
			want:   "load bundle trees",
		},
	} {
		path := filepath.Join(t.TempDir(), "provision.json")
		data, err := json.Marshal(tc.status)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		err = requireProvisioned(path)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v, want sealed", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if err := requireProvisioned(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a box with no provisioning status was sealed")
	}
}

// What a build put under the snapshot's directory goes, and the directory
// stays: on a container it is a bind's mount point.
func TestEmptyDirKeepsTheDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "proxy"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"snapshot.json", "runtime-config.json", "proxy/client.key"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := emptyDir(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("after emptyDir: %v, %v; want an empty directory", entries, err)
	}
	if err := emptyDir(filepath.Join(dir, "gone")); err != nil {
		t.Errorf("emptyDir of a missing directory: %v", err)
	}
}

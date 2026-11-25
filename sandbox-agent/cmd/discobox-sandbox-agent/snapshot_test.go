package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
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

// The build's progress is written as progress lines the taker reads back
// (ADR 26-11-23-004 §1).
func TestTheSnapshotCommandReportsProgressAsLinesTheTakerReads(t *testing.T) {
	var out bytes.Buffer
	reportSnapshotProgress(&out, provisionspec.Status{State: provisionspec.StateProvisioning, Features: map[string]provisionspec.FeatureStatus{
		"docker": {State: provisionspec.StateProvisioning, Step: "install"},
	}})
	reportSnapshotProgress(&out, provisionspec.Status{State: provisionspec.StateReady})
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("wrote %q, want two lines", out.String())
	}
	first, ok := agentlayer.ParseSnapshotProgress([]byte(lines[0]))
	if !ok || first.Features["docker"].Step != "install" {
		t.Fatalf("first line read as %+v, %v", first, ok)
	}
	if last, ok := agentlayer.ParseSnapshotProgress([]byte(lines[1])); !ok || last.State != provisionspec.StateReady {
		t.Fatalf("last line read as %+v, %v", last, ok)
	}
}

// The reporter never holds the tracker up: a writer that is behind gets the
// latest status alone, and the last one reaches the taker at the close.
func TestTheSnapshotProgressReporterWritesTheLatestOffTheTrackersLock(t *testing.T) {
	gate := make(chan struct{})
	w := &gatedWriter{gate: gate}
	reporter := newSnapshotProgressReporter(w)
	for _, state := range []provisionspec.State{provisionspec.StatePending, provisionspec.StateProvisioning, provisionspec.StateProvisioning, provisionspec.StateReady} {
		reporter.observe(provisionspec.Status{State: state}) // never blocks on the writer
	}
	close(gate)
	reporter.close()
	lines := strings.Split(strings.TrimSuffix(w.buf.String(), "\n"), "\n")
	last, ok := agentlayer.ParseSnapshotProgress([]byte(lines[len(lines)-1]))
	if !ok || last.State != provisionspec.StateReady || len(lines) > 2 {
		t.Fatalf("wrote %q, want at most the first and the latest", w.buf.String())
	}
}

// gatedWriter blocks every write until gate closes.
type gatedWriter struct {
	gate <-chan struct{}
	buf  bytes.Buffer
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	<-w.gate
	return w.buf.Write(p)
}

package provision

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

const netDevSample = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9000000     100    0    0    0     0          0         0  9000000     100    0    0    0     0       0          0
  eth0: %d    2000    0    0    0     0          0         0    5000      50    0    0    0     0       0          0
 tun-x:    300       3    0    0    0     0          0         0     300       3    0    0    0     0       0          0
`

func TestReceivedBytesSumsEveryInterfaceButLoopback(t *testing.T) {
	got, ok := receivedBytes(fmt.Appendf(nil, netDevSample, 1200))
	if !ok || got != 1500 {
		t.Fatalf("received = %d, %v; want 1500 (eth0 and tun-x, not lo)", got, ok)
	}
	if _, ok := receivedBytes([]byte("garbage\n")); ok {
		t.Fatal("a file with no interfaces counted something")
	}
}

// progressBox is a box whose run reports progress: a progress directory, and
// a network count the test moves.
func progressBox(t *testing.T) (*box, *Tracker, func(int64)) {
	t.Helper()
	sample := progressSample
	progressSample = 10 * time.Millisecond
	t.Cleanup(func() { progressSample = sample })
	b := newBox(t)
	tracker := NewTracker("")
	tracker.progressSpacing = time.Millisecond
	netDev := filepath.Join(t.TempDir(), "net-dev")
	setReceived := func(eth0 int64) {
		t.Helper()
		staged := netDev + ".new"
		if err := os.WriteFile(staged, fmt.Appendf(nil, netDevSample, eth0), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(staged, netDev); err != nil {
			t.Fatal(err)
		}
	}
	setReceived(1000)
	b.runner.Tracker = tracker
	b.runner.ProgressDir = filepath.Join(t.TempDir(), "progress")
	b.runner.NetDev = netDev
	return b, tracker, setReceived
}

// waitProgress waits for the feature's progress to satisfy ok.
func waitProgress(t *testing.T, tracker *Tracker, feature string, ok func(*provisionspec.Progress) bool) *provisionspec.Progress {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		progress := tracker.Status().Features[feature].Progress
		if ok(progress) {
			return progress
		}
		if time.Now().After(deadline) {
			t.Fatalf("progress of %s = %+v, never what was wanted", feature, progress)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A run step's script reports on the file it is told of, the box's received
// bytes are counted from the step's start, and both go when the step ends.
func TestARunStepReportsItsProgress(t *testing.T) {
	b, tracker, setReceived := progressBox(t)
	b.feature("tool", map[string]string{
		provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: install
    run: install.sh
`,
		"install.sh": `#!/bin/sh
set -eu
printf '%s\n' "$` + provisionspec.ProgressEnv + `" > progress-path
printf 'percent=40 installing the tool\n' >> "$` + provisionspec.ProgressEnv + `"
printf 'a line still being writ' >> "$` + provisionspec.ProgressEnv + `"
while [ ! -e release ]; do sleep 0.01; done
`,
	})
	done := make(chan error, 1)
	go func() { _, err := b.apply(); done <- err }()

	progress := waitProgress(t, tracker, "tool", func(p *provisionspec.Progress) bool { return p != nil && p.Detail != "" })
	if progress.Detail != "installing the tool" || progress.Percent != 40 || progress.Received != 0 {
		t.Fatalf("progress = %+v, want the script's last whole line and nothing received", progress)
	}
	started := progress.ReceivedAt
	if started.IsZero() {
		t.Fatal("a box that counts what it receives reported no time for it")
	}
	// The pool's own calls into the box, under the floor, move nothing.
	setReceived(1000 + 2048)
	time.Sleep(10 * progressSample)
	if progress := tracker.Status().Features["tool"].Progress; progress.Received != 0 || !progress.ReceivedAt.Equal(started) {
		t.Fatalf("progress = %+v after control traffic, want nothing received", progress)
	}
	setReceived(1000 + 1<<20)
	progress = waitProgress(t, tracker, "tool", func(p *provisionspec.Progress) bool { return p != nil && p.Received > 0 })
	if progress.Received != 1<<20 || !progress.ReceivedAt.After(started) {
		t.Fatalf("progress = %+v, want 1 MiB received since the step started, and when", progress)
	}
	// An interface going takes its count with it: what it received stays.
	setReceived(0)
	time.Sleep(10 * progressSample)
	setReceived(1 << 20)
	waitProgress(t, tracker, "tool", func(p *provisionspec.Progress) bool { return p != nil && p.Received == 2<<20 })

	dir := filepath.Join(b.tree, provisionspec.FeaturesDir, "tool")
	data, err := os.ReadFile(filepath.Join(dir, "progress-path"))
	if err != nil {
		t.Fatal(err)
	}
	file := strings.TrimSpace(string(data))
	if filepath.Dir(file) != b.runner.ProgressDir {
		t.Fatalf("the script was told %q, want a file in %s", file, b.runner.ProgressDir)
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	feature := tracker.Status().Features["tool"]
	if feature.State != provisionspec.StateReady || feature.Progress != nil {
		t.Fatalf("feature = %+v, want ready with no progress", feature)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("progress file after the step: %v, want it removed", err)
	}
}

// A box whose received count cannot be read reports none, rather than a
// count that never moves.
func TestAStepWithNoCountReportsNoReceived(t *testing.T) {
	b, tracker, _ := progressBox(t)
	b.runner.NetDev = filepath.Join(t.TempDir(), "missing")
	b.feature("tool", map[string]string{
		provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: install
    run: install.sh
`,
		"install.sh": `#!/bin/sh
printf 'working\n' >> "$` + provisionspec.ProgressEnv + `"
while [ ! -e release ]; do sleep 0.01; done
`,
	})
	done := make(chan error, 1)
	go func() { _, err := b.apply(); done <- err }()
	progress := waitProgress(t, tracker, "tool", func(p *provisionspec.Progress) bool { return p != nil && p.Detail != "" })
	if progress.Received != 0 || !progress.ReceivedAt.IsZero() {
		t.Fatalf("progress = %+v, want no received count", progress)
	}
	if err := os.WriteFile(filepath.Join(b.tree, provisionspec.FeaturesDir, "tool", "release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A script run outside a progress-reporting run is told of no file.
func TestARunWithoutProgressTellsScriptsNothing(t *testing.T) {
	b := newBox(t)
	b.feature("tool", map[string]string{
		provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: install
    run: install.sh
`,
		"install.sh": "#!/bin/sh\nprintf '%s' \"${" + provisionspec.ProgressEnv + "-unset}\" > told\n",
	})
	b.mustApply()
	data, err := os.ReadFile(filepath.Join(b.tree, provisionspec.FeaturesDir, "tool", "told"))
	if err != nil || string(data) != "unset" {
		t.Fatalf("told %q, %v; want the variable unset", data, err)
	}
}

// heldBody serves half of a body, then the rest once released.
type heldBody struct {
	first, rest io.Reader
	release     chan struct{}
}

func (h *heldBody) Read(p []byte) (int, error) {
	if n, err := h.first.Read(p); n > 0 || err != io.EOF {
		return n, err
	}
	<-h.release
	return h.rest.Read(p)
}

func (h *heldBody) Close() error { return nil }

// A download counts what it copies, of the response's length.
func TestADownloadReportsItsBytes(t *testing.T) {
	b, tracker, _ := progressBox(t)
	body := strings.Repeat("x", 4096)
	held := &heldBody{first: strings.NewReader(body[:1024]), rest: strings.NewReader(body[1024:]), release: make(chan struct{})}
	b.runner.Fetch = func(context.Context, string) (io.ReadCloser, int64, error) {
		return held, int64(len(body)), nil
	}
	b.feature("tool", map[string]string{
		provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: fetch
    download:
      path: /opt/tool
      arch:
        amd64: {url: "https://example.com/dist/tool.tar.gz?token=secret", sha256: "` + sha(body) + `"}
`,
	})
	done := make(chan error, 1)
	go func() { _, err := b.apply(); done <- err }()
	progress := waitProgress(t, tracker, "tool", func(p *provisionspec.Progress) bool { return p != nil && p.Done > 0 })
	want := provisionspec.Progress{Detail: "downloading tool.tar.gz from example.com", Done: 1024, Total: 4096}
	if progress.Detail != want.Detail || progress.Done != want.Done || progress.Total != want.Total {
		t.Fatalf("progress = %+v, want %+v", progress, want)
	}
	close(held.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A packages step has apt report its progress on the step's file.
func TestAPackagesStepHasAptReportItsProgress(t *testing.T) {
	b, _, _ := progressBox(t)
	var ran []string
	b.runner.Command = func(_ context.Context, _ io.Writer, _ string, env []string, name string, args ...string) error {
		switch name {
		case "dpkg-query":
			return fmt.Errorf("package missing")
		case "/bin/sh":
			var file string
			for _, kv := range env {
				if value, ok := strings.CutPrefix(kv, provisionspec.ProgressEnv+"="); ok {
					file = value
				}
			}
			if file == "" || args[0] != "-c" || args[1] != aptWithStatus {
				return fmt.Errorf("apt run as %s %q with %q", name, args, env)
			}
			ran = append(ran, strings.Join(args[2:], " "))
		default:
			ran = append(ran, strings.Join(append([]string{name}, args...), " "))
		}
		return nil
	}
	b.feature("tool", map[string]string{
		provisionspec.FileName: `apiVersion: discobox.ai/provision/v1
steps:
  - name: packages
    packages: {debian: [jq]}
`,
	})
	b.mustApply()
	want := "sh -o APT::Status-Fd=3 install -y --no-install-recommends -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold jq"
	if ran[len(ran)-1] != want {
		t.Fatalf("ran:\n%s\nwant last:\n%s", strings.Join(ran, "\n"), want)
	}
}

// Progress moves the status wait at most once per spacing, while Status
// reads it at once; progress for a step the feature has left is dropped.
func TestProgressIsCoalesced(t *testing.T) {
	tracker := NewTracker("")
	tracker.progressSpacing = time.Hour
	tracker.update(func(s *provisionspec.Status) {
		tracker.running = true
		s.Features = map[string]provisionspec.FeatureStatus{"tool": {State: provisionspec.StatePending}}
	})
	tracker.step("tool", "install")
	before := tracker.Changes().Version()
	tracker.progress("tool", "install", provisionspec.Progress{Detail: "first"})
	first := tracker.Changes().Version()
	if first == before {
		t.Fatal("the first progress did not move the status wait")
	}
	tracker.progress("tool", "install", provisionspec.Progress{Detail: "second"})
	if tracker.Changes().Version() != first {
		t.Fatal("progress within the spacing moved the status wait")
	}
	if got := tracker.Status().Features["tool"].Progress; got == nil || got.Detail != "second" {
		t.Fatalf("status progress = %+v, want the latest", got)
	}
	tracker.progress("tool", "other", provisionspec.Progress{Detail: "stale"})
	if got := tracker.Status().Features["tool"].Progress; got.Detail != "second" {
		t.Fatalf("progress of a step the feature is not on landed: %+v", got)
	}
	tracker.step("tool", "next")
	if got := tracker.Status().Features["tool"].Progress; got != nil {
		t.Fatalf("progress = %+v after the step changed, want none", got)
	}

	tracker.mu.Lock()
	tracker.progressSpacing = 20 * time.Millisecond
	tracker.progressTimer.Reset(0)
	tracker.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for tracker.Changes().Version() == first {
		if time.Now().After(deadline) {
			t.Fatal("the progress held back was never published")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

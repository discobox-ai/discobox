package provision

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"gopkg.in/yaml.v3"
)

func configurationFeature(t *testing.T, b *box) []provisionspec.Feature {
	t.Helper()
	dir := filepath.Join(b.tree, "features", "docker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "proxy.conf"), []byte("[Service]\nEnvironment=HTTPS_PROXY=http://127.0.0.1:17008\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return []provisionspec.Feature{{Name: "docker", Dir: dir, Spec: provisionspec.Spec{APIVersion: provisionspec.APIVersion, Async: true, Steps: []provisionspec.Step{
		{Name: "proxy", Configuration: true, Systemd: &provisionspec.SystemdStep{DropIn: &provisionspec.SystemdDrop{Unit: "docker.service", Name: "proxy.conf", Source: "proxy.conf"}}},
	}}}}
}

func TestConfigurationNeverWaitsForOrRestartsDaemon(t *testing.T) {
	b := newBox(t)
	features := configurationFeature(t, b)
	b.runner.Booted = true
	b.runner.Command = func(_ context.Context, _ io.Writer, _ string, _ []string, _ string, args ...string) error {
		command := strings.Join(args, " ")
		if slices.Contains(args, "start") || slices.Contains(args, "try-restart") || slices.Contains(args, "restart") || slices.Contains(args, "enable") {
			t.Fatalf("configuration lifecycle action: %s", command)
		}
		return nil
	}
	if err := b.runner.Configure(context.Background(), features, Options{}, "docker"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.root, "etc/systemd/system/docker.service.d/proxy.conf")); err != nil {
		t.Fatal(err)
	}
	units, err := b.runner.pendingRestarts("docker")
	if err != nil || len(units) != 1 {
		t.Fatalf("pending restart: %v, %v", units, err)
	}
}

func TestConfigurationFailureRetriesAndDoesNotPublishReadiness(t *testing.T) {
	b := newBox(t)
	features := configurationFeature(t, b)
	b.runner.Booted = true
	fail := true
	b.runner.Command = func(_ context.Context, _ io.Writer, _ string, _ []string, _ string, args ...string) error {
		if fail && args[len(args)-1] == "daemon-reload" {
			return errors.New("reload failed")
		}
		return nil
	}
	if err := b.runner.Configure(context.Background(), features, Options{}, "docker"); err == nil {
		t.Fatal("failed reload accepted")
	}
	fail = false
	if err := b.runner.Configure(context.Background(), features, Options{}, "docker"); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationSerializesIndependentRunners(t *testing.T) {
	b := newBox(t)
	features := configurationFeature(t, b)
	var reloading atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	b.runner.Booted = true
	b.runner.Command = func(_ context.Context, _ io.Writer, _ string, _ []string, _ string, args ...string) error {
		if args[len(args)-1] == "daemon-reload" && reloading.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	first := make(chan error, 1)
	go func() { first <- b.runner.Configure(context.Background(), features, Options{}, "docker") }()
	<-entered
	other := &Runner{Root: b.root, Records: b.runner.Records, Platform: ubuntu, Booted: true, Command: b.runner.Command}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := other.Configure(ctx, features, Options{}, "docker"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second runner passed lock: %v", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := other.Configure(context.Background(), features, Options{}, "docker"); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationValidatesSnapshotContentAndHonorsOff(t *testing.T) {
	b := newBox(t)
	features := configurationFeature(t, b)
	if err := b.runner.Configure(context.Background(), features, Options{}, "docker"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(b.root, "etc/systemd/system/docker.service.d/proxy.conf")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	// A matching persistent step record cannot release an unconfigured boot.
	if err := b.runner.Configure(context.Background(), features, Options{}, "docker"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := b.runner.Configure(context.Background(), features, Options{Off: map[string]string{"docker": "project choice"}}, "docker"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("off feature configured: %v", err)
	}
}

func TestConfigurationPlanAbsenceDoesNotMeanOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := WaitConfigurationPlan(ctx, filepath.Join(t.TempDir(), "absent"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("absent plan admitted daemon: %v", err)
	}
}

func TestConfigurationPlanRetainsSnapshotExclusions(t *testing.T) {
	b := newBox(t)
	features := configurationFeature(t, b)
	excluded := false
	features[0].Spec.Steps[0].Snapshot = &excluded
	data, err := yaml.Marshal(ConfigurationPlan{Version: 1, Features: features, Options: Options{Snapshot: true}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := WaitConfigurationPlan(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.runner.Configure(context.Background(), plan.Features, plan.Options, "docker"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.root, "etc/systemd/system/docker.service.d/proxy.conf")); !os.IsNotExist(err) {
		t.Fatalf("snapshot exclusion ignored: %v", err)
	}
}

func TestConfigurationDoesNotProbeAdoptionOrPruneProfiles(t *testing.T) {
	b := newBox(t)
	features := configurationFeature(t, b)
	features = append(features, provisionspec.Feature{Name: "unrelated", Spec: provisionspec.Spec{APIVersion: provisionspec.APIVersion, Adopt: &provisionspec.Adopt{Commands: []string{"docker"}}}})
	b.runner.Command = func(context.Context, io.Writer, string, []string, string, ...string) error {
		t.Fatal("configuration probed a command")
		return nil
	}
	profile := filepath.Join(b.root, EnvProfilePath("unrelated", "env"))
	if err := os.MkdirAll(filepath.Dir(profile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profile, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.runner.Configure(context.Background(), features, Options{Off: map[string]string{"unrelated": "off"}}, "docker"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(profile); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationRejectsEmptyPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WaitConfigurationPlan(context.Background(), path); err == nil {
		t.Fatal("empty plan admitted startup")
	}
}

// Captured for the pre-configuration Step representation: adding a false
// configuration flag must not make every persisted feature look changed.
func TestStepDigestKeepsLegacySteps(t *testing.T) {
	digest, err := StepDigest(provisionspec.Feature{Name: "python", Dir: "/bundle/features/python"}, provisionspec.Step{Name: "install", Group: &provisionspec.GroupStep{Name: "docker"}})
	if err != nil {
		t.Fatal(err)
	}
	const legacy = "sha256:45843f10b1da9954d243432b3762f97653cc596e962e9db59eac73ab0e2b940e"
	if digest != legacy {
		t.Fatalf("unchanged step invalidates persisted record: %s", digest)
	}
}

func TestDockerReopensFailedSocketOnMatchingRecords(t *testing.T) {
	b := newBox(t)
	copyTestTree(t, "../features/docker", filepath.Join(b.tree, provisionspec.FeaturesDir, "docker"))
	setups := 0
	listening := true
	b.runner.Command = func(ctx context.Context, _ io.Writer, _ string, _ []string, name string, args ...string) error {
		for _, arg := range append([]string{name}, args...) {
			switch filepath.Base(arg) {
			case "socket-activation.sh":
				setups++
			case "check-socket-activation.sh":
				if !listening {
					return exec.CommandContext(ctx, "sh", "-c", "exit 1").Run()
				}
			}
		}
		return nil
	}
	b.mustApply()
	if setups != 1 {
		t.Fatalf("initial setup count %d", setups)
	}
	listening = false
	outcomes := b.mustApply()
	if setups != 2 {
		t.Fatal("matching setup record kept a failed socket closed")
	}
	found := false
	for _, outcome := range outcomes {
		if outcome.Feature == "docker" && outcome.Step == "socket-activation" && outcome.Result == Reapplied {
			found = true
		}
	}
	if !found {
		t.Fatal("socket drift was not reapplied")
	}
}

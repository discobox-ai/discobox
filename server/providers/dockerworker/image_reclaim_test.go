package dockerworker

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/devimage"
	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/pool-agent/imagereap"
)

// An unset override must leave the pool container's configuration byte-identical
// to what it was before image retention existed. Materializing a default instead
// would change configRevision and recreate every pool on the daemon at upgrade,
// for a policy nobody asked for.
func TestUnsetImageRetentionLeavesPoolConfigurationUnchanged(t *testing.T) {
	engine, err := New(Config{Image: "pool:test"}, nopDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if engine.cfg.ImageRetention != 0 {
		t.Fatalf("ImageRetention = %s, want zero when unset", engine.cfg.ImageRetention)
	}

	env := engine.poolContainerEnv(poolagent.Bootstrap{ControlPlaneURL: "http://cp", PoolID: "pool-1", Token: "t"})
	if value, ok := env[imagereap.RetentionEnv]; ok {
		t.Fatalf("%s = %q, want absent when unset", imagereap.RetentionEnv, value)
	}

	// configRevision hashes the serialized Config, so the field must serialize
	// away entirely: any key at all, even a zero one, is a different hash than
	// the pools already running were stamped with.
	data, err := json.Marshal(engine.cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if strings.Contains(string(data), "imageRetention") {
		t.Fatalf("unset image retention serialized into the pool configuration: %s", data)
	}
}

func TestConfiguredImageRetentionReachesThePoolAgent(t *testing.T) {
	t.Setenv(imagereap.RetentionEnv, "72h")
	engine, err := New(Config{Image: "pool:test"}, nopDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if engine.cfg.ImageRetention != 72*time.Hour {
		t.Fatalf("ImageRetention = %s, want 72h", engine.cfg.ImageRetention)
	}

	// The pool agent reaps its own daemon, so the setting has to travel into the
	// container to govern it.
	env := engine.poolContainerEnv(poolagent.Bootstrap{ControlPlaneURL: "http://cp", PoolID: "pool-1", Token: "t"})
	if env[imagereap.RetentionEnv] != "72h0m0s" {
		t.Fatalf("%s = %q, want 72h0m0s", imagereap.RetentionEnv, env[imagereap.RetentionEnv])
	}
	// A policy change is a container configuration change, so pools must recreate.
	unset, err := New(Config{Image: "pool:test"}, nopDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	unset.cfg.ImageRetention = 0
	if engine.ConfigRevision() == configRevision(unset.cfg) {
		t.Fatalf("ConfigRevision unchanged after configuring image retention")
	}
}

func TestInvalidImageRetentionFailsEngineConstruction(t *testing.T) {
	t.Setenv(imagereap.RetentionEnv, "sometimes")
	if _, err := New(Config{Image: "pool:test"}, nopDriver{}); err == nil {
		t.Fatal("New succeeded with an unparsable image retention")
	}
}

func TestImageKeepReferencesCoverThePoolImageAndDevelopmentImages(t *testing.T) {
	sync, err := newDevelopmentImageSynchronizer([]devimage.Image{
		{Reference: "discobox-sandbox-agent:dev-abc", ID: "sha256:abc"},
	}, func() (*client.Client, error) { return nil, errors.New("no source daemon in unit tests") })
	if err != nil {
		t.Fatalf("new synchronizer: %v", err)
	}
	engine, err := New(Config{Image: "pool:test", DevelopmentImageSync: sync}, nopDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	// Nothing runs a development base image directly, so only the keep set
	// stops the reaper from taking the base every harness image is built FROM.
	want := map[string]bool{"pool:test": true, "discobox-sandbox-agent:dev-abc": true, "sha256:abc": true}
	got := engine.imageKeepReferences()
	if len(got) != len(want) {
		t.Fatalf("keep references = %v, want %v", got, want)
	}
	for _, reference := range got {
		if !want[reference] {
			t.Fatalf("unexpected keep reference %q", reference)
		}
	}
}

func TestImageReclaimThrottleAllowsOnePassPerIntervalPerPool(t *testing.T) {
	var throttle imageReclaimThrottle
	start := time.Now()
	interval := time.Hour

	if !throttle.claim("pool-1", start, interval) {
		t.Fatal("first claim denied")
	}
	if throttle.claim("pool-1", start.Add(interval-time.Minute), interval) {
		t.Fatal("second claim allowed within the interval")
	}
	// A second pool is a second daemon on every VM backend, so it is not
	// throttled by the first.
	if !throttle.claim("pool-2", start, interval) {
		t.Fatal("claim for another pool denied")
	}
	if !throttle.claim("pool-1", start.Add(interval), interval) {
		t.Fatal("claim denied after the interval elapsed")
	}
}

// A development daemon supersedes a multi-gigabyte image every few minutes, so
// it must not inherit the production window — and the sweep has to keep pace
// with it, or the shorter window changes nothing.
func TestDevelopmentDaemonsUseTheDevelopmentRetentionAndCadence(t *testing.T) {
	sync, err := newDevelopmentImageSynchronizer([]devimage.Image{
		{Reference: "discobox-sandbox-agent:dev-abc", ID: "sha256:abc"},
	}, func() (*client.Client, error) { return nil, errors.New("no source daemon in unit tests") })
	if err != nil {
		t.Fatalf("new synchronizer: %v", err)
	}
	development, err := New(Config{Image: "pool:test", DevelopmentImageSync: sync}, nopDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if development.imageRetention() != imagereap.DevelopmentRetention {
		t.Fatalf("development retention = %s, want %s", development.imageRetention(), imagereap.DevelopmentRetention)
	}
	if got := development.ImageReclaimInterval(); got >= time.Hour {
		t.Fatalf("development reclaim interval = %s, want well under the production hour", got)
	}

	// A production engine has no synchronizer and must be untouched by this.
	production, err := New(Config{Image: "pool:test"}, nopDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if production.imageRetention() != imagereap.DefaultRetention {
		t.Fatalf("production retention = %s, want %s", production.imageRetention(), imagereap.DefaultRetention)
	}
	if got := production.ImageReclaimInterval(); got != time.Hour {
		t.Fatalf("production reclaim interval = %s, want 1h", got)
	}
}

// An operator who sets the window explicitly means it, on a development daemon
// as much as anywhere else.
func TestExplicitImageRetentionOverridesTheDevelopmentDefault(t *testing.T) {
	t.Setenv(imagereap.RetentionEnv, "6h")
	sync, err := newDevelopmentImageSynchronizer([]devimage.Image{
		{Reference: "discobox-sandbox-agent:dev-abc", ID: "sha256:abc"},
	}, func() (*client.Client, error) { return nil, errors.New("no source daemon in unit tests") })
	if err != nil {
		t.Fatalf("new synchronizer: %v", err)
	}
	engine, err := New(Config{Image: "pool:test", DevelopmentImageSync: sync}, nopDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if engine.imageRetention() != 6*time.Hour {
		t.Fatalf("retention = %s, want the explicit 6h", engine.imageRetention())
	}
}

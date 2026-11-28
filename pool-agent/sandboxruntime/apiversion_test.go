package sandboxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

var testLayerDigest = "sha256:" + strings.Repeat("c", 64)

// The pool writes the sandbox.json version the pinned agent's bundle
// declares it reads, and the legacy one for an agent that declares nothing
// or a pool that places no agent (ADR 26-11-24-519 §2).
func TestThePoolWritesTheVersionThePinnedAgentReads(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		layers AgentLayers
		want   string
	}{
		{"a labeled agent", &fakeAgentLayers{digest: testLayerDigest, reads: sandboxconfig.ReadAPIVersions}, sandboxconfig.APIVersion},
		{"an unlabeled agent", &fakeAgentLayers{digest: testLayerDigest}, sandboxconfig.LegacyAPIVersion},
		{"no asset cache", nil, sandboxconfig.LegacyAPIVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := placementFixture(t, tc.layers)
			got, err := r.sandboxConfigFor(ctx, "sbx_1")
			if err != nil || got.APIVersion != tc.want {
				t.Fatalf("sandboxConfigFor = %+v, %v; want %q", got, err, tc.want)
			}
		})
	}
	r, _ := placementFixture(t, &fakeAgentLayers{digest: testLayerDigest, reads: []string{"discobox.ai/sandbox/v2"}})
	if got, err := r.sandboxConfigFor(ctx, "sbx_1"); err == nil {
		t.Fatalf("sandboxConfigFor = %+v for an agent that reads neither; want it refused", got)
	}
}

// What an agent reads is in its bundle's config, which the pool asks for only
// for an agent layer new to the sandbox, and remembers with what the file
// holds. A box from before the record whose bundle cannot be read any more is
// answered from the layer last placed in it, which booted on the legacy file
// (ADR 26-11-24-519).
func TestThePoolAsksWhatAnAgentReadsOnlyForALayerNewToTheSandbox(t *testing.T) {
	ctx := context.Background()
	newer := "sha256:" + strings.Repeat("d", 64)
	for _, tc := range []struct {
		name string
		// placed is the layer a pool from before the record placed, its
		// legacy file booted on; "" is none.
		placed   string
		record   *sandboxConfigRecord
		layers   *fakeAgentLayers
		want     sandboxConfigRecord
		refused  bool
		wantAsks int
	}{
		{
			name:     "recorded for this layer",
			record:   &sandboxConfigRecord{APIVersion: sandboxconfig.LegacyAPIVersion, Layer: testLayerDigest, Writes: sandboxconfig.APIVersion},
			layers:   &fakeAgentLayers{digest: testLayerDigest, readsErr: errors.New("gone")},
			want:     sandboxConfigRecord{APIVersion: sandboxconfig.APIVersion, Layer: testLayerDigest, Writes: sandboxconfig.APIVersion},
			wantAsks: 0,
		},
		{
			name:     "recorded for another layer",
			record:   &sandboxConfigRecord{APIVersion: sandboxconfig.LegacyAPIVersion, Layer: newer, Writes: sandboxconfig.LegacyAPIVersion},
			layers:   &fakeAgentLayers{digest: testLayerDigest, reads: sandboxconfig.ReadAPIVersions},
			want:     sandboxConfigRecord{APIVersion: sandboxconfig.APIVersion, Layer: testLayerDigest, Writes: sandboxconfig.APIVersion},
			wantAsks: 1,
		},
		{
			name:     "from before the record, its labeled agent asked",
			placed:   testLayerDigest,
			layers:   &fakeAgentLayers{digest: testLayerDigest, reads: sandboxconfig.ReadAPIVersions},
			want:     sandboxConfigRecord{APIVersion: sandboxconfig.APIVersion, Layer: testLayerDigest, Writes: sandboxconfig.APIVersion},
			wantAsks: 1,
		},
		{
			name:     "from before the record, its bundle gone",
			placed:   testLayerDigest,
			layers:   &fakeAgentLayers{digest: testLayerDigest, readsErr: errors.New("gone")},
			want:     sandboxConfigRecord{APIVersion: sandboxconfig.LegacyAPIVersion, Layer: testLayerDigest, Writes: sandboxconfig.LegacyAPIVersion},
			wantAsks: 1,
		},
		{
			name:     "from before the record, a bundle never placed and gone",
			placed:   newer,
			layers:   &fakeAgentLayers{digest: testLayerDigest, readsErr: errors.New("gone")},
			refused:  true,
			wantAsks: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := placementFixture(t, tc.layers)
			if err := os.MkdirAll(r.sandboxRoot("sbx_1"), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.placed != "" {
				if err := r.writePlaced("sbx_1", placedRecord{Container: "ctr_1", Layer: tc.placed}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.record != nil {
				if err := r.recordSandboxConfig("sbx_1", *tc.record); err != nil {
					t.Fatal(err)
				}
			}
			got, err := r.sandboxConfigFor(ctx, "sbx_1")
			if tc.refused != (err != nil) || !tc.refused && got != tc.want || tc.layers.readsAsked != tc.wantAsks {
				t.Fatalf("sandboxConfigFor = %+v, %v, the bundle asked %d times; want %+v (refused %t), asked %d", got, err, tc.layers.readsAsked, tc.want, tc.refused, tc.wantAsks)
			}
		})
	}
}

// Whether the agent reads the file a container holds is known without asking
// its bundle for the layer the record names, and for the layer last placed in
// a box from before the record. Another layer is asked once: the record then
// names it, with the newest it reads for the next file written (ADR
// 26-11-24-519 §3).
func TestWhetherTheAgentReadsTheFileIsAskedOncePerLayer(t *testing.T) {
	ctx := context.Background()
	layers := &fakeAgentLayers{digest: testLayerDigest, readsErr: errors.New("gone")}
	r, _ := placementFixture(t, layers)
	if err := os.MkdirAll(r.sandboxRoot("sbx_1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.writePlaced("sbx_1", placedRecord{Container: "ctr_1", Layer: testLayerDigest}); err != nil {
		t.Fatal(err)
	}
	if reads, err := r.agentReadsWritten(ctx, "sbx_1"); err != nil || !reads || layers.readsAsked != 0 {
		t.Fatalf("agentReadsWritten = %v, %v, asked %d; want the agent that booted on it, unasked", reads, err, layers.readsAsked)
	}

	// Moved in place to a labeled agent: it reads the legacy file, and the
	// next file written for it is the newest it reads.
	layers.digest, layers.readsErr, layers.reads = "sha256:"+strings.Repeat("d", 64), nil, sandboxconfig.ReadAPIVersions
	for range 2 {
		if reads, err := r.agentReadsWritten(ctx, "sbx_1"); err != nil || !reads {
			t.Fatalf("agentReadsWritten = %v, %v; want true", reads, err)
		}
	}
	if layers.readsAsked != 1 {
		t.Fatalf("the bundle was asked %d times, want once", layers.readsAsked)
	}
	want := sandboxConfigRecord{APIVersion: sandboxconfig.LegacyAPIVersion, Layer: layers.digest, Writes: sandboxconfig.APIVersion}
	if record, ok, err := r.readSandboxConfig("sbx_1"); err != nil || !ok || record != want {
		t.Fatalf("record = %+v, %v, %v; want %+v", record, ok, err, want)
	}
	if got, err := r.sandboxConfigFor(ctx, "sbx_1"); err != nil || got.APIVersion != sandboxconfig.APIVersion {
		t.Fatalf("sandboxConfigFor = %+v, %v; want the newest the agent reads", got, err)
	}
}

// A machine renders sandbox.json at every placement, under the version its
// agent reads now, so an agent pin that moves the version moves the
// bootstrap's digest, and the machine restarts onto it.
func TestAMachineBootstrapFollowsWhatItsAgentReads(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	render := func(layers *fakeAgentLayers) (string, string) {
		t.Helper()
		r.agentLayers = layers
		data, version, digest, err := r.renderBootstrap(ctx, "sbx_1", machineRecord{})
		if err != nil {
			t.Fatal(err)
		}
		var file struct {
			APIVersion string `json:"apiVersion"`
		}
		if err := json.Unmarshal(data, &file); err != nil {
			t.Fatal(err)
		}
		if version.APIVersion != file.APIVersion || version.Layer != layers.digest {
			t.Fatalf("rendered %q, and says %+v", file.APIVersion, version)
		}
		return file.APIVersion, digest
	}
	oldVersion, oldDigest := render(&fakeAgentLayers{digest: testLayerDigest})
	newVersion, newDigest := render(&fakeAgentLayers{digest: "sha256:" + strings.Repeat("d", 64), reads: sandboxconfig.ReadAPIVersions})
	if oldVersion != sandboxconfig.LegacyAPIVersion || newVersion != sandboxconfig.APIVersion {
		t.Fatalf("rendered %q for an unlabeled agent and %q for a labeled one", oldVersion, newVersion)
	}
	if oldDigest == newDigest {
		t.Fatal("the bootstrap's digest did not move with its apiVersion")
	}
}

// A running machine from before the record, whose bundle cannot be read, has
// its placement checked at every request: the bundle is asked once, and the
// legacy file its agent booted on is recorded, so no later check asks again.
func TestAnUnreadableBundleIsAskedOnceForABoxFromBeforeTheRecord(t *testing.T) {
	ctx := context.Background()
	layers := &fakeAgentLayers{digest: testLayerDigest, readsErr: errors.New("gone")}
	r, _ := placementFixture(t, layers)
	if err := os.MkdirAll(r.sandboxRoot("sbx_1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.writePlaced("sbx_1", placedRecord{Container: "ctr_1", Layer: testLayerDigest}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if got, err := r.sandboxConfigFor(ctx, "sbx_1"); err != nil || got.APIVersion != sandboxconfig.LegacyAPIVersion {
			t.Fatalf("sandboxConfigFor = %+v, %v; want the legacy file it booted on", got, err)
		}
	}
	if layers.readsAsked != 1 {
		t.Fatalf("the bundle was asked %d times, want once", layers.readsAsked)
	}
}

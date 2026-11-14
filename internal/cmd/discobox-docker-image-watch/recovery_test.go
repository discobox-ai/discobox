package main

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/devimage"
)

func harnessLayout(t *testing.T, metadata string) string {
	t.Helper()
	// A tiny OCI layout with only the harness label; no registry dependency is
	// needed to exercise the watcher's exported-layout publication boundary.
	config, err := json.Marshal(map[string]any{
		"os": "linux", "architecture": "amd64",
		"config": map[string]any{"Labels": map[string]string{harness.ImageLabel: metadata}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	configHash := fmt.Sprintf("%x", sha256.Sum256(config))
	manifest := fmt.Appendf(nil, `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:%s","size":%d},"layers":[]}`, configHash, len(config))
	manifestHash := fmt.Sprintf("%x", sha256.Sum256(manifest))
	index := fmt.Appendf(nil, `{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%s","size":%d}]}`, manifestHash, len(manifest))
	out := filepath.Join(t.TempDir(), "bundle.tar")
	file, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	for name, data := range map[string][]byte{
		"oci-layout":                   []byte(`{"imageLayoutVersion":"1.0.0"}`),
		"index.json":                   index,
		"blobs/sha256/" + configHash:   config,
		"blobs/sha256/" + manifestHash: manifest,
	} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWatcherRecoversFromStalledInspectionAndPublishesChangedHarness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script on PATH")
	}
	t.Setenv(buildModeEnv, "false")
	// The one stalling "docker image ls" is cut off by the watcher's Docker
	// deadline, which every other inspection and tag command shares (builds
	// have their own). Keep it long enough that a loaded runner still starts
	// and finishes those in time.
	const deadline = 2 * time.Second
	old := *dockerCommandTimeout
	*dockerCommandTimeout = deadline
	t.Cleanup(func() { *dockerCommandTimeout = old })
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1 $2" in
 'image inspect') echo sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef ;;
 'image ls')
   echo check >> checks
   if test ! -f stalled; then touch stalled; exec sleep 3600; fi
   echo base:local
   ;;
 'build test') cp "$(cat fixture)" .partial.tar ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	metadata := filepath.Join(dir, "image.json")
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before := `{"apiVersion":"discobox.dev/image/v1","harness":{"id":"test","name":"Before"}}`
	after := `{"apiVersion":"discobox.dev/image/v1","harness":{"id":"test","name":"After"}}`
	write("image.json", before)
	write("fixture", harnessLayout(t, before))
	spec := imageSpec{name: "harness-test", bundle: true, devPrefix: "test:dev-", buildDir: dir, layoutDir: dir, files: []string{metadata}, metadataFile: metadata, metadataArg: "HARNESS_METADATA", buildArgs: []string{"build", "test", "--build-arg", "HARNESS_METADATA="}}
	// Only a bound on a hung watcher: a build that still misses its deadline
	// is retried after imageBuildRetryInterval, and the waits below allow that.
	// The stub's stall must outlast it, or a presence check the deadline never
	// cut off would end on its own and pass as a recovered one.
	ctx, cancel := context.WithTimeout(t.Context(), 4*imageBuildRetryInterval)
	ticks, presence := make(chan time.Time), make(chan time.Time)
	done := make(chan error, 1)
	base := imageSpec{name: "base", baseImage: "base:local", devPrefix: "base:dev-", buildDir: dir, files: []string{metadata}, buildArgs: []string{"build", "base"}}
	go func() { done <- watchImages(ctx, dir, []imageSpec{spec, base}, ticks, presence, nil, nil) }()
	t.Cleanup(func() { cancel(); <-done })
	send := func(ch chan time.Time) {
		t.Helper()
		select {
		case ch <- time.Now():
		case <-ctx.Done():
			t.Fatal("watcher stopped processing ticks")
		}
	}
	manifest := func() (devimage.Manifest, error) {
		return devimage.Read(filepath.Join(dir, developmentImageManifestFile))
	}
	checks := func() int {
		data, _ := os.ReadFile(filepath.Join(dir, "checks"))
		return strings.Count(string(data), "check\n")
	}
	// wait sends on ch until condition holds, and says whether it did: the
	// watcher, not the clock, says when it has done the work.
	wait := func(ch chan time.Time, condition func() bool) bool {
		t.Helper()
		beat := time.NewTicker(10 * time.Millisecond)
		defer beat.Stop()
		for !condition() {
			select {
			case <-ctx.Done():
				return false
			case <-beat.C:
				send(ch)
			}
		}
		return true
	}
	var initial devimage.Manifest
	if !wait(ticks, func() bool {
		var err error
		initial, err = manifest()
		return err == nil
	}) {
		t.Fatal("watcher did not publish the initial images")
	}
	// A presence check is dropped while another runs, and one cut off before
	// it marks the stall is simply retried, so keep asking until it stalls.
	if !wait(presence, func() bool {
		_, err := os.Stat(filepath.Join(dir, "stalled"))
		return err == nil
	}) {
		t.Fatal("watcher did not stall inspecting presence")
	}
	// Every check logs itself before it can stall, so this includes the stalled one.
	stalledChecks := checks()
	// Edit the declaration while Docker presence inspection is still stalled.
	write("fixture", harnessLayout(t, after))
	write("image.json", after)
	updated := initial
	if !wait(ticks, func() bool {
		if read, err := manifest(); err == nil {
			updated = read
		}
		return updated.Bundles[0].Reference != initial.Bundles[0].Reference
	}) {
		t.Fatalf("changed harness reused stale layout reference %s", updated.Bundles[0].Reference)
	}
	if _, err := os.Stat(initial.Bundles[0].Layout); err != nil {
		t.Fatalf("previous layout was not retained: %v", err)
	}
	// The timed-out inspection must release its slot so a later tick can retry.
	if !wait(presence, func() bool { return checks() > stalledChecks }) {
		t.Fatal("watcher did not retry the stalled presence check")
	}
	// A further check starts only once the retry's result has been handled,
	// and the tick after it only once any rebuild that result asked for ran.
	if !wait(presence, func() bool { return checks() > stalledChecks+1 }) {
		t.Fatal("watcher did not finish the retried presence check")
	}
	send(ticks)
	final, err := manifest()
	if err != nil {
		t.Fatal(err)
	}
	if final.Bundles[0].Reference != updated.Bundles[0].Reference {
		t.Fatal("successful retry changed the published harness")
	}
}

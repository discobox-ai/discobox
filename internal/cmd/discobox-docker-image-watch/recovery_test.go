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
	old := *dockerCommandTimeout
	*dockerCommandTimeout = 100 * time.Millisecond
	t.Cleanup(func() { *dockerCommandTimeout = old })
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1 $2" in
 'image inspect') echo sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef ;;
 'image ls')
   echo check >> checks
   if test ! -f stalled; then touch stalled; exec sleep 60; fi
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
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
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
	send(ticks) // initial publication completed
	initial, err := manifest()
	if err != nil {
		t.Fatal(err)
	}
	send(presence)
	wait := func(ch chan time.Time, condition func() bool) {
		t.Helper()
		beat := time.NewTicker(10 * time.Millisecond)
		defer beat.Stop()
		for !condition() {
			select {
			case <-ctx.Done():
				t.Fatal("watcher did not recover")
			case <-beat.C:
				send(ch)
			}
		}
	}
	wait(ticks, func() bool { _, err := os.Stat(filepath.Join(dir, "stalled")); return err == nil })
	// Edit the declaration while Docker presence inspection is still stalled.
	write("fixture", harnessLayout(t, after))
	write("image.json", after)
	send(ticks)
	send(ticks) // rebuild and publication completed
	updated, err := manifest()
	if err != nil {
		t.Fatal(err)
	}
	if initial.Bundles[0].Reference == updated.Bundles[0].Reference {
		t.Fatal("changed harness reused stale layout reference")
	}
	if _, err := os.Stat(initial.Bundles[0].Layout); err != nil {
		t.Fatalf("previous layout was not retained: %v", err)
	}
	// The timed-out inspection must release its slot so a later tick can retry.
	wait(presence, func() bool {
		data, _ := os.ReadFile(filepath.Join(dir, "checks"))
		return len(data) >= len("check\ncheck\n")
	})
	send(ticks)
	final, err := manifest()
	if err != nil {
		t.Fatal(err)
	}
	if final.Bundles[0].Reference != updated.Bundles[0].Reference {
		t.Fatal("successful retry changed the published harness")
	}
}

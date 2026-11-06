package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestWatcherProcessesChangesWhilePresenceCheckStalls(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script on PATH")
	}
	t.Setenv(buildModeEnv, "false")
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1 $2" in
 'image ls') exec sleep 60 ;;
 'image inspect') echo sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	input := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(input, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	specs := []imageSpec{{name: "test", baseImage: "test:local", devPrefix: "test:dev-", buildDir: dir,
		buildArgs: []string{"build", "test"}, files: []string{input}, envImageKey: "TEST_IMAGE"}}
	ctx, cancel := context.WithCancel(t.Context())
	ticks, presence := make(chan time.Time), make(chan time.Time)
	done := make(chan error, 1)
	go func() { done <- watchImages(ctx, dir, specs, ticks, presence, nil, nil) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case presence <- time.Now(): // Initial build and publication have completed.
	case <-time.After(10 * time.Second):
		t.Fatal("initial build did not complete")
	}
	if err := os.WriteFile(input, []byte("changed input"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case ticks <- time.Now():
	case <-time.After(3 * time.Second):
		t.Fatal("watcher stopped accepting file-change ticks while docker image ls stalled")
	}
}

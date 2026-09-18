package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A file created after the watcher started — a new Go file in a package an
// image builds — is an input it could not stat, because it was not on the
// list. Discovering the inputs again is what puts it there.
func TestWatcherRebuildsWhenItsInputsGrow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script on PATH")
	}
	t.Setenv(buildModeEnv, "false")
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1 $2" in
  'build test') echo build >> calls ;;
  'image inspect') echo sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef ;;
  'image ls') echo test:local ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	dockerfile := filepath.Join(dir, "Dockerfile")
	added := filepath.Join(dir, "added.go")
	for _, file := range []string{dockerfile, added} {
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	spec := func(files ...string) imageSpec {
		return imageSpec{name: "test", baseImage: "test:local", devPrefix: "test:dev-",
			buildDir: dir, buildArgs: []string{"build", "test"}, files: files, envImageKey: "TEST_IMAGE"}
	}
	discovered := []imageSpec{spec(dockerfile)}
	rediscover := func(context.Context) ([]imageSpec, error) { return discovered, nil }

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	ticks := make(chan time.Time)
	presence := make(chan time.Time)
	discovery := make(chan time.Time)
	done := make(chan error, 1)
	go func() {
		done <- watchImages(ctx, dir, []imageSpec{spec(dockerfile)}, ticks, presence, discovery, rediscover)
	}()
	t.Cleanup(func() { cancel(); <-done })
	send := func(ch chan time.Time) {
		t.Helper()
		select {
		case ch <- time.Now():
		case <-ctx.Done():
			t.Fatal("watcher did not accept tick")
		}
	}
	builds := func() int { return strings.Count(readFile(t, filepath.Join(dir, "calls")), "build\n") }

	send(ticks) // the initial build
	send(discovery)
	send(ticks)
	if got := builds(); got != 1 {
		t.Fatalf("builds after discovering the same inputs = %d, want 1", got)
	}

	discovered = []imageSpec{spec(dockerfile, added)}
	send(discovery)
	send(ticks)
	if got := builds(); got != 2 {
		t.Fatalf("builds after an input was discovered = %d, want 2", got)
	}
}

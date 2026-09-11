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

func TestWatcherRetriesWithoutFileChanges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script on PATH")
	}
	for _, scenario := range []string{"rebuild", "initial build", "publication", "missing image"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv(buildModeEnv, "false")
			dir := t.TempDir()
			script := `#!/bin/sh
case "$1 $2" in
  'build test')
    echo build >> calls
    if test -f fail; then rm fail; exit 1; fi
    ;;
  'image inspect') echo sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef ;;
  'image ls')
    if test -f missing; then rm missing; else echo test:local; fi
    ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			write := func(name, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write("Dockerfile", "before")
			if scenario == "initial build" {
				write("fail", "")
			}
			specs := []imageSpec{{name: "test", baseImage: "test:local", devPrefix: "test:dev-",
				buildDir: dir, buildArgs: []string{"build", "test"}, files: []string{filepath.Join(dir, "Dockerfile")}, envImageKey: "TEST_IMAGE"}}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			ticks := make(chan time.Time)
			presence := make(chan time.Time)
			done := make(chan error, 1)
			go func() { done <- watchImages(ctx, dir, specs, ticks, presence) }()
			t.Cleanup(func() { cancel(); <-done })
			now := time.Now()
			tick := func(ch chan time.Time, elapsed time.Duration) {
				t.Helper()
				select {
				case ch <- now.Add(elapsed):
				case <-ctx.Done():
					t.Fatal("watcher did not accept tick")
				}
			}
			// Each unbuffered send waits for the previous pass to finish.
			tick(ticks, 0)
			switch scenario {
			case "rebuild":
				write("fail", "")
				write("Dockerfile", "changed input")
			case "publication":
				if err := os.Remove(filepath.Join(dir, envFile)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(dir, envFile), 0o700); err != nil {
					t.Fatal(err)
				}
				write("Dockerfile", "changed input")
			case "missing image":
				write("fail", "")
				write("missing", "")
				tick(presence, time.Second)
			}
			tick(ticks, 2*time.Second)
			// Older :local images exist, so presence alone will not retry.
			tick(presence, 3*time.Second)
			tick(ticks, 4*time.Second)
			wantBeforeRetry := 2
			if scenario == "initial build" {
				wantBeforeRetry = 1
			}
			if got := strings.Count(readFile(t, filepath.Join(dir, "calls")), "build\n"); got != wantBeforeRetry {
				t.Fatalf("build attempts before retry delay = %d, want %d", got, wantBeforeRetry)
			}
			if scenario == "publication" {
				if err := os.Remove(filepath.Join(dir, envFile)); err != nil {
					t.Fatal(err)
				}
			}
			tick(ticks, time.Minute)
			tick(ticks, 2*time.Minute)
			tick(ticks, 3*time.Minute)
			if got := strings.Count(readFile(t, filepath.Join(dir, "calls")), "build\n"); got != wantBeforeRetry+1 {
				t.Fatalf("build attempts = %d, want %d after one successful retry", got, wantBeforeRetry+1)
			}
			if env := readFile(t, filepath.Join(dir, envFile)); !strings.Contains(env, "TEST_IMAGE=test:dev-0123456789ab") {
				t.Fatalf("retry did not publish image environment: %s", env)
			}
		})
	}
}

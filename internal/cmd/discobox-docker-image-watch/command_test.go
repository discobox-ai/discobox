package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDockerCommandsHaveDeadlines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script on PATH")
	}
	oldProbe, oldBuild := *dockerCommandTimeout, *buildCommandTimeout
	*dockerCommandTimeout, *buildCommandTimeout = 50*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { *dockerCommandTimeout, *buildCommandTimeout = oldProbe, oldBuild })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexec sleep 60\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, args := range [][]string{{"image", "ls"}, {"info"}, {"context", "show"}, {"tag", "a", "b"}, {"build", "."}, {"buildx", "build", "."}} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			for _, output := range []bool{false, true} {
				start := time.Now()
				var err error
				if output {
					_, err = commandOutput(t.Context(), dir, "docker", args...)
				} else {
					err = runCommand(t.Context(), dir, "docker", args...)
				}
				if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "docker "+strings.Join(args, " ")) || !strings.Contains(err.Error(), "deadline") {
					t.Fatalf("command failure = %v", err)
				}
				if time.Since(start) > 3*time.Second {
					t.Fatalf("command did not stop promptly: %v", time.Since(start))
				}
			}
		})
	}
}

// Docker runs buildx as a child rather than execing it. Killing only the CLI
// would let that exporter overwrite the next attempt's partial layout.
func TestDeadlineStopsDescendantExporter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script on PATH")
	}
	old := *buildCommandTimeout
	*buildCommandTimeout = 100 * time.Millisecond
	t.Cleanup(func() { *buildCommandTimeout = old })
	for _, output := range []bool{false, true} {
		dir := t.TempDir()
		script := "#!/bin/sh\n(sleep 0.4; echo stale > escaped) &\nwait\n"
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		var err error
		if output {
			_, err = commandOutput(t.Context(), dir, "docker", "build", ".")
		} else {
			err = runCommand(t.Context(), dir, "docker", "build", ".")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("command error = %v", err)
		}
		// Let a surviving exporter reach its write, without keeping a test child
		// alive indefinitely when this regression is reproduced.
		timer := time.NewTimer(500 * time.Millisecond)
		<-timer.C
		if _, err := os.Stat(filepath.Join(dir, "escaped")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("descendant exporter survived deadline: %v", err)
		}
	}
}

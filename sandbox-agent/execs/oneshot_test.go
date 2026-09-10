//go:build !windows

package execs

import (
	"context"
	"testing"
	"time"
)

func TestOneShotBoundsOutputAndCancellation(t *testing.T) {
	manager, err := NewManagerWithConfig(ManagerConfig{WorkingRoot: t.TempDir(), RuntimeDir: t.TempDir(), Units: &fakeUnitManager{}})
	if err != nil {
		t.Fatal(err)
	}
	output, err := manager.RunOneShot(context.Background(), []string{"/bin/sh", "-c", `printf '%s' "$JUDGE_TEST_VALUE"`}, map[string]string{"JUDGE_TEST_VALUE": "associated"}, 100)
	if err != nil || string(output) != "associated" {
		t.Fatalf("output=%q err=%v", output, err)
	}
	for _, script := range []string{`while :; do printf 'overflow'; done`, `while :; do printf 'overflow' >&2; done`, `sleep 30 & wait`} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		start := time.Now()
		_, err := manager.RunOneShot(ctx, []string{"/bin/sh", "-c", script}, nil, 32)
		cancel()
		if err == nil {
			t.Fatal("unbounded process succeeded")
		}
		if time.Since(start) > 3*time.Second {
			t.Fatal("process outlived cancellation")
		}
	}
}

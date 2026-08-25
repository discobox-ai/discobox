package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestShowServerLogPrintsTheWholeLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(path, []byte("first\nsecond\nthird\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := showServerLog(context.Background(), &out, path, 0, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "first\nsecond\nthird\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestShowServerLogTailsTheLastLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(path, []byte("first\nsecond\nthird\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := showServerLog(context.Background(), &out, path, 2, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "second\nthird\n" {
		t.Fatalf("output = %q, want the last two lines", out.String())
	}
}

// A log nobody has written is not an empty file to print: nothing has started a
// server here, and saying so is the answer.
func TestShowServerLogSaysWhenThereIsNoLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	err := showServerLog(context.Background(), &bytes.Buffer{}, path, 0, false)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not say where it looked", err)
	}
}

// Following prints what arrives after the command started, and stops when the
// caller does.
func TestShowServerLogFollowsNewOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- showServerLog(ctx, out, path, 0, true) }()

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("after\n"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "after") {
		if time.Now().After(deadline) {
			t.Fatalf("followed output = %q, want the appended line", out.String())
		}
		time.Sleep(followInterval / 3)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

package processhelper

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if handled, code := HandleEntry(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func TestHelperTerminatesChildWhenParentInputCloses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses sh")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "late-marker")
	cmd, err := CommandContext(context.Background(), CommandOptions{
		Command: "sh",
		Args:    []string{"-c", "(sleep 1; echo late > \"$1\") & while read line; do :; done; sleep 30", "sh", marker},
		Grace:   200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	if _, err := stdin.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		_ = err
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("helper did not exit after parent stdin closed")
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("child process group was not killed; marker stat err=%v", err)
	}
}

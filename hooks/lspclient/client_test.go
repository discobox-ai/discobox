package lspclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestDidSaveSendsDocumentURIAndText(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()

	client := &Client{stdin: write, repoRoot: dir}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.DidSave(ctx, "main.go"); err != nil {
		t.Fatalf("did save: %v", err)
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}

	body, err := readMessage(bufio.NewReader(read))
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	var got struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Text string `json:"text"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if got.JSONRPC != "2.0" || got.Method != "textDocument/didSave" {
		t.Fatalf("unexpected notification: %#v", got)
	}
	if got.Params.TextDocument.URI != FileURI(filepath.Join(dir, "main.go")) {
		t.Fatalf("unexpected uri %q", got.Params.TextDocument.URI)
	}
	if got.Params.Text != "package main\n" {
		t.Fatalf("unexpected text %q", got.Params.Text)
	}
}

func TestDidChangeWatchedFilesSendsWorkspaceNotification(t *testing.T) {
	dir := t.TempDir()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()

	client := &Client{stdin: write, repoRoot: dir}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.DidChangeWatchedFiles(ctx, []FileChange{
		{URI: FileURI(filepath.Join(dir, "go.mod")), Type: FileChanged},
		{URI: FileURI(filepath.Join(dir, "old.go")), Type: FileDeleted},
	}); err != nil {
		t.Fatalf("did change watched files: %v", err)
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}

	body, err := readMessage(bufio.NewReader(read))
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	var got struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Changes []struct {
				URI  string `json:"uri"`
				Type int    `json:"type"`
			} `json:"changes"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if got.JSONRPC != "2.0" || got.Method != "workspace/didChangeWatchedFiles" {
		t.Fatalf("unexpected notification: %#v", got)
	}
	if len(got.Params.Changes) != 2 {
		t.Fatalf("changes = %#v, want 2 entries", got.Params.Changes)
	}
	if got.Params.Changes[0].URI != FileURI(filepath.Join(dir, "go.mod")) || got.Params.Changes[0].Type != int(FileChanged) {
		t.Fatalf("unexpected first change: %#v", got.Params.Changes[0])
	}
	if got.Params.Changes[1].URI != FileURI(filepath.Join(dir, "old.go")) || got.Params.Changes[1].Type != int(FileDeleted) {
		t.Fatalf("unexpected second change: %#v", got.Params.Changes[1])
	}
}

func TestDocumentVersionsIncrementPerURI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()

	client := &Client{stdin: write, repoRoot: dir, language: "go"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.DidOpen(ctx, "main.go"); err != nil {
		t.Fatalf("did open: %v", err)
	}
	if err := os.WriteFile(path, []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := client.DidChange(ctx, "main.go"); err != nil {
		t.Fatalf("did change: %v", err)
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(read)
	first := decodeTestNotification(t, reader)
	second := decodeTestNotification(t, reader)
	if first.Method != "textDocument/didOpen" || first.Params.TextDocument.Version != 1 {
		t.Fatalf("unexpected first notification: %#v", first)
	}
	if second.Method != "textDocument/didChange" || second.Params.TextDocument.Version != 2 {
		t.Fatalf("unexpected second notification: %#v", second)
	}
}

func TestContextCancelKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process group cleanup test is Unix-only")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "late-marker")
	script := writeTestScript(t, dir, "lsp-wrapper.sh", `#!/bin/sh
(sleep 1; echo late > late-marker)&
echo started
sleep 5
`)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, script)
	cmd.Dir = dir
	configureCommandForCleanup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start command: %v", err)
	}
	cancel()
	_ = cmd.Wait()

	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("process group child was not killed; marker stat err=%v", err)
	}
}

type testNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		TextDocument struct {
			URI     string `json:"uri"`
			Version int    `json:"version"`
		} `json:"textDocument"`
		Text string `json:"text"`
	} `json:"params"`
}

func decodeTestNotification(t *testing.T, reader *bufio.Reader) testNotification {
	t.Helper()
	body, err := readMessage(reader)
	if err != nil {
		if errors.Is(err, io.EOF) {
			t.Fatal("expected notification, got EOF")
		}
		t.Fatalf("read message: %v", err)
	}
	var got testNotification
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	return got
}

func writeTestScript(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

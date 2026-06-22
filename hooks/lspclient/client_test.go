package lspclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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

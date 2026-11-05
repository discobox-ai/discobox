//go:build darwin || linux

package sandboxgit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNoninteractiveGitSuppressesAskpassAndPreservesInteractiveHelper(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "asked")
	helper := filepath.Join(dir, "askpass")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho asked >> '"+marker+"'\necho answer\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ASKPASS", helper)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	_, err := Output(WithNoninteractive(t.Context()), "", nil, nil, "-c", "credential.helper=", "ls-remote", server.URL)
	if err == nil {
		t.Fatal("unauthenticated remote succeeded")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("askpass ran noninteractively: %v", err)
	}
	_, _ = Output(t.Context(), "", nil, NoPromptEnv, "-c", "credential.helper=", "ls-remote", server.URL)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("interactive IDE helper was disabled: %v", err)
	}
}

func TestNoninteractiveSSHUsesBatchAndStrictHostKeys(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	ssh := filepath.Join(dir, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+log+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_COMMAND", ssh+" -oBatchMode=no -oStrictHostKeyChecking=ask")
	_, _ = Output(WithNoninteractive(t.Context()), "", nil, nil, "ls-remote", "ssh://forge.example/repo")
	args, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(args), "-oBatchMode=yes\n-oStrictHostKeyChecking=yes\n") {
		t.Fatalf("SSH policy: %s", args)
	}
}

func TestNoninteractiveCancellationStopsGitHelperTree(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	finished := filepath.Join(dir, "finished")
	helper := filepath.Join(dir, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho yes > '"+started+"'\nsleep 2\necho survived > '"+finished+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(WithNoninteractive(t.Context()))
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Output(ctx, "", []byte("protocol=https\nhost=forge.example\n\n"), nil, "-c", "credential.helper=", "-c", "credential.helper="+helper, "credential", "fill")
		done <- err
	}()
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("helper not started")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel succeeded")
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("Git cancellation hung")
	}
	time.Sleep(2100 * time.Millisecond)
	if _, err := os.Stat(finished); !os.IsNotExist(err) {
		t.Fatalf("helper survived cancellation: %v", err)
	}
}

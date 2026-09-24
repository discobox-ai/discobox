//go:build !windows

package endpoint

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/health"
)

// A server holding its first start on a choice is neither starting nor ready.
// Waiting will not end it, so EnsureRunning hands the question back rather
// than waiting out its deadline or starting a second server (ADR 0148 §3); the
// answer goes back to the same endpoint.
func TestEnsureRunningHandsBackAServerWaitingForAChoice(t *testing.T) {
	choice := health.Choice{Provider: "libkrun", Reason: health.ReasonKVMUnavailable, Detail: "open /dev/kvm: no such file or directory", Alternatives: []string{"docker"}}
	answers := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(health.Path, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(health.Status{Status: health.StatusNeedsChoice, Choice: &choice})
	})
	mux.HandleFunc(health.SetupDefaultProviderPath, func(w http.ResponseWriter, r *http.Request) {
		var body health.DefaultProviderChoice
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		answers <- body.Provider
		w.WriteHeader(http.StatusAccepted)
	})
	socket := testSocketPath(t)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	started, err := EnsureRunning(t.Context(), LaunchOptions{
		Endpoint:     "unix://" + socket,
		LockPath:     filepath.Join(t.TempDir(), "lock"),
		ReadyTimeout: time.Minute,
		Command: func(context.Context) (Command, error) {
			t.Error("a server that answered was started again")
			return Command{}, errors.New("unreachable")
		},
	})
	var required *ChoiceRequiredError
	if !errors.As(err, &required) {
		t.Fatalf("EnsureRunning() = %v, want a *ChoiceRequiredError", err)
	}
	if started {
		t.Fatal("EnsureRunning() reports starting a server it found running")
	}
	if required.Choice.Reason != health.ReasonKVMUnavailable || required.Choice.Detail != choice.Detail {
		t.Fatalf("choice = %+v, want the server's", required.Choice)
	}

	if err := ChooseDefaultProvider(t.Context(), "unix://"+socket, "docker"); err != nil {
		t.Fatal(err)
	}
	if got := <-answers; got != "docker" {
		t.Fatalf("the server was told %q, want docker", got)
	}
}

// A server that is not waiting has nothing to choose, and saying so is the
// answer — not whatever its router says about a path it does not serve.
func TestChooseDefaultProviderSaysWhenNothingIsWaiting(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(health.Path, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(health.Status{Status: health.StatusReady})
	})
	mux.HandleFunc(health.SetupDefaultProviderPath, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a choice was posted to a server that was not waiting")
		w.WriteHeader(http.StatusForbidden)
	})
	socket := testSocketPath(t)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	err = ChooseDefaultProvider(t.Context(), "unix://"+socket, "docker")
	if err == nil || !strings.Contains(err.Error(), "not waiting") || !strings.Contains(err.Error(), "ready") {
		t.Fatalf("ChooseDefaultProvider() = %v, want it to say the server is ready and not waiting", err)
	}
}

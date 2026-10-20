package provision

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/provisionspec"
)

func writeStatus(t *testing.T, path string, status provisionspec.Status) {
	t.Helper()
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Wait waits for the document to appear and for each feature to be done,
// says once what it waits for, and treats a feature the box does not
// provision as done.
func TestWait(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("provision.json is a Linux box's; Windows refuses to replace a file another reader has open")
	}
	path := filepath.Join(t.TempDir(), "provision.json")
	waitPoll = 5 * time.Millisecond
	done := make(chan provisionspec.Status, 1)
	var told []string
	go func() {
		status, err := Wait(context.Background(), path, []string{"docker", "make"}, func(status provisionspec.Status, pending []string) {
			told = append(told, Describe(status, pending))
		})
		if err != nil {
			t.Error(err)
		}
		done <- status
	}()
	time.Sleep(30 * time.Millisecond)
	writeStatus(t, path, provisionspec.Status{State: provisionspec.StateProvisioning, Features: map[string]provisionspec.FeatureStatus{
		"docker": {State: provisionspec.StateProvisioning, Step: "install", Log: "/var/log/discobox/provision/docker.log"},
	}})
	time.Sleep(30 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("returned while docker was provisioning")
	default:
	}
	writeStatus(t, path, provisionspec.Status{State: provisionspec.StateFailed, Features: map[string]provisionspec.FeatureStatus{
		"docker": {State: provisionspec.StateFailed, Error: "exit status 1", Log: "/var/log/discobox/provision/docker.log"},
	}})
	status := <-done
	if len(told) != 1 {
		t.Errorf("told %q", told)
	}
	if err := Problem(status, "docker"); err == nil || !strings.Contains(err.Error(), "exit status 1") || !strings.Contains(err.Error(), "docker.log") {
		t.Errorf("problem = %v", err)
	}
	if err := Problem(status, "make"); err != nil {
		t.Errorf("a feature the box does not provision: %v", err)
	}
	status.Features["brew"] = provisionspec.FeatureStatus{State: provisionspec.StateOff, Reason: "turned off in .discobox/settings.json"}
	if err := Problem(status, "brew"); err == nil || !strings.Contains(err.Error(), "do not install it by hand") {
		t.Errorf("problem = %v", err)
	}
}

// A placeholder finds the real command past itself.
func TestLookPathSkipsThePlaceholders(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("placeholders are a Linux box's: they are found by executable bit, which Windows has none of")
	}
	dir := t.TempDir()
	brew := filepath.Join(dir, "bin", "brew")
	if err := os.MkdirAll(filepath.Dir(brew), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brew, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := LookPath("brew", PlaceholderDir+string(os.PathListSeparator)+filepath.Dir(brew))
	if err != nil || got != brew {
		t.Errorf("LookPath = %q, %v", got, err)
	}
	if _, err := LookPath("brew", PlaceholderDir); err == nil {
		t.Error("found the placeholder itself")
	}
}

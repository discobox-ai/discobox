package dockerworker

import (
	"testing"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// The pool agent runs the sandbox base image for every sandbox delivered its
// platform (ADR 26-10-16-423 §§7, 10), so the image travels in its
// environment, and a moved one recreates the pool container that would
// otherwise run the old one.
func TestTheSandboxBaseImageReachesThePoolAgent(t *testing.T) {
	engine, err := New(Config{SandboxBaseImage: "base:v1"}, nopDriver{})
	if err != nil {
		t.Fatal(err)
	}
	env := engine.poolPolicyEnv()
	if got := env[sandboxruntime.EnvSandboxBaseImage]; got != "base:v1" {
		t.Fatalf("%s = %q, want base:v1", sandboxruntime.EnvSandboxBaseImage, got)
	}
	moved, err := New(Config{SandboxBaseImage: "base:v2"}, nopDriver{})
	if err != nil {
		t.Fatal(err)
	}
	if moved.ConfigRevision() == engine.ConfigRevision() {
		t.Fatal("a moved sandbox base image left the pool container's revision as it was")
	}
}

package registry

import "testing"

func TestResolveSupportedID(t *testing.T) {
	got, err := ResolveSupportedID("codex")
	if err != nil {
		t.Fatal(err)
	}
	if got != "codex-acp" {
		t.Fatalf("got %q, want codex-acp", got)
	}
	if _, err := ResolveSupportedID("opencode"); err == nil {
		t.Fatal("expected unsupported agent error")
	}
}

func TestFindAgentRequiresSupportedID(t *testing.T) {
	reg := &Registry{Agents: []Agent{{ID: "codex-acp"}, {ID: "opencode"}}}
	if _, err := reg.FindAgent("codex-acp"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.FindAgent("opencode"); err == nil {
		t.Fatal("expected unsupported agent error")
	}
}

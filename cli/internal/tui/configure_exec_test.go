package tui

import (
	"bytes"
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestConfigureExecRunsConfigureHarness(t *testing.T) {
	f := &fakeSource{}
	c := &configureExec{ctx: context.Background(), ds: f, harnessID: "hc_a"}
	c.SetStdin(strings.NewReader(""))
	c.SetStdout(&bytes.Buffer{})
	c.SetStderr(&bytes.Buffer{})
	if err := c.Run(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if ids := f.configuredIDs(); len(ids) != 1 || ids[0] != "hc_a" {
		t.Fatalf("configured = %v, want [hc_a]", ids)
	}
}

func TestConfigureExecPropagatesError(t *testing.T) {
	f := &fakeSource{configureErr: errBoom}
	c := &configureExec{ctx: context.Background(), ds: f, harnessID: "hc_a"}
	c.SetStdin(strings.NewReader(""))
	c.SetStdout(&bytes.Buffer{})
	c.SetStderr(&bytes.Buffer{})
	if err := c.Run(); err == nil {
		t.Fatal("Run() should surface the configure error")
	}
}

func TestRootHarnessConfiguredSetsStatus(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(1), configs: makeConfigs()}
	m := New(context.Background(), f)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(openHarnessesMsg{})

	m.Update(harnessConfiguredMsg{name: "codex"})
	if m.statusError || !strings.Contains(m.statusText, "configured codex") {
		t.Fatalf("status = %q (err=%v), want configured codex", m.statusText, m.statusError)
	}

	m.Update(harnessConfiguredMsg{name: "codex", err: errBoom})
	if !m.statusError || !strings.Contains(m.statusText, "failed") {
		t.Fatalf("status = %q (err=%v), want a failure", m.statusText, m.statusError)
	}
}

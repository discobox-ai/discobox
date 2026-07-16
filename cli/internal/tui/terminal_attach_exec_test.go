package tui

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestTerminalAttachExecRunsAttachTerminal(t *testing.T) {
	f := &fakeSource{}
	c := &terminalAttachExec{ctx: context.Background(), ds: f, sandboxID: "sbx_a"}
	c.SetStdin(strings.NewReader(""))
	c.SetStdout(&bytes.Buffer{})
	c.SetStderr(&bytes.Buffer{})
	if err := c.Run(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if ids := f.attachedIDs(); len(ids) != 1 || ids[0] != "sbx_a" {
		t.Fatalf("attached = %v, want [sbx_a]", ids)
	}
}

func TestTerminalAttachExecPropagatesError(t *testing.T) {
	f := &fakeSource{attachErr: errBoom}
	c := &terminalAttachExec{ctx: context.Background(), ds: f, sandboxID: "sbx_a"}
	c.SetStdin(strings.NewReader(""))
	c.SetStdout(&bytes.Buffer{})
	c.SetStderr(&bytes.Buffer{})
	if err := c.Run(); err == nil {
		t.Fatal("Run() should surface the attach error")
	}
}

func TestRootFullscreenFinishedSetsStatus(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(1)}
	m := New(context.Background(), f)
	m.Update(fullscreenFinishedMsg{sandbox: f.sandboxes[0]})
	if m.statusError || !strings.Contains(m.statusText, "detached") {
		t.Fatalf("status = %q (err=%v), want detached", m.statusText, m.statusError)
	}

	m.Update(fullscreenFinishedMsg{sandbox: f.sandboxes[0], err: errBoom})
	if !m.statusError || !strings.Contains(m.statusText, "failed") {
		t.Fatalf("status = %q (err=%v), want a failure", m.statusText, m.statusError)
	}
}

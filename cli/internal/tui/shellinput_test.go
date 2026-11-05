package tui

import (
	"testing"
	"testing/synctest"
)

func TestShellInputRemainsObservableAfterDuplicateAttach(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, _ := openWorkspace(t, ds, "enter")

		// Creation publishes the exec before its completion reaches the model.
		created := m.newShell()().(workspaceTermMsg)
		// A listing can discover it and start another attach in that interval.
		duplicate := m.openExec(m.wsGen, created.exec, 40)().(workspaceTermMsg)
		// The original completion wins; the redundant attach is closed.
		d.dispatch(created)
		d.dispatch(duplicate)
		d.settle()

		d.key("ctrl+v")
		actual := created.term.(*fakeTerminal)
		d.wait("Ctrl-V on the live shell", func() bool { return actual.inputSoFar() == "\x16" })
		if got := ds.execTerm(created.exec.ID).inputSoFar(); got != "\x16" {
			t.Fatalf("execTerm observed %q, want Ctrl-V already delivered to the live shell", got)
		}
	})
}

func TestShellInputRemainsObservableWhenListingAttachWins(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, _ := openWorkspace(t, ds, "enter")

		// Creation publishes the exec before its completion reaches the model.
		created := m.newShell()().(workspaceTermMsg)
		// A listing can discover it and start another attach in that interval.
		duplicate := m.openExec(m.wsGen, created.exec, 40)().(workspaceTermMsg)
		// The listing completion wins; the original attach is closed.
		d.dispatch(duplicate)
		d.dispatch(created)
		d.settle()

		d.key("ctrl+v")
		actual := duplicate.term.(*fakeTerminal)
		d.wait("Ctrl-V on the live shell", func() bool { return actual.inputSoFar() == "\x16" })
		if got := ds.execTerm(created.exec.ID).inputSoFar(); got != "\x16" {
			t.Fatalf("execTerm observed %q, want Ctrl-V already delivered to the live shell", got)
		}
	})
}

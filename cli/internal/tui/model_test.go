package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestRootRoutesEnterToTerminalAndBack(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(2)}
	m := New(context.Background(), f)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.list.applySandboxes(f.sandboxes)

	// Entering a sandbox switches the active screen to the terminal pane.
	m.Update(selectSandboxMsg{sandbox: f.sandboxes[0]})
	if _, ok := m.active.(*terminalScreen); !ok {
		t.Fatalf("active screen = %T, want *terminalScreen", m.active)
	}
	if m.terminal == nil {
		t.Fatal("terminal screen not retained")
	}

	// Going back returns to the list and drops the terminal.
	m.Update(backMsg{})
	if _, ok := m.active.(*sandboxesScreen); !ok {
		t.Fatalf("active screen after back = %T, want *sandboxesScreen", m.active)
	}
	if m.terminal != nil {
		t.Fatal("terminal not released on back")
	}
}

func TestRootOpensFormAndCreates(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(2), harnesses: []Harness{{Name: "codex", Label: "codex", Default: true}}, defaultPath: "/work"}
	m := New(context.Background(), f)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.list.applySandboxes(f.sandboxes)

	// Opening the form switches the active screen.
	m.Update(openNewMsg{})
	if _, ok := m.active.(*newSessionScreen); !ok {
		t.Fatalf("active = %T, want *newSessionScreen", m.active)
	}

	// A created session returns to the list and queues the new row for selection.
	m.Update(sessionCreatedMsg{sandbox: Sandbox{ID: "sbx_new", Name: "new-one"}})
	if _, ok := m.active.(*sandboxesScreen); !ok {
		t.Fatalf("active after create = %T, want *sandboxesScreen", m.active)
	}
	if m.list.selectID != "sbx_new" {
		t.Fatalf("selectID = %q, want sbx_new", m.list.selectID)
	}
	if m.form != nil {
		t.Fatal("form not released after create")
	}
}

func TestRootRoutesToCodingAgentsAndBack(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(1), configs: makeConfigs()}
	m := New(context.Background(), f)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// `a` from the sandbox list opens the coding-agents dialog.
	m.Update(openHarnessesMsg{})
	if _, ok := m.active.(*harnessesScreen); !ok {
		t.Fatalf("active = %T, want *harnessesScreen", m.active)
	}

	// Opening the form, then saving, returns to the coding-agents dialog.
	m.Update(openHarnessFormMsg{})
	if _, ok := m.active.(*harnessFormScreen); !ok {
		t.Fatalf("active = %T, want *harnessFormScreen", m.active)
	}
	m.Update(harnessSavedMsg{config: HarnessConfig{ID: "hc_new"}, created: true})
	if _, ok := m.active.(*harnessesScreen); !ok {
		t.Fatalf("active after save = %T, want *harnessesScreen", m.active)
	}
	if m.harnessForm != nil {
		t.Fatal("harness form not released after save")
	}
	if m.harnesses.selectID != "hc_new" {
		t.Fatalf("selectID = %q, want hc_new", m.harnesses.selectID)
	}

	// Esc (backMsg) from the coding-agents dialog returns to the sandbox list.
	m.Update(backMsg{})
	if _, ok := m.active.(*sandboxesScreen); !ok {
		t.Fatalf("active after back = %T, want *sandboxesScreen", m.active)
	}
}

func TestTabBarFocusSwitchAndEnter(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(2), configs: makeConfigs()}
	m := New(context.Background(), f)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.list.applySandboxes(f.sandboxes)

	// Up from the "new" selector (the sandbox list's default focus) surfaces
	// focus to the tab bar.
	_, cmd := m.Update(keyPress("up"))
	if _, ok := runCmd(cmd).(focusTabsMsg); !ok {
		t.Fatal("Up on the sandbox list should emit focusTabsMsg")
	}
	m.Update(focusTabsMsg{})
	if !m.tabFocused {
		t.Fatal("tab bar should hold focus after focusTabsMsg")
	}

	// h/l move between tabs while keeping bar focus; the body follows.
	m.Update(keyPress("right"))
	if m.activeTab != tabAgents || !m.tabFocused {
		t.Fatalf("right: activeTab=%d focused=%v, want agents+focused", m.activeTab, m.tabFocused)
	}
	if _, ok := m.active.(*harnessesScreen); !ok {
		t.Fatalf("active = %T, want *harnessesScreen", m.active)
	}
	m.Update(keyPress("right"))
	if m.activeTab != tabSecrets {
		t.Fatalf("right: activeTab=%d, want secrets", m.activeTab)
	}
	if _, ok := m.active.(*secretsScreen); !ok {
		t.Fatalf("active = %T, want *secretsScreen", m.active)
	}
	// The last tab clamps: another right stays on secrets.
	m.Update(keyPress("right"))
	if m.activeTab != tabSecrets {
		t.Fatalf("right past last tab: activeTab=%d, want secrets", m.activeTab)
	}

	// Down drops focus into the active tab's body.
	m.Update(keyPress("down"))
	if m.tabFocused {
		t.Fatal("Down from the tab bar should drop focus into the body")
	}
	if _, ok := m.active.(*secretsScreen); !ok {
		t.Fatalf("active after entering body = %T, want *secretsScreen", m.active)
	}

	// Up from a placeholder tab body returns focus to the bar; h walks back left.
	_, cmd = m.Update(keyPress("up"))
	if _, ok := runCmd(cmd).(focusTabsMsg); !ok {
		t.Fatal("Up on the secrets tab should emit focusTabsMsg")
	}
	m.Update(focusTabsMsg{})
	m.Update(keyPress("h"))
	if m.activeTab != tabAgents {
		t.Fatalf("h: activeTab=%d, want agents", m.activeTab)
	}
}

func TestTabBarRendersAllTabs(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(1)}
	m := New(context.Background(), f)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.list.applySandboxes(f.sandboxes)

	view := m.View().Content
	for _, tab := range []string{"sandboxes", "agents", "secrets"} {
		if !strings.Contains(view, tab) {
			t.Fatalf("tab %q missing from header:\n%s", tab, view)
		}
	}
}

func TestRootSurfacesErrorStatus(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(1)}
	m := New(context.Background(), f)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	m.Update(errMsg{context: "list", err: errBoom})
	if !m.statusError {
		t.Fatal("statusError not set")
	}
	if !strings.Contains(m.statusText, "boom") {
		t.Fatalf("statusText = %q, want it to mention the error", m.statusText)
	}

	// A successful reload clears the error banner.
	m.Update(sandboxesLoadedMsg{sandboxes: f.sandboxes})
	if m.statusError {
		t.Fatal("statusError not cleared after successful load")
	}
}

func TestRootViewComposesChrome(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(2)}
	m := New(context.Background(), f)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.list.applySandboxes(f.sandboxes)

	view := m.View()
	if !view.AltScreen {
		t.Fatal("root view should use the alternate screen")
	}
	if !strings.Contains(view.Content, "discobox") {
		t.Fatal("header title missing from view")
	}
	if !strings.Contains(view.Content, "sandboxes") {
		t.Fatal("header context missing from view")
	}
}

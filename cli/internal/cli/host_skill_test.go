package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/hostskill"
	"github.com/discobox-ai/discobox/pkg/version"
)

func TestHostSkillNamesCommandsInTheCLI(t *testing.T) {
	content := hostskill.Content()
	if !hostskill.Marked([]byte(content)) || !strings.Contains(content, `version: "`+version.String()+`"`) {
		t.Fatalf("missing host marker or client version in skill:\n%s", content)
	}
	root, _ := newRootCommand(false)
	lines := regexp.MustCompile("`discobox ([^`]+)`").FindAllStringSubmatch(content, -1)
	if len(lines) == 0 {
		t.Fatal("no CLI commands found in the skill")
	}
	for _, line := range lines {
		beforeArgs := strings.FieldsFunc(line[1], func(r rune) bool { return r == '-' || r == '<' })
		fields := strings.Fields(beforeArgs[0])
		command, remaining, err := root.Find(fields)
		if err != nil || len(remaining) != 0 || command == root {
			t.Errorf("%s does not resolve to a CLI command: %v, leftover %v", line[0], err, remaining)
		}
	}
}

func runLocalSkillCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root, _ := newRootCommand(false)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(args)
	err := root.Execute()
	return output.String(), err
}

func TestHostSkillPrintAndManage(t *testing.T) {
	uninstallHome(t)
	parent := t.TempDir()
	path := filepath.Join(parent, hostskill.Name)
	if out, err := runLocalSkillCommand(t, "skill"); err != nil || out != hostskill.Content() {
		t.Fatalf("print skill: %v, %q", err, out)
	}
	for _, flags := range [][]string{
		{"--scope", "user"}, {"--dir", parent}, {"--install", "--uninstall"},
		{"--install", "--scope", "user", "--dir", parent}, {"--install", "--scope", "invalid"},
	} {
		if _, err := runLocalSkillCommand(t, append([]string{"skill"}, flags...)...); err == nil {
			t.Errorf("accepted invalid flags %v", flags)
		}
	}
	if out, err := runLocalSkillCommand(t, "skill", "--install", "--dir", parent); err != nil || !strings.Contains(out, path) {
		t.Fatalf("install: %v, %q", err, out)
	}
	// Simulate a hard stop after the previous directory was moved away.
	previous := filepath.Join(parent, hostSkillPrevious)
	if err := os.Rename(path, previous); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(previous, "stale")
	if err := os.WriteFile(stale, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runLocalSkillCommand(t, "skill", "--install", "--dir", parent); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(previous); !os.IsNotExist(err) {
		t.Fatalf("reinstall left the previous tree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "stale")); !os.IsNotExist(err) {
		t.Fatalf("reinstall left the stale file: %v", err)
	}
	if out, err := runLocalSkillCommand(t, "skill", "--uninstall", "--dir", parent); err != nil || !strings.Contains(out, path) {
		t.Fatalf("uninstall: %v, %q", err, out)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--install", "--uninstall"} {
		if _, err := runLocalSkillCommand(t, "skill", flag, "--dir", parent); err == nil {
			t.Errorf("%s accepted an unmanaged directory", flag)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unmanaged directory was changed: %v", err)
	}
}

func TestHostSkillProjectScopeUsesWorktreeRoot(t *testing.T) {
	uninstallHome(t)
	root := t.TempDir()
	sub := filepath.Join(root, "nested")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	if _, err := runLocalSkillCommand(t, "skill", "--install", "--scope", "project"); err == nil {
		t.Fatal("accepted --scope project outside a worktree")
	}
	if out, err := exec.CommandContext(t.Context(), "git", "init", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if _, err := runLocalSkillCommand(t, "skill", "--install", "--scope", "project"); err != nil {
		t.Fatal(err)
	}
	for _, parent := range hostskill.Directories(root) {
		content, err := os.ReadFile(filepath.Join(parent, hostskill.Name, "SKILL.md"))
		if err != nil || !hostskill.Marked(content) {
			t.Fatalf("missing installed skill in %s: %v", parent, err)
		}
	}
}

func TestHostSkillProjectScopeConfinesLinkedDirectories(t *testing.T) {
	uninstallHome(t)
	root := t.TempDir()
	t.Chdir(root)
	if out, err := exec.CommandContext(t.Context(), "git", "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	outside := t.TempDir()
	link := filepath.Join(root, ".claude", "skills")
	if err := os.Mkdir(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := runLocalSkillCommand(t, "skill", "--install", "--scope", "project"); err == nil {
		t.Fatal("project install accepted a link outside the worktree")
	}
	if _, err := os.Stat(filepath.Join(outside, hostskill.Name)); !os.IsNotExist(err) {
		t.Fatalf("project install wrote outside the worktree: %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", ".agents", "skills"), link); err != nil {
		t.Fatal(err)
	}
	if out, err := runLocalSkillCommand(t, "skill", "--install", "--scope", "project"); err != nil || strings.Count(out, "Installed ") != 1 {
		t.Fatalf("expected one install through the in-tree link: %v, %q", err, out)
	}
	if out, err := runLocalSkillCommand(t, "skill", "--uninstall", "--scope", "project"); err != nil || strings.Count(out, "Removed ") != 1 {
		t.Fatalf("expected one uninstall through the in-tree link: %v, %q", err, out)
	}
	if out, err := exec.CommandContext(t.Context(), "git", "status", "--porcelain", "--", ".agents").CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("project uninstall left artifacts: %v, %q", err, out)
	}
}

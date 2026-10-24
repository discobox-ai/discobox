// install is for a pool machine: Linux, booted with systemd.

//go:build linux

package bundle

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	poolagent "github.com/discobox-ai/discobox/pool-agent"
)

// machine is a pool machine's filesystem under a temporary root, with the
// commands install runs recorded rather than run.
type machine struct {
	root     string
	commands [][]string
	envs     [][]string
	hasGit   bool
	// failOn fails the systemctl verb it names.
	failOn string
}

func newMachine(t *testing.T) *machine {
	t.Helper()
	m := &machine{root: t.TempDir(), hasGit: true}
	for _, dir := range []string{"run/systemd/system", "etc"} {
		if err := os.MkdirAll(filepath.Join(m.root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// placeTree places a tree as a provider does: the agent and the bundle's own
// units, under its digest.
func (m *machine) placeTree(t *testing.T, digest string) string {
	t.Helper()
	tree := filepath.Join(m.root, filepath.FromSlash(TreeDir(digest)))
	if err := os.MkdirAll(filepath.Join(tree, filepath.FromSlash(UnitsDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(tree, filepath.FromSlash(AgentFile))
	if err := os.WriteFile(agent, []byte("agent "+digest), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, unit := range Units {
		content, err := os.ReadFile(filepath.Join("units", unit))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tree, filepath.FromSlash(UnitsDir), unit), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return agent
}

func (m *machine) install(t *testing.T, executable string) error {
	t.Helper()
	m.commands = nil
	return Install(context.Background(), Config{
		Root:       m.root,
		Executable: executable,
		Run: func(_ context.Context, env []string, argv ...string) error {
			m.commands = append(m.commands, argv)
			m.envs = append(m.envs, env)
			if argv[0] == "systemctl" && argv[1] == m.failOn {
				return errors.New("systemctl " + m.failOn + " failed")
			}
			if argv[0] == "apt-get" && argv[1] == "install" {
				m.hasGit = true
			}
			return nil
		},
		LookPath: func(file string) (string, error) {
			if file == "git" && m.hasGit {
				return "/usr/bin/git", nil
			}
			return "", exec.ErrNotFound
		},
		Log: io.Discard,
	})
}

func (m *machine) link(t *testing.T) string {
	t.Helper()
	target, err := os.Readlink(filepath.Join(m.root, CurrentLink))
	if err != nil {
		t.Fatal(err)
	}
	return target
}

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestInstallLinksTheTreeLaysTheUnitsAndRestartsThem(t *testing.T) {
	m := newMachine(t)
	agent := m.placeTree(t, digestA)

	if err := m.install(t, agent); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got, want := m.link(t), TreeDir(digestA); got != want {
		t.Fatalf("%s -> %s, want %s", CurrentLink, got, want)
	}
	for _, unit := range Units {
		laid, err := os.ReadFile(filepath.Join(m.root, unitDir, unit))
		if err != nil {
			t.Fatalf("unit %s not laid: %v", unit, err)
		}
		want, _ := os.ReadFile(filepath.Join("units", unit))
		if string(laid) != string(want) {
			t.Fatalf("unit %s laid as %q, want the tree's", unit, laid)
		}
	}
	want := [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", AgentUnit, ProxyUnit},
		{"systemctl", "restart", AgentUnit, ProxyUnit},
	}
	if !reflect.DeepEqual(m.commands, want) {
		t.Fatalf("commands = %q, want %q", m.commands, want)
	}
}

// Run again on the same tree, install changes nothing: no file is written and
// the agent it finds running is not restarted.
func TestInstallAgainChangesNothing(t *testing.T) {
	m := newMachine(t)
	agent := m.placeTree(t, digestA)
	if err := m.install(t, agent); err != nil {
		t.Fatalf("install: %v", err)
	}
	unit := filepath.Join(m.root, unitDir, AgentUnit)
	before, err := os.Stat(unit)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.install(t, agent); err != nil {
		t.Fatalf("install again: %v", err)
	}
	want := [][]string{
		{"systemctl", "start", AgentUnit, ProxyUnit},
	}
	if !reflect.DeepEqual(m.commands, want) {
		t.Fatalf("commands = %q, want %q", m.commands, want)
	}
	after, err := os.Stat(unit)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("%s was written again", AgentUnit)
	}
}

// A new tree and install moves the link and restarts the units onto it, even
// when its units are the same bytes.
func TestInstallFromANewTreeRestartsOntoIt(t *testing.T) {
	m := newMachine(t)
	if err := m.install(t, m.placeTree(t, digestA)); err != nil {
		t.Fatalf("install: %v", err)
	}

	if err := m.install(t, m.placeTree(t, digestB)); err != nil {
		t.Fatalf("install the new tree: %v", err)
	}
	if got, want := m.link(t), TreeDir(digestB); got != want {
		t.Fatalf("%s -> %s, want %s", CurrentLink, got, want)
	}
	want := [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", AgentUnit, ProxyUnit},
		{"systemctl", "restart", AgentUnit, ProxyUnit},
	}
	if !reflect.DeepEqual(m.commands, want) {
		t.Fatalf("commands = %q, want %q", m.commands, want)
	}
	// The tree it replaced runs nothing now, and is removed.
	if _, err := os.Stat(filepath.Join(m.root, filepath.FromSlash(TreeDir(digestA)))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the superseded tree is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.root, filepath.FromSlash(TreeDir(digestB)))); err != nil {
		t.Fatalf("the running tree was removed: %v", err)
	}
}

// An install cut short after it moved the link, before the restart took, is
// not done: run again, it restarts the units onto the tree rather than taking
// the moved link for a finished install.
func TestInstallCutShortBeforeTheRestartRestartsWhenRunAgain(t *testing.T) {
	m := newMachine(t)
	if err := m.install(t, m.placeTree(t, digestA)); err != nil {
		t.Fatalf("install: %v", err)
	}
	agent := m.placeTree(t, digestB)
	m.failOn = "restart"
	if err := m.install(t, agent); err == nil {
		t.Fatal("install with a failing restart succeeded")
	}
	m.failOn = ""
	// The old agent may still run from its tree, so it is kept.
	if _, err := os.Stat(filepath.Join(m.root, filepath.FromSlash(TreeDir(digestA)))); err != nil {
		t.Fatalf("the old tree was removed before the restart took: %v", err)
	}

	if err := m.install(t, agent); err != nil {
		t.Fatalf("install again: %v", err)
	}
	if got := m.commands[len(m.commands)-1]; !reflect.DeepEqual(got, []string{"systemctl", "restart", AgentUnit, ProxyUnit}) {
		t.Fatalf("commands = %q, want the units restarted onto the new tree", m.commands)
	}
	if _, err := os.Stat(filepath.Join(m.root, filepath.FromSlash(TreeDir(digestA)))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the superseded tree is still there after the restart: %v", err)
	}
}

func TestInstallInstallsGitOnDebianWhereItIsAbsent(t *testing.T) {
	m := newMachine(t)
	m.hasGit = false
	if err := os.WriteFile(filepath.Join(m.root, "etc/debian_version"), []byte("13.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.install(t, m.placeTree(t, digestA)); err != nil {
		t.Fatalf("install: %v", err)
	}
	want := [][]string{
		{"apt-get", "update"},
		{"apt-get", "install", "-y", "--no-install-recommends", "git"},
	}
	if !reflect.DeepEqual(m.commands[:2], want) {
		t.Fatalf("commands = %q, want git installed first", m.commands)
	}
	if !reflect.DeepEqual(m.envs[0], []string{"DEBIAN_FRONTEND=noninteractive"}) {
		t.Fatalf("apt-get env = %q, want it noninteractive", m.envs[0])
	}
}

// Off the Debian family, an absent git fails install before anything moves.
func TestInstallWithoutGitOffDebianChangesNothing(t *testing.T) {
	m := newMachine(t)
	m.hasGit = false

	err := m.install(t, m.placeTree(t, digestA))
	if err == nil || !strings.Contains(err.Error(), "Debian") {
		t.Fatalf("install = %v, want it refused for want of git", err)
	}
	if _, err := os.Lstat(filepath.Join(m.root, CurrentLink)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s was linked: %v", CurrentLink, err)
	}
	if len(m.commands) != 0 {
		t.Fatalf("commands = %q, want none", m.commands)
	}
}

func TestInstallRefusesAnAgentOutsideABundleTree(t *testing.T) {
	m := newMachine(t)
	stray := filepath.Join(m.root, "usr/local/bin/discobox-pool-agent")
	if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, nil, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := m.install(t, stray); err == nil || !strings.Contains(err.Error(), AgentFile) {
		t.Fatalf("install = %v, want it refused outside a tree", err)
	}
}

func TestInstallRefusesAMachineSystemdIsNotRunning(t *testing.T) {
	m := newMachine(t)
	agent := m.placeTree(t, digestA)
	if err := os.RemoveAll(filepath.Join(m.root, "run/systemd")); err != nil {
		t.Fatal(err)
	}

	if err := m.install(t, agent); err == nil || !strings.Contains(err.Error(), "systemd") {
		t.Fatalf("install = %v, want it refused without systemd", err)
	}
}

// The units run the agent from the stable link and name the bootstrap file
// the provider places; this package's constants are what the provider places
// by, so the two must agree.
func TestUnitsRunTheLinkedAgentWithTheBootstrapFile(t *testing.T) {
	agent, err := os.ReadFile(filepath.Join("units", AgentUnit))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ExecStart=" + AgentPath + "\n",
		"Environment=" + poolagent.EnvBootstrapFile + "=" + BootstrapFile + "\n",
		"Type=notify\n",
	} {
		if !strings.Contains(string(agent), want) {
			t.Fatalf("%s lacks %q", AgentUnit, want)
		}
	}
	proxy, err := os.ReadFile(filepath.Join("units", ProxyUnit))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ExecStart=" + AgentPath + " proxy\n",
		"After=network-online.target " + AgentUnit + "\n",
		"Requires=" + AgentUnit + "\n",
	} {
		if !strings.Contains(string(proxy), want) {
			t.Fatalf("%s lacks %q", ProxyUnit, want)
		}
	}
}

// The bundle's proxy unit is a twin of the image's (pool-agent/REVIEW.md):
// it runs the agent from the bundle's link and needs the agent's start,
// where the image's runs it from /usr/local/bin, and is otherwise the same.
func TestBundleProxyUnitMatchesTheImages(t *testing.T) {
	service := func(path, agent string) map[string]string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := map[string]string{}
		in := false
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "[") {
				in = line == "[Service]"
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !in || !ok || strings.HasPrefix(line, "#") {
				continue
			}
			lines[key] = strings.ReplaceAll(strings.TrimPrefix(value, "-"), agent, "<agent>")
		}
		return lines
	}
	bundle := service(filepath.Join("units", ProxyUnit), AgentPath)
	image := service(filepath.Join("..", "image", "systemd", "discobox-proxy.service"), "/usr/local/bin/discobox-pool-agent")
	if !reflect.DeepEqual(bundle, image) {
		t.Fatalf("the bundle's [Service] = %v, the image's = %v: change both", bundle, image)
	}
}

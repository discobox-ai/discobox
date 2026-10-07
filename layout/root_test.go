package layout

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrg/xdg"
)

// accessors is every exported path accessor, called with fixed IDs.
func accessors(r Root) map[string]string {
	return map[string]string{
		"PoolIdentity":            r.PoolIdentity("prj", "pool"),
		"PoolIdentityKey":         r.PoolIdentityKey("prj", "pool"),
		"ProjectData":             r.ProjectData("prj"),
		"ProjectPools":            r.ProjectPools("prj"),
		"PoolData":                r.PoolData("prj", "pool"),
		"PoolSandboxes":           r.PoolSandboxes("prj", "pool"),
		"PoolSourceData":          r.PoolSourceData("prj", "pool"),
		"SourceData":              r.SourceData("prj", "pool", "key"),
		"Sandbox":                 r.Sandbox("prj", "pool", "sb"),
		"SandboxData":             r.SandboxData("prj", "pool", "sb"),
		"SandboxSourceData":       r.SandboxSourceData("prj", "pool", "sb", "primary"),
		"SandboxConfig":           r.SandboxConfig("prj", "pool", "sb"),
		"SandboxSecrets":          r.SandboxSecrets("prj", "pool", "sb"),
		"SandboxSources":          r.SandboxSources("prj", "pool", "sb"),
		"SandboxOrigins":          r.SandboxOrigins("prj", "pool", "sb"),
		"ProjectCachePools":       r.ProjectCachePools("prj"),
		"PoolCache":               r.PoolCache("prj", "pool"),
		"PoolBuild":               r.PoolBuild("prj", "pool"),
		"ProxyCerts":              r.ProxyCerts("prj", "pool"),
		"ProxyControlKey":         r.ProxyControlKey("prj", "pool"),
		"ProxyProjectPools":       r.ProxyProjectPools("prj"),
		"ProxyPool":               r.ProxyPool("prj", "pool"),
		"ProxyPoolSandboxes":      r.ProxyPoolSandboxes("prj", "pool"),
		"ProxyAuditDB":            r.ProxyAuditDB("prj", "pool"),
		"ProxyCache":              r.ProxyCache("prj", "pool"),
		"ProxyStreams":            r.ProxyStreams("prj", "pool"),
		"ProxyBodies":             r.ProxyBodies("prj", "pool"),
		"ProxySecretsFile":        r.ProxySecretsFile("prj", "pool"),
		"ProxyResolveContextFile": r.ProxyResolveContextFile("prj", "pool"),
	}
}

// Every accessor resolves against the root it is asked of, and lays out the
// same tree below it whichever root that is.
func TestEveryAccessorResolvesAgainstItsRoot(t *testing.T) {
	container := accessors(Container())
	for name, root := range map[string]Root{
		"relocated container": ContainerAt(t.TempDir()),
		"host":                {state: t.TempDir(), host: true, native: true},
	} {
		for accessor, got := range accessors(root) {
			rel, err := filepath.Rel(root.Dir(), got)
			if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
				t.Errorf("%s: %s = %q, want it under %q", name, accessor, got, root.Dir())
				continue
			}
			if want := strings.TrimPrefix(container[accessor], ContainerRoot+"/"); filepath.ToSlash(rel) != want {
				t.Errorf("%s: %s is %q below its root, want %q as in a container", name, accessor, filepath.ToSlash(rel), want)
			}
		}
	}
}

// The relocated container is the whole container filesystem under one
// directory, which is what a test needs in place of the container's mounts.
func TestContainerAtRelocatesStateAndSystemPaths(t *testing.T) {
	dir := t.TempDir()
	root := ContainerAt(dir)
	if got, want := root.Dir(), filepath.Join(dir, "var", "lib", "discobox"); got != want {
		t.Fatalf("Dir() = %q, want %q", got, want)
	}
	if got, want := root.System("/etc/discobox/proxy.env"), filepath.Join(dir, "etc", "discobox", "proxy.env"); got != want {
		t.Fatalf("System() = %q, want %q", got, want)
	}
	if got := Container().System("/etc/discobox/proxy.env"); got != "/etc/discobox/proxy.env" {
		t.Fatalf("Container().System() = %q, want it unchanged", got)
	}
}

// A relocated root's daemon sees what the agent sees unless a driver says it
// keeps the state elsewhere, and then only paths under the root move.
func TestHostMappingTranslatesFromARelocatedRoot(t *testing.T) {
	root := ContainerAt(t.TempDir())
	data := root.PoolData("prj", "pool")
	if got := root.HostMapping("").HostPath(data); got != data {
		t.Fatalf("HostPath = %q, want it unchanged with no relocation", got)
	}
	if got, want := root.HostMapping("/var/lib/docker/discobox").HostPath(data),
		"/var/lib/docker/discobox/projects/prj/pools/pool"; got != want {
		t.Fatalf("HostPath = %q, want %q", got, want)
	}
	if outside := filepath.Join(filepath.Dir(root.Dir()), "discoboxfoo"); root.HostMapping("/x").HostPath(outside) != outside {
		t.Fatalf("HostPath(%q) was translated; a sibling of the root is not under it", outside)
	}
}

func TestHostRootHasNoPoolContainer(t *testing.T) {
	host := Root{state: t.TempDir(), host: true, native: true}
	for name, ask := range map[string]func(){
		"System":      func() { host.System("/etc/discobox/proxy.env") },
		"MountRoots":  func() { host.MountRoots() },
		"HostMapping": func() { host.HostMapping("") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s on a host root did not panic", name)
				}
			}()
			ask()
		}()
	}
	// Its own state paths are what a host root is for.
	if got := host.PoolData("prj", "pool"); !strings.HasPrefix(got, host.Dir()) {
		t.Errorf("PoolData = %q, want it under %q", got, host.Dir())
	}
}

// setXDG sets env for one test and reloads xdg from it, then reloads xdg from
// the real environment afterwards. The reload is registered before any
// t.Setenv, so it runs after every one of them has been restored.
func setXDG(t *testing.T, env map[string]string) {
	t.Helper()
	t.Cleanup(xdg.Reload)
	for key, value := range env {
		t.Setenv(key, value)
	}
	xdg.Reload()
}

// ADR 0144 §1: the host root is the server's own data directory, resolved the
// same way, with the pool agent's state beside the server's.
func TestHostStateIsUnderTheServersDataDirectory(t *testing.T) {
	data := t.TempDir()
	setXDG(t, map[string]string{"XDG_DATA_HOME": data})
	got, err := hostState()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(data, "discobox", HostStateName); got != want {
		t.Fatalf("hostState = %q, want %q", got, want)
	}
}

// A relative XDG_DATA_HOME is not one: the server ignores it and uses the
// platform default, and a host pool has to land in that same place rather
// than refuse to start.
func TestHostStateIgnoresARelativeDataHome(t *testing.T) {
	setXDG(t, map[string]string{"XDG_DATA_HOME": "relative/data"})
	got, err := hostState()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) || strings.Contains(filepath.ToSlash(got), "relative/data") {
		t.Fatalf("hostState = %q, want the platform default", got)
	}
}

func TestHostRefusesAPrivilegedProcess(t *testing.T) {
	if !privileged() {
		t.Skip("this process is not privileged")
	}
	if _, err := Host(); err == nil {
		t.Fatal("Host() succeeded in a privileged process")
	}
}

// A Root nobody chose is the container's view, so a Docker pool's paths are
// the same whether its root was named or left zero.
func TestZeroRootIsTheContainerView(t *testing.T) {
	zero, container := accessors(Root{}), accessors(Container())
	for name, got := range zero {
		if got != container[name] {
			t.Errorf("zero %s = %q, want %q", name, got, container[name])
		}
	}
	if got := (Root{}).Dir(); got != ContainerRoot {
		t.Errorf("zero Dir() = %q, want %q", got, ContainerRoot)
	}
}

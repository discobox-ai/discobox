package layout

import (
	"path/filepath"
	"strings"
	"testing"
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

// The daemon sees a relocated container's state where it would see the real
// container's, so the paths a test hands a fake daemon are the production ones.
func TestHostMappingTranslatesFromARelocatedRoot(t *testing.T) {
	root := ContainerAt(t.TempDir())
	if got, want := root.HostMapping("").HostPath(root.PoolData("prj", "pool")),
		"/var/lib/discobox/projects/prj/pools/pool"; got != want {
		t.Fatalf("HostPath = %q, want %q", got, want)
	}
	if got, want := root.HostMapping("/var/lib/docker/discobox").HostPath(root.Dir()),
		"/var/lib/docker/discobox"; got != want {
		t.Fatalf("HostPath(root) = %q, want %q", got, want)
	}
	if outside := filepath.Join(filepath.Dir(root.Dir()), "discoboxfoo"); root.HostMapping("").HostPath(outside) != outside {
		t.Fatalf("HostPath(%q) was translated; a sibling of the root is not under it", outside)
	}
}

func TestHostRootHasNoContainerFilesystem(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("System on a host root did not panic")
		}
	}()
	Root{state: t.TempDir(), host: true, native: true}.System("/etc/discobox/proxy.env")
}

// ADR 0144 §1: the host root is the user's data directory, by the convention
// the server's own data directory already follows on each platform.
func TestHostStatePerOS(t *testing.T) {
	for name, tc := range map[string]struct {
		goos string
		env  map[string]string
		home string
		want string
	}{
		"macOS": {
			goos: "darwin", home: "/Users/ada",
			want: "/Users/ada/Library/Application Support/discobox/pool-agent",
		},
		"macOS with XDG_DATA_HOME": {
			goos: "darwin", home: "/Users/ada", env: map[string]string{"XDG_DATA_HOME": "/Volumes/data/"},
			want: "/Volumes/data/discobox/pool-agent",
		},
		"Windows": {
			goos: "windows", home: `C:\Users\ada`, env: map[string]string{"LOCALAPPDATA": `C:\Users\ada\AppData\Local`},
			want: `C:\Users\ada\AppData\Local\discobox\pool-agent`,
		},
		"Windows without LOCALAPPDATA": {
			goos: "windows", home: `C:\Users\ada`,
			want: `C:\Users\ada\AppData\Local\discobox\pool-agent`,
		},
		"Windows with XDG_DATA_HOME": {
			goos: "windows", home: `C:\Users\ada`, env: map[string]string{"XDG_DATA_HOME": `D:\data`, "LOCALAPPDATA": `C:\Users\ada\AppData\Local`},
			want: `D:\data\discobox\pool-agent`,
		},
		"Linux": {
			goos: "linux", home: "/home/ada",
			want: "/home/ada/.local/share/discobox/pool-agent",
		},
	} {
		got, err := hostState(tc.goos, func(key string) string { return tc.env[key] }, tc.home)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: hostState = %q, want %q", name, got, tc.want)
		}
	}
}

// Without a data directory there is nowhere durable to put a pool's identity,
// and a temporary directory would cost the pool its identity on reboot.
func TestHostStateRefusesWithoutADataDirectory(t *testing.T) {
	for _, goos := range []string{"darwin", "windows", "linux"} {
		if got, err := hostState(goos, func(string) string { return "" }, ""); err == nil {
			t.Errorf("%s: hostState = %q, want an error", goos, got)
		}
	}
}

func TestHostResolvesOnThisMachine(t *testing.T) {
	if privileged() {
		if _, err := Host(); err == nil {
			t.Fatal("Host() succeeded in a privileged process")
		}
		return
	}
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	root, err := Host()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := root.Dir(), filepath.Join(data, "discobox", HostStateName); got != want {
		t.Fatalf("Dir() = %q, want %q", got, want)
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

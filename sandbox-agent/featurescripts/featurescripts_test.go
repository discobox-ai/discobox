// Package featurescripts_test runs the platform features' shell scripts that
// decide where a tool goes and what every user's PATH finds — core's
// link-command, the node feature's select step and npm-global, and the rust
// and python installers — against a fake box in a temporary directory, and is
// the only thing in this directory.
//
// What they decide is links, prefixes and modes, and a box's image may have
// put anything at the names they claim: a link into one user's nvm, an older
// copy, nothing. That is provable only by running them, as agentstore does
// for the agent store. Each script's fixed paths move with an environment
// variable for this, and every network fetch is a stub on PATH.
package featurescripts_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// features is the platform's feature directory, relative to this one.
const features = "../features"

// box is a fake box: its own /usr/local/bin, Discobox's own directory under
// /usr/local/lib/discobox, a home, and the stubs its scripts run.
type box struct {
	t     *testing.T
	root  string
	stubs string
	env   []string
}

func newBox(t *testing.T) *box {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the features provision Linux")
	}
	for _, tool := range []string{"sh", "bash", "readlink", "mktemp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available: %v", tool, err)
		}
	}
	root := t.TempDir()
	b := &box{t: t, root: root, stubs: filepath.Join(t.TempDir(), "stubs")}
	mkdir(t, b.stubs)
	mkdir(t, b.path("/usr/local/bin"))
	mkdir(t, b.path("/usr/bin"))
	b.env = append(os.Environ(),
		"PATH="+b.stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+t.TempDir(),
		"DISCOBOX_BIN_DIR="+b.path("/usr/local/bin"),
		"DISCOBOX_OWN_DIR="+b.path("/usr/local/lib/discobox"),
		"DISCOBOX_LINK_COMMAND="+script(t, "core/link-command"),
		"DISCOBOX_NODE_ENV="+b.path("/usr/local/lib/discobox/node.env"),
		"DISCOBOX_NPM_PREFIX="+b.path("/usr/local/lib/discobox/npm"),
		"DISCOBOX_INSTALLED_NODE_DIR="+b.path("/usr/bin"),
		"DISCOBOX_RUST_PREFIX="+b.path("/usr/local/lib/discobox/rust"),
		"DISCOBOX_UV_PREFIX="+b.path("/usr/local/lib/discobox/uv"),
	)
	return b
}

// path is a box path in the fake box.
func (b *box) path(p string) string { return filepath.Join(b.root, filepath.FromSlash(p)) }

// run runs a feature script with the box's environment and extra variables,
// and fails the test unless it exits 0.
func (b *box) run(name string, env []string, args ...string) string {
	b.t.Helper()
	out, err := b.exec(name, env, args...)
	if err != nil {
		b.t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return out
}

func (b *box) exec(name string, env []string, args ...string) (string, error) {
	b.t.Helper()
	//nolint:gosec // The command is a feature's own script and arguments this test wrote.
	cmd := exec.CommandContext(b.t.Context(), script(b.t, name), args...)
	cmd.Dir = filepath.Dir(script(b.t, name))
	cmd.Env = append(slices.Clone(b.env), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// stub writes a command onto the scripts' PATH.
func (b *box) stub(name, body string) {
	b.t.Helper()
	write(b.t, filepath.Join(b.stubs, name), "#!/bin/sh\n"+body, 0o755)
}

// node writes a node that prints version, and an npm beside it, at dir.
func (b *box) node(dir, version string) {
	b.t.Helper()
	write(b.t, filepath.Join(dir, "node"), "#!/bin/sh\necho "+version+"\n", 0o755)
	write(b.t, filepath.Join(dir, "npm"), "#!/bin/sh\necho npm beside "+version+"\n", 0o755)
}

// installedNode is NodeSource's package, as install-node.sh leaves it: node,
// npm and npx in /usr/bin, and npm the stub that installs a package into its
// --prefix the way npm does, saying which node it ran with.
func (b *box) installedNode() {
	b.t.Helper()
	write(b.t, b.path("/usr/bin/node"), "#!/bin/sh\necho v24.9.0\n", 0o755)
	write(b.t, b.path("/usr/bin/npx"), "#!/bin/sh\n", 0o755)
	write(b.t, b.path("/usr/bin/npm"), npmStub, 0o755)
}

// npmStub stands in for `npm install --global --prefix P PKG...`: each
// package is laid at P/lib/node_modules/PKG with a cli.js, and P/bin/<its
// command> a relative link to it, as npm makes them. The package's command is
// the last part of its name, claude-code's `claude`. It records the node PATH
// finds and the cache it was given, and fails with NPM_STATUS.
const npmStub = `#!/bin/sh
set -eu
printf 'node=%s\n' "$(command -v node)" >> "$TMPDIR/npm.log"
printf 'cache=%s\n' "$npm_config_cache" >> "$TMPDIR/npm.log"
[ -d "$npm_config_cache" ] || exit 91
[ "${NPM_STATUS:-0}" = 0 ] || exit "$NPM_STATUS"
prefix=
packages=
while [ $# -gt 0 ]; do
	case $1 in
	--prefix) prefix=$2; shift ;;
	install|--global) ;;
	*) packages="$packages $1" ;;
	esac
	shift
done
for package in $packages; do
	dir=$prefix/lib/node_modules/$package
	mkdir -p "$dir" "$prefix/bin"
	printf '{"name": "%s"}\n' "$package" > "$dir/package.json"
	printf '#!/bin/sh\necho %s\n' "$package" > "$dir/cli.js"
	chmod 0755 "$dir/cli.js"
	command=${package##*/}
	[ "$command" != claude-code ] || command=claude
	ln -sfn "../lib/node_modules/$package/cli.js" "$prefix/bin/$command"
done
`

// everyUserCanRun fails unless the file path resolves to can be run by any
// user: not in a home, and every directory on the way open to others — the
// node feature's adopt rule (sandbox-agent/provision/adopt.go).
func (b *box) everyUserCanRun(path string) {
	b.t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		b.t.Fatalf("%s: %v", path, err)
	}
	rel, err := filepath.Rel(b.root, resolved)
	if err != nil || strings.HasPrefix(rel, "..") {
		b.t.Fatalf("%s resolves to %s, outside the box", path, resolved)
	}
	if strings.HasPrefix(rel, "home"+string(filepath.Separator)) || strings.HasPrefix(rel, "root"+string(filepath.Separator)) {
		b.t.Fatalf("%s resolves to /%s, in a home", path, rel)
	}
	info, err := os.Stat(resolved)
	if err != nil || info.Mode().Perm()&0o001 == 0 {
		b.t.Fatalf("%s resolves to /%s, which others cannot run (%v)", path, rel, err)
	}
	for dir := filepath.Dir(resolved); len(dir) > len(b.root); dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm()&0o001 == 0 {
			b.t.Fatalf("%s is closed to others (%v)", dir, err)
		}
	}
}

func (b *box) readlink(path string) string {
	b.t.Helper()
	target, err := os.Readlink(b.path(path))
	if err != nil {
		b.t.Fatalf("%s is not a link: %v", path, err)
	}
	return target
}

// The rule a box with one user's Node ahead of the system's needs, for each
// way the box's own copy is refused: the node feature records and links the
// Node it chose, moves the box's aside, and a harness CLI npm-global installs
// lands in Discobox's prefix, is what /usr/local/bin has, and runs for every
// user — with a private scratch cache it removes.
func TestAHarnessCLILandsInDiscoboxsPrefixWhateverTheBoxHadOnPATH(t *testing.T) {
	cases := map[string]struct {
		box     func(b *box)
		adopted bool
		chosen  string
	}{
		// boxd: /usr/local/bin/node a link into a home others cannot enter.
		"a link into a closed home": {
			box: func(b *box) {
				b.node(b.path("/home/boxd/.nvm/versions/node/v24.22.1/bin"), "v24.22.1")
				for _, c := range []string{"node", "npm"} {
					symlink(t, b.path("/home/boxd/.nvm/versions/node/v24.22.1/bin/"+c), b.path("/usr/local/bin/"+c))
				}
				if err := os.Chmod(b.path("/home/boxd"), 0o750); err != nil {
					t.Fatal(err)
				}
				b.installedNode()
			},
			chosen: "/usr/bin",
		},
		"an older copy every user can run": {
			box: func(b *box) {
				b.node(b.path("/usr/local/bin"), "v18.2.0")
				b.installedNode()
			},
			chosen: "/usr/bin",
		},
		"a usable copy adopted behind a refused one": {
			box: func(b *box) {
				b.node(b.path("/home/boxd/.nvm/bin"), "v24.22.1")
				symlink(t, b.path("/home/boxd/.nvm/bin/node"), b.path("/usr/local/bin/node"))
				b.installedNode()
			},
			adopted: true,
			chosen:  "/usr/bin",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := newBox(t)
			tc.box(b)
			var env []string
			if tc.adopted {
				env = []string{
					"DISCOBOX_PROVISION_ADOPTED_NODE=" + b.path(tc.chosen+"/node"),
					"DISCOBOX_PROVISION_ADOPTED_NPM=" + b.path(tc.chosen+"/npm"),
				}
			}
			before := map[string]string{}
			for _, c := range []string{"node", "npm"} {
				if target, err := os.Readlink(b.path("/usr/local/bin/" + c)); err == nil {
					before[c] = "link:" + target
				} else if _, err := os.Stat(b.path("/usr/local/bin/" + c)); err == nil {
					before[c] = "file"
				}
			}

			b.run("node/select-node.sh", env)
			b.run("node/check-node.sh", env)

			record := readFile(t, b.path("/usr/local/lib/discobox/node.env"))
			for _, want := range []string{
				"DISCOBOX_NODE=" + b.path(tc.chosen+"/node") + "\n",
				"DISCOBOX_NPM=" + b.path(tc.chosen+"/npm") + "\n",
				"DISCOBOX_NPM_PREFIX=" + b.path("/usr/local/lib/discobox/npm") + "\n",
			} {
				if !strings.Contains(record, want) {
					t.Errorf("node.env = %q, want it to hold %q", record, want)
				}
			}
			for _, c := range []string{"node", "npm", "npx"} {
				if got := b.readlink("/usr/local/bin/" + c); got != b.path(tc.chosen+"/"+c) {
					t.Errorf("/usr/local/bin/%s -> %s, want the chosen %s", c, got, b.path(tc.chosen+"/"+c))
				}
			}
			// What the box had is moved aside, never deleted.
			for c, was := range before {
				aside := b.path("/usr/local/lib/discobox/displaced") + b.path("/usr/local/bin/"+c)
				switch {
				case strings.HasPrefix(was, "link:"):
					if got, err := os.Readlink(aside); err != nil || "link:"+got != was {
						t.Errorf("the box's %s, %s, is not at %s: %q, %v", c, was, aside, got, err)
					}
				default:
					if _, err := os.Stat(aside); err != nil {
						t.Errorf("the box's own %s is not at %s: %v", c, aside, err)
					}
				}
			}

			b.run("node/npm-global", nil, "install", "@anthropic-ai/claude-code")

			log := readFile(t, filepath.Join(envValue(b.env, "TMPDIR"), "npm.log"))
			if !strings.Contains(log, "node="+b.path(tc.chosen+"/node")+"\n") {
				t.Errorf("npm ran with another node than the chosen one:\n%s", log)
			}
			prefix := b.path("/usr/local/lib/discobox/npm")
			if got := b.readlink("/usr/local/bin/claude"); got != prefix+"/bin/claude" {
				t.Errorf("/usr/local/bin/claude -> %s, want %s", got, prefix+"/bin/claude")
			}
			b.everyUserCanRun(b.path("/usr/local/bin/claude"))
			b.everyUserCanRun(b.path("/usr/local/bin/node"))
			b.run("node/npm-global", nil, "check", "@anthropic-ai/claude-code")
		})
	}
}

// npm-global installs with a private cache, never the one a box mounts at
// ~/.npm, removes only that, and fails with npm.
func TestNPMGlobalOwnsItsScratchCache(t *testing.T) {
	for _, status := range []int{0, 23} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			b := newBox(t)
			b.installedNode()
			b.run("node/select-node.sh", nil)
			shared := filepath.Join(t.TempDir(), "shared-npm-cache")
			mkdir(t, shared)
			out, err := b.exec("node/npm-global", []string{"npm_config_cache=" + shared, fmt.Sprintf("NPM_STATUS=%d", status)}, "install", "pnpm")
			if code := exitCode(err); code != status {
				t.Fatalf("npm-global exited %d, want npm's %d: %v\n%s", code, status, err, out)
			}
			log := readFile(t, filepath.Join(envValue(b.env, "TMPDIR"), "npm.log"))
			cache := strings.TrimPrefix(lineWith(log, "cache="), "cache=")
			if cache == "" || cache == shared {
				t.Fatalf("npm was given the cache %q, want a private one", cache)
			}
			if _, err := os.Stat(cache); !os.IsNotExist(err) {
				t.Errorf("the private cache %s was left behind: %v", cache, err)
			}
			if _, err := os.Stat(shared); err != nil {
				t.Errorf("the shared cache went: %v", err)
			}
		})
	}
}

// npm-global's check fails when what it installed went or is shadowed again,
// so the step is applied again at the box's next boot.
func TestNPMGlobalCheckFindsDrift(t *testing.T) {
	b := newBox(t)
	b.installedNode()
	b.run("node/select-node.sh", nil)
	if _, err := b.exec("node/npm-global", nil, "check", "pnpm"); err == nil {
		t.Fatal("check passed before pnpm was installed")
	}
	b.run("node/npm-global", nil, "install", "pnpm")
	b.run("node/npm-global", nil, "check", "pnpm")

	// The box's image puts its own pnpm back over ours.
	if err := os.Remove(b.path("/usr/local/bin/pnpm")); err != nil {
		t.Fatal(err)
	}
	write(t, b.path("/usr/local/bin/pnpm"), "#!/bin/sh\n", 0o755)
	if _, err := b.exec("node/npm-global", nil, "check", "pnpm"); err == nil {
		t.Fatal("check passed with the box's pnpm shadowing ours")
	}
}

// The select step's check fails once the Node it recorded moves, so the
// record follows the adopt check's choice at the next boot.
func TestSelectNodesCheckFollowsTheChoice(t *testing.T) {
	b := newBox(t)
	b.installedNode()
	b.node(b.path("/opt/node/bin"), "v24.1.0")
	adopted := []string{
		"DISCOBOX_PROVISION_ADOPTED_NODE=" + b.path("/opt/node/bin/node"),
		"DISCOBOX_PROVISION_ADOPTED_NPM=" + b.path("/opt/node/bin/npm"),
	}
	b.run("node/select-node.sh", adopted)
	b.run("node/check-node.sh", adopted)
	if _, err := b.exec("node/check-node.sh", nil); err == nil {
		t.Fatal("check passed for an installed Node with the adopted one recorded")
	}
}

func TestLinkCommand(t *testing.T) {
	t.Run("an empty name is linked", func(t *testing.T) {
		b := newBox(t)
		write(t, b.path("/usr/local/lib/discobox/tool/tool"), "#!/bin/sh\n", 0o755)
		b.run("core/link-command", nil, b.path("/usr/local/lib/discobox/tool/tool"))
		if got := b.readlink("/usr/local/bin/tool"); got != b.path("/usr/local/lib/discobox/tool/tool") {
			t.Errorf("tool -> %s", got)
		}
		b.run("core/link-command", nil, "--check", b.path("/usr/local/lib/discobox/tool/tool"))
	})
	t.Run("the box's entries are moved aside, numbered", func(t *testing.T) {
		b := newBox(t)
		target := b.path("/usr/local/lib/discobox/tool/tool")
		write(t, target, "#!/bin/sh\n", 0o755)
		write(t, b.path("/usr/local/bin/tool"), "the box's own\n", 0o755)
		out := b.run("core/link-command", nil, target)
		if !strings.Contains(out, "moved the box's") {
			t.Errorf("said %q, want it to say what it moved", out)
		}
		// The box puts a link of its own back, into a home.
		if err := os.Remove(b.path("/usr/local/bin/tool")); err != nil {
			t.Fatal(err)
		}
		symlink(t, "/home/boxd/tool", b.path("/usr/local/bin/tool"))
		b.run("core/link-command", nil, target)

		aside := b.path("/usr/local/lib/discobox/displaced") + b.path("/usr/local/bin/tool")
		if got := readFile(t, aside); got != "the box's own\n" {
			t.Errorf("%s = %q", aside, got)
		}
		if got, err := os.Readlink(aside + ".1"); err != nil || got != "/home/boxd/tool" {
			t.Errorf("%s.1 -> %q, %v", aside, got, err)
		}
		if got := b.readlink("/usr/local/bin/tool"); got != target {
			t.Errorf("tool -> %s", got)
		}
	})
	t.Run("a link of Discobox's own is replaced in place", func(t *testing.T) {
		b := newBox(t)
		old, target := b.path("/usr/local/lib/discobox/old/tool"), b.path("/usr/local/lib/discobox/new/tool")
		write(t, old, "#!/bin/sh\n", 0o755)
		write(t, target, "#!/bin/sh\n", 0o755)
		b.run("core/link-command", nil, old)
		if _, err := b.exec("core/link-command", nil, "--check", target); err == nil {
			t.Error("check passed for a link to another target")
		}
		b.run("core/link-command", nil, target)
		if got := b.readlink("/usr/local/bin/tool"); got != target {
			t.Errorf("tool -> %s", got)
		}
		if _, err := os.Lstat(b.path("/usr/local/lib/discobox/displaced")); !os.IsNotExist(err) {
			t.Errorf("Discobox's own link was moved aside: %v", err)
		}
	})
	t.Run("a command already on PATH is left alone", func(t *testing.T) {
		b := newBox(t)
		write(t, b.path("/usr/local/bin/tool"), "#!/bin/sh\n", 0o755)
		b.run("core/link-command", nil, b.path("/usr/local/bin/tool"))
		b.run("core/link-command", nil, "--check", b.path("/usr/local/bin/tool"))
		if _, err := os.Readlink(b.path("/usr/local/bin/tool")); err == nil {
			t.Error("the command was replaced with a link to itself")
		}
	})
}

// rustup is installed into Discobox's prefix, not the installing root's
// ~/.cargo, every proxy is linked onto PATH, and a rustup found on PATH does
// not stand in for it.
func TestRustupIsInstalledForEveryUser(t *testing.T) {
	b := newBox(t)
	// One user's rustup on PATH, which the old guard took for installed.
	write(t, filepath.Join(b.stubs, "rustup"), "#!/bin/sh\necho rustup 1.0\n", 0o755)
	b.stub("curl", `printf 'curl %s\n' "$*" >> "$TMPDIR/curl.log"
cat <<'INIT'
printf 'init CARGO_HOME=%s RUSTUP_HOME=%s %s\n' "$CARGO_HOME" "$RUSTUP_HOME" "$*" >> "$TMPDIR/curl.log"
mkdir -p "$CARGO_HOME/bin" "$RUSTUP_HOME"
for c in rustup cargo rustc rustfmt; do printf '#!/bin/sh\n' > "$CARGO_HOME/bin/$c"; chmod 0755 "$CARGO_HOME/bin/$c"; done
INIT
`)
	b.run("rust/install-rustup.sh", nil)
	b.run("rust/check-rustup.sh", nil)
	b.run("rust/install-rustup.sh", nil)

	prefix := b.path("/usr/local/lib/discobox/rust")
	log := readFile(t, filepath.Join(envValue(b.env, "TMPDIR"), "curl.log"))
	if n := strings.Count(log, "curl "); n != 1 {
		t.Errorf("rustup was fetched %d times, want once:\n%s", n, log)
	}
	init := lineWith(log, "init ")
	if !strings.Contains(init, "CARGO_HOME="+prefix+" ") || !strings.Contains(init, "--no-modify-path") || !strings.Contains(init, "--default-toolchain none") {
		t.Errorf("rustup-init ran as %q", init)
	}
	rustupHome := strings.Fields(strings.TrimPrefix(init[strings.Index(init, "RUSTUP_HOME="):], "RUSTUP_HOME="))[0]
	if _, err := os.Stat(rustupHome); !os.IsNotExist(err) {
		t.Errorf("the install's RUSTUP_HOME %s was left behind: %v", rustupHome, err)
	}
	for _, c := range []string{"rustup", "cargo", "rustc", "rustfmt"} {
		if got := b.readlink("/usr/local/bin/" + c); got != prefix+"/bin/"+c {
			t.Errorf("/usr/local/bin/%s -> %s", c, got)
		}
		b.everyUserCanRun(b.path("/usr/local/bin/" + c))
	}
}

// uv is installed into a prefix of its own and linked from there, so a box's
// uv at /usr/local/bin — one the adopt check refused — is moved aside rather
// than replaced by the installer.
func TestUVIsLinkedFromItsOwnPrefix(t *testing.T) {
	b := newBox(t)
	symlink(t, "/home/boxd/.local/bin/uv", b.path("/usr/local/bin/uv"))
	b.stub("curl", `cat <<'INSTALL'
[ "$UV_NO_MODIFY_PATH" = 1 ] || { echo "the installer would write root's profiles" >&2; exit 1; }
mkdir -p "$UV_INSTALL_DIR"
for c in uv uvx; do printf '#!/bin/sh\n' > "$UV_INSTALL_DIR/$c"; chmod 0755 "$UV_INSTALL_DIR/$c"; done
INSTALL
`)
	b.run("python/install-uv.sh", nil)
	b.run("python/check-uv.sh", nil)

	prefix := b.path("/usr/local/lib/discobox/uv")
	for _, c := range []string{"uv", "uvx"} {
		if got := b.readlink("/usr/local/bin/" + c); got != prefix+"/"+c {
			t.Errorf("/usr/local/bin/%s -> %s", c, got)
		}
	}
	aside := b.path("/usr/local/lib/discobox/displaced") + b.path("/usr/local/bin/uv")
	if got, err := os.Readlink(aside); err != nil || got != "/home/boxd/.local/bin/uv" {
		t.Errorf("the box's uv is not at %s: %q, %v", aside, got, err)
	}
}

func script(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join(features, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	mkdir(t, filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func envValue(env []string, name string) string {
	for _, kv := range slices.Backward(env) {
		if value, ok := strings.CutPrefix(kv, name+"="); ok {
			return value
		}
	}
	return ""
}

func lineWith(text, prefix string) string {
	for line := range strings.Lines(text) {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSuffix(line, "\n")
		}
	}
	return ""
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

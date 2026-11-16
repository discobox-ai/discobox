package featurescripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// vncFiles are what the desktop feature and the boot flow laid for VNC before
// the desktop was streamed through voxl, under the systemd unit directory.
var vncFiles = []string{
	"x11vnc.socket",
	"x11vnc@.service",
	"x11vnc@.service.d/discobox-desktop-user.conf",
	"websockify-proxy.socket",
	"websockify-proxy.service",
	"websockify-proxy.service.d/discobox-desktop-user.conf",
	"websockify@.service",
}

// A box provisioned when the desktop was served over VNC is left with no VNC
// on it: its units stopped, disabled and removed, their drop-ins and the
// websockify helper with them, and its packages removed — except one that
// something else depends on, which stays. What else is in the unit directory
// stays, the desktop's own units included, and the step's check passes once it
// has run and fails while anything of VNC's is there.
func TestRetiringVNCLeavesNoneOfItOnTheBox(t *testing.T) {
	b := newBox(t)
	units := b.path("/etc/systemd/system")
	for _, name := range vncFiles {
		write(t, filepath.Join(units, name), "[Unit]\n", 0o644)
	}
	for _, socket := range []string{"x11vnc.socket", "websockify-proxy.socket", "discobox-desktop.socket"} {
		symlink(t, filepath.Join(units, socket), filepath.Join(units, "sockets.target.wants", socket))
	}
	write(t, filepath.Join(units, "discobox-desktop.socket"), "[Unit]\n", 0o644)
	write(t, b.path("/usr/local/bin/discobox-vnc-websockify"), "#!/bin/sh\n", 0o755)

	b.stub("systemctl", `echo "systemctl $*" >> "$TMPDIR/calls"`)
	// apt removes a package as it says it would: x11vnc alone, but
	// python3-websockify only with something else that depends on it.
	b.stub("apt-get", `if [ "$1" = -s ]; then
  echo "Remv $3 [1.0]"
  [ "$3" != python3-websockify ] || echo "Remv something-that-needs-it [2.0]"
  exit 0
fi
echo "apt-get $*" >> "$TMPDIR/calls"`)
	// Two installed, one removed earlier.
	b.stub("dpkg-query", `printf 'ii  x11vnc\nrc  novnc\nii  python3-websockify\n'`)
	env := []string{"DISCOBOX_SYSTEMD_DIR=" + units}

	if out, err := b.exec("desktop/check-retire-vnc.sh", env); err == nil {
		t.Fatalf("the check passed with VNC still laid:\n%s", out)
	}
	b.run("desktop/retire-vnc.sh", env)
	b.run("desktop/check-retire-vnc.sh", env)

	for _, name := range append(vncFiles, "sockets.target.wants/x11vnc.socket", "sockets.target.wants/websockify-proxy.socket") {
		if _, err := os.Lstat(filepath.Join(units, name)); !os.IsNotExist(err) {
			t.Errorf("%s is still there (%v)", name, err)
		}
	}
	for _, dir := range []string{"x11vnc@.service.d", "websockify-proxy.service.d"} {
		if _, err := os.Lstat(filepath.Join(units, dir)); !os.IsNotExist(err) {
			t.Errorf("the drop-in directory %s is still there (%v)", dir, err)
		}
	}
	if _, err := os.Lstat(b.path("/usr/local/bin/discobox-vnc-websockify")); !os.IsNotExist(err) {
		t.Errorf("the websockify helper is still there (%v)", err)
	}
	for _, kept := range []string{"discobox-desktop.socket", "sockets.target.wants/discobox-desktop.socket"} {
		if _, err := os.Lstat(filepath.Join(units, kept)); err != nil {
			t.Errorf("%s was removed with VNC: %v", kept, err)
		}
	}

	calls := readFile(t, filepath.Join(envValue(b.env, "TMPDIR"), "calls"))
	for _, want := range []string{
		"systemctl stop x11vnc.socket websockify-proxy.socket websockify-proxy.service x11vnc@*.service websockify@*.service",
		"systemctl disable x11vnc.socket websockify-proxy.socket",
		"systemctl daemon-reload",
		"apt-get remove -y -q x11vnc\n",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("calls do not include %q:\n%s", want, calls)
		}
	}
	for _, kept := range []string{"python3-websockify", "novnc", "purge", "autoremove"} {
		if strings.Contains(calls, kept) {
			t.Errorf("calls include %q, which had to be left alone:\n%s", kept, calls)
		}
	}
}

// A box that never had VNC — every box made since — is not touched at all.
func TestRetiringVNCOnABoxWithoutItDoesNothing(t *testing.T) {
	b := newBox(t)
	units := b.path("/etc/systemd/system")
	write(t, filepath.Join(units, "discobox-desktop.socket"), "[Unit]\n", 0o644)
	b.stub("systemctl", `echo "systemctl $*" >> "$TMPDIR/calls"`)
	b.stub("apt-get", `echo "apt-get $*" >> "$TMPDIR/calls"`)
	env := []string{"DISCOBOX_SYSTEMD_DIR=" + units}

	b.run("desktop/check-retire-vnc.sh", env)
	b.run("desktop/retire-vnc.sh", env)
	if _, err := os.Stat(filepath.Join(envValue(b.env, "TMPDIR"), "calls")); !os.IsNotExist(err) {
		t.Fatalf("a box without VNC was acted on: %v", err)
	}
}

// voxl is linked from the tree the build put it in, and the link is checked
// against the tree this run provisions from, so a new platform tree is linked
// again.
func TestVoxlIsLinkedFromItsTree(t *testing.T) {
	b := newBox(t)
	link := b.path("/usr/local/lib/discobox/voxl")
	tree := func() string {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "voxl", "voxl"), "#!/bin/sh\n", 0o755)
		write(t, filepath.Join(dir, "voxl", "web", "client.js"), "", 0o644)
		return dir
	}
	first, second := tree(), tree()
	env := func(feature string) []string {
		return []string{"DISCOBOX_VOXL_LINK=" + link, "DISCOBOX_PROVISION_FEATURE_DIR=" + feature}
	}

	if _, err := b.exec("desktop/check-voxl.sh", env(first)); err == nil {
		t.Fatal("the check passed before anything was linked")
	}
	b.run("desktop/link-voxl.sh", env(first))
	b.run("desktop/check-voxl.sh", env(first))
	if got := b.readlink("/usr/local/lib/discobox/voxl"); got != filepath.Join(first, "voxl") {
		t.Fatalf("link -> %s, want the tree's voxl", got)
	}

	if _, err := b.exec("desktop/check-voxl.sh", env(second)); err == nil {
		t.Fatal("the check passed for a link into another tree")
	}
	b.run("desktop/link-voxl.sh", env(second))
	if got := b.readlink("/usr/local/lib/discobox/voxl"); got != filepath.Join(second, "voxl") {
		t.Fatalf("link -> %s after relinking, want the new tree's voxl", got)
	}
}

// A package removal that fails fails the step before VNC's files are touched,
// so the check still fails and the next boot tries again: the packages are not
// left behind by a step that recorded itself done.
func TestRetiringVNCIsRetriedWhenARemovalFails(t *testing.T) {
	b := newBox(t)
	units := b.path("/etc/systemd/system")
	for _, name := range vncFiles {
		write(t, filepath.Join(units, name), "[Unit]\n", 0o644)
	}
	b.stub("systemctl", `true`)
	b.stub("dpkg-query", `printf 'ii  x11vnc\n'`)
	b.stub("apt-get", `if [ "$1" = -s ]; then echo "Remv $3 [1.0]"; exit 0; fi
echo "E: Could not get lock /var/lib/dpkg/lock-frontend" >&2
exit 100`)
	env := []string{"DISCOBOX_SYSTEMD_DIR=" + units}

	if out, err := b.exec("desktop/retire-vnc.sh", env); err == nil {
		t.Fatalf("the step succeeded with the removal failing:\n%s", out)
	}
	if _, err := b.exec("desktop/check-retire-vnc.sh", env); err == nil {
		t.Fatal("the check passes after a failed removal, so it would never be tried again")
	}
}

package sandboxruntime

import (
	"archive/tar"
	"context"
	"io"
	"strings"
	"testing"
)

// A machine sandbox boots its base's own /etc/hostname unless the pool
// places one: no container runtime mounts the sandbox's name over it, as
// Docker does for a container sandbox (sandboxHostname). So the placement
// names the machine for its sandbox, as a container sandbox is named, and
// gives that name an /etc/hosts entry, so `sudo` and anything else that
// resolves its own hostname finds it.
func TestMachineSandboxIsNamedForItsSandbox(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_1")); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-1")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := r.engine.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	want := sandboxHostname("sbx_1")
	read := func(path string) string {
		rc, err := guest.CopyFrom(ctx, path)
		if err != nil {
			return ""
		}
		defer rc.Close()
		tr := tar.NewReader(rc)
		if _, err := tr.Next(); err != nil {
			return ""
		}
		data, _ := io.ReadAll(tr)
		return string(data)
	}
	if got := strings.TrimSpace(read("/etc/hostname")); got != want {
		t.Errorf("machine's /etc/hostname = %q, want the sandbox's name %q", got, want)
	}
	hosts := read("/etc/hosts")
	resolves := false
	for _, line := range strings.Split(hosts, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && !strings.HasPrefix(fields[0], "#") {
			for _, name := range fields[1:] {
				resolves = resolves || name == want
			}
		}
	}
	if !resolves {
		t.Errorf("machine's /etc/hosts = %q, want an entry for %q", hosts, want)
	}
}

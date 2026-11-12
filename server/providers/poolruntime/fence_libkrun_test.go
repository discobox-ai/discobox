//go:build linux && amd64

package poolruntime

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	_ "github.com/discobox-ai/vm/pkg/machine/libkrun" // a held driver on KVM, linked by this test alone
)

// The libkrun pool machine this test boots is made from what a disco-vm
// checkout at the module's pin builds, named by these: the static linux
// `disco-vm` (its shim, and the guest's agent), a Linux base directory
// (`go tool task build:linux-guest`), and a runtime directory
// (`go tool task build:krun-runtime`). The released base and runtime are not
// pinned by digest yet (ADR 26-11-11-963 §§6, 11), so the test runs only
// where they are named.
const (
	libkrunDiscoVMEnv = "DISCOBOX_TEST_LIBKRUN_DISCO_VM"
	libkrunBaseEnv    = "DISCOBOX_TEST_LIBKRUN_BASE"
	libkrunRuntimeEnv = "DISCOBOX_TEST_LIBKRUN_RUNTIME"
)

// A held driver's pool machine, on libkrun: the fence loads and reads back
// before anything of the pool is placed, and on every boot after, the rule
// set is loaded before the network and before any pool unit, and a rule set
// that does not load keeps every pool unit down (ADR 26-11-11-963 §6).
func TestFenceBoxOnALibkrunPoolMachine(t *testing.T) {
	discoVM, base, runtime := os.Getenv(libkrunDiscoVMEnv), os.Getenv(libkrunBaseEnv), os.Getenv(libkrunRuntimeEnv)
	if discoVM == "" || base == "" || runtime == "" {
		t.Skipf("set %s, %s and %s to boot a libkrun pool machine", libkrunDiscoVMEnv, libkrunBaseEnv, libkrunRuntimeEnv)
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("no KVM: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	dir, scope := t.TempDir(), "fence-test"
	config, err := json.Marshal(map[string]string{"runtime": runtime})
	if err != nil {
		t.Fatal(err)
	}
	driver, err := machine.New("libkrun", machine.Config{
		Dir:          dir,
		Pool:         scope,
		Base:         base,
		ShimCommand:  []string{discoVM, "--root", dir, "--driver", "libkrun", "--pool", scope, "--driver-config", string(config), "shim"},
		DriverConfig: config,
	})
	if err != nil {
		t.Fatal(err)
	}
	if caps := driver.Capabilities(); !caps.Ports || caps.Keys.Mode != machine.KeysNone {
		t.Fatalf("libkrun is not a held driver: %+v", caps)
	}
	e, err := engine.Open(driver, scope)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := e.Create(ctx, engine.CreateOptions{Name: "pool-fence"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Remove(context.Background(), inst, true) })
	boot := func() machine.Guest {
		t.Helper()
		if err := e.Start(ctx, inst, engine.StartOptions{Timeout: 5 * time.Minute}); err != nil {
			t.Fatal(err)
		}
		guest, err := e.Guest(ctx, inst)
		if err != nil {
			t.Fatal(err)
		}
		return guest
	}
	reboot := func() machine.Guest {
		t.Helper()
		if err := e.Stop(ctx, inst, time.Minute); err != nil {
			t.Fatal(err)
		}
		return boot()
	}
	run := func(guest machine.Guest, script string) string {
		t.Helper()
		out, err := runGuest(ctx, guest, script)
		if err != nil {
			t.Fatalf("%s: %v", script, err)
		}
		return strings.TrimSpace(out)
	}

	guest := boot()
	if err := FenceBox(ctx, guest); err != nil {
		t.Fatal(err)
	}
	// Fenced again, in place, as a pool provisioned again is: nothing is
	// restarted, and it still reads back.
	if err := FenceBox(ctx, guest); err != nil {
		t.Fatalf("fenced again: %v", err)
	}
	if rules := run(guest, "cat "+FenceRulesFile); !strings.Contains(rules, `iifname "eth0" drop`) {
		t.Fatalf("libkrun's device is not fenced:\n%s", rules)
	}

	// A stand-in for a pool unit: the drop-in holds it, as it holds every
	// discobox-*.service the pool bundle lays.
	if err := PlaceBoxFiles(ctx, guest, BoxFile{Path: "/etc/systemd/system/discobox-fence-probe.service", Data: []byte(`[Unit]
Description=stand-in for a pool unit
[Service]
Type=simple
ExecStart=/bin/sleep infinity
[Install]
WantedBy=multi-user.target
`)}); err != nil {
		t.Fatal(err)
	}
	run(guest, "systemctl daemon-reload && systemctl enable discobox-fence-probe.service")
	for _, property := range []string{"Requires", "After"} {
		deps := strings.Fields(run(guest, "systemctl show -P "+property+" discobox-fence-probe.service"))
		if !slices.Contains(deps, "nftables.service") {
			t.Fatalf("a pool unit's %s does not name nftables.service: %v", property, deps)
		}
	}

	// After a reboot the rule set is loaded before the network and before the
	// pool unit.
	guest = reboot()
	listed := run(guest, "nft list table inet "+fenceTable)
	_, want, _ := strings.Cut(run(guest, "cat "+FenceRulesFile), "delete table inet "+fenceTable+"\n")
	if !sameRules(listed, want) {
		t.Fatalf("after a reboot the kernel holds\n%s", listed)
	}
	at := func(unit, property string) int64 {
		t.Helper()
		v, err := strconv.ParseInt(run(guest, "systemctl show -P "+property+" "+unit), 10, 64)
		if err != nil || v == 0 {
			t.Fatalf("%s's %s: %d %v", unit, property, v, err)
		}
		return v
	}
	fenced := at("nftables.service", "ActiveEnterTimestampMonotonic")
	network := at("network-pre.target", "ActiveEnterTimestampMonotonic")
	probe := at("discobox-fence-probe.service", "ExecMainStartTimestampMonotonic")
	t.Logf("monotonic µs: fence loaded %d, network-pre %d, pool unit started %d", fenced, network, probe)
	if fenced > network || fenced > probe {
		t.Fatalf("the fence loaded at %dµs, after the network (%dµs) or a pool unit (%dµs)", fenced, network, probe)
	}

	// A rule set that does not load keeps every pool unit down on the next
	// boot.
	run(guest, "echo 'this is not a rule set' > "+FenceRulesFile)
	guest = reboot()
	if state := run(guest, "systemctl is-active discobox-fence-probe.service || true"); state == "active" {
		t.Fatal("a pool unit started on a boot whose fence did not load")
	}
	if state := run(guest, "systemctl is-active nftables.service || true"); state == "active" {
		t.Fatal("a rule set that is not one loaded")
	}

	// Provisioned again, the fence is whole: placed, loaded, read back, and
	// the pool unit comes up.
	if err := FenceBox(ctx, guest); err != nil {
		t.Fatal(err)
	}
	run(guest, "systemctl start discobox-fence-probe.service")
	if state := run(guest, "systemctl is-active discobox-fence-probe.service"); state != "active" {
		t.Fatalf("the pool unit is %s once fenced again", state)
	}
}

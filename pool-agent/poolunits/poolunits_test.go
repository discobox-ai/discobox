package poolunits

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/pool-agent/handover"
)

// fakeSystemd answers systemctl as a box's systemd would for the units
// Apply drives: what is active, and what each says its status is. A unit
// started is a standby, as the agent and the proxy are once ready, until the
// one before it in the other slot is told to drain.
type fakeSystemd struct {
	mu       sync.Mutex
	active   map[string]bool
	status   map[string]string
	commands [][]string
	// failStart fails a start of the unit.
	failStart string
	// noTakeOver leaves a new slot a standby once the old one drains.
	noTakeOver bool
	// onKill runs as a slot is told to drain.
	onKill func()
	// ignoreKill leaves a slot told to drain as it was.
	ignoreKill bool
	// failKillUnit fails every kill of the unit.
	failKillUnit string
	// failKill fails every kill, a drain signal systemd cannot send.
	failKill bool
}

func newFakeSystemd() *fakeSystemd {
	return &fakeSystemd{active: map[string]bool{}, status: map[string]string{}}
}

func (f *fakeSystemd) systemctl(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if args[0] != "show" {
		f.commands = append(f.commands, args)
	}
	units := unitArgs(args[1:])
	switch args[0] {
	case "is-system-running":
		return "running\n", nil
	case "enable":
		if slices.Contains(args, "--now") {
			for _, unit := range units {
				f.active[unit] = true
			}
		}
	case "show":
		unit := args[len(args)-1]
		if strings.Contains(args[1], "ActiveState") {
			if f.active[unit] {
				return "active\n", nil
			}
			return "inactive\n", nil
		}
		return f.status[unit] + "\n", nil
	case "start", "restart":
		for _, unit := range units {
			if unit == f.failStart {
				return "", errors.New("start failed")
			}
		}
		for _, unit := range units {
			f.active[unit] = true
			f.status[unit] = handover.StatusStandby
			// Nothing else holds the active lock: it takes over at once.
			if peer := f.peer(unit); !f.active[peer] || args[0] == "restart" {
				f.status[unit] = handover.StatusActive
			}
		}
	case "try-restart":
		for _, unit := range units {
			if f.active[unit] {
				f.status[unit] = handover.StatusActive
			}
		}
	case "stop":
		for _, unit := range units {
			f.active[unit] = false
			f.status[unit] = ""
		}
	case "kill":
		if f.failKill || (f.failKillUnit != "" && slices.Contains(units, f.failKillUnit)) {
			return "", errors.New("kill failed")
		}
		if f.onKill != nil {
			f.onKill()
		}
		if f.ignoreKill {
			return "", nil
		}
		for _, unit := range units {
			f.status[unit] = handover.StatusDraining
			if peer := f.peer(unit); f.active[peer] && f.status[peer] == handover.StatusStandby && !f.noTakeOver {
				f.status[peer] = handover.StatusActive
			}
		}
	}
	return "", nil
}

// peer is the unit in the other slot of unit's kind.
func (f *fakeSystemd) peer(unit string) string {
	for _, slot := range bundle.Slots {
		if unit == bundle.AgentUnit(slot) {
			return bundle.AgentUnit(other(slot))
		}
		if unit == bundle.ProxyUnit(slot) {
			return bundle.ProxyUnit(other(slot))
		}
	}
	return ""
}

func unitArgs(args []string) []string {
	var units []string
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			units = append(units, arg)
		}
	}
	return units
}

// ran reports whether a command with exactly these arguments ran, and
// clears what was recorded when clear is set.
func (f *fakeSystemd) ran(args ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.commands, func(command []string) bool { return slices.Equal(command, args) })
}

func (f *fakeSystemd) ranAny(verb string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var got [][]string
	for _, command := range f.commands {
		if command[0] == verb {
			got = append(got, command)
		}
	}
	return got
}

func (f *fakeSystemd) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = nil
}

type box struct {
	cfg     Config
	systemd *fakeSystemd
	dir     string
}

func newBox(t *testing.T, handOver bool) *box {
	t.Helper()
	dir := t.TempDir()
	systemd := newFakeSystemd()
	b := &box{dir: dir, systemd: systemd, cfg: Config{
		SystemdDir: filepath.Join(dir, "etc", "systemd", "system"),
		StateFile:  filepath.Join(dir, "var", "lib", "discobox", "pool-units.json"),
		Binary:     filepath.Join(dir, "discobox-pool-agent"),
		Inputs:     []string{filepath.Join(dir, "bootstrap.json")},
		Sockets: []Socket{
			{Name: IngressSocket, Listen: "0.0.0.0:7443", Role: RoleAgent},
			{Name: AssetsSocket, Listen: "/var/lib/discobox/proxy/assets.sock", Role: RoleAgent},
			{Name: ProxySocket, Listen: "0.0.0.0:17080", Role: RoleProxy},
			{Name: MediatorSocket, Listen: "0.0.0.0:17081", Role: RoleMediator},
		},
		HandOver:  handOver,
		Booted:    true,
		Systemctl: systemd.systemctl,
		TakeOver:  2 * time.Second,
	}}
	b.write(t, b.cfg.Binary, "agent v1")
	b.write(t, b.cfg.Inputs[0], `{"poolId":"pool-1"}`)
	for _, template := range []string{bundle.AgentTemplate, bundle.ProxyTemplate} {
		b.write(t, filepath.Join(b.cfg.SystemdDir, template), "[Service]\n")
	}
	return b
}

func (b *box) write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (b *box) apply(t *testing.T) {
	t.Helper()
	if err := Apply(context.Background(), b.cfg); err != nil {
		t.Fatal(err)
	}
}

func (b *box) slot(t *testing.T) string {
	t.Helper()
	current, err := readState(b.cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	return current.Slot
}

// The first apply renders a socket unit for each of the pool's sockets, and
// the slots' drop-ins naming theirs, binds them, and starts and enables the
// first slot alone.
func TestTheFirstApplyBindsTheSocketsAndStartsTheFirstSlot(t *testing.T) {
	b := newBox(t, true)
	b.apply(t)

	ingress, err := os.ReadFile(filepath.Join(b.cfg.SystemdDir, "discobox-pool-ingress.socket"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ListenStream=0.0.0.0:7443\n", "FileDescriptorName=ingress\n", "Service=" + bundle.UnitsInstance("ingress") + "\n", "WantedBy=sockets.target\n"} {
		if !strings.Contains(string(ingress), want) {
			t.Errorf("the ingress socket's unit lacks %q:\n%s", want, ingress)
		}
	}
	assets, _ := os.ReadFile(filepath.Join(b.cfg.SystemdDir, "discobox-pool-assets.socket"))
	if !strings.Contains(string(assets), "SocketMode=0600\n") {
		t.Errorf("a Unix socket is not root's alone:\n%s", assets)
	}
	mediator, _ := os.ReadFile(filepath.Join(b.cfg.SystemdDir, "discobox-pool-buildkit.socket"))
	if !strings.Contains(string(mediator), "Service="+bundle.MediatorUnit+"\n") {
		t.Errorf("the mediator's socket does not start the mediator:\n%s", mediator)
	}
	agentDropIn, _ := os.ReadFile(filepath.Join(b.cfg.SystemdDir, bundle.AgentTemplate+".d", "sockets.conf"))
	proxyDropIn, _ := os.ReadFile(filepath.Join(b.cfg.SystemdDir, bundle.ProxyTemplate+".d", "sockets.conf"))
	if !strings.Contains(string(agentDropIn), "Sockets=discobox-pool-ingress.socket\n") || !strings.Contains(string(agentDropIn), "Sockets=discobox-pool-assets.socket\n") || strings.Contains(string(agentDropIn), "proxy.socket") {
		t.Errorf("the agent's drop-in names the wrong sockets:\n%s", agentDropIn)
	}
	if !strings.Contains(string(proxyDropIn), "Sockets=discobox-pool-proxy.socket\n") || strings.Contains(string(proxyDropIn), "ingress") {
		t.Errorf("the proxy's drop-in names the wrong sockets:\n%s", proxyDropIn)
	}

	if !b.systemd.ran("start", bundle.AgentUnit("a"), bundle.ProxyUnit("a")) {
		t.Errorf("the first slot was not started: %q", b.systemd.commands)
	}
	if !b.systemd.ran("enable", bundle.AgentUnit("a"), bundle.ProxyUnit("a")) || !b.systemd.ran("disable", bundle.AgentUnit("b"), bundle.ProxyUnit("b")) {
		t.Errorf("the first slot alone is not enabled: %q", b.systemd.commands)
	}
	if len(b.systemd.ranAny("kill")) != 0 {
		t.Errorf("a first apply told something to drain: %q", b.systemd.commands)
	}
	if got := b.slot(t); got != "a" {
		t.Fatalf("slot recorded = %q, want a", got)
	}
}

// An apply when nothing moved starts, restarts and drains nothing.
func TestAnApplyWhenNothingMovedMovesNothing(t *testing.T) {
	b := newBox(t, true)
	b.apply(t)
	b.systemd.reset()
	b.apply(t)
	for _, verb := range []string{"start", "restart", "try-restart", "stop", "kill"} {
		if got := b.systemd.ranAny(verb); len(got) != 0 {
			t.Errorf("an apply that moved nothing ran %q", got)
		}
	}
}

// A pool that does not ask for a handover restarts its running slot when what
// it starts with moves, as every pool did before.
func TestAPoolThatDoesNotHandOverRestartsItsSlot(t *testing.T) {
	b := newBox(t, false)
	b.apply(t)
	b.systemd.reset()
	b.write(t, b.cfg.Inputs[0], `{"poolId":"pool-1","controlPlaneUrl":"https://moved"}`)
	b.apply(t)
	if !b.systemd.ran("restart", bundle.AgentUnit("a"), bundle.ProxyUnit("a")) {
		t.Fatalf("a moved placement did not restart the running slot: %q", b.systemd.commands)
	}
	if len(b.systemd.ranAny("kill")) != 0 || b.slot(t) != "a" {
		t.Fatalf("a pool that does not hand over handed over: %q, slot %s", b.systemd.commands, b.slot(t))
	}
}

// A pool that asks for a handover starts the idle slot beside the running
// one, records it and runs it at boot in place of the old, then tells the
// running one to drain and waits for the new one to take over.
func TestAPoolThatHandsOverStartsTheIdleSlotAndDrainsTheOld(t *testing.T) {
	b := newBox(t, true)
	b.apply(t)
	b.systemd.active[bundle.MediatorUnit] = true
	b.systemd.reset()
	b.write(t, b.cfg.Binary, "agent v2")
	var slotAtKill string
	b.systemd.onKill = func() {
		if slotAtKill == "" {
			slotAtKill = b.slot(t)
		}
	}
	b.apply(t)
	if slotAtKill != "b" {
		t.Errorf("the slot recorded as the old one was told to drain = %q, want b", slotAtKill)
	}

	commands := b.systemd.commands
	start := slices.IndexFunc(commands, func(c []string) bool {
		return slices.Equal(c, []string{"start", bundle.AgentUnit("b"), bundle.ProxyUnit("b")})
	})
	kill := slices.IndexFunc(commands, func(c []string) bool { return c[0] == "kill" && c[len(c)-1] == bundle.AgentUnit("a") })
	enable := slices.IndexFunc(commands, func(c []string) bool {
		return slices.Equal(c, []string{"enable", bundle.AgentUnit("b"), bundle.ProxyUnit("b")})
	})
	if start < 0 || kill < 0 || enable < 0 || start >= enable || enable >= kill {
		t.Fatalf("a handover ran %q; want the new slot started, then enabled, then the old told to drain", commands)
	}
	if !slices.Contains(commands[kill], "--signal=SIGUSR2") {
		t.Errorf("the old slot was told to drain with %q", commands[kill])
	}
	if !b.systemd.ran("kill", "--kill-who=main", "--signal=SIGUSR2", bundle.ProxyUnit("a")) {
		t.Errorf("the old proxy was not told to drain: %q", commands)
	}
	if !b.systemd.ran("disable", bundle.AgentUnit("a"), bundle.ProxyUnit("a")) {
		t.Errorf("the old slot still runs at boot: %q", commands)
	}
	for _, c := range b.systemd.ranAny("stop") {
		if slices.Contains(c, bundle.AgentUnit("a")) || slices.Contains(c, bundle.ProxyUnit("a")) {
			t.Errorf("a handover stopped the old slot, cutting what it serves: %q", c)
		}
	}
	if len(b.systemd.ranAny("restart")) != 0 {
		t.Errorf("a handover restarted something: %q", commands)
	}
	// The mediator runs the moved binary: restarted, without the apply
	// waiting on its build drain.
	if !b.systemd.ran("try-restart", "--no-block", bundle.MediatorUnit) {
		t.Errorf("the mediator was not restarted without blocking: %q", commands)
	}
	if got := b.slot(t); got != "b" {
		t.Fatalf("slot recorded = %q, want b", got)
	}

	// The next handover, once the first slot has drained and exited, goes
	// back to it.
	for _, unit := range []string{bundle.AgentUnit("a"), bundle.ProxyUnit("a")} {
		b.systemd.active[unit], b.systemd.status[unit] = false, ""
	}
	b.systemd.reset()
	b.write(t, b.cfg.Inputs[0], `{"poolId":"pool-1","moved":true}`)
	b.apply(t)
	if !b.systemd.ran("start", bundle.AgentUnit("a"), bundle.ProxyUnit("a")) {
		t.Fatalf("the second handover ran %q; want the first slot started again", b.systemd.commands)
	}
	if got := b.slot(t); got != "a" {
		t.Fatalf("slot recorded = %q, want a", got)
	}
}

// A new slot that does not come up is stopped, and the old one serves on, as
// it did: nothing told it to drain.
func TestANewSlotThatFailsLeavesTheOldServing(t *testing.T) {
	b := newBox(t, true)
	b.apply(t)
	b.systemd.reset()
	b.systemd.failStart = bundle.AgentUnit("b")
	b.write(t, b.cfg.Binary, "agent v2")
	if err := Apply(context.Background(), b.cfg); err == nil {
		t.Fatal("an apply whose new slot failed to start succeeded")
	}
	if len(b.systemd.ranAny("kill")) != 0 {
		t.Fatalf("the old slot was told to drain though the new one failed: %q", b.systemd.commands)
	}
	if !b.systemd.ran("stop", bundle.ProxyUnit("b"), bundle.AgentUnit("b")) {
		t.Errorf("the failed slot was not stopped: %q", b.systemd.commands)
	}
	if got := b.slot(t); got != "a" {
		t.Fatalf("slot recorded = %q, want a", got)
	}

	// The next apply tries the handover again.
	b.systemd.failStart = ""
	b.systemd.reset()
	b.apply(t)
	if got := b.slot(t); got != "b" {
		t.Fatalf("slot after the retry = %q, want b", got)
	}
}

// A pool whose agent binds its own listener cannot share it with a new agent,
// so it restarts, whatever its bootstrap asks.
func TestAPoolWhoseIngressIsNotHeldRestarts(t *testing.T) {
	b := newBox(t, true)
	b.cfg.Sockets = slices.DeleteFunc(b.cfg.Sockets, func(s Socket) bool { return s.Name == IngressSocket })
	b.apply(t)
	b.systemd.reset()
	b.write(t, b.cfg.Binary, "agent v2")
	b.apply(t)
	if !b.systemd.ran("restart", bundle.AgentUnit("a"), bundle.ProxyUnit("a")) || len(b.systemd.ranAny("kill")) != 0 {
		t.Fatalf("ran %q; want the running slot restarted", b.systemd.commands)
	}
}

// A box provisioned before slots ran the agent and the proxy as units of
// their own, which bound their sockets: they are stopped and removed once,
// and the first slot started on the sockets systemd now holds.
func TestTheUnitsFromBeforeSlotsAreRetiredOnce(t *testing.T) {
	b := newBox(t, true)
	legacy := filepath.Join(b.cfg.SystemdDir, legacyAgentUnit)
	b.write(t, legacy, "[Service]\n")
	b.write(t, filepath.Join(b.cfg.SystemdDir, legacyProxyUnit), "[Service]\n")
	b.apply(t)
	if !b.systemd.ran("stop", legacyProxyUnit, legacyAgentUnit) || !b.systemd.ran("disable", legacyProxyUnit, legacyAgentUnit) {
		t.Fatalf("the units from before slots were not stopped and disabled: %q", b.systemd.commands)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the agent's unit from before slots is still laid: %v", err)
	}
	if !b.systemd.ran("start", bundle.AgentUnit("a"), bundle.ProxyUnit("a")) {
		t.Fatalf("the first slot was not started: %q", b.systemd.commands)
	}
}

// A first upgrade whose slot fails to come up leaves the pool served: the
// units from before slots are put back, their files kept, the slots and
// their sockets stopped and disabled, and the next apply retires them again.
func TestAFailedFirstUpgradePutsTheUnitsFromBeforeSlotsBack(t *testing.T) {
	b := newBox(t, true)
	legacy := filepath.Join(b.cfg.SystemdDir, legacyAgentUnit)
	b.write(t, legacy, "[Service]\n")
	b.write(t, filepath.Join(b.cfg.SystemdDir, legacyProxyUnit), "[Service]\n")
	b.systemd.failStart = bundle.AgentUnit("a")
	if err := Apply(context.Background(), b.cfg); err == nil {
		t.Fatal("an apply whose first slot failed to start succeeded")
	}
	if !b.systemd.ran("enable", "--now", legacyAgentUnit, legacyProxyUnit) {
		t.Fatalf("the units from before slots were not put back: %q", b.systemd.commands)
	}
	if !slices.ContainsFunc(b.systemd.commands, func(c []string) bool {
		return c[0] == "disable" && slices.Contains(c, "--now") && slices.Contains(c, bundle.AgentUnit("a"))
	}) {
		t.Fatalf("the slot was left enabled beside the units it replaced: %q", b.systemd.commands)
	}
	if !slices.ContainsFunc(b.systemd.commands, func(c []string) bool {
		return c[0] == "disable" && slices.Contains(c, "--now") && slices.Contains(c, "discobox-pool-ingress.socket")
	}) {
		t.Fatalf("the slot's sockets were left holding the ports: %q", b.systemd.commands)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("the agent's unit from before slots is gone after a failed upgrade: %v", err)
	}
	if !b.systemd.ran("stop", unitsInstances) {
		t.Fatalf("a units start traffic began was left to start a slot beside them: %q", b.systemd.commands)
	}
	for _, socket := range b.cfg.Sockets {
		if _, err := os.Stat(filepath.Join(b.cfg.SystemdDir, socket.Unit())); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s is left laid, so the next apply would not bind it: %v", socket.Unit(), err)
		}
	}

	b.systemd.failStart = ""
	b.systemd.reset()
	b.apply(t)
	if !b.systemd.ran("start", bundle.AgentUnit("a"), bundle.ProxyUnit("a")) {
		t.Fatalf("the next apply did not start the first slot: %q", b.systemd.commands)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the agent's unit from before slots is still laid once the slot serves: %v", err)
	}
	if !slices.ContainsFunc(b.systemd.commands, func(c []string) bool {
		return c[0] == "enable" && slices.Contains(c, "--now") && slices.Contains(c, "discobox-pool-buildkit.socket")
	}) {
		t.Fatalf("the next apply did not bind the mediator's sockets: %q", b.systemd.commands)
	}
}

// A socket whose address moved cannot be handed over: every slot socket
// closes first, so no traffic starts anything on them meanwhile, what served it
// stops, the socket is bound at its new address, and the slot starts on it.
// The mediator's are bound again only once the slot serves, since its stop
// waits out its builds.
func TestAMovedSocketIsBoundAgainUnderItsSlot(t *testing.T) {
	b := newBox(t, true)
	b.apply(t)
	b.systemd.active[bundle.MediatorUnit] = true
	b.systemd.reset()
	b.cfg.Sockets[0].Listen = "0.0.0.0:7444"
	b.cfg.Sockets[3].Listen = "0.0.0.0:17082"
	b.apply(t)
	commands := b.systemd.commands
	index := func(want ...string) int {
		return slices.IndexFunc(commands, func(c []string) bool { return slices.Equal(c, want) })
	}
	closeSocket := index("stop", "discobox-pool-ingress.socket", "discobox-pool-assets.socket", "discobox-pool-proxy.socket")
	stopSlot := slices.IndexFunc(commands, func(c []string) bool { return c[0] == "stop" && slices.Contains(c, bundle.AgentUnit("a")) })
	bind := index("enable", "--now", "discobox-pool-ingress.socket", "discobox-pool-assets.socket", "discobox-pool-proxy.socket")
	start := index("start", bundle.AgentUnit("a"), bundle.ProxyUnit("a"))
	stopMediator := index("stop", bundle.MediatorUnit)
	startMediator := index("start", bundle.MediatorUnit)
	if closeSocket < 0 || stopSlot < 0 || bind < 0 || start < 0 || closeSocket >= stopSlot || stopSlot >= bind || bind >= start {
		t.Fatalf("ran %q; want the socket closed, the slots stopped, the socket bound again, then the slot started", commands)
	}
	if stopMediator < start || startMediator < stopMediator {
		t.Fatalf("ran %q; want the mediator stopped and started again only once the slot serves", commands)
	}
	if slices.Contains(commands[stopSlot], bundle.MediatorUnit) {
		t.Errorf("the mediator was stopped with the slots: %q", commands[stopSlot])
	}

	// One no longer rendered is closed and removed.
	b.systemd.reset()
	b.cfg.Sockets = slices.DeleteFunc(b.cfg.Sockets, func(s Socket) bool { return s.Name == AssetsSocket })
	b.apply(t)
	if !b.systemd.ran("disable", "--now", "discobox-pool-assets.socket") {
		t.Fatalf("a socket no longer the pool's was not closed: %q", b.systemd.commands)
	}
	if _, err := os.Stat(filepath.Join(b.cfg.SystemdDir, "discobox-pool-assets.socket")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a socket no longer the pool's is still laid: %v", err)
	}
}

// A new slot that does not take over once the old one was told to drain is
// stopped, and the old slot is started again, so the pool is never left with
// A new slot that does not take over once the old one drains is left
// running, for systemd to restart until it does, and the old one is left to
// finish its drain: nothing that drains is stopped or restarted. The apply
// fails, with the new slot recorded.
func TestANewSlotThatDoesNotTakeOverLeavesTheOldToDrain(t *testing.T) {
	b := newBox(t, true)
	b.cfg.TakeOver = 200 * time.Millisecond
	b.apply(t)
	b.systemd.noTakeOver = true
	b.systemd.reset()
	b.write(t, b.cfg.Binary, "agent v2")
	if err := Apply(context.Background(), b.cfg); !errors.Is(err, ErrNotTakenOver) {
		t.Fatalf("an apply whose new slot did not take over = %v, want ErrNotTakenOver", err)
	}
	for _, unit := range []string{bundle.AgentUnit("a"), bundle.ProxyUnit("a")} {
		if b.systemd.ran("restart", bundle.AgentUnit("a"), bundle.ProxyUnit("a")) || slices.ContainsFunc(b.systemd.commands, func(c []string) bool { return c[0] == "stop" && slices.Contains(c, unit) }) {
			t.Fatalf("the draining slot was stopped or restarted: %q", b.systemd.commands)
		}
		if got := b.systemd.status[unit]; got != handover.StatusDraining {
			t.Fatalf("%s says %q, want draining", unit, got)
		}
	}
	if !b.systemd.active[bundle.AgentUnit("b")] || !b.systemd.active[bundle.ProxyUnit("b")] {
		t.Fatalf("the new slot was stopped: %q", b.systemd.commands)
	}
	if got := b.slot(t); got != "b" {
		t.Fatalf("slot recorded = %q, want b", got)
	}

	// The next apply finds the old slot still draining and leaves it.
	b.systemd.reset()
	b.write(t, b.cfg.Binary, "agent v3")
	err := Apply(context.Background(), b.cfg)
	if !errors.Is(err, ErrNotTakenOver) || errors.Is(err, ErrSlotDraining) || !strings.Contains(err.Error(), "still drains") {
		t.Fatalf("an apply over a slot still draining, its successor not taken over = %v, want ErrNotTakenOver alone, saying what drains", err)
	}
	if slices.ContainsFunc(b.systemd.commands, func(c []string) bool {
		return (c[0] == "stop" || c[0] == "restart" || c[0] == "start") && (slices.Contains(c, bundle.AgentUnit("a")) || slices.Contains(c, bundle.ProxyUnit("a")))
	}) {
		t.Fatalf("an apply over a draining slot touched it: %q", b.systemd.commands)
	}
}

// A handover whose new slot never took over is reported by every apply
// after it that moves nothing, until that slot says it is active: the pool
// is not served as it should be, and nothing says so but this.
func TestATakeOverThatStalledIsReportedUntilItTakes(t *testing.T) {
	b := newBox(t, true)
	b.cfg.TakeOver = 200 * time.Millisecond
	b.apply(t)
	b.systemd.noTakeOver = true
	b.systemd.reset()
	b.write(t, b.cfg.Binary, "agent v2")
	if err := Apply(context.Background(), b.cfg); !errors.Is(err, ErrNotTakenOver) {
		t.Fatalf("an apply whose new slot did not take over = %v, want ErrNotTakenOver", err)
	}

	// The old slot has exited; the new one is still a standby.
	for _, unit := range []string{bundle.AgentUnit("a"), bundle.ProxyUnit("a")} {
		b.systemd.active[unit] = false
	}
	b.systemd.reset()
	if err := Apply(context.Background(), b.cfg); !errors.Is(err, ErrNotTakenOver) || errors.Is(err, ErrSlotDraining) {
		t.Fatalf("an apply that moved nothing, the new slot a standby = %v, want ErrNotTakenOver alone", err)
	}

	// Once it says it is active, the apply succeeds and stops reporting it.
	for _, unit := range []string{bundle.AgentUnit("b"), bundle.ProxyUnit("b")} {
		b.systemd.status[unit] = handover.StatusActive
	}
	b.systemd.reset()
	if err := Apply(context.Background(), b.cfg); err != nil {
		t.Fatalf("an apply once the new slot took over = %v", err)
	}
	b.systemd.reset()
	if err := Apply(context.Background(), b.cfg); err != nil {
		t.Fatalf("the apply after that = %v", err)
	}
}

// stall leaves b's new slot recorded as not taken over, the old slot a
// draining.
func (b *box) stall(t *testing.T) {
	t.Helper()
	b.cfg.TakeOver = 200 * time.Millisecond
	b.apply(t)
	b.systemd.noTakeOver = true
	b.systemd.reset()
	b.write(t, b.cfg.Binary, "agent v2")
	if err := Apply(context.Background(), b.cfg); !errors.Is(err, ErrNotTakenOver) {
		t.Fatalf("an apply whose new slot did not take over = %v, want ErrNotTakenOver", err)
	}
	b.systemd.noTakeOver = false
}

func (b *box) pending(t *testing.T) bool {
	t.Helper()
	current, err := readState(b.cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	return current.TakeOverPending
}

// A stalled takeover is judged after what the apply repairs: an old agent
// found serving again is told to drain (finishHandOver), and once the new
// slot takes over the same apply succeeds.
func TestAStalledTakeOverStillFinishesAHandOver(t *testing.T) {
	b := newBox(t, true)
	b.stall(t)
	// The old agent came back serving; the new proxy took over meanwhile.
	b.systemd.status[bundle.AgentUnit("a")] = handover.StatusActive
	b.systemd.status[bundle.ProxyUnit("b")] = handover.StatusActive
	b.systemd.reset()
	if err := Apply(context.Background(), b.cfg); err != nil {
		t.Fatalf("an apply that finished the handover = %v", err)
	}
	if !slices.ContainsFunc(b.systemd.commands, func(c []string) bool { return c[0] == "kill" && slices.Contains(c, bundle.AgentUnit("a")) }) {
		t.Fatalf("the old agent serving again was not told to drain: %q", b.systemd.commands)
	}
	if b.pending(t) {
		t.Fatal("the takeover is still recorded pending once both took over")
	}
}

// A new proxy that never took over is the stall too, though its agent did.
func TestANewProxyThatDidNotTakeOverIsAStall(t *testing.T) {
	b := newBox(t, true)
	b.stall(t)
	b.systemd.status[bundle.AgentUnit("b")] = handover.StatusActive
	b.systemd.reset()
	err := Apply(context.Background(), b.cfg)
	if !errors.Is(err, ErrNotTakenOver) || !strings.Contains(err.Error(), bundle.ProxyUnit("b")) {
		t.Fatalf("an apply whose new proxy is a standby = %v, want ErrNotTakenOver naming it", err)
	}
	if !b.pending(t) {
		t.Fatal("the stall is no longer recorded")
	}
}

// A moved bundle whose new slot fails to start leaves the stall recorded:
// the slot it falls back to is the one that never took over.
func TestAFailedHandOverFromAStallKeepsItRecorded(t *testing.T) {
	b := newBox(t, true)
	b.stall(t)
	for _, unit := range []string{bundle.AgentUnit("a"), bundle.ProxyUnit("a")} {
		b.systemd.active[unit] = false
		b.systemd.status[unit] = ""
	}
	b.systemd.failStart = bundle.AgentUnit("a")
	b.systemd.reset()
	b.write(t, b.cfg.Binary, "agent v3")
	if err := Apply(context.Background(), b.cfg); err == nil || errors.Is(err, ErrSlotDraining) {
		t.Fatalf("an apply whose new slot failed to start = %v, want that failure", err)
	}
	if got := b.slot(t); got != "b" || !b.pending(t) {
		t.Fatalf("recorded slot %q, pending %v; want b, still pending", got, b.pending(t))
	}
}

// A box whose state names no slot (one recorded before slots) falls back,
// when its new slot fails to start, to the slot it serves from, recorded and
// enabled by name.
func TestAFailedHandOverWithNoSlotRecordedFallsBackToTheServingSlot(t *testing.T) {
	b := newBox(t, true)
	b.apply(t)
	current, err := readState(b.cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	current.Slot = ""
	if err := writeState(b.cfg.StateFile, current); err != nil {
		t.Fatal(err)
	}
	b.systemd.failStart = bundle.AgentUnit("b")
	b.systemd.reset()
	b.write(t, b.cfg.Binary, "agent v2")
	if err := Apply(context.Background(), b.cfg); err == nil {
		t.Fatal("an apply whose new slot failed to start succeeded")
	}
	if got := b.slot(t); got != "a" {
		t.Fatalf("slot recorded = %q, want a", got)
	}
	if !b.systemd.ran("enable", bundle.AgentUnit("a"), bundle.ProxyUnit("a")) {
		t.Fatalf("the serving slot is not run at boot again: %q", b.systemd.commands)
	}
}

// A new slot that does not take over while the old one never began to drain
// is stopped, and the old one serves on as it was.
func TestANewSlotThatDoesNotTakeOverBeforeAnyDrainIsStopped(t *testing.T) {
	b := newBox(t, true)
	b.cfg.TakeOver = 200 * time.Millisecond
	b.apply(t)
	b.systemd.noTakeOver = true
	// The old agent took the signal, but says it serves on.
	b.systemd.onKill = func() {
		for _, unit := range []string{bundle.AgentUnit("a"), bundle.ProxyUnit("a")} {
			b.systemd.status[unit] = handover.StatusActive
		}
	}
	b.systemd.ignoreKill = true
	b.systemd.reset()
	b.write(t, b.cfg.Binary, "agent v2")
	if err := Apply(context.Background(), b.cfg); err == nil {
		t.Fatal("an apply whose new slot did not take over succeeded")
	}
	if b.systemd.active[bundle.AgentUnit("b")] || b.systemd.active[bundle.ProxyUnit("b")] {
		t.Fatalf("the new slot is left running beside an old one that never drained: %q", b.systemd.commands)
	}
	if got := b.slot(t); got != "a" {
		t.Fatalf("slot recorded = %q, want a", got)
	}
	if !b.systemd.ran("enable", bundle.AgentUnit("a"), bundle.ProxyUnit("a")) {
		t.Fatalf("the old slot is not run at boot again: %q", b.systemd.commands)
	}
}

// A new slot whose old one cannot be told to drain is stopped, and the old
// slot, never told, serves on uncut.
func TestAnOldSlotThatCannotBeToldIsLeftServing(t *testing.T) {
	b := newBox(t, true)
	b.apply(t)
	b.systemd.failKill = true
	b.systemd.reset()
	b.write(t, b.cfg.Binary, "agent v2")
	if err := Apply(context.Background(), b.cfg); err == nil {
		t.Fatal("an apply whose old slot could not be told to drain succeeded")
	}
	if len(b.systemd.ranAny("restart")) != 0 {
		t.Fatalf("the old slot was restarted, cutting what it serves, though it was never told to drain: %q", b.systemd.commands)
	}
	if b.systemd.active[bundle.AgentUnit("b")] || b.systemd.active[bundle.ProxyUnit("b")] {
		t.Fatalf("the new slot is left running beside the old: %q", b.systemd.commands)
	}
	if !b.systemd.active[bundle.AgentUnit("a")] || b.systemd.status[bundle.AgentUnit("a")] != handover.StatusActive {
		t.Fatalf("the old slot is not serving: %q", b.systemd.commands)
	}
}

// A bundle applied where systemd does not run, in an image's build, renders
// and starts nothing: no bootstrap says what the sockets are.
func TestAnApplyWhereSystemdDoesNotRunDoesNothing(t *testing.T) {
	b := newBox(t, true)
	b.cfg.Booted = false
	b.apply(t)
	if len(b.systemd.commands) != 0 {
		t.Fatalf("ran %q where systemd does not run", b.systemd.commands)
	}
	if _, err := os.Stat(filepath.Join(b.cfg.SystemdDir, "discobox-pool-ingress.socket")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rendered a socket where systemd does not run: %v", err)
	}
}

// What a socket starts when traffic finds no slot serving it is the slot the
// box runs.
func TestStartStartsTheSlotTheBoxRuns(t *testing.T) {
	b := newBox(t, true)
	b.apply(t)
	b.write(t, b.cfg.Binary, "agent v2")
	b.apply(t)
	b.systemd.reset()
	if err := Start(context.Background(), b.cfg); err != nil {
		t.Fatal(err)
	}
	if !b.systemd.ran("start", "--no-block", bundle.AgentUnit("b"), bundle.ProxyUnit("b")) {
		t.Fatalf("ran %q; want the slot the box runs started", b.systemd.commands)
	}
}

// A socket's start while an apply runs waits it out, starting nothing
// meanwhile: the apply starts the slot once it has bound its sockets, and a
// start that returned at once would have its socket start it again and
// again, to the socket's trigger limit. Once the apply is done, it starts the
// slot the box runs, which the apply has already started.
func TestStartWaitsOutAnApplyUnderWay(t *testing.T) {
	b := newBox(t, true)
	b.apply(t)
	lock, err := handover.Acquire(context.Background(), b.cfg.StateFile+".lock")
	if err != nil {
		t.Fatal(err)
	}
	b.systemd.reset()
	done := make(chan error, 1)
	go func() { done <- Start(context.Background(), b.cfg) }()
	select {
	case err := <-done:
		_ = lock.Release()
		t.Fatalf("start returned (%v) while an apply held the units", err)
	case <-time.After(300 * time.Millisecond):
	}
	if len(b.systemd.ranAny("start")) != 0 {
		_ = lock.Release()
		t.Fatalf("start ran %q while an apply held the units", b.systemd.commands)
	}
	_ = lock.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start did not end once the apply had")
	}
	if !b.systemd.ran("start", "--no-block", bundle.AgentUnit("a"), bundle.ProxyUnit("a")) {
		t.Fatalf("start did not start the slot the box runs: %q", b.systemd.commands)
	}
}

// A handover whose drain signal reached the old agent but not the old proxy
// is finished by the next apply, which tells the proxy, though nothing it
// fingerprints moved.
func TestAnApplyFinishesAHandOverLeftHalfTold(t *testing.T) {
	b := newBox(t, true)
	b.cfg.TakeOver = 200 * time.Millisecond
	b.apply(t)
	b.systemd.failKillUnit = bundle.ProxyUnit("a")
	b.systemd.reset()
	b.write(t, b.cfg.Binary, "agent v2")
	if err := Apply(context.Background(), b.cfg); err == nil {
		t.Fatal("an apply that could not tell the old proxy to drain succeeded")
	}
	if got := b.systemd.status[bundle.ProxyUnit("a")]; got == handover.StatusDraining {
		t.Fatal("the old proxy drains though it was never told")
	}
	if got := b.slot(t); got != "b" {
		t.Fatalf("slot recorded = %q, want b", got)
	}

	b.systemd.failKillUnit = ""
	b.systemd.reset()
	b.apply(t)
	if !b.systemd.ran("kill", "--kill-who=main", "--signal=SIGUSR2", bundle.ProxyUnit("a")) {
		t.Fatalf("the next apply did not tell the old proxy to drain: %q", b.systemd.commands)
	}
	if b.systemd.ran("kill", "--kill-who=main", "--signal=SIGUSR2", bundle.AgentUnit("a")) {
		t.Fatalf("the next apply told the already draining agent again: %q", b.systemd.commands)
	}
}

// A slot whose units have exited is no drain, whatever status they last
// said: a handover to it goes ahead.
func TestAnExitedSlotIsNoDrain(t *testing.T) {
	b := newBox(t, true)
	b.apply(t)
	for _, unit := range []string{bundle.AgentUnit("b"), bundle.ProxyUnit("b")} {
		b.systemd.active[unit], b.systemd.status[unit] = false, handover.StatusDraining
	}
	b.systemd.reset()
	b.write(t, b.cfg.Binary, "agent v2")
	b.apply(t)
	if got := b.slot(t); got != "b" {
		t.Fatalf("slot recorded = %q, want b: a slot whose units exited was taken for one draining", got)
	}
}

// A standby of the old slot, an old process restarted mid-drain, is stopped
// by the next apply rather than told to drain: it serves nothing, and a
// drain would leave it waiting on the serving slot's lock.
func TestAnApplyStopsAStandbyOfTheOldSlot(t *testing.T) {
	b := newBox(t, true)
	b.apply(t)
	b.systemd.reset()
	b.write(t, b.cfg.Binary, "agent v2")
	b.apply(t)
	if got := b.slot(t); got != "b" {
		t.Fatalf("slot recorded = %q, want b", got)
	}
	// The old agent restarted as a standby.
	b.systemd.active[bundle.AgentUnit("a")], b.systemd.status[bundle.AgentUnit("a")] = true, handover.StatusStandby
	b.systemd.reset()
	b.apply(t)
	if b.systemd.ran("kill", "--kill-who=main", "--signal=SIGUSR2", bundle.AgentUnit("a")) {
		t.Fatalf("a standby of the old slot was told to drain: %q", b.systemd.commands)
	}
	if !b.systemd.ran("stop", bundle.AgentUnit("a")) {
		t.Fatalf("a standby of the old slot was not stopped: %q", b.systemd.commands)
	}
}

// A socket change while a slot still drains from the last handover binds
// nothing, and records nothing, so the draining slot is not cut; the next
// apply once it has exited binds the sockets.
func TestASocketChangeWaitsOutADrainingSlot(t *testing.T) {
	b := newBox(t, true)
	b.apply(t)
	b.systemd.reset()
	b.write(t, b.cfg.Binary, "agent v2")
	b.apply(t)
	if got := b.slot(t); got != "b" {
		t.Fatalf("slot recorded = %q, want b", got)
	}
	// Slot a drains; a socket moves.
	b.cfg.Sockets[0].Listen = "0.0.0.0:7444"
	b.systemd.reset()
	err := Apply(context.Background(), b.cfg)
	if err == nil || !strings.Contains(err.Error(), "still drains") {
		t.Fatalf("a socket change over a draining slot = %v, want it refused", err)
	}
	if slices.ContainsFunc(b.systemd.commands, func(c []string) bool {
		return c[0] == "stop" && (slices.Contains(c, bundle.AgentUnit("a")) || slices.Contains(c, bundle.ProxyUnit("a")))
	}) {
		t.Fatalf("the draining slot was stopped for a socket change: %q", b.systemd.commands)
	}

	// Slot a has exited: the next apply binds the moved socket.
	for _, unit := range []string{bundle.AgentUnit("a"), bundle.ProxyUnit("a")} {
		b.systemd.active[unit], b.systemd.status[unit] = false, ""
	}
	b.systemd.reset()
	b.apply(t)
	if !b.systemd.ran("enable", "--now", b.cfg.Sockets[0].Unit()) && !slices.ContainsFunc(b.systemd.commands, func(c []string) bool {
		return c[0] == "enable" && slices.Contains(c, "--now") && slices.Contains(c, b.cfg.Sockets[0].Unit())
	}) {
		t.Fatalf("the moved socket was not bound once the drain ended: %q", b.systemd.commands)
	}
}

// A slot socket that is not listening, which a refused start left down, is
// bound again by the next apply though nothing it fingerprints moved.
func TestAnApplyBindsASocketLeftDown(t *testing.T) {
	b := newBox(t, true)
	b.apply(t)
	down := b.cfg.Sockets[0].Unit()
	b.systemd.active[down] = false
	b.systemd.reset()
	b.apply(t)
	if !slices.ContainsFunc(b.systemd.commands, func(c []string) bool {
		return c[0] == "enable" && slices.Contains(c, "--now") && slices.Contains(c, down)
	}) {
		t.Fatalf("a socket left down was not bound again: %q", b.systemd.commands)
	}
	if !b.systemd.ran("stop", unitsInstances) {
		t.Fatalf("the sockets were bound with a socket's own units start still running: %q", b.systemd.commands)
	}
}

// A socket the pool no longer has is not closed while a slot drains: the
// draining slot still serves on it. Its unit file stays, so the apply once
// that slot has exited closes it.
func TestASocketDroppedWhileASlotDrainsClosesOnceItHasExited(t *testing.T) {
	b := newBox(t, true)
	b.apply(t)
	b.systemd.reset()
	b.write(t, b.cfg.Binary, "agent v2")
	b.apply(t)
	dropped := b.cfg.Sockets[len(b.cfg.Sockets)-1]
	b.cfg.Sockets = b.cfg.Sockets[:len(b.cfg.Sockets)-1]
	b.systemd.reset()
	if err := Apply(context.Background(), b.cfg); !errors.Is(err, ErrSlotDraining) {
		t.Fatalf("dropping a socket while a slot drains = %v, want ErrSlotDraining", err)
	}
	if slices.ContainsFunc(b.systemd.commands, func(c []string) bool { return slices.Contains(c, dropped.Unit()) }) {
		t.Fatalf("the dropped socket was touched while a slot drains: %q", b.systemd.commands)
	}
	if _, err := os.Stat(filepath.Join(b.cfg.SystemdDir, dropped.Unit())); err != nil {
		t.Fatalf("the dropped socket's unit file is gone before it was closed: %v", err)
	}

	for _, unit := range []string{bundle.AgentUnit("a"), bundle.ProxyUnit("a")} {
		b.systemd.active[unit], b.systemd.status[unit] = false, ""
	}
	b.systemd.reset()
	b.apply(t)
	if !b.systemd.ran("disable", "--now", dropped.Unit()) {
		t.Fatalf("the dropped socket was not closed once the drain ended: %q", b.systemd.commands)
	}
}

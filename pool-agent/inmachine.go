package poolagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pool-agent/childproc"
)

// A machine pool on a driver that can authenticate as its machine (boxd's
// in-machine token; disco-vm's Definition.InMachine) runs beside an endpoint
// that mints the machine owner's credential for any process on the machine
// that asks. The pool's engine is the only process here that may: the pool's
// proxy, which a sandbox's and a build step's egress goes through, and the
// builder, which fetches what a Dockerfile names itself, must never reach it
// (ADR 26-11-11-080 §9). The agent loads a rule set before anything else of
// the pool starts that refuses every connection to the endpoints but the
// engine's, which it marks.

const (
	// inMachineTable is the rule set's one table, the agent's own: loading it
	// replaces it alone, never another's, such as BuildKit's CNI rules or a
	// held driver's fence (discobox_fence).
	inMachineTable = "discobox_in_machine"
	// engineMark marks every connection the pool's engine opens (its
	// machine.Config.Dial), the one way past the rule set. Setting a mark
	// takes CAP_NET_ADMIN, which nothing of the pool but root holds, and
	// root on the pool's machine can mint the credential anyway (ADR
	// 26-11-11-080 §9, stated).
	engineMark = 0x64697363
)

// inMachineAuth is the in-machine auth the bootstrap's driver declares, nil
// for a driver that has none, and whether its configuration turns it on: the
// one place the bootstrap says how the pool's engine authenticates (with no
// key, as its machine), so nothing can say it twice and disagree.
func inMachineAuth(b Bootstrap) (*machine.InMachineAuth, bool, error) {
	def, err := machine.DefinitionOf(b.Driver)
	if err != nil {
		return nil, false, fmt.Errorf("the %s driver's definition: %w", b.Driver, err)
	}
	if def.InMachine == nil || len(bytes.TrimSpace(b.DriverConfig)) == 0 {
		return def.InMachine, false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b.DriverConfig, &fields); err != nil {
		return nil, false, fmt.Errorf("the %s driver's configuration: %w", b.Driver, err)
	}
	var on bool
	if raw, ok := fields[def.InMachine.Field]; ok {
		if err := json.Unmarshal(raw, &on); err != nil {
			return nil, false, fmt.Errorf("the %s driver's %s is not a boolean", b.Driver, def.InMachine.Field)
		}
	}
	return def.InMachine, on, nil
}

// fenceInMachine refuses every process on this machine but the pool's engine
// the endpoints its driver's in-machine auth names, whether its configuration
// uses them or not: they answer any process here (disco-vm's InMachineAuth).
// Each unit that carries a sandbox's traffic loads it before it starts
// (RunInMachineFence, its ExecStartPre), so none runs without it after a
// reboot, which clears the kernel's tables; the agent loads it too, so a
// machine that cannot fails the pool's start, where it is reported. Loading
// it again replaces the table with the same one.
func fenceInMachine(ctx context.Context, auth *machine.InMachineAuth, routeFile string) error {
	targets, err := inMachineTargets(ctx, auth.Endpoints, routeFile, net.DefaultResolver)
	if err != nil {
		return fmt.Errorf("the endpoints of this pool's in-machine credential: %w", err)
	}
	rules := inMachineRules(targets)
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := childproc.Run(cmd); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return errors.New("this pool's machine has no nft, which keeps its in-machine credential's endpoints from its proxy and builder (ADR 26-11-11-080 §9): give the provider a base that has nftables")
		}
		return fmt.Errorf("load the rule set that keeps this pool's in-machine credential from its proxy and builder: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// InMachineFenceCommand is the agent's subcommand that loads the rule set
// (RunInMachineFence): every unit that carries a sandbox's traffic — the
// proxy, BuildKit, its mediator, the registry — runs it as an ExecStartPre,
// and does not start when it fails.
const InMachineFenceCommand = "in-machine-fence"

// RunInMachineFence loads the rule set for the pool its bootstrap describes,
// read as the agent reads it (Load, Validate: the file its unit names, with
// the environment's overrides), and does nothing for a pool that has none to
// load: a Docker pool, or one whose driver declares no in-machine auth. A
// bootstrap it cannot read or that does not validate is an error, so the unit
// does not start: when in doubt, nothing that carries a sandbox's traffic
// runs unfenced.
func RunInMachineFence(ctx context.Context) error {
	bootstrap, err := Load()
	if err != nil {
		return err
	}
	if err := bootstrap.Validate(); err != nil {
		return err
	}
	if bootstrap.EffectiveRuntime() != RuntimeMachine {
		return nil
	}
	auth, _, err := inMachineAuth(bootstrap)
	if err != nil || auth == nil {
		return err
	}
	return fenceInMachine(ctx, auth, layout.Container().System("/proc/net/route"))
}

// inMachineTarget is one endpoint as the rule set refuses it: addresses and
// a TCP port.
type inMachineTarget struct {
	prefix netip.Prefix
	port   int
}

// inMachineTargets resolves each endpoint as this machine sees it now: the
// default gateway from the route table (as disco-vm's boxd driver finds it),
// an address or CIDR as given, and a host name to every address it resolves
// to. A machine whose gateway or name moves later is fenced as it was at
// this start, and again at the next.
func inMachineTargets(ctx context.Context, endpoints []machine.MachineEndpoint, routeFile string, resolver *net.Resolver) ([]inMachineTarget, error) {
	var targets []inMachineTarget
	add := func(addr netip.Addr, port int) {
		addr = addr.Unmap()
		target := inMachineTarget{prefix: netip.PrefixFrom(addr, addr.BitLen()), port: port}
		if !slices.Contains(targets, target) {
			targets = append(targets, target)
		}
	}
	for _, e := range endpoints {
		switch {
		case e.Gateway:
			gateway, err := defaultGateway(routeFile)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e, err)
			}
			add(gateway, e.Port)
		case e.Host != "":
			if addr, err := netip.ParseAddr(e.Host); err == nil {
				add(addr, e.Port)
				continue
			}
			if prefix, err := netip.ParsePrefix(e.Host); err == nil {
				targets = append(targets, inMachineTarget{prefix: prefix.Masked(), port: e.Port})
				continue
			}
			addrs, err := resolver.LookupNetIP(ctx, "ip", e.Host)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e, err)
			}
			for _, addr := range addrs {
				add(addr, e.Port)
			}
		default:
			return nil, fmt.Errorf("%s names neither the gateway nor a host", e)
		}
	}
	if len(targets) == 0 {
		return nil, errors.New("none is declared")
	}
	return targets, nil
}

// inMachineRules is the rule set: on output, every connection to a target
// that is not the engine's is reset; on forward, every one is, since nothing
// the machine routes (a build step's network namespace) is the engine. A
// reset, not a drop, so a refused caller fails at once rather than timing
// out. It replaces the table whole, so loading it again is the same table.
func inMachineRules(targets []inMachineTarget) string {
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s\ndelete table inet %s\ntable inet %s {\n", inMachineTable, inMachineTable, inMachineTable)
	for _, chain := range []struct{ name, guard string }{
		{"output", fmt.Sprintf("meta mark != 0x%x ", engineMark)},
		{"forward", ""},
	} {
		fmt.Fprintf(&b, "\tchain %s {\n\t\ttype filter hook %s priority filter; policy accept;\n", chain.name, chain.name)
		for _, t := range targets {
			family := "ip"
			if t.prefix.Addr().Is6() {
				family = "ip6"
			}
			fmt.Fprintf(&b, "\t\t%s%s daddr %s tcp dport %d reject with tcp reset\n", chain.guard, family, t.prefix, t.port)
		}
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// defaultGateway is the IPv4 gateway of the default route in a Linux route
// table (/proc/net/route): the address boxd's metadata adapter listens on.
// It reads the table exactly as disco-vm's boxd driver does (its unexported
// defaultGateway, vmtoken.go): the first default route whose gateway is not
// zero, its flags unread. The two must agree, since the address this fences
// is the one the driver asks.
func defaultGateway(routeFile string) (netip.Addr, error) {
	data, err := os.ReadFile(routeFile) //nolint:gosec // G304: the kernel's route table
	if err != nil {
		return netip.Addr{}, fmt.Errorf("read the route table: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines[min(1, len(lines)):] {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}
		ip, err := strconv.ParseUint(fields[2], 16, 32)
		if err != nil || ip == 0 {
			// A default route with no gateway (a point-to-point link):
			// another may have one.
			continue
		}
		return netip.AddrFrom4([4]byte{byte(ip), byte(ip >> 8), byte(ip >> 16), byte(ip >> 24)}), nil
	}
	return netip.Addr{}, errors.New("the route table has no default gateway")
}

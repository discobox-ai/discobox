package poolruntime

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/discobox-ai/vm/pkg/machine"
)

// A held driver's pool machine keeps a network device for its proxy's way
// out (vz's NAT, libkrun's passt), and whatever else shares it could reach
// what the pool listens on: the registry, the BuildKit mediator, the proxy's
// tunnel listener. Every path into such a machine is vsock, so the first step
// of making it a pool's box fences that device (ADR 26-11-11-963 §6, keeping
// ADR 26-10-31-206 §1's step): a rule set that admits nothing arriving on it
// but replies to the machine's own connections, loaded by the base's
// nftables.service before the network on every boot, and a drop-in that
// holds every pool unit to that service. Nothing of the pool is placed until
// the rule set reads back as written.

const (
	// FenceRulesFile is where the base's nftables.service reads its rule
	// set on every boot, ordered before network-pre.target.
	FenceRulesFile = "/etc/nftables.conf"
	// FenceDropIn holds every pool unit (each is discobox-*.service) to
	// nftables.service, as a prefix drop-in, so no boot starts one without
	// the rule set and a unit the pool bundle adds later is held too. The
	// bundle's own units do not change (ADR 26-10-27-767).
	FenceDropIn = "/etc/systemd/system/discobox-.service.d/discobox-fence.conf"
	// fenceTable is the rule set's one table, the provider's own: loading it
	// replaces it alone, never another's, such as BuildKit's CNI rules.
	fenceTable = "discobox_fence"
)

// fenceDropInData requires nftables.service and orders the unit after it: a
// rule set that does not load fails the service, and the unit with it.
const fenceDropInData = `# Placed by discobox (ADR 26-11-11-963 §6): no pool unit starts without the
# network fence in ` + FenceRulesFile + `.
[Unit]
Requires=nftables.service
After=nftables.service
`

// uplinkCommand prints the machine's network devices, one per line: each
// that is a device of the machine's (one with a device link), not a bridge or
// veth the pool makes, nor loopback. Which name the driver gives it (libkrun's
// eth0, vz's enp0s1) is learned here, not assumed. It fails, saying so, when
// the base lacks what the fence needs (ADR 26-10-31-206 §1's base contract).
const uplinkCommand = `command -v nft >/dev/null || { echo "the machine's base has no nft" >&2; exit 1; }
systemctl is-enabled --quiet nftables.service || { echo "the machine's base does not enable nftables.service" >&2; exit 1; }
for dev in /sys/class/net/*; do
	[ -e "$dev/device" ] && echo "${dev##*/}"
done
exit 0`

// loadFenceCommand loads the rule set as the base's unit does, makes sure the
// unit is active, so the drop-in's requirement holds now and not only at the
// next boot, and prints the table as the kernel holds it. Starting an active
// unit does nothing, so a pool provisioned again restarts nothing.
const loadFenceCommand = `systemctl daemon-reload
nft -f ` + FenceRulesFile + `
systemctl start nftables.service
nft list table inet ` + fenceTable

// deviceName is what a network device's name may be: the kernel's own limit
// of fifteen bytes, in the characters any driver names one with.
var deviceName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)

// fenceRules is the rule set that fences uplinks: on each, input and forward
// admit only replies to the machine's connections, and IPv6 neighbour and
// router discovery, without which the device has no IPv6 way out. Anything on
// loopback, or on a bridge the pool makes for its builds, is not touched.
// Each device has a rule of its own, not a set, so the kernel lists the rules
// in the order they are written and the read back compares as text.
//
// The table is declared, deleted and made in one transaction, so a load
// replaces the provider's table alone, atomically.
func fenceRules(uplinks []string) string {
	var input, forward strings.Builder
	for _, dev := range uplinks {
		fmt.Fprintf(&input, "\t\tiifname %q ct state established,related accept\n", dev)
		fmt.Fprintf(&input, "\t\tiifname %q icmpv6 type { nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept\n", dev)
		fmt.Fprintf(&input, "\t\tiifname %q drop\n", dev)
		fmt.Fprintf(&forward, "\t\tiifname %q ct state established,related accept\n", dev)
		fmt.Fprintf(&forward, "\t\tiifname %q drop\n", dev)
	}
	return fmt.Sprintf(`#!/usr/sbin/nft -f
# Placed by discobox (ADR 26-11-11-963 §6): every path into this pool machine
# is vsock, so nothing arriving on its network device is admitted but replies.
table inet %[1]s
delete table inet %[1]s
%[2]s`, fenceTable, fenceTableRules(input.String(), forward.String()))
}

// fenceTableRules is the table as the rule set writes it and as nft lists it
// back.
func fenceTableRules(input, forward string) string {
	return `table inet ` + fenceTable + ` {
	chain input {
		type filter hook input priority filter; policy accept;
` + input + `	}

	chain forward {
		type filter hook forward priority filter; policy accept;
` + forward + `	}
}
`
}

// FenceBox fences the network of the running box guest, a held driver's pool
// machine (ADR 26-11-11-963 §6): it learns the machine's network devices,
// places the rule set and the pool units' drop-in, loads the rule set and
// reads it back. Nothing else is placed, and an error means the box is not
// fenced, and must not be made the pool's. It is idempotent.
func FenceBox(ctx context.Context, guest machine.Guest) error {
	out, err := runGuest(ctx, guest, uplinkCommand)
	if err != nil {
		return fmt.Errorf("fence the pool machine's network: %w", err)
	}
	var uplinks []string
	for _, dev := range strings.Fields(out) {
		if !deviceName.MatchString(dev) {
			return fmt.Errorf("fence the pool machine's network: the machine names a network device %q", dev)
		}
		uplinks = append(uplinks, dev)
	}
	if len(uplinks) == 0 {
		return fmt.Errorf("fence the pool machine's network: the machine has no network device")
	}
	rules := fenceRules(uplinks)
	if err := PlaceBoxFiles(ctx, guest,
		BoxFile{Path: FenceRulesFile, Data: []byte(rules)},
		BoxFile{Path: FenceDropIn, Data: []byte(fenceDropInData)},
	); err != nil {
		return fmt.Errorf("place the pool machine's network fence: %w", err)
	}
	listed, err := runGuest(ctx, guest, loadFenceCommand)
	if err != nil {
		return fmt.Errorf("load the pool machine's network fence: %w", err)
	}
	_, want, _ := strings.Cut(rules, "delete table inet "+fenceTable+"\n")
	if !sameRules(listed, want) {
		return fmt.Errorf("the pool machine's network fence does not read back as loaded: it lists\n%s", strings.TrimSpace(listed))
	}
	return nil
}

// sameRules compares two listings of a table line by line, ignoring
// indentation and blank lines.
func sameRules(a, b string) bool {
	return slices.Equal(ruleLines(a), ruleLines(b))
}

func ruleLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// runGuest runs script under sh -e in guest and answers its output, or an
// error carrying what it wrote last to stderr.
func runGuest(ctx context.Context, guest machine.Guest, script string) (string, error) {
	var stdout, stderr strings.Builder
	code, err := machine.Run(ctx, guest, machine.ExecRequest{Argv: []string{"sh", "-ec", script}}, nil, &stdout, &stderr)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("exited with status %d: %s", code, lastLines(strings.TrimSpace(stderr.String()), 20))
	}
	return stdout.String(), nil
}
